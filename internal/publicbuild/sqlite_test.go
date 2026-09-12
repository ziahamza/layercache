package publicbuild_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/publicbuild"
)

func TestSQLiteCoordinatorPersistsCompletedBuildLogsAndDeduplication(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "public-builds.db")
	coordinator := openSQLiteCoordinator(t, path)
	request := validRequest()
	request.Inputs = []publicbuild.DeclaredInput{{Name: "compatibility", Value: "linux-amd64-node@24"}, {Name: "feature", Value: "enabled"}, {Name: "runtime", Value: "node@24"}}
	requested, err := coordinator.Request(ctx, request)
	if err != nil {
		t.Fatalf("request Public Build: %v", err)
	}
	lease, err := coordinator.LeaseNext(ctx, localFakeWorker("worker", allIntegrations(), allPlatforms()))
	if err != nil {
		t.Fatalf("lease Public Build: %v", err)
	}
	if err := coordinator.AppendLog(ctx, lease, "using secret-token"); err != nil {
		t.Fatalf("append log: %v", err)
	}
	wantPublication := testPublication("archive")
	wantPublication.ProducerDuration = 17 * time.Second
	completed, err := coordinator.Complete(ctx, lease, wantPublication)
	if err != nil {
		t.Fatalf("complete Public Build: %v", err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatalf("close coordinator: %v", err)
	}

	reopened := openSQLiteCoordinator(t, path)
	defer reopened.Close()
	got, err := reopened.Inspect(ctx, requested.Build.ID)
	if err != nil {
		t.Fatalf("inspect after restart: %v", err)
	}
	if !reflect.DeepEqual(got, completed) {
		t.Fatalf("persisted build = %#v, want %#v", got, completed)
	}
	logs, err := reopened.Logs(ctx, requested.Build.ID)
	if err != nil {
		t.Fatalf("logs after restart: %v", err)
	}
	if len(logs) != 1 || logs[0].Sequence != 1 || logs[0].Message != "using [REDACTED]" {
		t.Fatalf("persisted logs = %#v", logs)
	}
	reused, err := reopened.Request(ctx, request)
	if err != nil {
		t.Fatalf("request completed identity after restart: %v", err)
	}
	if !reused.Reused || reused.Build.ID != completed.ID {
		t.Fatalf("reused result = %#v, want build %q", reused, completed.ID)
	}
}

