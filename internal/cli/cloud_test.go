package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/portal"
	"github.com/layercache/layercache/internal/server"
)

func TestConnectCreatesIsolatedConfigAndPreservesExisting(t *testing.T) {
	root := t.TempDir()
	gh := filepath.Join(root, "gh")
	if err := os.WriteFile(gh, []byte("#!/bin/sh\nprintf test-github-token"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/cli/projects":
			if r.Header.Get("Authorization") != "Bearer test-github-token" {
				t.Error("missing authentication")
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(cloudProjects{Teams: []portal.Team{{ID: "team-one", Name: "Team"}}, Projects: []portal.Project{{ID: "project-one", TeamID: "team-one", Name: "App", Repository: "owner/repo", DefaultRef: "refs/heads/main"}}})
		case "/v1/auth/github/exchange":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["project"] != "project-one" || body["githubToken"] != "test-github-token" {
				t.Error("wrong exchange scope")
			}
			json.NewEncoder(w).Encode(capabilityExchange{Token: "scoped-cache-token", ExpiresAt: time.Now().Add(time.Hour)})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer service.Close()
	path := filepath.Join(root, "connected.json")
	var output bytes.Buffer
	args := []string{"connect", "--cloud", service.URL, "--github-cli", "--config", path, "--json"}
	if err := Run(context.Background(), args, &output, &output); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TeamURL != service.URL || cfg.ProjectID != "project-one" || cfg.TeamToken != "scoped-cache-token" || cfg.GitHubCLIPath != gh {
		t.Fatalf("wrong connection fields")
	}
	if cfg.DataDir == root || cfg.Listen == config.DefaultListen {
		t.Fatal("connection not isolated")
	}
	if err = verifyOwnershipMarker(cfg, path); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err = Run(context.Background(), args, &output, &output); err == nil {
		t.Fatal("overwrote existing config")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("existing configuration changed")
	}
	if strings.Contains(output.String(), "test-github-token") || strings.Contains(output.String(), "scoped-cache-token") {
		t.Fatal("credential disclosure")
	}
}

func TestCloudProjectSelectionRequiresUnambiguousMembership(t *testing.T) {
	list := cloudProjects{Teams: []portal.Team{{ID: "a", Name: "One"}, {ID: "b", Name: "Two"}}, Projects: []portal.Project{{ID: "p-a", TeamID: "a", Name: "App", Repository: "o/r", DefaultRef: "refs/heads/main"}, {ID: "p-b", TeamID: "b", Name: "App", Repository: "o/r", DefaultRef: "refs/heads/main"}}}
	if _, err := selectCloudProject(list, "", "App"); err == nil {
		t.Fatal("ambiguous selection accepted")
	}
	p, err := selectCloudProject(list, "Two", "App")
	if err != nil || p.ID != "p-b" {
		t.Fatal("team selection failed", err)
	}
	if _, err := selectCloudProject(list, "missing", ""); err == nil {
		t.Fatal("missing membership accepted")
	}
}

func TestConnectRejectsUnsafeOriginsBeforeReadingCredentials(t *testing.T) {
	for _, origin := range []string{"http://cloud.example", "https://user:secret@cloud.example", "https://cloud.example/path", "https://cloud.example?token=x"} {
		if err := Run(context.Background(), []string{"connect", "--cloud", origin, "--github-cli"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Fatal("accepted", origin)
		}
	}
}

func TestCloudConfigRequiresProtectedSecretsAndExplicitQuota(t *testing.T) {
	root := t.TempDir()
	cfg, err := config.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	template := filepath.Join(root, "template.json")
	if err = config.Save(template, cfg); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "oauth")
	key := filepath.Join(root, "key")
	os.WriteFile(secret, []byte("oauth-secret"), 0600)
	os.WriteFile(key, bytes.Repeat([]byte{42}, 32), 0600)
	file := cloudFile{Listen: "127.0.0.1:8080", Origin: "https://cloud.example", DataDir: filepath.Join(root, "data"), GitHubClientID: "client", GitHubClientSecretFile: secret, SessionKeyFile: key, ProjectTemplateFile: template, StoragePool: server.StoragePoolConfig{Path: root, MaxBytes: 1 << 30}}
	path := filepath.Join(root, "cloud.json")
	save := func() {
		data, _ := json.Marshal(file)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	save()
	_, loaded, err := loadCloud(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.SessionKey) != 32 || loaded.GitHubClientSecret != "oauth-secret" {
		t.Fatal("missing protected credentials")
	}
	file.StoragePool.MaxBytes = 0
	save()
	if _, _, err = loadCloud(path); err == nil {
		t.Fatal("accepted missing quota")
	}
	file.StoragePool.MaxBytes = 1 << 30
	file.Listen = "0.0.0.0:8080"
	save()
	if _, _, err = loadCloud(path); err == nil {
		t.Fatal("accepted public plaintext listener")
	}
	file.Listen = "127.0.0.1:8080"
	save()
	os.Chmod(secret, 0644)
	if _, _, err = loadCloud(path); err == nil {
		t.Fatal("accepted unprotected secret")
	}
}

func TestCloudGETNeverForwardsGitHubCredentialAcrossRedirect(t *testing.T) {
	reached := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true; w.WriteHeader(200) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	var result cloudProjects
	if err := cloudGET(context.Background(), origin.URL, "/api/cli/projects", "private-github-token", &result); err == nil {
		t.Fatal("accepted redirect")
	}
	if reached {
		t.Fatal("credential redirected to another origin")
	}
}

func TestCloudDeviceDiagnosticsNeverIncludeProviderBody(t *testing.T) {
	err := cloudDeviceError(context.Background(), "start", errors.New("HTTP 500: token=secret-credential"))
	if strings.Contains(err.Error(), "secret-credential") {
		t.Fatal("provider secret exposed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(cloudDeviceError(ctx, "complete", errors.New("provider-body")), context.Canceled) {
		t.Fatal("lost cancellation")
	}
}
