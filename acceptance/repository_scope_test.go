package acceptance_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/config"
)

func TestRunBypassesUnrelatedRepository(t *testing.T) {
	t.Setenv("GITHUB_REF", "")
	binary := buildLayerCache(t)
	root := t.TempDir()
	configured := filepath.Join(root, "configured")
	unrelated := filepath.Join(root, "unrelated")
	initRunRepository(t, configured, "acme/widgets", "main")
	initRunRepository(t, unrelated, "other/widgets", "feature")
	configPath := filepath.Join(root, "config.json")
	address := availableAddress(t)
	runBinaryInDirectory(t, configured, binary,
		"setup", "--config", configPath, "--data-dir", filepath.Join(root, "cache"),
		"--listen", address, "--buildkit-team-repository", "registry.test/team/acme/widgets", "--non-interactive", "--json",
	)
	server := startLayerCache(t, binary, configPath, address)
	defer server.stop(t)

	for _, test := range []struct {
		name      string
		directory string
		change    func()
	}{
		{name: "unrelated", directory: unrelated},
		{name: "fork at configured path", directory: configured, change: func() {
			runGitInDirectory(t, configured, "remote", "set-url", "origin", "https://github.com/fork/widgets.git")
		}},
		{name: "removed origin", directory: configured, change: func() {
			runGitInDirectory(t, configured, "remote", "remove", "origin")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.change != nil {
				test.change()
			}
			// Simulate entering another checkout inside an already wrapped command.
			command := exec.Command(binary, "run", "--config", configPath, "--", "sh", "-c", `printf '{"token":"%s","actions":"%s","config":"%s","measurement":"%s","bypass":"%s","summarySet":"%s"}' "$TURBO_TOKEN" "$ACTIONS_RUNTIME_TOKEN" "$LAYER_CACHE_CONFIG" "$LAYER_CACHE_MEASUREMENT_TOKEN" "$LAYER_CACHE_BYPASS" "${TURBO_RUN_SUMMARY+x}"`)
			command.Dir = test.directory
			command.Env = append(os.Environ(), "TURBO_TOKEN=parent-token", "ACTIONS_RUNTIME_TOKEN=parent-token", "LAYER_CACHE_CONFIG="+configPath, "LAYER_CACHE_MEASUREMENT_TOKEN=parent-token")
			var stderr bytes.Buffer
			command.Stderr = &stderr
			output, err := command.Output()
			if err != nil {
				t.Fatalf("bypassed build failed: %v\n%s", err, stderr.String())
			}
			var environment map[string]string
			if err := json.Unmarshal(output, &environment); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"token", "actions", "config", "measurement", "summarySet"} {
				if environment[name] != "" {
					t.Errorf("mismatching repository inherited %s authority", name)
				}
			}
			if environment["bypass"] != "all" || !strings.Contains(stderr.String(), "does not match configured project") {
				t.Fatalf("scope mismatch lacks a visible bypass: bypass=%q stderr=%s", environment["bypass"], stderr.String())
			}
		})
	}

	output := runBinaryInDirectory(t, unrelated, binary, "run", "--config", configPath, "--",
		binary, "buildx", "plan", "--config", configPath, "--team-import", "main", "--team-export", "wrong-project", "--", ".")
	jsonStart := bytes.IndexByte(output, '{')
	if jsonStart < 0 {
		t.Fatalf("nested Buildx plan missing: %s", output)
	}
	var plan struct {
		Command struct{ Args []string }
	}
	if err := json.Unmarshal(output[jsonStart:], &plan); err != nil {
		t.Fatal(err)
	}
	arguments := strings.Join(plan.Command.Args, " ")
	if !strings.Contains(arguments, "--no-cache") || strings.Contains(arguments, "--cache-from") || strings.Contains(arguments, "--cache-to") {
		t.Fatalf("nested Buildx retained cache authority: %s", arguments)
	}
	output = runBinaryInDirectory(t, unrelated, binary, "buildx", "plan", "--config", configPath,
		"--team-import", "main", "--team-export", "wrong-project", "--", ".")
	jsonStart = bytes.IndexByte(output, '{')
	if jsonStart < 0 || json.Unmarshal(output[jsonStart:], &plan) != nil {
		t.Fatalf("direct Buildx plan missing: %s", output)
	}
	arguments = strings.Join(plan.Command.Args, " ")
	if !strings.Contains(arguments, "--no-cache") || strings.Contains(arguments, "--cache-from") || strings.Contains(arguments, "--cache-to") {
		t.Fatalf("direct Buildx retained cache authority: %s", arguments)
	}
}

