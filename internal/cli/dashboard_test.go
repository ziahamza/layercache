package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/dashboard"
)

func TestDashboardCommandConnectsMultipleProjects(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/status" {
			json.NewEncoder(w).Encode(map[string]any{"projectId": r.Header.Get("X-LayerCache-Project"), "usageBytes": 1024})
		} else {
			io.WriteString(w, `{"schemaVersion":"2"}`)
		}
	}))
	defer upstream.Close()
	args := []string{"dashboard"}
	for _, name := range []string{"first", "second"} {
		cfg, err := config.Defaults()
		if err != nil {
			t.Fatal(err)
		}
		cfg.ProjectID = "github.com/acme/" + name
		cfg.DataDir = filepath.Join(t.TempDir(), "cache")
		cfg.Listen = strings.TrimPrefix(upstream.URL, "http://")
		if name == "first" {
			cfg.TeamURL, cfg.TeamToken = upstream.URL, "team-secret"
		}
		path := filepath.Join(t.TempDir(), "config.json")
		if err := config.Save(path, cfg); err != nil {
			t.Fatal(err)
		}
		args = append(args, "--config", path)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, args, writer, io.Discard); writer.Close() }()
	links := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "http://") {
				links <- scanner.Text()
			}
		}
	}()
	var link string
	select {
	case link = <-links:
	case <-time.After(5 * time.Second):
		t.Fatal("dashboard did not start")
	}
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	token := parsed.Fragment
	parsed.Fragment = ""
	parsed.Path = "/api/snapshot"
	r, _ := http.NewRequest("GET", parsed.String(), nil)
	r.Header.Set("Authorization", "Bearer "+token)
	client := newLocalCLIHTTPClient(5 * time.Second)
	defer client.CloseIdleConnections()
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var snapshot dashboard.Snapshot
	if err := json.NewDecoder(response.Body).Decode(&snapshot); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || len(snapshot.Projects) != 2 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.Projects[0].Local.State != "connected" || snapshot.Projects[0].Team.State != "connected" || snapshot.Projects[1].Team.State != "not-connected" {
		t.Fatalf("project states = %#v", snapshot.Projects)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dashboard failed to stop")
	}
}

func TestDashboardReadsOnlySelectedProjectAndRedactsRuntimeDetails(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer upstream-secret" || r.Header.Get("X-LayerCache-Project") != "github.com/acme/one" {
			t.Error("wrong authority")
		}
		switch r.URL.Path {
		case "/v1/status":
			io.WriteString(w, `{"projectId":"github.com/acme/one","usageBytes":1048576,"maxBytes":2097152,"artifacts":3,"runtimePid":123,"storagePool":{"path":"private-path"},"localToken":"upstream-secret"}`)
		case "/v1/reports":
			if r.URL.Query().Get("from") == "" || r.URL.Query().Get("to") == "" {
				t.Error("missing period")
			}
			io.WriteString(w, `{"schemaVersion":"2","eligible":10,"hits":8,"misses":2,"netEstimatedBuildTimeSaved":{"milliseconds":null,"confidence":"unknown","known":0,"total":10}}`)
		default:
			t.Error("unexpected upstream route")
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	now := time.Now().UTC()
	got := readDashboardCache(context.Background(), newCLIHTTPClient(time.Second), upstream.URL, "upstream-secret", "github.com/acme/one", now.Add(-time.Hour), now)
	if requests != 2 || got.State != "connected" || got.Status.UsageBytes != 1048576 || got.Report.Hits != 8 || got.Report.NetEstimatedBuildTimeSaved.Milliseconds != nil {
		t.Fatalf("unexpected projection: %#v", got)
	}
	encoded, _ := json.Marshal(got)
	for _, secret := range []string{"upstream-secret", "private-path", "runtimePid"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("leaked %s", secret)
		}
	}
}

func TestDashboardUnavailableIsNotAnEmptyCache(t *testing.T) {
	for _, test := range []struct {
		name, body string
		code       int
		state      string
	}{
		{"wrong project", `{"projectId":"github.com/other/project"}`, 200, "unavailable"},
		{"auth revoked", "upstream-secret", 403, "authentication-required"},
		{"outage", "upstream-secret", 503, "unavailable"},
		{"malformed", "{}{}", 200, "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(test.code); io.WriteString(w, test.body) }))
			defer upstream.Close()
			now := time.Now()
			got := readDashboardCache(context.Background(), newCLIHTTPClient(time.Second), upstream.URL, "token", "github.com/acme/one", now.Add(-time.Hour), now)
			if got.State != test.state || got.Status != nil || got.Report != nil {
				t.Fatalf("unexpected state: %#v", got)
			}
		})
	}
}

func TestDashboardNeverFollowsCredentialRedirect(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected = true }))
	defer target.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer upstream.Close()
	now := time.Now()
	got := readDashboardCache(context.Background(), newCLIHTTPClient(time.Second), upstream.URL, "token", "github.com/acme/one", now.Add(-time.Hour), now)
	if redirected || got.State != "unavailable" {
		t.Fatal("followed redirect")
	}
}

func TestDashboardPreservesUsageWhenReportingFails(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/status" {
			io.WriteString(w, `{"projectId":"github.com/acme/one","usageBytes":17}`)
		} else {
			w.WriteHeader(503)
		}
	}))
	defer upstream.Close()
	now := time.Now()
	got := readDashboardCache(context.Background(), newCLIHTTPClient(time.Second), upstream.URL, "token", "github.com/acme/one", now.Add(-time.Hour), now)
	if got.Status == nil || got.Status.UsageBytes != 17 || got.Report != nil || got.ReportState != "unavailable" {
		t.Fatalf("unexpected projection: %#v", got)
	}
}

func TestDashboardReportAuthenticationFailureRemainsActionable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/status" {
			io.WriteString(w, `{"projectId":"github.com/acme/one","usageBytes":17}`)
		} else {
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer upstream.Close()
	now := time.Now()
	got := readDashboardCache(context.Background(), newCLIHTTPClient(time.Second), upstream.URL, "token", "github.com/acme/one", now.Add(-time.Hour), now)
	if got.Status == nil || got.Status.UsageBytes != 17 || got.Report != nil || got.ReportState != "authentication-required" {
		t.Fatalf("unexpected projection: %#v", got)
	}
}
