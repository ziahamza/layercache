package acceptance_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

type statusResult struct {
	Configured bool   `json:"configured"`
	DataDir    string `json:"dataDir"`
	MaxBytes   int64  `json:"maxBytes"`
}

func TestSetupMakesHostCacheVisibleThroughStatus(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	dataDir := filepath.Join(root, "cache")

	runLayerCache(t,
		"setup",
		"--config", configPath,
		"--data-dir", dataDir,
		"--max-size", "1048576",
		"--non-interactive",
		"--json",
	)

	output := runLayerCache(t, "status", "--config", configPath, "--json")
	var got statusResult
	if err := json.Unmarshal(output, &got); err != nil {
		t.Fatalf("decode status JSON: %v\n%s", err, output)
	}

	if !got.Configured {
		t.Fatal("status reported an unconfigured installation")
	}
	if got.DataDir != dataDir {
		t.Fatalf("dataDir = %q, want %q", got.DataDir, dataDir)
	}
	if got.MaxBytes != 1048576 {
		t.Fatalf("maxBytes = %d, want 1048576", got.MaxBytes)
	}
	if info, err := os.Stat(dataDir); err != nil || !info.IsDir() {
		t.Fatalf("setup did not create data directory: %v", err)
	}
}

func runLayerCache(t *testing.T, args ...string) []byte {
	t.Helper()

	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	commandArgs := append([]string{"run", "./cmd/layercache"}, args...)
	cmd := exec.Command("go", commandArgs...)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("layercache %v failed: %v\n%s", args, err, output)
	}
	return output
}
