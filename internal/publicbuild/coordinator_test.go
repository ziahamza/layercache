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
		SanitizeLog:  func(message string) string { return message },
		SourcePolicy: publicbuild.SourcePolicyFunc(func(context.Context, string, string) error { return nil }),
		RecipePolicy: publicbuild.RecipePolicyFunc(func(context.Context, publicbuild.Integration, string, string) error { return nil }),
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
	first.Inputs = []publicbuild.DeclaredInput{{Name: "runtime", Value: "node@24"}, {Name: "feature", Value: "enabled"}, {Name: "compatibility", Value: "linux-amd64-node@24"}}
	firstResult, err := coordinator.Request(context.Background(), first)
	if err != nil {
		t.Fatalf("request canonical identity: %v", err)
	}

	second := validRequest()
	second.Inputs = []publicbuild.DeclaredInput{{Name: "compatibility", Value: "linux-amd64-node@24"}, {Name: "feature", Value: "enabled"}, {Name: "runtime", Value: "node@24"}}
	secondResult, err := coordinator.Request(context.Background(), second)
	if err != nil {
		t.Fatalf("request equivalent identity: %v", err)
	}
	if !secondResult.Reused || secondResult.Build.ID != firstResult.Build.ID {
		t.Fatalf("equivalent identity = %#v, want reused build %q", secondResult, firstResult.Build.ID)
	}
	if got := firstResult.Build.Request.Inputs; !reflect.DeepEqual(got, second.Inputs) {
		t.Fatalf("canonical inputs = %#v, want %#v", got, second.Inputs)
	}
	changed := second
	changed.Inputs[2].Value = "node@22"
	changedResult, err := coordinator.Request(context.Background(), changed)
	if err != nil {
		t.Fatalf("request changed declared input: %v", err)
	}
	if changedResult.Reused || changedResult.Build.ID == firstResult.Build.ID {
		t.Fatalf("changed declared input reused %#v", changedResult)
	}
}

