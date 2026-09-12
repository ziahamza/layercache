package cli

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/layercache/layercache/internal/config"
)

func TestLocalCIIntegrationRejectsOwnedTargetChange(t *testing.T) {
	configPath, dataDir := setupLocalConfig(t)
	root := filepath.Dir(configPath)
	firstPath := filepath.Join(root, "local-ci-a.env")
	secondPath := filepath.Join(root, "local-ci-b.env")

	runCLI(t,
		"integration", "local-ci", "--config", configPath,
		"--env-file", firstPath, "--apply", "--json",
	)
	firstBefore, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dataDir, "integrations", "state.json")
	stateBefore, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	secondBefore := []byte("USER_SETTING=keep\n")
	if err := os.WriteFile(secondPath, secondBefore, 0o640); err != nil {
		t.Fatal(err)
	}

	for _, force := range []bool{false, true} {
		arguments := []string{
			"integration", "local-ci", "--config", configPath,
			"--env-file", secondPath, "--apply", "--json",
		}
		if force {
			arguments = append(arguments, "--force")
		}
		_, _, err = callCLI(arguments...)
		assertOwnedIntegrationTargetChange(t, err, "local-ci", firstPath, secondPath)
		assertFileContents(t, firstPath, firstBefore)
		assertFileContents(t, secondPath, secondBefore)
		assertFileContents(t, statePath, stateBefore)
		assertFileMode(t, secondPath, 0o640)
	}

	runCLI(t, "uninstall", "--config", configPath, "--preserve-cache", "--json")
	if _, err := os.Stat(firstPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("uninstall left the original Local CI handoff: %v", err)
	}
	assertFileContents(t, secondPath, secondBefore)
	assertFileMode(t, secondPath, 0o640)
}

func TestTurboIntegrationRejectsOwnedTargetChange(t *testing.T) {
	configPath, dataDir := setupLocalConfig(t)
	root := filepath.Dir(configPath)
	firstRoot := filepath.Join(root, "turbo-a")
	secondRoot := filepath.Join(root, "turbo-b")
	if err := os.MkdirAll(firstRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(secondRoot, ".turbo"), 0o700); err != nil {
		t.Fatal(err)
	}
	firstPath := filepath.Join(firstRoot, ".turbo", "config.json")
	secondPath := filepath.Join(secondRoot, ".turbo", "config.json")

	runCLI(t,
		"integration", "turbo", "--config", configPath,
		"--root", firstRoot, "--apply", "--json",
	)
	firstBefore, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dataDir, "integrations", "state.json")
	stateBefore, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	secondBefore := []byte("{\n  \"apiUrl\": \"https://user.example\",\n  \"keep\": true\n}\n")
	if err := os.WriteFile(secondPath, secondBefore, 0o640); err != nil {
		t.Fatal(err)
	}

	for _, force := range []bool{false, true} {
		arguments := []string{
			"integration", "turbo", "--config", configPath,
			"--root", secondRoot, "--apply", "--json",
		}
		if force {
			arguments = append(arguments, "--force")
		}
		_, _, err = callCLI(arguments...)
		assertOwnedIntegrationTargetChange(t, err, "turbo", firstPath, secondPath)
		assertFileContents(t, firstPath, firstBefore)
		assertFileContents(t, secondPath, secondBefore)
		assertFileContents(t, statePath, stateBefore)
		assertFileMode(t, secondPath, 0o640)
	}

	runCLI(t, "uninstall", "--config", configPath, "--preserve-cache", "--json")
	if _, err := os.Stat(firstPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("uninstall left the original Turbo configuration: %v", err)
	}
	assertFileContents(t, secondPath, secondBefore)
	assertFileMode(t, secondPath, 0o640)
}

