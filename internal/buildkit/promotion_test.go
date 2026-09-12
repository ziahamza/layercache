package buildkit_test

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/buildkit"
)

func TestExecuteSerializesMutableTeamPromotion(t *testing.T) {
	t.Parallel()

	runner := &promotionConcurrencyRunner{}
	config := buildkit.Config{
		DockerCommand: "docker", BuilderName: "layercache", TeamRepository: "cache.example/team/widget",
		PromotionLockDir: t.TempDir(),
	}
	first, err := buildkit.NewWithRunner(config, runner)
	if err != nil {
		t.Fatalf("new first adapter: %v", err)
	}
	second, err := buildkit.NewWithRunner(config, runner)
	if err != nil {
		t.Fatalf("new second adapter: %v", err)
	}
	requests := []buildkit.BuildRequest{
		{
			ContextPath: ".", TargetPlatform: "linux/amd64", Output: buildkit.OutputLoad,
			TeamExportID: "first-u0123456789abcdef0123456789abcdef", TeamPromoteTag: "main",
		},
		{
			ContextPath: ".", TargetPlatform: "linux/amd64", Output: buildkit.OutputLoad,
			TeamExportID: "second-u0123456789abcdef0123456789abcdef", TeamPromoteTag: "main",
		},
	}
	var wait sync.WaitGroup
	errors := make(chan error, 2)
	for index, adapter := range []*buildkit.Adapter{first, second} {
		wait.Add(1)
		go func(adapter *buildkit.Adapter, request buildkit.BuildRequest) {
			defer wait.Done()
			_, err := adapter.Execute(context.Background(), request, io.Discard, io.Discard)
			errors <- err
		}(adapter, requests[index])
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("execute build: %v", err)
		}
	}
	if got := runner.maxPromotions.Load(); got != 1 {
		t.Fatalf("concurrent mutable promotions = %d, want 1", got)
	}
}

type promotionConcurrencyRunner struct {
	activePromotions atomic.Int32
	maxPromotions    atomic.Int32
}

func (runner *promotionConcurrencyRunner) Run(_ context.Context, command buildkit.Command, stdout, stderr io.Writer) error {
	if slices.Contains(command.Args, "ls") {
		_, _ = fmt.Fprintln(stdout, `{"Name":"layercache","Driver":"docker-container"}`)
		return nil
	}
	if slices.Contains(command.Args, "build") {
		_, _ = fmt.Fprintln(stderr, `{"id":"build","completed":"2026-08-30T10:00:01Z"}`)
		return nil
	}
	if slices.Contains(command.Args, "imagetools") {
		active := runner.activePromotions.Add(1)
		for {
			maximum := runner.maxPromotions.Load()
			if active <= maximum || runner.maxPromotions.CompareAndSwap(maximum, active) {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
		runner.activePromotions.Add(-1)
	}
	return nil
}
