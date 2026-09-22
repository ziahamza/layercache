package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/dashboard"
	"github.com/layercache/layercache/internal/measurement"
)

type dashboardCredentials struct {
	source    config.Config
	token     string
	expiresAt time.Time
}

func (cached dashboardCredentials) matches(cfg config.Config) bool {
	return sameCredentialScope(cached.source, cfg) &&
		cached.source.TeamToken == cfg.TeamToken && cached.source.TeamTokenExpiresAt.Equal(cfg.TeamTokenExpiresAt) &&
		cached.source.PublicAccessToken == cfg.PublicAccessToken && cached.source.PublicAccessTokenExpiresAt.Equal(cfg.PublicAccessTokenExpiresAt)
}

func runDashboard(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var paths []string
	flags.Func("config", "project configuration; repeat for multiple projects", func(path string) error { paths = append(paths, path); return nil })
	listen := flags.String("listen", "127.0.0.1:0", "loopback dashboard address")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("dashboard accepts flags only")
	}
	if len(paths) == 0 {
		paths = []string{defaultPath}
	}
	if len(paths) > 32 {
		return errors.New("dashboard supports at most 32 project configurations")
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || (host != "127.0.0.1" && host != "::1") {
		return errors.New("dashboard must listen on 127.0.0.1 or ::1")
	}
	ids := make([]string, len(paths))
	seen := map[string]bool{}
	for i, path := range paths {
		cfg, err := config.Load(path)
		if err != nil {
			return fmt.Errorf("load dashboard configuration %d: %w", i+1, err)
		}
		if cfg.Role != "local" {
			return errors.New("dashboard requires local CLI configurations; connect Team Cache with layercache login")
		}
		if seen[cfg.ProjectID] {
			return fmt.Errorf("duplicate dashboard project %s", cfg.ProjectID)
		}
		seen[cfg.ProjectID], ids[i] = true, cfg.ProjectID
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	token := hex.EncodeToString(secret)
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Serialize refreshes: credential managers and config refresh must not race
	// when several browser tabs refresh together.
	gate := make(chan struct{}, 1)
	credentials := make([]dashboardCredentials, len(paths))
	handler := dashboard.New(listener.Addr().String(), token, func(ctx context.Context, period time.Duration) dashboard.Snapshot {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		select {
		case gate <- struct{}{}:
			defer func() { <-gate }()
		case <-ctx.Done():
			return dashboard.Snapshot{}
		}
		now := time.Now().UTC()
		result := dashboard.Snapshot{Projects: make([]dashboard.Project, len(paths)), UpdatedAt: now, From: now.Add(-period), To: now}
		var work sync.WaitGroup
		for i, path := range paths {
			work.Add(1)
			go func(i int, path string) {
				defer work.Done()
				project := dashboard.Project{ID: ids[i], Local: dashboard.Cache{State: "unavailable", ReportState: "unavailable"}, Team: dashboard.Cache{State: "unavailable", ReportState: "unavailable"}}
				defer func() { result.Projects[i] = project }()
				cfg, err := config.Load(path)
				if err != nil || cfg.Role != "local" || cfg.ProjectID != ids[i] {
					return
				}
				project.Local = readDashboardCache(ctx, newLocalCLIHTTPClient(5*time.Second), localRuntimeURL(cfg.Listen), cfg.LocalToken, cfg.ProjectID, result.From, result.To)
				if cfg.TeamURL == "" {
					project.Team = dashboard.Cache{State: "not-connected", ReportState: "unavailable"}
					return
				}
				disk := cfg
				if credentials[i].matches(cfg) {
					cfg.TeamToken, cfg.TeamTokenExpiresAt = credentials[i].token, credentials[i].expiresAt
				}
				refreshCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				if err := refreshTeamCapability(refreshCtx, &cfg); err != nil {
					// Refresh is proactive. An unexpired capability can still be
					// checked by Team Cache while GitHub/the credential manager is down.
					if cfg.TeamToken == "" || !cfg.TeamTokenExpiresAt.After(time.Now()) {
						project.Team.State = "authentication-required"
						return
					}
				}
				credentials[i] = dashboardCredentials{source: disk, token: cfg.TeamToken, expiresAt: cfg.TeamTokenExpiresAt}
				project.Team = readDashboardCache(ctx, newCLIHTTPClient(5*time.Second), cfg.TeamURL, cfg.TeamToken, cfg.ProjectID, result.From, result.To)
			}(i, path)
		}
		work.Wait()
		return result
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, WriteTimeout: 40 * time.Second}
	go func() { <-ctx.Done(); _ = server.Close() }()
	if _, err := fmt.Fprintf(stdout, "Layer Cache dashboard\nhttp://%s/#%s\nConnected to %d project(s). Keep this process running; press Ctrl-C to stop.\n", listener.Addr(), token, len(paths)); err != nil {
		return err
	}
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func readDashboardCache(ctx context.Context, client *http.Client, endpoint, token, project string, from, to time.Time) dashboard.Cache {
	defer client.CloseIdleConnections()
	result := dashboard.Cache{State: "unavailable", ReportState: "unavailable"}
	if token == "" {
		result.State = "authentication-required"
		return result
	}
	read := func(path string, target any) string {
		request, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(endpoint, "/")+path, nil)
		if err != nil {
			return "unavailable"
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("X-LayerCache-Project", project)
		response, err := client.Do(request)
		if err != nil {
			return "unavailable"
		}
		defer response.Body.Close()
		if response.StatusCode == 401 || response.StatusCode == 403 {
			return "authentication-required"
		}
		if response.StatusCode != 200 {
			return "unavailable"
		}
		decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
		if decoder.Decode(target) != nil || decoder.Decode(&struct{}{}) != io.EOF {
			return "unavailable"
		}
		return "connected"
	}
	var status dashboard.Status
	result.State = read("/v1/status", &status)
	if result.State != "connected" {
		return result
	}
	if status.ProjectID != project {
		result.State = "unavailable"
		return result
	}
	result.Status = &status
	var report measurement.PeriodReport
	query := url.Values{"from": {from.Format(time.RFC3339Nano)}, "to": {to.Format(time.RFC3339Nano)}}
	result.ReportState = read("/v1/reports?"+query.Encode(), &report)
	if result.ReportState == "connected" && report.SchemaVersion == measurement.SchemaVersion {
		result.Report = &report
	} else if result.ReportState == "connected" {
		result.ReportState = "unavailable"
	}
	return result
}