func TestSetupRejectsOwnedDataDirectoryChangeAndPreservesIntegrationCleanup(t *testing.T) {
	configPath, dataDir := setupLocalConfig(t)
	root := filepath.Dir(configPath)
	turboRoot := filepath.Join(root, "turbo-project")
	turboPath := filepath.Join(turboRoot, ".turbo", "config.json")
	if err := os.MkdirAll(filepath.Dir(turboPath), 0o700); err != nil {
		t.Fatal(err)
	}
	originalTurbo := []byte("{\n  \"apiUrl\": \"https://user.example\",\n  \"keep\": true\n}\n")
	if err := os.WriteFile(turboPath, originalTurbo, 0o640); err != nil {
		t.Fatal(err)
	}
	runCLI(t,
		"integration", "turbo", "--config", configPath,
		"--root", turboRoot, "--apply", "--force", "--json",
	)
	markerPath := filepath.Join(dataDir, ownershipMarkerName)
	statePath := filepath.Join(dataDir, "integrations", "state.json")
	markerBefore, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	stateBefore, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	newDataDir := filepath.Join(root, "moved-cache")
	_, _, err = callCLI(
		"setup", "--config", configPath, "--data-dir", newDataDir,
		"--non-interactive", "--json",
	)
	if err == nil || !strings.Contains(err.Error(), "--data-dir") ||
		!strings.Contains(err.Error(), dataDir) || !strings.Contains(err.Error(), newDataDir) ||
		!strings.Contains(err.Error(), "uninstall") {
		t.Fatalf("data-directory change error = %v", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != dataDir {
		t.Fatalf("setup changed dataDir to %q, want %q", cfg.DataDir, dataDir)
	}
	if _, err := os.Stat(newDataDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("rejected setup created the new data directory: %v", err)
	}
	assertFileContents(t, markerPath, markerBefore)
	assertFileContents(t, statePath, stateBefore)

	runCLI(t, "uninstall", "--config", configPath, "--preserve-cache", "--json")
	assertFileContents(t, turboPath, originalTurbo)
}

func TestSetupRejectsChangesThatInvalidateOwnedIntegrations(t *testing.T) {
	tests := []struct {
		name        string
		integration string
		setupArgs   []string
	}{
		{name: "turbo listen", integration: "turbo", setupArgs: []string{"--listen", "127.0.0.1:17438"}},
		{name: "local CI listen", integration: "local-ci", setupArgs: []string{"--listen", "127.0.0.1:17438"}},
		{name: "local CI token", integration: "local-ci", setupArgs: []string{"--local-token", "rotated-local-secret"}},
		{name: "local CI project", integration: "local-ci", setupArgs: []string{"--project", "github.com/acme/other"}},
		{name: "local CI compatibility", integration: "local-ci", setupArgs: []string{"--compatibility-id", "linux-amd64-schema1"}},
		{name: "local CI repository", integration: "local-ci", setupArgs: []string{"--actions-repository", "acme/other"}},
		{name: "local CI ref", integration: "local-ci", setupArgs: []string{"--actions-ref", "refs/heads/other"}},
		{name: "local CI default ref", integration: "local-ci", setupArgs: []string{"--actions-default-ref", "refs/heads/trunk"}},
		{name: "BuildKit builder", integration: "buildkit", setupArgs: []string{"--buildkit-builder", "layercache-other"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configPath, _ := setupLocalConfig(t)
			cfg, err := config.Load(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if test.setupArgs[0] == "--compatibility-id" && cfg.CompatibilityID == test.setupArgs[1] {
				test.setupArgs[1] = "linux-arm64-schema1"
			}
			switch test.integration {
			case "turbo":
				root := filepath.Join(t.TempDir(), "project")
				if err := os.MkdirAll(root, 0o700); err != nil {
					t.Fatal(err)
				}
				runCLI(t, "integration", "turbo", "--config", configPath, "--root", root, "--apply", "--json")
			case "local-ci":
				runCLI(t, "integration", "local-ci", "--config", configPath, "--apply", "--json")
			case "buildkit":
				state, err := loadIntegrationState(cfg)
				if err != nil {
					t.Fatal(err)
				}
				state.Records["buildkit"] = integrationRecord{
					Name: "buildkit", State: "active", OwnershipCaptured: true,
					Builder: cfg.BuildkitBuilder,
				}
				if err := saveIntegrationState(cfg, state); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			arguments := append([]string{"setup", "--config", configPath}, test.setupArgs...)
			arguments = append(arguments, "--non-interactive", "--json")
			_, _, err = callCLI(arguments...)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.integration)) ||
				!strings.Contains(err.Error(), "uninstall") || !strings.Contains(err.Error(), "reapply") {
				t.Fatalf("owned %s setup mutation error = %v", test.integration, err)
			}
			assertFileContents(t, configPath, before)
		})
	}
}

func TestOperationalStatusRejectsOwnedIntegrationScopeDrift(t *testing.T) {
	configPath, _ := setupLocalConfig(t)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	turboRoot := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(turboRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	runCLI(t, "integration", "turbo", "--config", configPath, "--root", turboRoot, "--apply", "--json")
	runCLI(t, "integration", "local-ci", "--config", configPath, "--apply", "--json")

	turboDrift := cfg
	turboDrift.Listen = "127.0.0.1:17438"
	status := inspectOperationalStatus(context.Background(), turboDrift)
	if condition := status.Integrations["turbo"]; condition.Active || condition.State != "endpoint-stale" {
		t.Fatalf("Turbo endpoint drift status = %#v", condition)
	}

	tests := []struct {
		name   string
		state  string
		mutate func(*config.Config)
	}{
		{name: "endpoint", state: "endpoint-stale", mutate: func(value *config.Config) { value.Listen = "127.0.0.1:17438" }},
		{name: "signing key", state: "credential-invalid", mutate: func(value *config.Config) { value.LocalToken = "rotated-local-secret" }},
		{name: "project", state: "credential-scope-stale", mutate: func(value *config.Config) { value.ProjectID = "github.com/acme/other" }},
		{name: "compatibility", state: "credential-scope-stale", mutate: func(value *config.Config) { value.CompatibilityID = "linux-amd64-schema1" }},
		{name: "ref", state: "credential-scope-stale", mutate: func(value *config.Config) { value.ActionsRef = "refs/heads/other" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := cfg
			test.mutate(&changed)
			status := inspectOperationalStatus(context.Background(), changed)
			condition := status.Integrations["actions"]
			if condition.Active || condition.State != test.state {
				t.Fatalf("Local CI drift status = %#v, want %s", condition, test.state)
			}
		})
	}
}

func assertOwnedIntegrationTargetChange(t *testing.T, err error, integration, firstPath, secondPath string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), integration) ||
		!strings.Contains(err.Error(), firstPath) || !strings.Contains(err.Error(), secondPath) ||
		!strings.Contains(err.Error(), "uninstall") {
		t.Fatalf("target-change error = %v", err)
	}
}

func assertFileContents(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s contents = %q, want %q", path, got, want)
	}
}

func assertFileMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("%s mode = %04o, want %04o", path, info.Mode().Perm(), want)
	}
}
