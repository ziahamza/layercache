package buildkit_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

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
	if stdout.String() != "image output\n" {
		t.Fatalf("stdout = %q, want image output", stdout.String())
	}
	if stderr.String() != progress {
		t.Fatalf("stderr was not forwarded: %q", stderr.String())
	}
}
