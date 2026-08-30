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
	requested, err := coordinator.Request(ctx, validRequest())
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
	reused, err := reopened.Request(ctx, validRequest())
	if err != nil {
		t.Fatalf("request completed identity after restart: %v", err)
	}
	if !reused.Reused || reused.Build.ID != completed.ID {
		t.Fatalf("reused result = %#v, want build %q", reused, completed.ID)
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
	arm.Integration = publicbuild.IntegrationBuildKit
	arm.Platform = publicbuild.PlatformLinuxARM64
	arm.Target = "arm-runtime"
	first, err := coordinator.Request(ctx, arm)
	if err != nil {
		t.Fatalf("request first build: %v", err)
	}
	amd := validRequest()
	amd.Target = "amd-test"
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
	armLease, err := reopened.LeaseNext(ctx, localFakeWorker("arm", []publicbuild.Integration{publicbuild.IntegrationBuildKit}, []publicbuild.Platform{publicbuild.PlatformLinuxARM64}))
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
	failedRequest.Target = "failed"
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
	cancelledRequest.Target = "cancelled"
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
		request.Target = "sqlite-race-" + time.Unix(0, int64(attempt)).Format("150405.000000000")
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
		request.Target = "cross-race-" + time.Unix(0, int64(attempt)).Format("150405.000000000")
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
	})
	if err != nil {
		t.Fatalf("open SQLite coordinator: %v", err)
	}
	return coordinator
}
