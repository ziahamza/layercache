package acceptance_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/dashboard"
	"github.com/layercache/layercache/internal/measurement"
	"github.com/layercache/layercache/internal/server"
)

// Exercise the installed CLI through a real multi-project gateway and real
// persistent reports, using reader capabilities rather than an admin token.
func TestDashboardGatewayReportsAndRecovery(t *testing.T) {
	binary := buildLayerCache(t)
	ctx := context.Background()
	teamConfigs := map[string]config.Config{}
	for index, alias := range []string{"one", "two"} {
		cfg, err := config.Defaults()
		if err != nil {
			t.Fatal(err)
		}
		cfg.Role, cfg.ProjectID = "team", "github.com/dashboard-qa/"+alias
		cfg.DataDir = filepath.Join(t.TempDir(), "cache")
		if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
			t.Fatal(err)
		}
		repository, err := measurement.OpenSQLiteRepository(filepath.Join(cfg.DataDir, "measurements.db"))
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now().UTC().Add(-time.Minute)
		cost := time.Second
		result := measurement.ResultHit
		source := measurement.SourceTeamCache
		if index == 1 {
			result = measurement.ResultMiss
			source = measurement.SourceNone
		}
		err = repository.Record(measurement.FinalOutcome{
			RunID: "dashboard-" + alias, WorkID: "task-" + alias, ArtifactID: "artifact-" + alias,
			Integration: measurement.IntegrationTurbo, Result: result, Source: source,
			StartedAt: started, FinishedAt: started.Add(10 * time.Millisecond),
			ProducerDuration: &cost, ExecutionDuration: &cost,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.Close(); err != nil {
			t.Fatal(err)
		}
		teamConfigs[alias] = cfg
	}
	gateway, err := server.NewProjectGateway(ctx, server.ProjectGatewayConfig{Origin: "http://127.0.0.1", Projects: teamConfigs})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	upstream := httptest.NewServer(gateway)
	defer upstream.Close()
	var paths []string
	var localConfigs []config.Config
	for _, alias := range []string{"one", "two"} {
		team := teamConfigs[alias]
		cfg, err := config.Defaults()
		if err != nil {
			t.Fatal(err)
		}
		cfg.ProjectID, cfg.TeamURL, cfg.DataDir = team.ProjectID, upstream.URL, filepath.Join(t.TempDir(), "cache")
		cfg.Listen = availableAddress(t) // no Local Cache daemon: Team must remain visible
		cfg.TeamToken, err = access.MintCapabilityToken(team.LocalToken, access.Claims{
			Subject: "dashboard-reader", Project: team.ProjectID,
			Capabilities: []access.Capability{access.CapabilityRead}, ExpiresAt: time.Now().Add(time.Hour),
		}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "config.json")
		if err := config.Save(path, cfg); err != nil {
			t.Fatal(err)
		}
		paths, localConfigs = append(paths, path), append(localConfigs, cfg)
	}
	read, stop := startDashboardAcceptance(t, binary, paths)
	defer stop()
	snapshot := read()
	if len(snapshot.Projects) != 2 {
		t.Fatalf("projects = %d", len(snapshot.Projects))
	}
	for index, project := range snapshot.Projects {
		if project.ID != localConfigs[index].ProjectID || project.Local.State != "unavailable" || project.Team.State != "connected" || project.Team.Report == nil {
			t.Fatalf("project %d missing isolated Team report: %#v", index, project)
		}
		if project.Team.Report.Hits != 1-index || project.Team.Report.Eligible != 1 {
			t.Fatalf("project %d received wrong project's report: %#v", index, project.Team.Report)
		}
	}
	// A new CLI login/config edit must replace the previous authorization without
	// restarting the dashboard; cached credentials must not mask revocation.
	bad := localConfigs[0]
	bad.TeamToken = localConfigs[1].TeamToken
	if err := config.Save(paths[0], bad); err != nil {
		t.Fatal(err)
	}
	snapshot = read()
	if snapshot.Projects[0].Team.State == "connected" || snapshot.Projects[0].Team.Report != nil || snapshot.Projects[1].Team.State != "connected" {
		t.Fatalf("cross-project credential was not isolated: %#v", snapshot.Projects)
	}
	if err := config.Save(paths[0], localConfigs[0]); err != nil {
		t.Fatal(err)
	}
	if read().Projects[0].Team.State != "connected" {
		t.Fatal("did not recover after restored credentials")
	}
	upstream.Close()
	for _, project := range read().Projects {
		if project.Team.State != "unavailable" || project.Team.Status != nil || project.Team.Report != nil {
			t.Fatal("outage displayed stale Team data")
		}
	}
}

func startDashboardAcceptance(t *testing.T, binary string, paths []string) (func() dashboard.Snapshot, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	args := []string{"dashboard"}
	for _, path := range paths {
		args = append(args, "--config", path)
	}
	command := exec.CommandContext(ctx, binary, args...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	deferOnFailure := func() { cancel(); _ = command.Wait() }
	t.Cleanup(cancel)
	links := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "http://") {
				links <- scanner.Text()
			}
		}
	}()
	var link string
	select {
	case link = <-links:
	case <-time.After(10 * time.Second):
		deferOnFailure()
		t.Fatal("dashboard did not print a connection link")
	}
	parsed, err := url.Parse(link)
	if err != nil {
		deferOnFailure()
		t.Fatal("dashboard link invalid")
	}
	token := parsed.Fragment
	parsed.Fragment, parsed.Path = "", "/api/snapshot"
	client := &http.Client{Timeout: 10 * time.Second}
	read := func() dashboard.Snapshot {
		t.Helper()
		request, err := http.NewRequest("GET", parsed.String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("dashboard request failed")
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("dashboard returned %d", response.StatusCode)
		}
		var snapshot dashboard.Snapshot
		if err := json.NewDecoder(response.Body).Decode(&snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	stop := func() {
		client.CloseIdleConnections()
		_ = command.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Error("dashboard did not exit cleanly")
			}
		case <-time.After(5 * time.Second):
			cancel()
			<-done
			t.Error("dashboard shutdown timed out")
		}
		cancel()
	}
	return read, stop
}
