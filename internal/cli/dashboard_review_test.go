package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/dashboard"
)

func reviewDashboardStart(t *testing.T, paths ...string) func() dashboard.Snapshot {
	t.Helper()
	args := []string{}
	for _, p := range paths {
		args = append(args, "--config", p)
	}
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- runDashboard(ctx, args, writer, io.Discard); writer.Close() }()
	links := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "http://") {
				links <- scanner.Text()
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		reader.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("dashboard failed to stop")
		}
	})
	var link string
	select {
	case link = <-links:
	case <-time.After(5 * time.Second):
		t.Fatal("dashboard failed to start")
	}
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	token := parsed.Fragment
	parsed.Fragment = ""
	parsed.Path = "/api/snapshot"
	client := newLocalCLIHTTPClient(5 * time.Second)
	t.Cleanup(client.CloseIdleConnections)
	return func() dashboard.Snapshot {
		t.Helper()
		request, _ := http.NewRequest("GET", parsed.String(), nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var snap dashboard.Snapshot
		if err := json.NewDecoder(response.Body).Decode(&snap); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != 200 {
			t.Fatal(response.Status)
		}
		return snap
	}
}

func TestDashboardReviewRetainsRefreshedCapabilityAndPicksUpLogin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell credential fixture")
	}
	dir := t.TempDir()
	gh := filepath.Join(dir, "gh")
	if err := os.WriteFile(gh, []byte("#!/bin/sh\nprintf 'fixture-github-token\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var exchanges atomic.Int32
	var expected atomic.Value
	expected.Store("fresh-team-token")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/github/exchange":
			if exchanges.Add(1) > 1 {
				w.WriteHeader(503)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"teamToken": "fresh-team-token", "expiresAt": time.Now().Add(time.Hour)})
		case "/v1/status":
			if r.Header.Get("Authorization") != "Bearer "+expected.Load().(string) && r.Header.Get("Authorization") != "Bearer local-token" {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"projectId": r.Header.Get("X-LayerCache-Project"), "usageBytes": 123})
		case "/v1/reports":
			io.WriteString(w, `{"schemaVersion":"2"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	cfg, err := config.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = filepath.Join(dir, "cache")
	cfg.LocalToken = "local-token"
	cfg.Listen = strings.TrimPrefix(upstream.URL, "http://")
	cfg.TeamURL = upstream.URL
	cfg.TeamToken = "expired-token"
	cfg.TeamTokenExpiresAt = time.Now().Add(-time.Hour)
	cfg.GitHubCLIPath = gh
	path := filepath.Join(dir, "config.json")
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	fetch := reviewDashboardStart(t, path)
	if got := fetch().Projects[0].Team; got.State != "connected" {
		t.Fatalf("first refresh: %#v", got)
	}
	if got := fetch().Projects[0].Team; got.State != "connected" {
		t.Errorf("valid refreshed token discarded; second refresh = %#v, exchanges=%d", got, exchanges.Load())
	}
	if exchanges.Load() != 1 {
		t.Errorf("exchanged %d times instead of retaining fresh capability", exchanges.Load())
	}
	disk, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if disk.TeamToken != "expired-token" {
		t.Error("dashboard persisted refreshed capability")
	}
	// A changed Team endpoint must not receive the old endpoint's cached capability.
	var leaked atomic.Bool
	var newEndpointExchanges atomic.Int32
	replacement := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer fresh-team-token" {
			leaked.Store(true)
		}
		switch r.URL.Path {
		case "/v1/auth/github/exchange":
			newEndpointExchanges.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"teamToken": "replacement-team-token", "expiresAt": time.Now().Add(time.Hour)})
		case "/v1/status":
			if r.Header.Get("Authorization") != "Bearer replacement-team-token" {
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"projectId": r.Header.Get("X-LayerCache-Project"), "usageBytes": 456})
		case "/v1/reports":
			io.WriteString(w, `{"schemaVersion":"2"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer replacement.Close()
	cfg.TeamURL = replacement.URL
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if got := fetch().Projects[0].Team; got.State != "connected" || got.Status == nil || got.Status.UsageBytes != 456 {
		t.Errorf("new endpoint did not obtain its own capability: %#v", got)
	}
	if leaked.Load() || newEndpointExchanges.Load() != 1 {
		t.Errorf("cross-endpoint cached capability reuse: leaked=%v exchanges=%d", leaked.Load(), newEndpointExchanges.Load())
	}
	cfg.TeamURL = upstream.URL
	cfg.TeamToken = "new-login-token"
	cfg.TeamTokenExpiresAt = time.Now().Add(2 * time.Hour)
	expected.Store(cfg.TeamToken)
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if got := fetch().Projects[0].Team; got.State != "connected" {
		t.Errorf("subsequent CLI login not picked up: %#v", got)
	}
	// CLI logout must invalidate the in-memory capability even though the endpoint is unchanged.
	cfg.TeamToken = ""
	cfg.TeamTokenExpiresAt = time.Time{}
	cfg.GitHubCLIPath = ""
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if got := fetch().Projects[0].Team; got.State != "authentication-required" || got.Status != nil {
		t.Errorf("logout left dashboard authenticated: %#v", got)
	}
	cfg.ProjectID = "github.com/replaced/project"
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if got := fetch().Projects[0]; got.Local.Status != nil || got.Team.Status != nil {
		t.Errorf("replaced config leaked other project: %#v", got)
	}
}

func TestDashboardReviewUsesUnexpiredCapabilityWhenRefreshUnavailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell credential fixture")
	}
	dir := t.TempDir()
	gh := filepath.Join(dir, "gh")
	if err := os.WriteFile(gh, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/status" {
			json.NewEncoder(w).Encode(map[string]any{"projectId": r.Header.Get("X-LayerCache-Project"), "usageBytes": 321})
		} else {
			io.WriteString(w, `{"schemaVersion":"2"}`)
		}
	}))
	defer upstream.Close()
	cfg, err := config.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.Listen = strings.TrimPrefix(upstream.URL, "http://")
	cfg.TeamURL = upstream.URL
	cfg.TeamToken = "still-valid-token"
	cfg.TeamTokenExpiresAt = time.Now().Add(4 * time.Minute)
	cfg.GitHubCLIPath = gh
	path := filepath.Join(dir, "config.json")
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	fetch := reviewDashboardStart(t, path)
	if got := fetch().Projects[0].Team; got.State != "connected" {
		t.Fatalf("unexpired authorized token unnecessarily discarded on refresh failure: %#v", got)
	}
	cfg.TeamTokenExpiresAt = time.Now().Add(-time.Minute)
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if got := fetch().Projects[0].Team; got.State != "authentication-required" || got.Status != nil {
		t.Errorf("expired capability accepted after failed refresh: %#v", got)
	}
}

func TestDashboardReviewRejectsInvalidCLIArguments(t *testing.T) {
	cfg, err := config.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	tests := map[string][]string{
		"positional":          {"unexpected"},
		"public bind":         {"--listen", "0.0.0.0:7438", "--config", path},
		"localhost ambiguous": {"--listen", "localhost:7438", "--config", path},
		"invalid port":        {"--listen", "127.0.0.1:99999", "--config", path},
		"missing file":        {"--config", filepath.Join(t.TempDir(), "missing.json")},
		"duplicate project":   {"--config", path, "--config", path},
	}
	var many []string
	for i := 0; i < 33; i++ {
		many = append(many, "--config", path)
	}
	tests["project limit"] = many
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			if err := runDashboard(context.Background(), args, io.Discard, io.Discard); err == nil {
				t.Fatal("invalid arguments accepted")
			}
		})
	}
}
