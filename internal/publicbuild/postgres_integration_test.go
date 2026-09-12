package publicbuild_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/publicbuild"
)

const publicBuildPostgresTestURL = "LAYER_CACHE_QA_POSTGRES_URL"

func TestPostgresCoordinatorMultiInstanceLifecycle(t *testing.T) {
	postgresURL := os.Getenv(publicBuildPostgresTestURL)
	if postgresURL == "" {
		t.Skip("set " + publicBuildPostgresTestURL + " to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	project := postgresTestProject(t)
	cleanupPostgresProject(t, postgresURL, project)
	configuration := testCoordinatorConfig(time.Now)
	configuration.SanitizeLog = func(message string) string {
		return strings.ReplaceAll(message, "secret-token", "[REDACTED]")
	}
	first := openPostgresCoordinator(t, ctx, postgresURL, project, configuration)
	defer first.Close()
	second := openPostgresCoordinator(t, ctx, postgresURL, project, configuration)
	defer second.Close()

	const requesters = 20
	request := validRequest()
	request.Inputs = []publicbuild.DeclaredInput{{Name: "runtime", Value: "node@24"}, {Name: "feature", Value: "enabled"}, {Name: "compatibility", Value: "linux-amd64-node@24"}}
	results := make(chan publicbuild.RequestResult, requesters)
	errorsSeen := make(chan error, requesters)
	var requests sync.WaitGroup
	for index := range requesters {
		requests.Add(1)
		go func() {
			defer requests.Done()
			coordinator := first
			if index%2 == 1 {
				coordinator = second
			}
			result, err := coordinator.Request(ctx, request)
			if err != nil {
				errorsSeen <- err
				return
			}
			results <- result
		}()
	}
	requests.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatalf("concurrent request: %v", err)
	}
	var buildID string
	newBuilds := 0
	for result := range results {
		if !result.Reused {
			newBuilds++
		}
		if buildID == "" {
			buildID = result.Build.ID
		} else if result.Build.ID != buildID {
			t.Fatalf("deduplicated build = %q, want %q", result.Build.ID, buildID)
		}
	}
	if newBuilds != 1 {
		t.Fatalf("new builds = %d, want 1", newBuilds)
	}

	worker := localFakeWorker("multi-instance-worker", allIntegrations(), allPlatforms())
	start := make(chan struct{})
	leaseResults := make(chan struct {
		lease publicbuild.Lease
		err   error
	}, 2)
	for _, coordinator := range []*publicbuild.PostgresCoordinator{first, second} {
		go func() {
			<-start
			lease, err := coordinator.LeaseNext(ctx, worker)
			leaseResults <- struct {
				lease publicbuild.Lease
				err   error
			}{lease: lease, err: err}
		}()
	}
	close(start)
	var lease publicbuild.Lease
	claimed := 0
	noWork := 0
	for range 2 {
		result := <-leaseResults
		switch {
		case result.err == nil:
			claimed++
			lease = result.lease
		case errors.Is(result.err, publicbuild.ErrNoWork):
			noWork++
		default:
			t.Fatalf("lease error: %v", result.err)
		}
	}
	if claimed != 1 || noWork != 1 || lease.Build.ID != buildID {
		t.Fatalf("claimed=%d noWork=%d lease=%#v", claimed, noWork, lease)
	}

	const logCount = 16
	var appends sync.WaitGroup
	appendErrors := make(chan error, logCount)
	for index := range logCount {
		appends.Add(1)
		go func() {
			defer appends.Done()
			coordinator := first
			if index%2 == 1 {
				coordinator = second
			}
			appendErrors <- coordinator.AppendLog(ctx, lease, "entry secret-token "+lease.Token)
		}()
	}
	appends.Wait()
	close(appendErrors)
	for err := range appendErrors {
		if err != nil {
			t.Fatalf("append concurrent log: %v", err)
		}
	}
	logs, err := second.Logs(ctx, buildID)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != logCount {
		t.Fatalf("logs = %d, want %d", len(logs), logCount)
	}
	for index, entry := range logs {
		if entry.Sequence != uint64(index+1) || entry.Message != "entry [REDACTED] [REDACTED]" {
			t.Fatalf("log %d = %#v", index, entry)
		}
	}

	forged := lease
	forged.Token = "forged-raw-lease-token"
	if _, err := second.Renew(ctx, forged); !errors.Is(err, publicbuild.ErrLeaseLost) {
		t.Fatalf("forged renewal = %v, want ErrLeaseLost", err)
	}
	permit, err := second.BeginLeasedPublication(ctx, lease)
	if err != nil {
		t.Fatalf("begin exact leased publication: %v", err)
	}
	if _, err := first.Cancel(ctx, buildID); !errors.Is(err, publicbuild.ErrInvalidTransition) {
		t.Fatalf("cancel after publication claim = %v, want ErrInvalidTransition", err)
	}
	credentialBearing := testPublication("credential-bearing-output")
	credentialBearing.Outputs[0].MediaType += "; token=" + permit.Token
	if _, err := first.CommitPublication(ctx, permit, credentialBearing); !errors.Is(err, publicbuild.ErrRejected) {
		t.Fatalf("credential-bearing publication = %v, want ErrRejected", err)
	}
	assertPostgresStoresOnlyTokenHashes(t, postgresURL, project, buildID, lease.Token, permit.Token)
	completed, err := first.CommitPublication(ctx, permit, testPublication("trusted-output"))
	if err != nil {
		t.Fatalf("commit trusted publication: %v", err)
	}
	if completed.State != publicbuild.StateSucceeded || completed.Publication == nil {
		t.Fatalf("completed build = %#v", completed)
	}
	if _, err := second.Fail(ctx, lease, "late failure"); !errors.Is(err, publicbuild.ErrLeaseLost) {
		t.Fatalf("late worker failure = %v, want ErrLeaseLost", err)
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openPostgresCoordinator(t, ctx, postgresURL, project, configuration)
	defer reopened.Close()
	stored, err := reopened.Inspect(ctx, buildID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != publicbuild.StateSucceeded || stored.Publication == nil || len(stored.Publication.Outputs) != 1 {
		t.Fatalf("stored build = %#v", stored)
	}
	status, err := reopened.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Succeeded != 1 || status.LatestBuildID != buildID || status.LatestState != publicbuild.StateSucceeded {
		t.Fatalf("status = %#v", status)
	}

	isolatedProject := project + "-isolated"
	cleanupPostgresProject(t, postgresURL, isolatedProject)
	isolated := openPostgresCoordinator(t, ctx, postgresURL, isolatedProject, configuration)
	defer isolated.Close()
	if _, err := isolated.Inspect(ctx, buildID); !errors.Is(err, publicbuild.ErrNotFound) {
		t.Fatalf("cross-project build inspection = %v, want ErrNotFound", err)
	}
	isolatedRequest, err := isolated.Request(ctx, validRequest())
	if err != nil {
		t.Fatal(err)
	}
	if isolatedRequest.Reused || isolatedRequest.Build.ID == buildID {
		t.Fatalf("isolated request = %#v, original build = %q", isolatedRequest, buildID)
	}
}

func TestPostgresCoordinatorRecoversExpiredLeaseAndPreservesLiveLeaseOnReopen(t *testing.T) {
	postgresURL := os.Getenv(publicBuildPostgresTestURL)
	if postgresURL == "" {
		t.Skip("set " + publicBuildPostgresTestURL + " to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	project := postgresTestProject(t)
	cleanupPostgresProject(t, postgresURL, project)
	now := time.Date(2026, time.August, 31, 12, 0, 0, 123, time.UTC)
	clock := now
	configuration := testCoordinatorConfig(func() time.Time { return clock })
	first := openPostgresCoordinator(t, ctx, postgresURL, project, configuration)
	defer first.Close()
	second := openPostgresCoordinator(t, ctx, postgresURL, project, configuration)
	defer second.Close()

	unsupported := validRequest()
	unsupported.Target = "@acme/widgets#unsupported"
	unsupported.RecipeDigest = "sha256:" + strings.Repeat("c", 64)
	if _, err := first.Request(ctx, unsupported); err != nil {
		t.Fatal(err)
	}
	supported := validRequest()
	supported.Target = "@acme/widgets#supported"
	queued, err := first.Request(ctx, supported)
	if err != nil {
		t.Fatal(err)
	}
	worker := localFakeWorker("recipe-worker", allIntegrations(), []publicbuild.Platform{publicbuild.PlatformLinuxAMD64})
	worker.Supported.Recipes = []publicbuild.WorkerRecipeCapability{{
		Integration: supported.Integration, Target: supported.Target, RecipeDigest: supported.RecipeDigest,
	}}
	stale, err := second.LeaseNext(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	if stale.Build.ID != queued.Build.ID {
		t.Fatalf("leased build %q, want recipe-compatible %q", stale.Build.ID, queued.Build.ID)
	}

	// Opening another process must not steal a still-live cloud lease.
	reopened := openPostgresCoordinator(t, ctx, postgresURL, project, configuration)
	if _, err := reopened.Renew(ctx, stale); err != nil {
		reopened.Close()
		t.Fatalf("renew live lease after reopen: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	clock = now.Add(3 * time.Minute)
	fresh, err := first.LeaseNext(ctx, worker)
	if err != nil {
		t.Fatalf("recover expired lease: %v", err)
	}
	if fresh.Build.ID != stale.Build.ID || fresh.Token == stale.Token {
		t.Fatalf("fresh lease = %#v, stale lease = %#v", fresh, stale)
	}
	if _, err := second.BeginLeasedPublication(ctx, stale); !errors.Is(err, publicbuild.ErrPublicationLost) {
		t.Fatalf("stale publication claim = %v, want ErrPublicationLost", err)
	}
	if _, err := second.Cancel(ctx, fresh.Build.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := first.BeginLeasedPublication(ctx, fresh); !errors.Is(err, publicbuild.ErrPublicationLost) {
		t.Fatalf("cancelled publication claim = %v, want ErrPublicationLost", err)
	}
}

func openPostgresCoordinator(
	t *testing.T,
	ctx context.Context,
	postgresURL string,
	project string,
	configuration publicbuild.Config,
) *publicbuild.PostgresCoordinator {
	t.Helper()
	coordinator, err := publicbuild.OpenPostgresCoordinator(ctx, postgresURL, project, configuration)
	if err != nil {
		t.Fatalf("open PostgreSQL coordinator: %v", err)
	}
	return coordinator
}

func postgresTestProject(t *testing.T) string {
	t.Helper()
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	return "publicbuild-test-" + hex.EncodeToString(random[:])
}

func cleanupPostgresProject(t *testing.T, postgresURL, project string) {
	t.Helper()
	database, err := sql.Open("pgx", postgresURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer database.Close()
		_, _ = database.ExecContext(context.Background(),
			`DELETE FROM layercache_public_builds_v1 WHERE project_id = $1`, project)
	})
}

func assertPostgresStoresOnlyTokenHashes(
	t *testing.T,
	postgresURL string,
	project string,
	buildID string,
	leaseToken string,
	publicationToken string,
) {
	t.Helper()
	database, err := sql.Open("pgx", postgresURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var leaseHash string
	var publicationHash string
	sequence, err := strconv.ParseInt(strings.TrimPrefix(buildID, "public-build-"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRowContext(context.Background(), `
		SELECT lease_token_hash, publication_token_hash
		FROM layercache_public_builds_v1 WHERE project_id = $1 AND sequence = $2`, project, sequence).Scan(
		&leaseHash, &publicationHash,
	); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"lease": leaseHash, "publication": publicationHash} {
		if value == "" || value == leaseToken || value == publicationToken || len(value) != 64 {
			t.Fatalf("stored %s credential = %q", name, value)
		}
	}
}
