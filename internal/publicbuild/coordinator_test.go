package publicbuild_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/publicbuild"
)

func TestRequestQueuesAllowlistedImmutableBuildAndDeduplicatesIdentity(t *testing.T) {
	t.Parallel()

	coordinator := newCoordinator(t)
	request := validRequest()

	first, err := coordinator.Request(context.Background(), request)
	if err != nil {
		t.Fatalf("request Public Build: %v", err)
	}
	if first.Reused {
		t.Fatal("first request was reported as reused")
	}
	if first.Build.ID == "" {
		t.Fatal("Public Build id is empty")
	}
	if first.Build.State != publicbuild.StateQueued {
		t.Fatalf("state = %q, want %q", first.Build.State, publicbuild.StateQueued)
	}
	if !reflect.DeepEqual(first.Build.Request, request) {
		t.Fatalf("request = %#v, want %#v", first.Build.Request, request)
	}
	if first.Build.RequestedAt.IsZero() {
		t.Fatal("requested timestamp is empty")
	}

	second, err := coordinator.Request(context.Background(), request)
	if err != nil {
		t.Fatalf("request duplicate Public Build: %v", err)
	}
	if !second.Reused {
		t.Fatal("duplicate request was queued again")
	}
	if second.Build.ID != first.Build.ID {
		t.Fatalf("duplicate id = %q, want %q", second.Build.ID, first.Build.ID)
	}
}

func TestNewCoordinatorRequiresAdmissionLimitsAllowlistAndLogSanitizer(t *testing.T) {
	t.Parallel()

	validConfig := publicbuild.Config{
		AllowlistedRepositories: []string{"https://github.com/acme/widgets"},
		Limits: publicbuild.Resources{
			CPUMillis:   4_000,
			MemoryBytes: 8 << 30,
			DiskBytes:   40 << 30,
			Timeout:     30 * time.Minute,
		},
		SanitizeLog: func(message string) string { return message },
	}
	tests := []struct {
		name   string
		mutate func(*publicbuild.Config)
	}{
		{
			name: "allowlist is empty",
			mutate: func(config *publicbuild.Config) {
				config.AllowlistedRepositories = nil
			},
		},
		{
			name: "allowlist contains a non-GitHub repository",
			mutate: func(config *publicbuild.Config) {
				config.AllowlistedRepositories = []string{"https://gitlab.com/acme/widgets"}
			},
		},
		{
			name: "resource limit is not positive",
			mutate: func(config *publicbuild.Config) {
				config.Limits.Timeout = 0
			},
		},
		{
			name: "sanitizer is absent",
			mutate: func(config *publicbuild.Config) {
				config.SanitizeLog = nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validConfig
			test.mutate(&config)
			if _, err := publicbuild.NewCoordinator(config); err == nil {
				t.Fatal("NewCoordinator succeeded with invalid configuration")
			}
		})
	}
}

