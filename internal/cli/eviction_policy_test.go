package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/layercache/layercache/internal/config"
)

func TestSetupSelectsAndValidatesEvictionPolicy(t *testing.T) {
	path, _ := setupLocalConfig(t)
	output, _, err := callCLI("setup", "--config", path, "--eviction-policy", "impact", "--non-interactive", "--json")
	if err != nil || !strings.Contains(output, `"evictionPolicy":"impact"`) {
		t.Fatalf("select impact policy: %s, %v", output, err)
	}
	cfg, err := config.Load(path)
	if err != nil || string(cfg.EvictionPolicy) != "impact" {
		t.Fatalf("persisted policy: %v", err)
	}
	if _, _, err := callCLI("setup", "--config", path, "--eviction-policy", "random", "--non-interactive"); err == nil {
		t.Fatal("unsupported policy accepted")
	}
	cfg, _ = config.Load(path)
	if string(cfg.EvictionPolicy) != "impact" {
		t.Fatal("invalid policy changed configuration")
	}
}

func TestBacktestReportsSelectedEvictionPolicy(t *testing.T) {
	input := filepath.Join(t.TempDir(), "history.json")
	if err := os.WriteFile(input, []byte(`{"schemaVersion":"1","runs":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, policy := range []string{"lru", "impact"} {
		output, _, err := callCLI("backtest", "--input", input, "--retention", "24h", "--source", "localCache", "--eviction-policy", policy, "--json")
		if err != nil || !strings.Contains(output, `"evictionPolicy":"`+policy+`"`) {
			t.Fatalf("backtest policy %s: %s, %v", policy, output, err)
		}
	}
	if _, _, err := callCLI("backtest", "--input", input, "--retention", "24h", "--source", "localCache", "--eviction-policy", "unknown"); err == nil {
		t.Fatal("unsupported backtest policy accepted")
	}
}
