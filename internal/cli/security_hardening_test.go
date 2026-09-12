//go:build linux || darwin

package cli

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/config"
)

func TestResolveSecretFlagReadsOnlyProtectedRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	value := flags.String("token", "", "")
	filePath := flags.String("token-file", "", "")
	if err := flags.Parse([]string{"--token-file", path}); err != nil {
		t.Fatal(err)
	}
	set, err := resolveSecretFlag(flags, "token", "token-file", value, *filePath)
	if err != nil || !set || *value != "secret-value" {
		t.Fatalf("resolved secret = %q, set=%v, err=%v", *value, set, err)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretFile(path); err == nil || !strings.Contains(err.Error(), "group or other") {
		t.Fatalf("unsafe-mode secret error = %v", err)
	}
}

func TestResolveSecretFlagRejectsValueAndFileTogether(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	value := flags.String("token", "", "")
	filePath := flags.String("token-file", "", "")
	if err := flags.Parse([]string{"--token", "from-argv", "--token-file", path}); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSecretFlag(flags, "token", "token-file", value, *filePath); err == nil || !strings.Contains(err.Error(), "choose only one") {
		t.Fatalf("conflicting secret flags error = %v", err)
	}
}

func TestSetupClearsCredentialsWhenRemoteScopeChanges(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	dataDir := filepath.Join(root, "cache")
	trustKey := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	runCLI(t,
		"setup", "--config", configPath, "--data-dir", dataDir,
		"--team-url", "http://127.0.0.1:18081", "--team-token", "team-one",
		"--public-url", "http://127.0.0.1:18082", "--public-trust-key", trustKey,
		"--public-access-token", "public-one", "--non-interactive", "--json",
	)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.TeamTokenExpiresAt = time.Now().Add(time.Hour)
	cfg.PublicAccessTokenExpiresAt = time.Now().Add(time.Hour)
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}

	runCLI(t,
		"setup", "--config", configPath, "--data-dir", dataDir,
		"--team-url", "http://127.0.0.1:18083",
		"--public-url", "http://127.0.0.1:18084", "--public-trust-key", trustKey,
		"--non-interactive", "--json",
	)
	cfg, err = config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TeamToken != "" || cfg.PublicAccessToken != "" || !cfg.TeamTokenExpiresAt.IsZero() || !cfg.PublicAccessTokenExpiresAt.IsZero() {
		t.Fatalf("retargeted credentials were preserved: team=%q public=%q teamExpiry=%v publicExpiry=%v", cfg.TeamToken, cfg.PublicAccessToken, cfg.TeamTokenExpiresAt, cfg.PublicAccessTokenExpiresAt)
	}
}

func TestSetupReadsTeamTokenFromProtectedFile(t *testing.T) {
	root := t.TempDir()
	secretPath := filepath.Join(root, "team-token")
	if err := os.WriteFile(secretPath, []byte("team-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.json")
	runCLI(t,
		"setup", "--config", configPath, "--data-dir", filepath.Join(root, "cache"),
		"--team-url", "http://127.0.0.1:18081", "--team-token-file", secretPath,
		"--non-interactive", "--json",
	)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TeamToken != "team-from-file" {
		t.Fatalf("team token = %q", cfg.TeamToken)
	}
	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		t.Fatal("setup did not write config")
	}
}

func TestPublicSetupRejectsExplicitTrustKeyWithoutMatchingPrivateKey(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	dataDir := filepath.Join(root, "cache")
	_, _, err := callCLI(
		"setup", "--config", configPath, "--data-dir", dataDir,
		"--role", "public", "--public-trust-key", "invalid",
		"--non-interactive", "--json",
	)
	if err == nil || !strings.Contains(err.Error(), "publicTrustKey") {
		t.Fatalf("public setup error = %v, want invalid publicTrustKey", err)
	}
	if _, statErr := os.Stat(configPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("invalid public setup wrote configuration: %v", statErr)
	}
	if _, statErr := os.Stat(dataDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("invalid public setup created data directory: %v", statErr)
	}
}

func TestPersistRefreshedCapabilitiesMergesOnlyCredentialFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	expected, err := config.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	expected.TeamURL = "http://127.0.0.1:18081"
	expected.TeamToken = "old"
	expected.GitHubCredentialAccount = "github-account"
	if err := config.Save(path, expected); err != nil {
		t.Fatal(err)
	}
	current := expected
	current.MaxBytes++
	if err := config.Save(path, current); err != nil {
		t.Fatal(err)
	}
	refreshed := expected
	refreshed.TeamToken = "new"
	refreshed.TeamTokenExpiresAt = time.Now().Add(time.Hour).UTC()
	if err := persistRefreshedCapabilities(context.Background(), path, expected, refreshed); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.TeamToken != "new" || loaded.MaxBytes != current.MaxBytes {
		t.Fatalf("merged config token=%q maxBytes=%d, want token=new maxBytes=%d", loaded.TeamToken, loaded.MaxBytes, current.MaxBytes)
	}
}
