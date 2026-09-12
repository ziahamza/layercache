package cli

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/config"
)

const cliTestRepository = "https://github.com/acme/widgets.git"

func TestIntegrationsRejectChangedRepositoryOrigin(t *testing.T) {
	t.Setenv("GITHUB_REF", "")
	clone := filepath.Join(t.TempDir(), "clone")
	initCLIRepository(t, clone, "main", "setup")
	t.Chdir(clone)
	configPath, _ := setupLocalConfig(t)
	// An existing unexpired handoff must not skip current repository validation.
	runCLI(t, "integration", "local-ci", "--config", configPath, "--apply", "--json")
	runCLIGit(t, clone, "remote", "set-url", "origin", "https://github.com/fork/widgets.git")

	for _, integration := range []string{"turbo", "local-ci"} {
		t.Run(integration, func(t *testing.T) {
			_, _, err := callCLI("integration", integration, "--config", configPath, "--apply", "--json")
			if err == nil || !strings.Contains(err.Error(), "does not match configured project") {
				t.Fatalf("changed-origin %s integration error = %v", integration, err)
			}
		})
	}
}

func TestVMRouteUsesCurrentCloneActionsScope(t *testing.T) {
	t.Setenv("GITHUB_REF", "")
	root := canonicalTestTempDir(t)
	setupClone := filepath.Join(root, "setup-clone")
	featureClone := filepath.Join(root, "feature-clone")
	initCLIRepository(t, setupClone, "main", "setup")
	featureCommit := initCLIRepository(t, featureClone, "feature/cache-scope", "feature")

	t.Chdir(setupClone)
	configPath, _ := setupLocalConfig(t)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectRoot != setupClone || cfg.ActionsRef != "refs/heads/main" {
		t.Fatalf("setup source scope = root %q ref %q", cfg.ProjectRoot, cfg.ActionsRef)
	}

	t.Chdir(featureClone)
	stdout, _ := runCLI(t,
		"vm-route", "issue", "--config", configPath,
		"--endpoint", "http://127.0.0.1:7437", "--integration", "actions", "--json",
	)
	var response vmRouteResponse
	if err := json.Unmarshal([]byte(stdout), &response); err != nil {
		t.Fatalf("decode VM route: %v\n%s", err, stdout)
	}
	claims, err := access.ParseCapabilityToken(
		cfg.LocalToken, response.Environment["ACTIONS_RUNTIME_TOKEN"], time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Repository != "acme/widgets" || claims.Ref != "refs/heads/feature/cache-scope" ||
		claims.DefaultRef != "refs/heads/main" || claims.SourceCommit != featureCommit {
		t.Fatalf("VM Actions scope = %+v", claims)
	}
	if response.Environment["TURBO_TOKEN"] != "" {
		t.Fatal("Actions-only VM route included a Turbo credential")
	}
}

func TestVMRouteRequiresExplicitImmutableActionsScopeOutsideRepository(t *testing.T) {
	t.Setenv("GITHUB_REF", "")
	root := t.TempDir()
	setupClone := filepath.Join(root, "setup-clone")
	initCLIRepository(t, setupClone, "main", "setup")
	t.Chdir(setupClone)
	configPath, _ := setupLocalConfig(t)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(outside)
	_, _, err = callCLI(
		"vm-route", "issue", "--config", configPath,
		"--endpoint", "http://127.0.0.1:7437", "--integration", "actions", "--json",
	)
	if err == nil || !strings.Contains(err.Error(), "current Git checkout") ||
		!strings.Contains(err.Error(), "--actions-source-commit") {
		t.Fatalf("unscoped Actions VM route error = %v", err)
	}

	commit := strings.Repeat("a", 40)
	stdout, _ := runCLI(t,
		"vm-route", "issue", "--config", configPath,
		"--endpoint", "http://127.0.0.1:7437", "--integration", "actions",
		"--actions-repository", "acme/widgets",
		"--actions-ref", "refs/pull/17/merge",
		"--actions-default-ref", "refs/heads/main",
		"--actions-source-commit", commit,
		"--json",
	)
	var response vmRouteResponse
	if err := json.Unmarshal([]byte(stdout), &response); err != nil {
		t.Fatal(err)
	}
	claims, err := access.ParseCapabilityToken(
		cfg.LocalToken, response.Environment["ACTIONS_RUNTIME_TOKEN"], time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Repository != "acme/widgets" || claims.Ref != "refs/pull/17/merge" ||
		claims.DefaultRef != "refs/heads/main" || claims.SourceCommit != commit {
		t.Fatalf("explicit VM Actions scope = %+v", claims)
	}

	_, _, err = callCLI(
		"vm-route", "issue", "--config", configPath,
		"--endpoint", "http://127.0.0.1:7437", "--integration", "actions",
		"--actions-repository", "acme/widgets",
		"--json",
	)
	if err == nil || !strings.Contains(err.Error(), "must be provided together") {
		t.Fatalf("partial explicit Actions scope error = %v", err)
	}
	_, _, err = callCLI(
		"vm-route", "issue", "--config", configPath,
		"--endpoint", "http://127.0.0.1:7437", "--integration", "actions",
		"--actions-repository", "acme/other",
		"--actions-ref", "refs/heads/main",
		"--actions-default-ref", "refs/heads/main",
		"--actions-source-commit", commit,
		"--json",
	)
	if err == nil || !strings.Contains(err.Error(), "does not match configured repository") {
		t.Fatalf("mismatched explicit Actions repository error = %v", err)
	}
}

func TestVMRouteRejectsUnprovenDefaultRef(t *testing.T) {
	t.Setenv("GITHUB_REF", "")
	root := t.TempDir()
	clone := filepath.Join(root, "clone")
	initCLIRepository(t, clone, "main", "fixture")
	t.Chdir(clone)
	configPath, _ := setupLocalConfig(t)
	runCLIGit(t, clone, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")

	_, _, err := callCLI(
		"vm-route", "issue", "--config", configPath,
		"--endpoint", "http://127.0.0.1:7437", "--integration", "actions",
		"--json",
	)
	if err == nil || !strings.Contains(err.Error(), "origin/HEAD") ||
		!strings.Contains(err.Error(), "--actions-source-commit") {
		t.Fatalf("unproven default ref error = %v", err)
	}
}

func TestTurboIntegrationPrefersMatchingCurrentClone(t *testing.T) {
	t.Setenv("GITHUB_REF", "")
	root := canonicalTestTempDir(t)
	setupClone := filepath.Join(root, "setup-clone")
	currentClone := filepath.Join(root, "current-clone")
	initCLIRepository(t, setupClone, "main", "setup")
	initCLIRepository(t, currentClone, "feature/worktree", "current")

	t.Chdir(setupClone)
	configPath, _ := setupLocalConfig(t)
	t.Chdir(currentClone)
	runCLI(t, "integration", "turbo", "--config", configPath, "--apply", "--json")
	setupConfig := filepath.Join(setupClone, ".turbo", "config.json")
	currentConfig := filepath.Join(currentClone, ".turbo", "config.json")
	if _, err := os.Stat(currentConfig); err != nil {
		t.Fatalf("current clone did not receive Turbo integration: %v", err)
	}
	if _, err := os.Stat(setupConfig); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("persisted setup clone was modified: %v", err)
	}

	t.Chdir(setupClone)
	_, _, err := callCLI("integration", "turbo", "--config", configPath, "--apply", "--json")
	if err == nil || !strings.Contains(err.Error(), currentConfig) ||
		!strings.Contains(err.Error(), setupConfig) || !strings.Contains(err.Error(), "uninstall") {
		t.Fatalf("owned clone retarget error = %v", err)
	}
	runCLI(t, "uninstall", "--config", configPath, "--preserve-cache", "--json")
	if _, err := os.Stat(currentConfig); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("uninstall left current clone integration: %v", err)
	}
}

func initCLIRepository(t *testing.T, root, branch, contents string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	runCLIGit(t, root, "init", "-b", branch)
	runCLIGit(t, root, "config", "user.email", "qa@layercache.dev")
	runCLIGit(t, root, "config", "user.name", "Layer Cache QA")
	runCLIGit(t, root, "remote", "add", "origin", cliTestRepository)
	if err := os.WriteFile(filepath.Join(root, "fixture.txt"), []byte(contents+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCLIGit(t, root, "add", "fixture.txt")
	runCLIGit(t, root, "commit", "-m", contents)
	runCLIGit(t, root, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	return runCLIGit(t, root, "rev-parse", "HEAD")
}

func runCLIGit(t *testing.T, root string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}
