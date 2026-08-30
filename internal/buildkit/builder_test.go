package buildkit_test

import (
	"context"
	"fmt"
	"io"
	"reflect"
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
