package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/measurement"
)

// Opt-in manual/installed-client acceptance. Uses real Turbo and two fresh
// workspaces so its own local cache cannot masquerade as Team Cache reuse.
func TestTurboReportRealClient(t *testing.T) {
	binary := os.Getenv("LAYERCACHE_TURBO_QA_BINARY")
	if binary == "" {
		t.Skip("set LAYERCACHE_TURBO_QA_BINARY to a real Turbo executable")
	}
	cfg := config.Config{Version: 1, Role: "team", DataDir: t.TempDir(), Listen: "127.0.0.1:7437", MaxBytes: 128 << 20, ProjectID: "github.com/acme/widget", LocalToken: "qa-only-secret", CompatibilityID: "linux-node24", ActionsRepository: "acme/widget", ActionsRef: "refs/heads/main", ActionsDefaultRef: "refs/heads/main", BuildkitBuilder: "qa"}
	instance, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	endpoint := httptest.NewServer(instance.Handler())
	t.Cleanup(endpoint.Close)
	for index, check := range []string{"producer", "consumer"} {
		workspace := t.TempDir()
		for _, name := range []string{"package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml", "turbo.json", "packages/app/package.json", "packages/app/build.mjs"} {
			contents, err := os.ReadFile(filepath.Join("../../qa/fixtures/turbo", name))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(workspace, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, contents, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		claims := access.Claims{Subject: "github-actions:qa", Project: cfg.ProjectID, Integration: "turbo", Compatibility: cfg.CompatibilityID, Repository: cfg.ActionsRepository, RunID: "github-actions:acme/widget:qa:1:check:" + check, WorkspaceID: "github-actions-check:acme/widget:" + check, Capabilities: []access.Capability{access.CapabilityWrite}, ExpiresAt: time.Now().Add(time.Hour)}
		token, err := access.MintCapabilityToken(cfg.LocalToken, claims, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		command := exec.CommandContext(ctx, binary, "run", "build")
		command.Dir = workspace
		for _, value := range os.Environ() {
			if !strings.HasPrefix(value, "TURBO_") && !strings.HasPrefix(value, "EXECUTION_COUNTER=") {
				command.Env = append(command.Env, value)
			}
		}
		command.Env = append(command.Env, "TURBO_API="+endpoint.URL, "TURBO_TEAM="+cfg.ProjectID, "TURBO_TOKEN="+token, "TURBO_RUN_SUMMARY=true", "TURBO_TELEMETRY_DISABLED=1")
		output, err := command.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("%s Turbo: %v\n%s", check, err, output)
		}
		summaries, err := filepath.Glob(filepath.Join(workspace, ".turbo/runs/*.json"))
		if err != nil || len(summaries) != 1 {
			t.Fatalf("summaries=%v err=%v", summaries, err)
		}
		document, err := os.ReadFile(summaries[0])
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(map[string]any{"summaries": []json.RawMessage{document}})
		if err != nil {
			t.Fatal(err)
		}
		response := postTurboReport(t, instance, claims, body)
		if response.Code != 200 {
			t.Fatalf("%s report=%d %s", check, response.Code, response.Body.String())
		}
		report, err := instance.measurements.RunReport(claims.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if report.Eligible != 1 || report.Hits != index {
			t.Fatalf("%s report=%+v", check, report)
		}
		if index == 1 && (len(report.Outcomes) != 1 || report.Outcomes[0].Source != measurement.SourceTeamCache || report.GrossAvoidedTaskTime.Known != 1) {
			t.Fatalf("consumer provenance/timing=%+v", report)
		}
		artifact, err := os.ReadFile(filepath.Join(workspace, "packages/app/dist/result.txt"))
		if err != nil || string(artifact) != "layercache turbo fixture\n" {
			t.Fatalf("artifact=%q err=%v", artifact, err)
		}
		t.Logf("%s: %d/%d hits; %s", check, report.Hits, report.Eligible, response.Body.String())
	}
}