func TestSQLiteCoordinatorQueuesRepairForUnavailableSucceededPublication(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	lookupErr := error(publicbuild.ErrPublicationNotFound)
	config := testCoordinatorConfig(func() time.Time { return now })
	config.Publications = publicbuild.PublicationIndexFunc(func(context.Context, publicbuild.BuildRequest) (publicbuild.ExistingPublication, error) {
		return publicbuild.ExistingPublication{}, lookupErr
	})
	coordinator, err := publicbuild.OpenSQLiteCoordinator(filepath.Join(t.TempDir(), "public-builds.db"), config)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	request := validRequest()
	first, err := coordinator.Request(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := coordinator.LeaseNext(ctx, localFakeWorker("worker", allIntegrations(), allPlatforms()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Complete(ctx, lease, testPublication("trusted-output")); err != nil {
		t.Fatal(err)
	}

	lookupErr = publicbuild.ErrPublicationUnavailable
	repair, err := coordinator.Request(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if repair.Reused || repair.Build.State != publicbuild.StateQueued || repair.Build.ID == first.Build.ID {
		t.Fatalf("unavailable publication repair = %#v", repair)
	}
	retired, err := coordinator.Inspect(ctx, first.Build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retired.State != publicbuild.StateFailed || retired.Publication != nil || retired.Failure == "" {
		t.Fatalf("retired completed build = %#v", retired)
	}
}

func TestSQLiteCoordinatorRecoversRunningBuildAndRejectsStaleLease(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "public-builds.db")
	coordinator := openSQLiteCoordinator(t, path)
	requested, err := coordinator.Request(ctx, validRequest())
	if err != nil {
		t.Fatalf("request Public Build: %v", err)
	}
	staleLease, err := coordinator.LeaseNext(ctx, localFakeWorker("worker-before-restart", allIntegrations(), allPlatforms()))
	if err != nil {
		t.Fatalf("lease Public Build: %v", err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatalf("close coordinator: %v", err)
	}

	reopened := openSQLiteCoordinator(t, path)
	defer reopened.Close()
	recovered, err := reopened.Inspect(ctx, requested.Build.ID)
	if err != nil {
		t.Fatalf("inspect recovered build: %v", err)
	}
	if recovered.State != publicbuild.StateQueued || recovered.WorkerID != "" || !recovered.StartedAt.IsZero() {
		t.Fatalf("recovered build = %#v, want clean queued state", recovered)
	}
	if err := reopened.AppendLog(ctx, staleLease, "too late"); !errors.Is(err, publicbuild.ErrLeaseLost) {
		t.Fatalf("stale append error = %v, want ErrLeaseLost", err)
	}
	if _, err := reopened.Complete(ctx, staleLease, testPublication("too-late")); !errors.Is(err, publicbuild.ErrLeaseLost) {
		t.Fatalf("stale completion error = %v, want ErrLeaseLost", err)
	}

	freshLease, err := reopened.LeaseNext(ctx, localFakeWorker("worker-after-restart", allIntegrations(), allPlatforms()))
	if err != nil {
		t.Fatalf("lease recovered Public Build: %v", err)
	}
	if freshLease.Build.ID != requested.Build.ID || freshLease.Token == staleLease.Token {
		t.Fatalf("fresh lease = %#v, stale lease = %#v", freshLease, staleLease)
	}
	if _, err := reopened.Complete(ctx, freshLease, testPublication("archive")); err != nil {
		t.Fatalf("complete recovered Public Build: %v", err)
	}
}

func TestSQLiteLeaseNextUsesExactRecipeCapabilities(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	coordinator := openSQLiteCoordinator(t, filepath.Join(t.TempDir(), "public-builds.db"))
	defer coordinator.Close()
	unsupported := validRequest()
	unsupported.Target = "@acme/widgets#unsupported"
	unsupported.RecipeDigest = "sha256:" + strings.Repeat("c", 64)
	if _, err := coordinator.Request(ctx, unsupported); err != nil {
		t.Fatal(err)
	}
	supported := validRequest()
	supported.Target = "@acme/widgets#supported"
	queued, err := coordinator.Request(ctx, supported)
	if err != nil {
		t.Fatal(err)
	}
	worker := localFakeWorker("recipe-worker", allIntegrations(), []publicbuild.Platform{publicbuild.PlatformLinuxAMD64})
	worker.Supported.Recipes = []publicbuild.WorkerRecipeCapability{{
		Integration: supported.Integration, Target: supported.Target, RecipeDigest: supported.RecipeDigest,
	}}
	lease, err := coordinator.LeaseNext(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Build.ID != queued.Build.ID {
		t.Fatalf("leased %q, want exact recipe-compatible build %q", lease.Build.ID, queued.Build.ID)
	}
}

func TestSQLiteCoordinatorPersistsNeitherRejectedSecretsNorRawLeaseCredentials(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	directory := t.TempDir()
	path := filepath.Join(directory, "public-builds.db")
	coordinator := openSQLiteCoordinator(t, path)
	secret := "credential-marker-that-must-never-reach-sqlite"
	rejected := validRequest()
	rejected.Secrets = []string{secret}
	if _, err := coordinator.Request(ctx, rejected); !errors.Is(err, publicbuild.ErrRejected) {
		t.Fatalf("secret-bearing request error = %v, want ErrRejected", err)
	}
	if _, err := coordinator.Request(ctx, validRequest()); err != nil {
		t.Fatalf("request Public Build: %v", err)
	}
	lease, err := coordinator.LeaseNext(ctx, localFakeWorker("worker", allIntegrations(), allPlatforms()))
	if err != nil {
		t.Fatalf("lease Public Build: %v", err)
	}
	if err := coordinator.AppendLog(ctx, lease, "worker echoed "+lease.Token); err != nil {
		t.Fatalf("append lease credential to sanitized log: %v", err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatalf("close coordinator: %v", err)
	}

	files, err := filepath.Glob(path + "*")
	if err != nil {
		t.Fatalf("list SQLite files: %v", err)
	}
	for _, file := range files {
		contents, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", filepath.Base(file), err)
		}
		if strings.Contains(string(contents), secret) {
			t.Fatalf("rejected secret was persisted in %s", filepath.Base(file))
		}
		if strings.Contains(string(contents), lease.Token) {
			t.Fatalf("raw lease credential was persisted in %s", filepath.Base(file))
		}
	}
}

func TestSQLiteCoordinatorPreservesFIFOCompatibilityAcrossRestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "public-builds.db")
	coordinator := openSQLiteCoordinator(t, path)
	arm := validRequest()
	arm.Integration = publicbuild.IntegrationActions
	arm.Platform = publicbuild.PlatformLinuxARM64
	arm.Inputs = actionsPublicInputs("linux-arm64-node@24")
	arm.Target = ".github/workflows/public-cache.yml#arm-runtime"
	first, err := coordinator.Request(ctx, arm)
	if err != nil {
		t.Fatalf("request first build: %v", err)
	}
	amd := validRequest()
	amd.Target = "@acme/widgets#amd-test"
	second, err := coordinator.Request(ctx, amd)
	if err != nil {
		t.Fatalf("request second build: %v", err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatalf("close coordinator: %v", err)
	}

	reopened := openSQLiteCoordinator(t, path)
	defer reopened.Close()
	amdLease, err := reopened.LeaseNext(ctx, localFakeWorker("amd", []publicbuild.Integration{publicbuild.IntegrationTurbo}, []publicbuild.Platform{publicbuild.PlatformLinuxAMD64}))
	if err != nil {
		t.Fatalf("lease compatible second build: %v", err)
	}
	if amdLease.Build.ID != second.Build.ID {
		t.Fatalf("leased build = %q, want compatible build %q", amdLease.Build.ID, second.Build.ID)
	}
	armLease, err := reopened.LeaseNext(ctx, localFakeWorker("arm", []publicbuild.Integration{publicbuild.IntegrationActions}, []publicbuild.Platform{publicbuild.PlatformLinuxARM64}))
	if err != nil {
		t.Fatalf("lease compatible first build: %v", err)
	}
	if armLease.Build.ID != first.Build.ID {
		t.Fatalf("leased build = %q, want earlier build %q", armLease.Build.ID, first.Build.ID)
	}
}

func TestSQLiteCoordinatorKeepsFailedAndCancelledIdentitiesRetryableAcrossRestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "public-builds.db")
	coordinator := openSQLiteCoordinator(t, path)
	failedRequest := validRequest()
	failedRequest.Target = "@acme/widgets#failed"
	failed, err := coordinator.Request(ctx, failedRequest)
	if err != nil {
		t.Fatalf("request failed candidate: %v", err)
	}
	failedLease, err := coordinator.LeaseNext(ctx, localFakeWorker("worker", allIntegrations(), allPlatforms()))
	if err != nil {
		t.Fatalf("lease failed candidate: %v", err)
	}
	if _, err := coordinator.Fail(ctx, failedLease, "secret-token exploded"); err != nil {
		t.Fatalf("fail candidate: %v", err)
	}
	cancelledRequest := validRequest()
	cancelledRequest.Target = "@acme/widgets#cancelled"
	cancelled, err := coordinator.Request(ctx, cancelledRequest)
	if err != nil {
		t.Fatalf("request cancelled candidate: %v", err)
	}
	if _, err := coordinator.Cancel(ctx, cancelled.Build.ID); err != nil {
		t.Fatalf("cancel candidate: %v", err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatalf("close coordinator: %v", err)
	}

	reopened := openSQLiteCoordinator(t, path)
	defer reopened.Close()
	failedRetry, err := reopened.Request(ctx, failedRequest)
	if err != nil {
		t.Fatalf("retry failed identity: %v", err)
	}
	if failedRetry.Reused || failedRetry.Build.ID == failed.Build.ID {
		t.Fatalf("failed retry = %#v", failedRetry)
	}
	cancelledRetry, err := reopened.Request(ctx, cancelledRequest)
	if err != nil {
		t.Fatalf("retry cancelled identity: %v", err)
	}
	if cancelledRetry.Reused || cancelledRetry.Build.ID == cancelled.Build.ID {
		t.Fatalf("cancelled retry = %#v", cancelledRetry)
	}
	inspectedFailure, err := reopened.Inspect(ctx, failed.Build.ID)
	if err != nil {
		t.Fatalf("inspect failure: %v", err)
	}
	if strings.Contains(inspectedFailure.Failure, "secret-token") || inspectedFailure.Failure != "[REDACTED] exploded" {
		t.Fatalf("persisted failure was not sanitized: %q", inspectedFailure.Failure)
	}
}

func TestSQLiteCoordinatorConcurrentRequestAndLeaseClaimsAreAtomic(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	coordinator := openSQLiteCoordinator(t, filepath.Join(t.TempDir(), "public-builds.db"))
	defer coordinator.Close()

	const requesters = 24
	results := make(chan publicbuild.RequestResult, requesters)
	errorsSeen := make(chan error, requesters)
	var requests sync.WaitGroup
	for range requesters {
		requests.Add(1)
		go func() {
			defer requests.Done()
			result, err := coordinator.Request(ctx, validRequest())
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
	newBuilds := 0
	buildID := ""
	for result := range results {
		if !result.Reused {
			newBuilds++
		}
		if buildID == "" {
			buildID = result.Build.ID
		} else if result.Build.ID != buildID {
			t.Fatalf("deduplicated id = %q, want %q", result.Build.ID, buildID)
		}
	}
	if newBuilds != 1 {
		t.Fatalf("new builds = %d, want 1", newBuilds)
	}

	const workers = 16
	leases := make(chan publicbuild.Lease, workers)
	leaseErrors := make(chan error, workers)
	var claims sync.WaitGroup
	for index := range workers {
		claims.Add(1)
		go func() {
			defer claims.Done()
			lease, err := coordinator.LeaseNext(ctx, localFakeWorker("worker-"+string(rune('a'+index)), allIntegrations(), allPlatforms()))
			if err != nil {
				leaseErrors <- err
				return
			}
			leases <- lease
		}()
	}
	claims.Wait()
	close(leases)
	close(leaseErrors)
	claimed := 0
	for range leases {
		claimed++
	}
	noWork := 0
	for err := range leaseErrors {
		if !errors.Is(err, publicbuild.ErrNoWork) {
			t.Fatalf("lease claim error = %v", err)
		}
		noWork++
	}
	if claimed != 1 || noWork != workers-1 {
		t.Fatalf("claimed = %d, no work = %d", claimed, noWork)
	}
}

func TestSQLiteCoordinatorLeaseClaimIsAtomicAcrossDatabaseConnections(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "public-builds.db")
	first := openSQLiteCoordinator(t, path)
	defer first.Close()
	second := openSQLiteCoordinator(t, path)
	defer second.Close()
	if _, err := first.Request(ctx, validRequest()); err != nil {
		t.Fatalf("request Public Build: %v", err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	claim := func(coordinator *publicbuild.SQLiteCoordinator, workerID string) {
		<-start
		_, err := coordinator.LeaseNext(ctx, localFakeWorker(workerID, allIntegrations(), allPlatforms()))
		results <- err
	}
	go claim(first, "first-worker")
	go claim(second, "second-worker")
	close(start)
	firstErr := <-results
	secondErr := <-results
	succeeded := 0
	noWork := 0
	for _, err := range []error{firstErr, secondErr} {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, publicbuild.ErrNoWork):
			noWork++
		default:
			t.Fatalf("cross-connection lease error = %v", err)
		}
	}
	if succeeded != 1 || noWork != 1 {
		t.Fatalf("successful claims = %d, no work = %d", succeeded, noWork)
	}
}

func TestSQLiteCoordinatorCancelCompleteRaceIsFenced(t *testing.T) {
	ctx := context.Background()
	coordinator := openSQLiteCoordinator(t, filepath.Join(t.TempDir(), "public-builds.db"))
	defer coordinator.Close()
	worker := localFakeWorker("worker", allIntegrations(), allPlatforms())

	for attempt := range 30 {
		request := validRequest()
		request.Target = "@acme/widgets#sqlite-race-" + time.Unix(0, int64(attempt)).Format("150405.000000000")
		requested, err := coordinator.Request(ctx, request)
		if err != nil {
			t.Fatalf("request attempt %d: %v", attempt, err)
		}
		lease, err := coordinator.LeaseNext(ctx, worker)
		if err != nil {
			t.Fatalf("lease attempt %d: %v", attempt, err)
		}
		start := make(chan struct{})
		completeResult := make(chan error, 1)
		cancelResult := make(chan error, 1)
		go func() {
			<-start
			_, err := coordinator.Complete(ctx, lease, testPublication("output"))
			completeResult <- err
		}()
		go func() {
			<-start
			_, err := coordinator.Cancel(ctx, requested.Build.ID)
			cancelResult <- err
		}()
		close(start)
		completeErr := <-completeResult
		cancelErr := <-cancelResult
		got, err := coordinator.Inspect(ctx, requested.Build.ID)
		if err != nil {
			t.Fatalf("inspect attempt %d: %v", attempt, err)
		}
		switch {
		case cancelErr == nil:
			if !errors.Is(completeErr, publicbuild.ErrLeaseLost) || got.State != publicbuild.StateCancelled || got.Publication != nil {
				t.Fatalf("cancel won attempt %d: complete=%v build=%#v", attempt, completeErr, got)
			}
		case completeErr == nil:
			if !errors.Is(cancelErr, publicbuild.ErrInvalidTransition) || got.State != publicbuild.StateSucceeded || got.Publication == nil {
				t.Fatalf("complete won attempt %d: cancel=%v build=%#v", attempt, cancelErr, got)
			}
		default:
			t.Fatalf("neither transition won attempt %d: complete=%v cancel=%v", attempt, completeErr, cancelErr)
		}
	}
}

func TestSQLiteCoordinatorCancelCompleteRaceIsFencedAcrossDatabaseConnections(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "public-builds.db")
	first := openSQLiteCoordinator(t, path)
	defer first.Close()
	second := openSQLiteCoordinator(t, path)
	defer second.Close()
	worker := localFakeWorker("worker", allIntegrations(), allPlatforms())

	for attempt := range 20 {
		request := validRequest()
		request.Target = "@acme/widgets#cross-race-" + time.Unix(0, int64(attempt)).Format("150405.000000000")
		requested, err := first.Request(ctx, request)
		if err != nil {
			t.Fatalf("request attempt %d: %v", attempt, err)
		}
		lease, err := first.LeaseNext(ctx, worker)
		if err != nil {
			t.Fatalf("lease attempt %d: %v", attempt, err)
		}
		start := make(chan struct{})
		completeResult := make(chan error, 1)
		cancelResult := make(chan error, 1)
		go func() {
			<-start
			_, err := first.Complete(ctx, lease, testPublication("output"))
			completeResult <- err
		}()
		go func() {
			<-start
			_, err := second.Cancel(ctx, requested.Build.ID)
			cancelResult <- err
		}()
		close(start)
		completeErr := <-completeResult
		cancelErr := <-cancelResult
		got, err := first.Inspect(ctx, requested.Build.ID)
		if err != nil {
			t.Fatalf("inspect attempt %d: %v", attempt, err)
		}
		switch {
		case cancelErr == nil:
			if !errors.Is(completeErr, publicbuild.ErrLeaseLost) || got.State != publicbuild.StateCancelled || got.Publication != nil {
				t.Fatalf("cancel won attempt %d: complete=%v build=%#v", attempt, completeErr, got)
			}
		case completeErr == nil:
			if !errors.Is(cancelErr, publicbuild.ErrInvalidTransition) || got.State != publicbuild.StateSucceeded || got.Publication == nil {
				t.Fatalf("complete won attempt %d: cancel=%v build=%#v", attempt, cancelErr, got)
			}
		default:
			t.Fatalf("neither transition won attempt %d: complete=%v cancel=%v", attempt, completeErr, cancelErr)
		}
	}
}

func TestSQLiteCoordinatorRecoversExpiredLeaseWhileRunning(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.August, 31, 10, 0, 0, 0, time.UTC)
	clock := now
	config := testCoordinatorConfig(func() time.Time { return clock })
	coordinator, err := publicbuild.OpenSQLiteCoordinator(filepath.Join(t.TempDir(), "public-builds.db"), config)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	if _, err := coordinator.Request(ctx, validRequest()); err != nil {
		t.Fatal(err)
	}
	worker := localFakeWorker("worker", allIntegrations(), allPlatforms())
	stale, err := coordinator.LeaseNext(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	clock = now.Add(3 * time.Minute)
	fresh, err := coordinator.LeaseNext(ctx, worker)
	if err != nil {
		t.Fatalf("recover expired lease: %v", err)
	}
	if fresh.Build.ID != stale.Build.ID || fresh.Token == stale.Token {
		t.Fatalf("fresh lease = %#v, stale lease = %#v", fresh, stale)
	}
	if err := coordinator.AppendLog(ctx, stale, "too late"); !errors.Is(err, publicbuild.ErrLeaseLost) {
		t.Fatalf("stale lease append = %v, want ErrLeaseLost", err)
	}
	if _, err := coordinator.BeginLeasedPublication(ctx, stale); !errors.Is(err, publicbuild.ErrPublicationLost) {
		t.Fatalf("stale leased publication = %v, want ErrPublicationLost", err)
	}
	if _, err := coordinator.Cancel(ctx, fresh.Build.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.BeginLeasedPublication(ctx, fresh); !errors.Is(err, publicbuild.ErrPublicationLost) {
		t.Fatalf("cancelled leased publication = %v, want ErrPublicationLost", err)
	}
}

func TestSQLitePublicationPermitFencesCancellationAndWorkerFailure(t *testing.T) {
	ctx := context.Background()
	coordinator := openSQLiteCoordinator(t, filepath.Join(t.TempDir(), "public-builds.db"))
	defer coordinator.Close()
	requested, err := coordinator.Request(ctx, validRequest())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := coordinator.LeaseNext(ctx, localFakeWorker("worker", allIntegrations(), allPlatforms()))
	if err != nil {
		t.Fatal(err)
	}
	permit, err := coordinator.BeginLeasedPublication(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Cancel(ctx, requested.Build.ID); !errors.Is(err, publicbuild.ErrInvalidTransition) {
		t.Fatalf("cancel after publication began = %v, want ErrInvalidTransition", err)
	}
	if _, err := coordinator.Fail(ctx, lease, "late worker failure"); !errors.Is(err, publicbuild.ErrLeaseLost) {
		t.Fatalf("worker failure after publication began = %v, want ErrLeaseLost", err)
	}
	completed, err := coordinator.CommitPublication(ctx, permit, testPublication("trusted-output"))
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != publicbuild.StateSucceeded || completed.Publication == nil {
		t.Fatalf("completed build = %#v", completed)
	}
}

func openSQLiteCoordinator(t *testing.T, path string) *publicbuild.SQLiteCoordinator {
	t.Helper()
	coordinator, err := publicbuild.OpenSQLiteCoordinator(path, publicbuild.Config{
		AllowlistedRepositories: []string{"https://github.com/acme/widgets"},
		Limits: publicbuild.Resources{
			CPUMillis:   4_000,
			MemoryBytes: 8 << 30,
			DiskBytes:   40 << 30,
			Timeout:     30 * time.Minute,
		},
		SanitizeLog: func(message string) string {
			return strings.ReplaceAll(message, "secret-token", "[REDACTED]")
		},
		SourcePolicy: publicbuild.SourcePolicyFunc(func(context.Context, string, string) error { return nil }),
		RecipePolicy: publicbuild.RecipePolicyFunc(func(context.Context, publicbuild.Integration, string, string) error { return nil }),
	})
	if err != nil {
		t.Fatalf("open SQLite coordinator: %v", err)
	}
	return coordinator
}
