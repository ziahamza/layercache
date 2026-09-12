package buildkit_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/buildkit"
)

func TestExecuteRunsStockBuildxAndReturnsProgressMetrics(t *testing.T) {
	t.Parallel()

	progress := strings.Join([]string{
		`{"id":"cached","completed":"2026-08-30T10:00:01Z","cached":true}`,
		`{"id":"built","completed":"2026-08-30T10:00:03Z"}`,
	}, "\n") + "\n"
	runner := &recordingRunner{
		stdoutOutputs: []string{"{\"Name\":\"layercache\",\"Driver\":\"docker-container\"}\n", "", "image output\n"},
		stderrOutputs: []string{"", "", progress},
	}
	adapter, err := buildkit.NewWithRunner(buildkit.Config{
		DockerCommand: "docker",
		BuilderName:   "layercache",
	}, runner)
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	result, err := adapter.Execute(context.Background(), buildkit.BuildRequest{
		ContextPath:    ".",
		TargetPlatform: "linux/amd64",
		Output:         buildkit.OutputLoad,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("execute build: %v", err)
	}

	if got := len(runner.commands); got != 3 {
		t.Fatalf("executed %d commands, want inspect, bootstrap, and build", got)
	}
	if got := runner.commands[2]; got.Path != result.Plan.Command.Path || strings.Join(got.Args, "\x00") != strings.Join(result.Plan.Command.Args, "\x00") {
		t.Fatalf("executed command %#v, planned %#v", got, result.Plan.Command)
	}
	if result.Metrics.CompletedVertices != 2 || result.Metrics.CachedVertices != 1 || result.Metrics.CacheHitRate != 0.5 {
		t.Fatalf("unexpected progress metrics: %#v", result.Metrics)
	}
	if result.BuildStartedAt.IsZero() || result.BuildFinishedAt.Before(result.BuildStartedAt) {
		t.Fatalf("build timing window = %s to %s", result.BuildStartedAt, result.BuildFinishedAt)
	}
	if stdout.String() != "image output\n" {
		t.Fatalf("stdout = %q, want image output", stdout.String())
	}
	if stderr.String() != progress {
		t.Fatalf("stderr was not forwarded: %q", stderr.String())
	}
}

func TestExecutePrunesDynamicBudgetBeforeAndAfterBuild(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{stderrOutputs: []string{"", "", "", "", `{"id":"built","completed":"2026-08-30T10:00:03Z"}` + "\n"}}
	adapter, err := buildkit.NewWithRunner(buildkit.Config{
		DockerCommand: "docker", BuilderName: "layercache-budgeted",
		LocalBudget: &buildkit.LocalCacheBudget{
			StateDir: t.TempDir(), MaxBytes: 20 << 30, BuildkitMaxBytes: 5 << 30,
			UsedBytes: 19 << 30, MinFreeBytes: 2 << 30,
		},
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Execute(context.Background(), buildkit.BuildRequest{
		ContextPath: ".", TargetPlatform: "linux/amd64", Output: buildkit.OutputLoad,
	}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(runner.commands) != 6 {
		t.Fatalf("commands = %d, want list, create, bootstrap, pre-prune, build, post-prune", len(runner.commands))
	}
	if !slices.Equal(runner.commands[3].Args, runner.commands[5].Args) || runner.commands[3].Args[1] != "prune" {
		t.Fatalf("pre/post prune commands differ: %#v / %#v", runner.commands[3], runner.commands[5])
	}
	if !slices.Contains(runner.commands[5].Args, "1073741824B") {
		t.Fatalf("post-build prune did not enforce current remaining byte budget: %#v", runner.commands[5])
	}
}

func TestExecuteKeepsSuccessfulBuildWhenTeamPromotionFails(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{
		stdoutOutputs: []string{"{\"Name\":\"layercache\",\"Driver\":\"docker-container\"}\n", "", "image output\n", ""},
		stderrOutputs: []string{"", "", `{\"id\":\"built\",\"completed\":\"2026-08-30T10:00:03Z\"}` + "\n", "registry unavailable\n"},
		results:       []error{nil, nil, nil, errors.New("registry unavailable")},
	}
	adapter, err := buildkit.NewWithRunner(buildkit.Config{
		DockerCommand: "docker", BuilderName: "layercache",
		TeamRepository: "cache.example/team/acme/widget", PromotionLockDir: t.TempDir(),
	}, runner)
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	result, err := adapter.Execute(context.Background(), buildkit.BuildRequest{
		ContextPath: ".", TargetPlatform: "linux/amd64", Output: buildkit.OutputLoad,
		TeamExportID: "run-123-u0123456789abcdef0123456789abcdef", TeamPromoteTag: "main",
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("promotion error failed the image build: %v", err)
	}
	if !result.Metrics.TeamPromotionAttempted || result.Metrics.TeamPromotionSucceeded {
		t.Fatalf("unexpected promotion metrics: %+v", result.Metrics)
	}
	if len(result.CacheWarnings) != 1 || !strings.Contains(result.CacheWarnings[0], "registry unavailable") {
		t.Fatalf("promotion warnings = %v", result.CacheWarnings)
	}
	if got := runner.commands[len(runner.commands)-1]; got.Args[1] != "imagetools" {
		t.Fatalf("last command was not promotion: %#v", got)
	}
}

func TestExecuteSkipsPromotionWhenRemoteCoordinationFails(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{
		stdoutOutputs: []string{"{\"Name\":\"layercache\",\"Driver\":\"docker-container\"}\n", "", "image output\n"},
		stderrOutputs: []string{"", "", `{\"id\":\"built\",\"completed\":\"2026-08-30T10:00:03Z\"}` + "\n"},
	}
	adapter, err := buildkit.NewWithRunner(buildkit.Config{
		DockerCommand: "docker", BuilderName: "layercache",
		TeamRepository: "cache.example/team/acme/widget", PromotionLockDir: t.TempDir(),
		PromotionCoordinator: rejectedPromotionCoordinator{},
	}, runner)
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	result, err := adapter.Execute(context.Background(), buildkit.BuildRequest{
		ContextPath: ".", TargetPlatform: "linux/amd64", Output: buildkit.OutputLoad,
		TeamExportID: "run-123-u0123456789abcdef0123456789abcdef", TeamPromoteTag: "main",
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("coordination failure failed the image build: %v", err)
	}
	if len(runner.commands) != 3 {
		t.Fatalf("promotion ran without remote lease: %v", runner.commands)
	}
	if len(result.CacheWarnings) != 1 || !strings.Contains(result.CacheWarnings[0], "promotion lease") {
		t.Fatalf("coordination warnings = %v", result.CacheWarnings)
	}
}

func TestExecuteCancelsMutablePromotionWhenRemoteLeaseRenewalFails(t *testing.T) {
	t.Parallel()

	runner := &renewalCancellationRunner{}
	adapter, err := buildkit.NewWithRunner(buildkit.Config{
		DockerCommand: "docker", BuilderName: "layercache",
		TeamRepository: "cache.example/team/acme/widget", PromotionLockDir: t.TempDir(),
		PromotionCoordinator: renewalFailureCoordinator{},
	}, runner)
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	result, err := adapter.Execute(context.Background(), buildkit.BuildRequest{
		ContextPath: ".", TargetPlatform: "linux/amd64", Output: buildkit.OutputLoad,
		TeamExportID: "run-123-u0123456789abcdef0123456789abcdef", TeamPromoteTag: "main",
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("lease renewal failure changed successful image result: %v", err)
	}
	if !runner.promotionCancelled.Load() {
		t.Fatal("mutable registry promotion kept running after its remote lease was lost")
	}
	if len(result.CacheWarnings) != 1 || !strings.Contains(result.CacheWarnings[0], "maintain Team Cache promotion lease") {
		t.Fatalf("promotion warnings = %v", result.CacheWarnings)
	}
}

type rejectedPromotionCoordinator struct{}

func (rejectedPromotionCoordinator) Acquire(context.Context, string) (buildkit.PromotionLease, error) {
	return nil, errors.New("coordinator unavailable")
}

type renewalFailureCoordinator struct{}

func (renewalFailureCoordinator) Acquire(context.Context, string) (buildkit.PromotionLease, error) {
	return renewalFailureLease{}, nil
}

type renewalFailureLease struct{}

func (renewalFailureLease) Maintain(ctx context.Context) error {
	timer := time.NewTimer(25 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errors.New("lease renewal rejected")
	}
}

func (renewalFailureLease) Release(context.Context) error { return nil }

type renewalCancellationRunner struct {
	promotionCancelled atomic.Bool
}

func (runner *renewalCancellationRunner) Run(ctx context.Context, command buildkit.Command, stdout, stderr io.Writer) error {
	switch {
	case slices.Contains(command.Args, "ls"):
		_, _ = io.WriteString(stdout, "{\"Name\":\"layercache\",\"Driver\":\"docker-container\"}\n")
	case slices.Contains(command.Args, "build"):
		_, _ = io.WriteString(stderr, "{\"id\":\"build\",\"completed\":\"2026-08-30T10:00:01Z\"}\n")
	case slices.Contains(command.Args, "imagetools"):
		<-ctx.Done()
		runner.promotionCancelled.Store(true)
		return ctx.Err()
	}
	return nil
}