func TestRequestRejectsUnsafeOrIncompleteBuildBeforeQueueing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*publicbuild.BuildRequest)
	}{
		{
			name: "repository is not HTTPS",
			mutate: func(request *publicbuild.BuildRequest) {
				request.Repository = "http://github.com/acme/widgets"
			},
		},
		{
			name: "repository is not GitHub",
			mutate: func(request *publicbuild.BuildRequest) {
				request.Repository = "https://gitlab.com/acme/widgets"
			},
		},
		{
			name: "repository is not allowlisted",
			mutate: func(request *publicbuild.BuildRequest) {
				request.Repository = "https://github.com/acme/private"
			},
		},
		{
			name: "mutable ref is present",
			mutate: func(request *publicbuild.BuildRequest) {
				request.Ref = "refs/heads/main"
			},
		},
		{
			name: "commit is not hexadecimal",
			mutate: func(request *publicbuild.BuildRequest) {
				request.Commit = strings.Repeat("z", 40)
			},
		},
		{
			name: "commit is not a complete identity",
			mutate: func(request *publicbuild.BuildRequest) {
				request.Commit = strings.Repeat("a", 39)
			},
		},
		{
			name: "integration is unsupported",
			mutate: func(request *publicbuild.BuildRequest) {
				request.Integration = "npm"
			},
		},
		{
			name: "target is empty",
			mutate: func(request *publicbuild.BuildRequest) {
				request.Target = ""
			},
		},
		{
			name: "target escapes its namespace",
			mutate: func(request *publicbuild.BuildRequest) {
				request.Target = "../build"
			},
		},
		{
			name: "recipe digest is incomplete",
			mutate: func(request *publicbuild.BuildRequest) {
				request.RecipeDigest = "sha256:" + strings.Repeat("b", 63)
			},
		},
		{
			name: "platform is unsupported",
			mutate: func(request *publicbuild.BuildRequest) {
				request.Platform = "darwin/arm64"
			},
		},
		{
			name: "secret is present",
			mutate: func(request *publicbuild.BuildRequest) {
				request.Secrets = []string{"TOKEN"}
			},
		},
		{
			name: "privileged mode is requested",
			mutate: func(request *publicbuild.BuildRequest) {
				request.Privileged = true
			},
		},
		{
			name: "host Docker socket is requested",
			mutate: func(request *publicbuild.BuildRequest) {
				request.HostDockerSocket = true
			},
		},
		{
			name: "CPU is zero",
			mutate: func(request *publicbuild.BuildRequest) {
				request.Resources.CPUMillis = 0
			},
		},
		{
			name: "CPU exceeds limit",
			mutate: func(request *publicbuild.BuildRequest) {
				request.Resources.CPUMillis = 4_001
			},
		},
		{
			name: "memory exceeds limit",
			mutate: func(request *publicbuild.BuildRequest) {
				request.Resources.MemoryBytes = (8 << 30) + 1
			},
		},
		{
			name: "disk exceeds limit",
			mutate: func(request *publicbuild.BuildRequest) {
				request.Resources.DiskBytes = (40 << 30) + 1
			},
		},
		{
			name: "timeout exceeds limit",
			mutate: func(request *publicbuild.BuildRequest) {
				request.Resources.Timeout = 30*time.Minute + time.Nanosecond
			},
		},
	}

	coordinator := newCoordinator(t)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validRequest()
			test.mutate(&request)
			_, err := coordinator.Request(context.Background(), request)
			if !errors.Is(err, publicbuild.ErrRejected) {
				t.Fatalf("error = %v, want ErrRejected", err)
			}
		})
	}

	accepted, err := coordinator.Request(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("request valid Public Build after rejections: %v", err)
	}
	if accepted.Build.ID != "public-build-1" {
		t.Fatalf("id = %q, want first queued id", accepted.Build.ID)
	}
}

func TestRequestCanonicalizesCompleteIdentity(t *testing.T) {
	t.Parallel()

	coordinator := newCoordinator(t)
	first := validRequest()
	first.Repository = "https://github.com/ACME/WIDGETS.git/"
	first.Commit = strings.ToUpper(first.Commit)
	first.RecipeDigest = "sha256:" + strings.ToUpper(strings.TrimPrefix(first.RecipeDigest, "sha256:"))
	firstResult, err := coordinator.Request(context.Background(), first)
	if err != nil {
		t.Fatalf("request canonical identity: %v", err)
	}

	secondResult, err := coordinator.Request(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("request equivalent identity: %v", err)
	}
	if !secondResult.Reused || secondResult.Build.ID != firstResult.Build.ID {
		t.Fatalf("equivalent identity = %#v, want reused build %q", secondResult, firstResult.Build.ID)
	}
}

func TestRequestAcceptsEverySupportedIntegrationPlatformAndCommitIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		integration publicbuild.Integration
		platform    publicbuild.Platform
		commit      string
	}{
		{"turbo linux amd64 SHA-1", publicbuild.IntegrationTurbo, publicbuild.PlatformLinuxAMD64, strings.Repeat("a", 40)},
		{"BuildKit linux arm64 SHA-256", publicbuild.IntegrationBuildKit, publicbuild.PlatformLinuxARM64, strings.Repeat("b", 64)},
		{"Actions linux amd64 SHA-1", publicbuild.IntegrationActions, publicbuild.PlatformLinuxAMD64, strings.Repeat("c", 40)},
	}
	coordinator := newCoordinator(t)
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validRequest()
			request.Integration = test.integration
			request.Platform = test.platform
			request.Commit = test.commit
			request.Target = fmt.Sprintf("target-%d", index)
			result, err := coordinator.Request(context.Background(), request)
			if err != nil {
				t.Fatalf("request supported Public Build: %v", err)
			}
			if result.Build.State != publicbuild.StateQueued {
				t.Fatalf("state = %q, want queued", result.Build.State)
			}
		})
	}
}

