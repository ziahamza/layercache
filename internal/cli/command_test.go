package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRunCommandExecutesChildWithoutInjectionWhenDaemonIsUnavailable(t *testing.T) {
	configPath, _ := setupLocalConfig(t)
	t.Setenv("LAYERCACHE_RUN_HELPER", "success")
	t.Setenv("LAYER_CACHE_RUN_ID", "parent-run-id")
	t.Setenv("TURBO_API", "https://parent-turbo.example")
	t.Setenv("ACTIONS_RUNTIME_TOKEN", "parent-actions-token")

	stdout, stderr, err := callCLI(
		"run", "--config", configPath, "--", os.Args[0], "-test.run=^TestRunCommandChildProcess$",
	)
	if err != nil {
		t.Fatalf("run unavailable-daemon child: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	if !strings.Contains(stderr, "the local daemon is unavailable; running the command without Layer Cache environment injection") {
		t.Fatalf("stderr lacks fail-open warning: %s", stderr)
	}
	if strings.Contains(stderr, "Layer Cache run:") {
		t.Fatalf("stderr reported an injected cache run: %s", stderr)
	}
	want := "parent-run-id|https://parent-turbo.example|parent-actions-token\n"
	if stdout != want {
		t.Fatalf("child environment = %q, want inherited values %q", stdout, want)
	}
}

func TestRunCommandStillReturnsChildFailureWhenDaemonIsUnavailable(t *testing.T) {
	configPath, _ := setupLocalConfig(t)
	t.Setenv("LAYERCACHE_RUN_HELPER", "failure")

	stdout, stderr, err := callCLI(
		"run", "--config", configPath, "--", os.Args[0], "-test.run=^TestRunCommandChildProcess$",
	)
	if err == nil || !strings.Contains(err.Error(), "command failed while Layer Cache was unavailable") {
		t.Fatalf("child failure = %v, want scoped command failure\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	if !strings.Contains(stderr, "the local daemon is unavailable") {
		t.Fatalf("stderr lacks fail-open warning: %s", stderr)
	}
	if code := ExitCode(err); code != 23 {
		t.Fatalf("child exit code = %d, want 23", code)
	}
}

func TestRunCommandChildProcess(t *testing.T) {
	switch os.Getenv("LAYERCACHE_RUN_HELPER") {
	case "success":
		_, _ = fmt.Fprintf(os.Stdout, "%s|%s|%s\n",
			os.Getenv("LAYER_CACHE_RUN_ID"), os.Getenv("TURBO_API"), os.Getenv("ACTIONS_RUNTIME_TOKEN"),
		)
		os.Exit(0)
	case "failure":
		os.Exit(23)
	}
}

func TestTurboSummaryCollectorReadsOnlyPathReportedByChild(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	runs := filepath.Join(root, ".turbo", "runs")
	if err := os.MkdirAll(runs, 0o700); err != nil {
		t.Fatal(err)
	}
	concurrentPath := filepath.Join(runs, "concurrent.json")
	if err := os.WriteFile(concurrentPath, []byte(`{"id":"other-run"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	newPath := filepath.Join(runs, "new.json")
	want := []byte(`{"id":"new"}`)
	if err := os.WriteFile(newPath, want, 0o600); err != nil {
		t.Fatal(err)
	}
	collector := newTurboSummaryPathCollector()
	var output bytes.Buffer
	observer := collector.observe(&output)
	if _, err := observer.Write([]byte("task: Summary: " + concurrentPath + "\n\x1b[32mSumm")); err != nil {
		t.Fatal(err)
	}
	if _, err := observer.Write([]byte("ary:\x1b[0m  " + newPath)); err != nil {
		t.Fatal(err)
	}
	documents, err := turboSummaryDocuments([]string{root}, collector.paths())
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || string(documents[0]) != string(want) {
		t.Fatalf("new documents = %q", documents)
	}
}

func TestNewTurboSummaryDocumentsRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior requires privileges on Windows")
	}
	t.Parallel()

	root := t.TempDir()
	runs := filepath.Join(root, ".turbo", "runs")
	if err := os.MkdirAll(runs, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target.json")
	if err := os.WriteFile(target, []byte(`{"id":"not-a-summary"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(runs, "summary.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := turboSummaryDocuments([]string{root}, []string{filepath.Join(runs, "summary.json")}); err == nil {
		t.Fatal("symlinked summary was accepted")
	}
}

func TestTurboSummaryRootsDeduplicatesPaths(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	roots := turboSummaryRoots(root, filepath.Join(root, "."))
	count := 0
	for _, candidate := range roots {
		if candidate == filepath.Clean(root) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("configured root occurs %d times in %#v", count, roots)
	}
}
