package buildkit_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/layercache/layercache/internal/buildkit"
)

func TestEnsureBuilderCreatesMissingPersistentDockerContainerBuilder(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{
		results: []error{nil, nil, nil},
	}
	adapter, err := buildkit.NewWithRunner(buildkit.Config{
		DockerCommand: "docker",
		BuilderName:   "layercache",
	}, runner)
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	if err := adapter.EnsureBuilder(context.Background(), io.Discard, io.Discard); err != nil {
		t.Fatalf("ensure builder: %v", err)
	}

	want := []buildkit.Command{
		{Path: "docker", Args: []string{"buildx", "ls", "--format", "{{json .}}"}},
		{Path: "docker", Args: []string{"buildx", "create", "--name", "layercache", "--driver", "docker-container", "--use"}},
		{Path: "docker", Args: []string{"buildx", "inspect", "layercache", "--bootstrap"}},
	}
	if !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("unexpected builder commands\n got: %#v\nwant: %#v", runner.commands, want)
	}
}

func TestEnsureBuilderConfiguresAndEnforcesRemainingLocalBudget(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	runner := &recordingRunner{results: []error{nil, nil, nil, nil}}
	adapter, err := buildkit.NewWithRunner(buildkit.Config{
		DockerCommand: "docker", BuilderName: "layercache-budget",
		LocalBudget: &buildkit.LocalCacheBudget{
			StateDir: stateDir, MaxBytes: 20 << 30, BuildkitMaxBytes: 5 << 30,
			UsedBytes: 17 << 30, MinFreeBytes: 2 << 30,
		},
	}, runner)
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	if err := adapter.EnsureBuilder(context.Background(), io.Discard, io.Discard); err != nil {
		t.Fatalf("ensure builder: %v", err)
	}
	effectiveMinFree, err := buildkit.EffectiveMinFreeBytes(stateDir, 2<<30)
	if err != nil {
		t.Fatalf("resolve effective free-space floor: %v", err)
	}
	configPath := filepath.Join(stateDir, "buildkitd.toml")
	want := []buildkit.Command{
		{Path: "docker", Args: []string{"buildx", "ls", "--format", "{{json .}}"}},
		{Path: "docker", Args: []string{
			"buildx", "create", "--name", "layercache-budget", "--driver", "docker-container",
			"--buildkitd-config", configPath, "--use",
		}},
		{Path: "docker", Args: []string{"buildx", "inspect", "layercache-budget", "--bootstrap"}},
		{Path: "docker", Args: []string{
			"buildx", "prune", "--builder", "layercache-budget", "--force",
			"--reserved-space", "322122547B", "--max-used-space", "3221225472B", "--min-free-space", fmt.Sprintf("%dB", effectiveMinFree),
		}},
	}
	if !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("unexpected builder commands\n got: %#v\nwant: %#v", runner.commands, want)
	}
	encoded, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read buildkitd config: %v", err)
	}
	for _, setting := range []string{
		`reservedSpace = "536870912B"`, `maxUsedSpace = "5368709120B"`, fmt.Sprintf(`minFreeSpace = "%dB"`, effectiveMinFree),
	} {
		if !strings.Contains(string(encoded), setting) {
			t.Fatalf("BuildKit GC config does not contain %q:\n%s", setting, encoded)
		}
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat buildkitd config: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("buildkitd config permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestExistingBuilderKeepsStableDaemonPolicyAndUsesCurrentPruneTarget(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	first, err := buildkit.NewWithRunner(buildkit.Config{
		DockerCommand: "docker", BuilderName: "layercache-stable",
		LocalBudget: &buildkit.LocalCacheBudget{
			StateDir: stateDir, MaxBytes: 20 << 30, BuildkitMaxBytes: 5 << 30, UsedBytes: 17 << 30, MinFreeBytes: 2 << 30,
		},
	}, &recordingRunner{})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.EnsureBuilder(context.Background(), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(stateDir, "buildkitd.toml")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	runner := &recordingRunner{stdoutOutputs: []string{`{"Name":"layercache-stable","Driver":"docker-container"}` + "\n"}}
	second, err := buildkit.NewWithRunner(buildkit.Config{
		DockerCommand: "docker", BuilderName: "layercache-stable",
		LocalBudget: &buildkit.LocalCacheBudget{
			StateDir: stateDir, MaxBytes: 20 << 30, BuildkitMaxBytes: 5 << 30, UsedBytes: 19 << 30, MinFreeBytes: 2 << 30,
		},
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.EnsureBuilder(context.Background(), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	effectiveMinFree, err := buildkit.EffectiveMinFreeBytes(stateDir, 2<<30)
	if err != nil {
		t.Fatalf("resolve effective free-space floor: %v", err)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("daemon policy changed with transient Local Cache usage\nbefore: %s\nafter: %s", before, after)
	}
	wantPrune := buildkit.Command{Path: "docker", Args: []string{
		"buildx", "prune", "--builder", "layercache-stable", "--force",
		"--reserved-space", "107374182B", "--max-used-space", "1073741824B", "--min-free-space", fmt.Sprintf("%dB", effectiveMinFree),
	}}
	if got := runner.commands[len(runner.commands)-1]; !reflect.DeepEqual(got, wantPrune) {
		t.Fatalf("current prune command = %#v, want %#v", got, wantPrune)
	}
}

type recordingRunner struct {
	commands      []buildkit.Command
	results       []error
	stdoutOutputs []string
	stderrOutputs []string
}

func (r *recordingRunner) Run(_ context.Context, command buildkit.Command, stdout, stderr io.Writer) error {
	r.commands = append(r.commands, command)
	call := len(r.commands) - 1
	if call < len(r.stdoutOutputs) {
		_, _ = fmt.Fprint(stdout, r.stdoutOutputs[call])
	}
	if call < len(r.stderrOutputs) {
		_, _ = fmt.Fprint(stderr, r.stderrOutputs[call])
	}
	if len(r.results) == 0 {
		return nil
	}
	result := r.results[0]
	r.results = r.results[1:]
	return result
}