func TestWorkerLeasesCompatibleBuildAndCompletesWithSanitizedLogs(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	coordinator := newCoordinator(t)
	requested, err := coordinator.Request(ctx, validRequest())
	if err != nil {
		t.Fatalf("request Public Build: %v", err)
	}
	worker := localFakeWorker("worker-amd64", []publicbuild.Integration{
		publicbuild.IntegrationTurbo,
	}, []publicbuild.Platform{
		publicbuild.PlatformLinuxAMD64,
	})

	lease, err := coordinator.LeaseNext(ctx, worker)
	if err != nil {
		t.Fatalf("lease Public Build: %v", err)
	}
	if lease.Token == "" {
		t.Fatal("lease token is empty")
	}
	if lease.WorkerID != worker.WorkerID {
		t.Fatalf("lease worker = %q, want %q", lease.WorkerID, worker.WorkerID)
	}
	if lease.Build.ID != requested.Build.ID || lease.Build.State != publicbuild.StateRunning {
		t.Fatalf("leased build = %#v, want running build %q", lease.Build, requested.Build.ID)
	}
	if lease.LeasedAt.IsZero() || lease.Build.StartedAt.IsZero() {
		t.Fatal("lease did not record its start time")
	}

	if err := coordinator.AppendLog(ctx, lease, "downloading with secret-token"); err != nil {
		t.Fatalf("append worker log: %v", err)
	}
	logs, err := coordinator.Logs(ctx, requested.Build.ID)
	if err != nil {
		t.Fatalf("list worker logs: %v", err)
	}
	if len(logs) != 1 || logs[0].Sequence != 1 || logs[0].Message != "downloading with [REDACTED]" {
		t.Fatalf("logs = %#v, want one sequenced sanitized entry", logs)
	}
	if logs[0].Timestamp.IsZero() {
		t.Fatal("log timestamp is empty")
	}

	publication := publicbuild.Publication{
		Outputs: []publicbuild.OutputDescriptor{
			{
				Name:      "turbo-archive",
				Digest:    "sha256:" + strings.Repeat("c", 64),
				SizeBytes: 1234,
				MediaType: "application/vnd.layercache.turbo",
			},
		},
		ProducerDuration: 2 * time.Minute,
	}
	completed, err := coordinator.Complete(ctx, lease, publication)
	if err != nil {
		t.Fatalf("complete Public Build: %v", err)
	}
	if completed.State != publicbuild.StateSucceeded || completed.FinishedAt.IsZero() {
		t.Fatalf("completed build = %#v, want succeeded with finish time", completed)
	}
	if !reflect.DeepEqual(completed.Publication, &publication) {
		t.Fatalf("publication = %#v, want %#v", completed.Publication, publication)
	}

	inspected, err := coordinator.Inspect(ctx, requested.Build.ID)
	if err != nil {
		t.Fatalf("inspect completed Public Build: %v", err)
	}
	if !reflect.DeepEqual(inspected, completed) {
		t.Fatalf("inspected build = %#v, want %#v", inspected, completed)
	}
	reused, err := coordinator.Request(ctx, validRequest())
	if err != nil {
		t.Fatalf("request completed identity: %v", err)
	}
	if !reused.Reused || reused.Build.ID != completed.ID {
		t.Fatalf("completed duplicate = %#v, want reused build %q", reused, completed.ID)
	}

	if _, err := coordinator.Complete(ctx, lease, publication); !errors.Is(err, publicbuild.ErrLeaseLost) {
		t.Fatalf("second completion error = %v, want ErrLeaseLost", err)
	}
}

func TestLeaseNextPreservesQueueOrderAmongWorkerCompatibleBuilds(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	coordinator := newCoordinator(t)
	buildKitRequest := validRequest()
	buildKitRequest.Integration = publicbuild.IntegrationBuildKit
	buildKitRequest.Target = "runtime"
	buildKitRequest.Platform = publicbuild.PlatformLinuxARM64
	first, err := coordinator.Request(ctx, buildKitRequest)
	if err != nil {
		t.Fatalf("request BuildKit Public Build: %v", err)
	}
	turboRequest := validRequest()
	turboRequest.Target = "test"
	second, err := coordinator.Request(ctx, turboRequest)
	if err != nil {
		t.Fatalf("request Turbo Public Build: %v", err)
	}

	amd64Turbo := localFakeWorker("turbo-amd64", []publicbuild.Integration{
		publicbuild.IntegrationTurbo,
	}, []publicbuild.Platform{
		publicbuild.PlatformLinuxAMD64,
	})
	secondLease, err := coordinator.LeaseNext(ctx, amd64Turbo)
	if err != nil {
		t.Fatalf("lease compatible Turbo build: %v", err)
	}
	if secondLease.Build.ID != second.Build.ID {
		t.Fatalf("leased id = %q, want compatible queued id %q", secondLease.Build.ID, second.Build.ID)
	}

	arm64BuildKit := localFakeWorker("buildkit-arm64", []publicbuild.Integration{
		publicbuild.IntegrationBuildKit,
	}, []publicbuild.Platform{
		publicbuild.PlatformLinuxARM64,
	})
	firstLease, err := coordinator.LeaseNext(ctx, arm64BuildKit)
	if err != nil {
		t.Fatalf("lease earlier compatible BuildKit build: %v", err)
	}
	if firstLease.Build.ID != first.Build.ID {
		t.Fatalf("leased id = %q, want first queued id %q", firstLease.Build.ID, first.Build.ID)
	}

	if _, err := coordinator.LeaseNext(ctx, amd64Turbo); !errors.Is(err, publicbuild.ErrNoWork) {
		t.Fatalf("empty compatible queue error = %v, want ErrNoWork", err)
	}
}