func TestRequestRejectsInvalidDeclaredInputs(t *testing.T) {
	t.Parallel()
	tooMany := []publicbuild.DeclaredInput{{Name: "compatibility", Value: "linux-amd64-node@24"}}
	for index := 0; index < 32; index++ {
		tooMany = append(tooMany, publicbuild.DeclaredInput{Name: fmt.Sprintf("input%02d", index), Value: "value"})
	}
	tooLarge := []publicbuild.DeclaredInput{{Name: "compatibility", Value: "linux-amd64-node@24"}}
	for index := 0; index < 16; index++ {
		tooLarge = append(tooLarge, publicbuild.DeclaredInput{Name: fmt.Sprintf("input%02d", index), Value: strings.Repeat("x", 1024)})
	}
	tests := []struct {
		name   string
		inputs []publicbuild.DeclaredInput
	}{
		{"duplicate", []publicbuild.DeclaredInput{{Name: "runtime", Value: "one"}, {Name: "runtime", Value: "two"}}},
		{"unsafe name", []publicbuild.DeclaredInput{{Name: "Runtime", Value: "node@24"}}},
		{"control value", []publicbuild.DeclaredInput{{Name: "runtime", Value: "node@24\nsecret"}}},
		{"invalid UTF-8 value", []publicbuild.DeclaredInput{{Name: "runtime", Value: string([]byte{0xff})}}},
		{"too many", tooMany},
		{"value too large", []publicbuild.DeclaredInput{{Name: "compatibility", Value: "linux-amd64-node@24"}, {Name: "runtime", Value: strings.Repeat("x", 1025)}}},
		{"total too large", tooLarge},
		{"invalid compatibility", []publicbuild.DeclaredInput{{Name: "compatibility", Value: "linux-amd64-bad value"}}},
		{"wrong-platform compatibility", []publicbuild.DeclaredInput{{Name: "compatibility", Value: "linux-arm64-node@24"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validRequest()
			request.Inputs = test.inputs
			if _, err := newCoordinator(t).Request(context.Background(), request); !errors.Is(err, publicbuild.ErrRejected) {
				t.Fatalf("request error = %v, want ErrRejected", err)
			}
		})
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
		{"Turbo linux arm64 SHA-256", publicbuild.IntegrationTurbo, publicbuild.PlatformLinuxARM64, strings.Repeat("b", 64)},
		{"Actions linux amd64 SHA-1", publicbuild.IntegrationActions, publicbuild.PlatformLinuxAMD64, strings.Repeat("c", 40)},
	}
	coordinator := newCoordinator(t)
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validRequest()
			request.Integration = test.integration
			request.Platform = test.platform
			compatibility := strings.ReplaceAll(string(test.platform), "/", "-") + "-node@24"
			request.Inputs = []publicbuild.DeclaredInput{{Name: "compatibility", Value: compatibility}}
			request.Commit = test.commit
			request.Target = fmt.Sprintf("@acme/widgets#target-%d", index)
			if test.integration == publicbuild.IntegrationActions {
				request.Target = fmt.Sprintf(".github/workflows/public-cache.yml#target-%d", index)
				request.Inputs = actionsPublicInputs(compatibility)
			}
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

func TestRequestAcceptsBuildKitForCompleteOCICollectors(t *testing.T) {
	t.Parallel()
	request := validRequest()
	request.Integration = publicbuild.IntegrationBuildKit
	result, err := newCoordinator(t).Request(context.Background(), request)
	if err != nil {
		t.Fatalf("request BuildKit Public Build: %v", err)
	}
	if result.Build.State != publicbuild.StateQueued {
		t.Fatalf("state = %q, want queued", result.Build.State)
	}
}

func TestPublicCompatibilityIdentityUsesWorkloadPlatformNotServerHost(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		request publicbuild.BuildRequest
		want    string
	}{
		{request: publicbuild.BuildRequest{Integration: publicbuild.IntegrationBuildKit, Platform: publicbuild.PlatformLinuxAMD64}, want: "linux-amd64"},
		{request: publicbuild.BuildRequest{Integration: publicbuild.IntegrationBuildKit, Platform: publicbuild.PlatformLinuxARM64}, want: "linux-arm64"},
		{request: publicbuild.BuildRequest{
			Integration: publicbuild.IntegrationTurbo, Platform: publicbuild.PlatformLinuxARM64,
			Inputs: []publicbuild.DeclaredInput{{Name: "compatibility", Value: "linux-arm64-node@24-schema1"}},
		}, want: "linux-arm64-node@24-schema1"},
	} {
		got, err := publicbuild.CompatibilityIdentity(test.request)
		if err != nil {
			t.Fatalf("derive compatibility for %#v: %v", test.request, err)
		}
		if got != test.want {
			t.Fatalf("compatibility = %q, want %q", got, test.want)
		}
	}
}

func TestPublicationProjectIdentityUsesNativeActionsNamespace(t *testing.T) {
	t.Parallel()

	got, err := publicbuild.PublicationProjectIdentity(
		publicbuild.IntegrationActions,
		"github.com/acme/widget",
		"acme/widget",
		"https://github.com/acme/widget",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got != "acme/widget" {
		t.Fatalf("Actions publication project = %q", got)
	}
	if _, err := publicbuild.PublicationProjectIdentity(
		publicbuild.IntegrationActions,
		"github.com/acme/widget",
		"acme/other",
		"https://github.com/acme/widget",
	); err == nil {
		t.Fatal("mismatched Actions repository was accepted")
	}
	got, err = publicbuild.PublicationProjectIdentity(
		publicbuild.IntegrationTurbo,
		"github.com/acme/widget",
		"acme/widget",
		"https://github.com/acme/widget",
	)
	if err != nil || got != "github.com/acme/widget" {
		t.Fatalf("Turbo publication project = %q, error = %v", got, err)
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
	actionsRequest := validRequest()
	actionsRequest.Integration = publicbuild.IntegrationActions
	actionsRequest.Target = ".github/workflows/public-cache.yml#runtime"
	actionsRequest.Platform = publicbuild.PlatformLinuxARM64
	actionsRequest.Inputs = actionsPublicInputs("linux-arm64-node@24")
	first, err := coordinator.Request(ctx, actionsRequest)
	if err != nil {
		t.Fatalf("request Actions Public Build: %v", err)
	}
	turboRequest := validRequest()
	turboRequest.Target = "@acme/widgets#test"
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

	arm64Actions := localFakeWorker("actions-arm64", []publicbuild.Integration{
		publicbuild.IntegrationActions,
	}, []publicbuild.Platform{
		publicbuild.PlatformLinuxARM64,
	})
	firstLease, err := coordinator.LeaseNext(ctx, arm64Actions)
	if err != nil {
		t.Fatalf("lease earlier compatible Actions build: %v", err)
	}
	if firstLease.Build.ID != first.Build.ID {
		t.Fatalf("leased id = %q, want first queued id %q", firstLease.Build.ID, first.Build.ID)
	}

	if _, err := coordinator.LeaseNext(ctx, amd64Turbo); !errors.Is(err, publicbuild.ErrNoWork) {
		t.Fatalf("empty compatible queue error = %v, want ErrNoWork", err)
	}
}

func TestLeaseNextUsesExactRecipeCapabilities(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	coordinator := newCoordinator(t)
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
		request.Target = fmt.Sprintf("@acme/widgets#race-%d", attempt)
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

func TestExpiredLeaseIsRecoveredWithoutCoordinatorRestart(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.August, 31, 10, 0, 0, 0, time.UTC)
	clock := now
	coordinator, err := publicbuild.NewCoordinator(testCoordinatorConfig(func() time.Time { return clock }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Request(ctx, validRequest()); err != nil {
		t.Fatal(err)
	}
	worker := localFakeWorker("worker", allIntegrations(), allPlatforms())
	stale, err := coordinator.LeaseNext(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	clock = now.Add(3 * time.Minute)
	if _, err := coordinator.Renew(ctx, stale); !errors.Is(err, publicbuild.ErrLeaseLost) {
		t.Fatalf("expired renewal error = %v, want ErrLeaseLost", err)
	}
	fresh, err := coordinator.LeaseNext(ctx, worker)
	if err != nil {
		t.Fatalf("lease recovered build: %v", err)
	}
	if fresh.Build.ID != stale.Build.ID || fresh.Token == stale.Token || !fresh.ExpiresAt.After(clock) {
		t.Fatalf("fresh lease = %#v, stale lease = %#v", fresh, stale)
	}
}

func TestLeasedPublicationRejectsExpiredReplacementAndCancellation(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.August, 31, 10, 0, 0, 0, time.UTC)
	clock := now
	coordinator, err := publicbuild.NewCoordinator(testCoordinatorConfig(func() time.Time { return clock }))
	if err != nil {
		t.Fatal(err)
	}
	requested, err := coordinator.Request(ctx, validRequest())
	if err != nil {
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
		t.Fatal(err)
	}
	if _, err := coordinator.BeginLeasedPublication(ctx, stale); !errors.Is(err, publicbuild.ErrPublicationLost) {
		t.Fatalf("stale leased publication = %v, want ErrPublicationLost", err)
	}
	if _, err := coordinator.Cancel(ctx, requested.Build.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.BeginLeasedPublication(ctx, fresh); !errors.Is(err, publicbuild.ErrPublicationLost) {
		t.Fatalf("cancelled leased publication = %v, want ErrPublicationLost", err)
	}
}

func TestPublicationPermitFencesAcceptedCancellation(t *testing.T) {
	ctx := context.Background()
	coordinator, err := publicbuild.NewCoordinator(testCoordinatorConfig(time.Now))
	if err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("cancel after collection began = %v, want ErrInvalidTransition", err)
	}
	completed, err := coordinator.CommitPublication(ctx, permit, testPublication("trusted-output"))
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != publicbuild.StateSucceeded {
		t.Fatalf("state = %q, want succeeded", completed.State)
	}

	retry := validRequest()
	retry.Target = "@acme/widgets#cancel-first"
	queued, err := coordinator.Request(ctx, retry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Cancel(ctx, queued.Build.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRequestReusesPublicationIndexBeforeQueueing(t *testing.T) {
	config := testCoordinatorConfig(time.Now)
	config.Publications = publicbuild.PublicationIndexFunc(func(_ context.Context, request publicbuild.BuildRequest) (publicbuild.ExistingPublication, error) {
		return publicbuild.ExistingPublication{
			Identity: "public-identity", BuildID: "public-build-existing",
			Digest: "sha256:" + strings.Repeat("c", 64), SizeBytes: 42,
			MediaType: "application/vnd.layercache.turbo", ProducerDuration: time.Second,
		}, nil
	})
	coordinator, err := publicbuild.NewCoordinator(config)
	if err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.Request(context.Background(), validRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reused || result.Build.ID != "public-build-existing" || result.Build.State != publicbuild.StateSucceeded {
		t.Fatalf("existing publication result = %#v", result)
	}
	if _, err := coordinator.LeaseNext(context.Background(), localFakeWorker("worker", allIntegrations(), allPlatforms())); !errors.Is(err, publicbuild.ErrNoWork) {
		t.Fatalf("publication reuse queued work: %v", err)
	}
}

func TestRequestRepairsSucceededBuildWhenPublicationBecomesUnavailable(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	lookupErr := error(publicbuild.ErrPublicationNotFound)
	config := testCoordinatorConfig(func() time.Time { return now })
	config.Publications = publicbuild.PublicationIndexFunc(func(context.Context, publicbuild.BuildRequest) (publicbuild.ExistingPublication, error) {
		return publicbuild.ExistingPublication{}, lookupErr
	})
	coordinator, err := publicbuild.NewCoordinator(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	request := validRequest()
	first, err := coordinator.Request(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := coordinator.LeaseNext(ctx, localFakeWorker("worker", allIntegrations(), allPlatforms()))
	if err != nil {
		t.Fatal(err)
	}
	completed, err := coordinator.Complete(ctx, lease, testPublication("trusted-output"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := coordinator.Request(ctx, request); !errors.Is(err, publicbuild.ErrPublicationPending) {
		t.Fatalf("recent completed build request = %v, want ErrPublicationPending", err)
	}

	lookupErr = publicbuild.ErrPublicationUnavailable
	repair, err := coordinator.Request(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if repair.Reused || repair.Build.State != publicbuild.StateQueued || repair.Build.ID == first.Build.ID {
		t.Fatalf("unavailable publication repair = %#v", repair)
	}
	retired, err := coordinator.Inspect(ctx, completed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retired.State != publicbuild.StateFailed || retired.Publication != nil || retired.Failure == "" {
		t.Fatalf("retired completed build = %#v", retired)
	}
}

func TestRequestRepairsUnregisteredSucceededBuildAfterRecoveryWindow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	config := testCoordinatorConfig(func() time.Time { return now })
	config.Publications = publicbuild.PublicationIndexFunc(func(context.Context, publicbuild.BuildRequest) (publicbuild.ExistingPublication, error) {
		return publicbuild.ExistingPublication{}, publicbuild.ErrPublicationNotFound
	})
	coordinator, err := publicbuild.NewCoordinator(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
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

	now = now.Add(6 * time.Minute)
	repair, err := coordinator.Request(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if repair.Reused || repair.Build.State != publicbuild.StateQueued || repair.Build.ID == first.Build.ID {
		t.Fatalf("unregistered publication repair = %#v", repair)
	}
}

func testCoordinatorConfig(now func() time.Time) publicbuild.Config {
	return publicbuild.Config{
		AllowlistedRepositories: []string{"https://github.com/acme/widgets"},
		Limits: publicbuild.Resources{
			CPUMillis: 4_000, MemoryBytes: 8 << 30, DiskBytes: 40 << 30, Timeout: 30 * time.Minute,
		},
		SanitizeLog:   func(message string) string { return message },
		SourcePolicy:  publicbuild.SourcePolicyFunc(func(context.Context, string, string) error { return nil }),
		RecipePolicy:  publicbuild.RecipePolicyFunc(func(context.Context, publicbuild.Integration, string, string) error { return nil }),
		LeaseDuration: 2 * time.Minute,
		Now:           now,
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
		SourcePolicy: publicbuild.SourcePolicyFunc(func(context.Context, string, string) error { return nil }),
		RecipePolicy: publicbuild.RecipePolicyFunc(func(context.Context, publicbuild.Integration, string, string) error { return nil }),
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
		Target:       "@acme/widgets#build",
		RecipeDigest: "sha256:" + strings.Repeat("b", 64),
		Platform:     publicbuild.PlatformLinuxAMD64,
		Inputs:       []publicbuild.DeclaredInput{{Name: "compatibility", Value: "linux-amd64-node@24"}},
		Resources: publicbuild.Resources{
			CPUMillis:   2_000,
			MemoryBytes: 4 << 30,
			DiskBytes:   20 << 30,
			Timeout:     10 * time.Minute,
		},
	}
}

func actionsPublicInputs(compatibility string) []publicbuild.DeclaredInput {
	return []publicbuild.DeclaredInput{
		{Name: "actions.key", Value: "fixture-key"},
		{Name: "actions.ref", Value: "refs/heads/main"},
		{Name: "actions.version", Value: strings.Repeat("1", 64)},
		{Name: "compatibility", Value: compatibility},
	}
}
