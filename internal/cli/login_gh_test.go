//go:build linux || darwin

package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/config"
)

func TestGitHubCLILoginAndRefresh(t *testing.T) {
	dir := t.TempDir()
	gh := filepath.Join(dir, "gh")
	if err := os.WriteFile(gh, []byte("#!/bin/sh\n[ \"$*\" = 'auth token --hostname github.com' ] || exit 1\nprintf 'github-secret\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	calls := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["githubToken"] != "github-secret" {
			t.Error("missing GitHub credential")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"teamToken": "project-capability", "expiresAt": time.Now().Add(time.Hour)})
	}))
	defer remote.Close()
	path := filepath.Join(dir, "config.json")
	runCLI(t, "setup", "--config", path, "--data-dir", filepath.Join(dir, "data"), "--team-url", remote.URL, "--non-interactive", "--json")
	if err := runLogin(context.Background(), []string{"--config", path, "--github-cli"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitHubCLIPath != gh || cfg.TeamToken != "project-capability" || cfg.GitHubCredentialAccount != "" {
		t.Fatal("wrong credential provider or capability")
	}
	encoded, _ := os.ReadFile(path)
	if strings.Contains(string(encoded), "github-secret") {
		t.Fatal("GitHub credential persisted")
	}
	if _, refreshable := nextCapabilityRefresh(cfg, time.Now()); !refreshable {
		t.Fatal("refresh not scheduled")
	}
	if err := refreshTeamCapability(context.Background(), &cfg); err != nil || calls != 1 {
		t.Fatal("fresh credential unnecessarily exchanged", err)
	}
	cfg.TeamTokenExpiresAt = time.Now()
	if err := refreshTeamCapability(context.Background(), &cfg); err != nil || calls != 2 {
		t.Fatal("expired capability not refreshed", err)
	}
	other := cfg
	other.GitHubCLIPath = "/different/gh"
	if sameCredentialScope(cfg, other) {
		t.Fatal("provider change must invalidate refresh scope")
	}
	runCLI(t, "setup", "--config", path, "--team-url", "http://127.0.0.1:18081", "--non-interactive", "--json")
	cfg, err = config.Load(path)
	if err != nil || cfg.GitHubCLIPath != "" {
		t.Fatal("retarget retained GitHub CLI authority", err)
	}
}

func TestGitHubCLIFailureDoesNotExposeCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf 'secret-output'\nprintf 'secret-error' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	_, err := githubCLIToken(context.Background(), path)
	if err == nil || strings.Contains(err.Error(), "secret-") {
		t.Fatal("unsafe subprocess diagnostic")
	}
	if _, err := githubCLIToken(context.Background(), "gh"); err == nil {
		t.Fatal("relative executable accepted")
	}
	var output boundedCredentialOutput
	if _, err := output.Write(make([]byte, 16385)); err == nil {
		t.Fatal("unbounded credential output")
	}
}

func TestCapabilityExchangeDoesNotEchoRemoteCredentials(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.Copy(w, r.Body)
	}))
	defer remote.Close()
	_, err := exchangeGitHubCapabilityAt(context.Background(), remote.URL, config.Config{}, "secret-github-token")
	if err == nil || strings.Contains(err.Error(), "secret-github-token") || !strings.Contains(err.Error(), "401") {
		t.Fatal("unsafe exchange diagnostic", err)
	}
}