func TestFailurePublishesNothingAndAllowsACompleteIdentityRetry(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	coordinator := newCoordinator(t)
	requested, err := coordinator.Request(ctx, validRequest())
	if err != nil {
		t.Fatalf("request Public Build: %v", err)
	}
	lease, err := coordinator.LeaseNext(ctx, localFakeWorker("worker", allIntegrations(), allPlatforms()))
	if err != nil {
		t.Fatalf("lease Public Build: %v", err)
	}

	failed, err := coordinator.Fail(ctx, lease, "executor exposed secret-token")
	if err != nil {
		t.Fatalf("fail Public Build: %v", err)
	}
	if failed.State != publicbuild.StateFailed || failed.Failure != "executor exposed [REDACTED]" {
		t.Fatalf("failed build = %#v, want sanitized failure", failed)
	}
	if failed.Publication != nil || failed.FinishedAt.IsZero() {
		t.Fatalf("failed build = %#v, want no publication and a finish time", failed)
	}

	retry, err := coordinator.Request(ctx, validRequest())
	if err != nil {
		t.Fatalf("retry failed identity: %v", err)
	}
	if retry.Reused || retry.Build.ID == requested.Build.ID {
		t.Fatalf("retry = %#v, want a newly queued build", retry)
	}
}

func TestAcceptedCancellationPreventsPublication(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	coordinator := newCoordinator(t)
	queued, err := coordinator.Request(ctx, validRequest())
	if err != nil {
		t.Fatalf("request queued Public Build: %v", err)
	}
	cancelled, err := coordinator.Cancel(ctx, queued.Build.ID)
	if err != nil {
		t.Fatalf("cancel queued Public Build: %v", err)
	}
	if cancelled.State != publicbuild.StateCancelled || cancelled.Publication != nil {
		t.Fatalf("cancelled queued build = %#v", cancelled)
	}
	if _, err := coordinator.LeaseNext(ctx, localFakeWorker("worker", allIntegrations(), allPlatforms())); !errors.Is(err, publicbuild.ErrNoWork) {
		t.Fatalf("lease after queued cancellation error = %v, want ErrNoWork", err)
	}

	retry, err := coordinator.Request(ctx, validRequest())
	if err != nil {
		t.Fatalf("request retry after cancellation: %v", err)
	}
	lease, err := coordinator.LeaseNext(ctx, localFakeWorker("worker", allIntegrations(), allPlatforms()))
	if err != nil {
		t.Fatalf("lease retry: %v", err)
	}
	cancelled, err = coordinator.Cancel(ctx, retry.Build.ID)
	if err != nil {
		t.Fatalf("cancel running Public Build: %v", err)
	}
	publication := publicbuild.Publication{
		Outputs: []publicbuild.OutputDescriptor{{
			Name:      "too-late",
			Digest:    "sha256:" + strings.Repeat("d", 64),
			SizeBytes: 1,
			MediaType: "application/octet-stream",
		}},
	}
	if _, err := coordinator.Complete(ctx, lease, publication); !errors.Is(err, publicbuild.ErrLeaseLost) {
		t.Fatalf("completion after accepted cancellation error = %v, want ErrLeaseLost", err)
	}
	inspected, err := coordinator.Inspect(ctx, retry.Build.ID)
	if err != nil {
		t.Fatalf("inspect cancelled Public Build: %v", err)
	}
	if inspected.State != publicbuild.StateCancelled || inspected.Publication != nil {
		t.Fatalf("build after late completion = %#v, want cancelled without publication", inspected)
	}
}