func TestRunMatchingClonesAndWorktreesReuseArtifactsWithCurrentSourceScope(t *testing.T) {
	t.Setenv("GITHUB_REF", "")
	binary := buildLayerCache(t)
	for _, projectID := range []string{"github.com/acme/widgets", "team-issued-project-123"} {
		t.Run(projectID, func(t *testing.T) {
			root := t.TempDir()
			configured := filepath.Join(root, "configured")
			clone := filepath.Join(root, "clone")
			worktree := filepath.Join(root, "worktree")
			initRunRepository(t, configured, "acme/widgets", "main")
			cloneCommit := initRunRepository(t, clone, "acme/widgets", "feature/clone")
			runGitInDirectory(t, configured, "worktree", "add", "-b", "feature/worktree", worktree)
			runGitInDirectory(t, worktree, "-c", "user.name=Layer Cache QA", "-c", "user.email=qa@layercache.dev", "commit", "--allow-empty", "-m", "worktree")
			worktreeCommit := runGitInDirectory(t, worktree, "rev-parse", "HEAD")
			configPath := filepath.Join(root, "config.json")
			address := availableAddress(t)
			runBinaryInDirectory(t, configured, binary,
				"setup", "--config", configPath, "--data-dir", filepath.Join(root, "cache"),
				"--listen", address, "--project", projectID, "--non-interactive", "--json",
			)
			server := startLayerCache(t, binary, configPath, address)
			defer server.stop(t)
			cfg, err := config.Load(configPath)
			if err != nil {
				t.Fatal(err)
			}
			for index, workspace := range []struct{ directory, ref, commit string }{
				{configured, "refs/heads/main", runGitInDirectory(t, configured, "rev-parse", "HEAD")},
				{clone, "refs/heads/feature/clone", cloneCommit},
				{worktree, "refs/heads/feature/worktree", worktreeCommit},
			} {
				command := exec.Command(binary, "run", "--config", configPath, "--", "sh", "-c", `printf '{"turbo":"%s","actions":"%s"}' "$TURBO_TOKEN" "$ACTIONS_RUNTIME_TOKEN"`)
				command.Dir = workspace.directory
				var stderr bytes.Buffer
				command.Stderr = &stderr
				output, err := command.Output()
				if err != nil {
					t.Fatalf("run matching checkout: %v\n%s", err, stderr.String())
				}
				var tokens map[string]string
				if err := json.Unmarshal(output, &tokens); err != nil {
					t.Fatal(err)
				}
				for integration, token := range tokens {
					claims, err := access.ParseCapabilityToken(cfg.LocalToken, token, time.Now().UTC())
					if err != nil {
						t.Fatal(err)
					}
					if claims.Project != projectID || claims.Integration != integration || claims.Repository != "acme/widgets" ||
						claims.Ref != workspace.ref || claims.SourceCommit != workspace.commit || claims.DefaultRef != "refs/heads/main" {
						t.Fatalf("wrong %s source scope for %s", integration, workspace.directory)
					}
				}
				method := http.MethodGet
				if index == 0 {
					method = http.MethodPut
				}
				request, err := http.NewRequest(method, "http://"+address+"/v8/artifacts/abcdef1234567890", strings.NewReader("shared-workspace-artifact"))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Authorization", "Bearer "+tokens["turbo"])
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil || response.StatusCode != http.StatusOK || index > 0 && string(body) != "shared-workspace-artifact" {
					t.Fatalf("artifact reuse for %s: status=%d body=%q error=%v", workspace.directory, response.StatusCode, body, err)
				}
			}
		})
	}
}

func initRunRepository(t *testing.T, directory, repository, branch string) string {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitInDirectory(t, directory, "init", "-b", branch)
	runGitInDirectory(t, directory, "-c", "user.name=Layer Cache QA", "-c", "user.email=qa@layercache.dev", "commit", "--allow-empty", "-m", branch)
	runGitInDirectory(t, directory, "remote", "add", "origin", "https://github.com/"+repository+".git")
	runGitInDirectory(t, directory, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	return runGitInDirectory(t, directory, "rev-parse", "HEAD")
}

func runGitInDirectory(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	return strings.TrimSpace(string(runBinaryInDirectory(t, directory, "git", arguments...)))
}

func runBinaryInDirectory(t *testing.T, directory, binary string, arguments ...string) []byte {
	t.Helper()
	command := exec.Command(binary, arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", binary, arguments, err, output)
	}
	return output
}