func TestInvalidCompletionAndTransitionsDoNotMutateRunningBuild(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	coordinator := newCoordinator(t)
	requested, err := coordinator.Request(ctx, validRequest())
	if err != nil {
		t.Fatalf("request Public Build: %v", err)
	}
	lease, err := coordinator.LeaseNext(ctx, localFakeWorker("worker", allIntegrations(), allPlatforms()))
	if err != nil {
		t.Fatalf("lease Public Build: %v", err)
	}

	invalidPublication := publicbuild.Publication{Outputs: []publicbuild.OutputDescriptor{{
		Name:      "archive",
		Digest:    "not-a-digest",
		SizeBytes: 42,
		MediaType: "application/octet-stream",
	}}}
	if _, err := coordinator.Complete(ctx, lease, invalidPublication); !errors.Is(err, publicbuild.ErrRejected) {
		t.Fatalf("invalid completion error = %v, want ErrRejected", err)
	}
	inspected, err := coordinator.Inspect(ctx, requested.Build.ID)
	if err != nil {
		t.Fatalf("inspect running Public Build: %v", err)
	}
	if inspected.State != publicbuild.StateRunning || inspected.Publication != nil {
		t.Fatalf("build after invalid completion = %#v, want unchanged running build", inspected)
	}

	forged := lease
	forged.Token = "forged"
	if _, err := coordinator.Fail(ctx, forged, "forged failure"); !errors.Is(err, publicbuild.ErrLeaseLost) {
		t.Fatalf("forged failure error = %v, want ErrLeaseLost", err)
	}
	if err := coordinator.AppendLog(ctx, forged, "forged log"); !errors.Is(err, publicbuild.ErrLeaseLost) {
		t.Fatalf("forged log error = %v, want ErrLeaseLost", err)
	}

	validPublication := testPublication("valid")
	succeeded, err := coordinator.Complete(ctx, lease, validPublication)
	if err != nil {
		t.Fatalf("complete Public Build: %v", err)
	}
	if _, err := coordinator.Cancel(ctx, succeeded.ID); !errors.Is(err, publicbuild.ErrInvalidTransition) {
		t.Fatalf("cancel succeeded build error = %v, want ErrInvalidTransition", err)
	}
}

func TestConcurrentRequestsDeduplicateACompleteIdentity(t *testing.T) {
	t.Parallel()

	coordinator := newCoordinator(t)
	const callers = 32
	results := make(chan publicbuild.RequestResult, callers)
	errorsSeen := make(chan error, callers)
	var start sync.WaitGroup
	start.Add(1)
	var callersDone sync.WaitGroup
	callersDone.Add(callers)
	for range callers {
		go func() {
			defer callersDone.Done()
			start.Wait()
			result, err := coordinator.Request(context.Background(), validRequest())
			if err != nil {
				errorsSeen <- err
				return
			}
			results <- result
		}()
	}
	start.Done()
	callersDone.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatalf("concurrent request: %v", err)
	}

	firstID := ""
	newBuilds := 0
	for result := range results {
		if firstID == "" {
			firstID = result.Build.ID
		}
		if result.Build.ID != firstID {
			t.Fatalf("concurrent request id = %q, want %q", result.Build.ID, firstID)
		}
		if !result.Reused {
			newBuilds++
		}
	}
	if newBuilds != 1 {
		t.Fatalf("new builds = %d, want exactly one", newBuilds)
	}
}

func TestCancelAndCompleteRaceNeverPublishesAfterAcceptedCancellation(t *testing.T) {
	ctx := context.Background()
	coordinator := newCoordinator(t)
	worker := localFakeWorker("worker", allIntegrations(), allPlatforms())
	const attempts = 50
	for attempt := range attempts {
		request := validRequest()
		request.Target = fmt.Sprintf("race-%d", attempt)
		requested, err := coordinator.Request(ctx, request)
		if err != nil {
			t.Fatalf("request attempt %d: %v", attempt, err)
		}
		lease, err := coordinator.LeaseNext(ctx, worker)
		if err != nil {
			t.Fatalf("lease attempt %d: %v", attempt, err)
		}

		start := make(chan struct{})
		completion := make(chan error, 1)
		cancellation := make(chan error, 1)
		go func() {
			<-start
			_, err := coordinator.Complete(ctx, lease, testPublication(fmt.Sprintf("output-%d", attempt)))
			completion <- err
		}()
		go func() {
			<-start
			_, err := coordinator.Cancel(ctx, requested.Build.ID)
			cancellation <- err
		}()
		close(start)
		completeErr := <-completion
		cancelErr := <-cancellation

		inspected, err := coordinator.Inspect(ctx, requested.Build.ID)
		if err != nil {
			t.Fatalf("inspect attempt %d: %v", attempt, err)
		}
		switch {
		case cancelErr == nil:
			if !errors.Is(completeErr, publicbuild.ErrLeaseLost) {
				t.Fatalf("attempt %d completion error = %v after accepted cancel, want ErrLeaseLost", attempt, completeErr)
			}
			if inspected.State != publicbuild.StateCancelled || inspected.Publication != nil {
				t.Fatalf("attempt %d build = %#v after accepted cancel", attempt, inspected)
			}
		case completeErr == nil:
			if !errors.Is(cancelErr, publicbuild.ErrInvalidTransition) {
				t.Fatalf("attempt %d cancellation error = %v after completion, want ErrInvalidTransition", attempt, cancelErr)
			}
			if inspected.State != publicbuild.StateSucceeded || inspected.Publication == nil {
				t.Fatalf("attempt %d build = %#v after completed publication", attempt, inspected)
			}
		default:
			t.Fatalf("attempt %d had neither accepted cancellation nor completion: cancel=%v complete=%v", attempt, cancelErr, completeErr)
		}
	}
}

func TestLocalFakeWorkerExecutesInProcessCallback(t *testing.T) {
	t.Parallel()

	build := publicbuild.Build{ID: "build-under-test"}
	written := ""
	worker := localFakeWorker("local-fake", allIntegrations(), allPlatforms())
	worker.ExecuteFunc = func(ctx context.Context, got publicbuild.Build, logs publicbuild.LogSink) (publicbuild.Publication, error) {
		if got.ID != build.ID {
			t.Fatalf("executed build id = %q, want %q", got.ID, build.ID)
		}
		if err := logs(ctx, "local output"); err != nil {
			return publicbuild.Publication{}, err
		}
		return testPublication("fake-output"), nil
	}
	publication, err := worker.Execute(context.Background(), build, func(_ context.Context, message string) error {
		written = message
		return nil
	})
	if err != nil {
		t.Fatalf("execute local fake: %v", err)
	}
	if written != "local output" || len(publication.Outputs) != 1 {
		t.Fatalf("fake result = %#v, log = %q", publication, written)
	}
}

func testPublication(name string) publicbuild.Publication {
	return publicbuild.Publication{Outputs: []publicbuild.OutputDescriptor{{
		Name:      name,
		Digest:    "sha256:" + strings.Repeat("e", 64),
		SizeBytes: 42,
		MediaType: "application/octet-stream",
	}}}
}

func localFakeWorker(id string, integrations []publicbuild.Integration, platforms []publicbuild.Platform) *publicbuild.LocalFakeWorker {
	return &publicbuild.LocalFakeWorker{
		WorkerID: id,
		Supported: publicbuild.WorkerCapabilities{
			Integrations: integrations,
			Platforms:    platforms,
		},
	}
}

func allIntegrations() []publicbuild.Integration {
	return []publicbuild.Integration{
		publicbuild.IntegrationTurbo,
		publicbuild.IntegrationBuildKit,
		publicbuild.IntegrationActions,
	}
}

func allPlatforms() []publicbuild.Platform {
	return []publicbuild.Platform{
		publicbuild.PlatformLinuxAMD64,
		publicbuild.PlatformLinuxARM64,
	}
}

func newCoordinator(t *testing.T) publicbuild.Coordinator {
	t.Helper()
	coordinator, err := publicbuild.NewCoordinator(publicbuild.Config{
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
		t.Fatalf("new coordinator: %v", err)
	}
	return coordinator
}

func validRequest() publicbuild.BuildRequest {
	return publicbuild.BuildRequest{
		Repository:   "https://github.com/acme/widgets",
		Commit:       strings.Repeat("a", 40),
		Integration:  publicbuild.IntegrationTurbo,
		Target:       "build",
		RecipeDigest: "sha256:" + strings.Repeat("b", 64),
		Platform:     publicbuild.PlatformLinuxAMD64,
		Resources: publicbuild.Resources{
			CPUMillis:   2_000,
			MemoryBytes: 4 << 30,
			DiskBytes:   20 << 30,
			Timeout:     10 * time.Minute,
		},
	}
}
