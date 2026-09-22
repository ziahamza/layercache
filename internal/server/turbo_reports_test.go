package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/measurement"
)

func turboReportFixture(t *testing.T) (*Server, *measurement.SQLiteRepository, access.Claims) {
	t.Helper()
	repository, err := measurement.OpenSQLiteRepository(filepath.Join(t.TempDir(), "measurements.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	instance := &Server{config: config.Config{Role: "team", ProjectID: "github.com/acme/widget", LocalToken: "report-secret"}, measurements: repository, mux: http.NewServeMux()}
	instance.routes()
	claims := access.Claims{Subject: "github-actions:repo:acme/widget:ref:refs/heads/main", Project: instance.config.ProjectID, Integration: "turbo", Compatibility: "linux-node24", Repository: "acme/widget", RunID: "github-actions:acme/widget:123:1:check:456", WorkspaceID: "github-actions-check:acme/widget:456", Capabilities: []access.Capability{access.CapabilityRead}, ExpiresAt: time.Now().Add(time.Hour)}
	return instance, repository, claims
}
func postTurboReport(t *testing.T, instance *Server, claims access.Claims, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	token, err := access.MintCapabilityToken(instance.config.LocalToken, claims, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/reports/turbo", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	// Action posts contain only a signed bearer token, not a project header.
	// Exercise the production gateway's untrusted-hint routing then verification.
	gateway := &ProjectGateway{projects: map[string]*Server{instance.config.ProjectID: instance}}
	gateway.ServeHTTP(response, request)
	return response
}
func reportSummary(t *testing.T, id, status string, now time.Time) []byte {
	t.Helper()
	result, err := json.Marshal(map[string]any{"summaries": []any{map[string]any{"id": id, "version": "1", "execution": map[string]any{"startTime": now.Add(-time.Second).UnixMilli(), "endTime": now.UnixMilli()}, "tasks": []any{map[string]any{"taskId": "app#build", "hash": "build-hash", "dependencies": []string{}, "resolvedTaskDefinition": map[string]bool{"cache": true}, "cache": map[string]any{"status": status, "remote": status == "HIT", "source": "REMOTE", "timeSaved": 5000}, "execution": map[string]any{"startTime": now.Add(-time.Second).UnixMilli(), "endTime": now.UnixMilli(), "exitCode": 0}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func TestTurboReportEndpointReconcilesSignedJobAndIsIdempotent(t *testing.T) {
	instance, repository, claims := turboReportFixture(t)
	now := time.Now().Add(-time.Second).UTC()
	err := repository.ObserveTurbo(measurement.TurboObservation{RunID: claims.RunID, ArtifactID: measurement.TurboArtifactIdentity(claims.Project, claims.Compatibility, "build-hash"), Result: measurement.ResultHit, Source: measurement.SourceTeamCache, StartedAt: now.Add(-time.Second), FinishedAt: now.Add(-900 * time.Millisecond), Timing: measurement.Timing{Download: 100 * time.Millisecond}, Bytes: measurement.Bytes{Downloaded: 42}})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		response := postTurboReport(t, instance, claims, reportSummary(t, "summary-1", "HIT", now))
		if response.Code != 200 {
			t.Fatalf("response=%d %s", response.Code, response.Body.String())
		}
		report, err := repository.RunReport(claims.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if report.Hits != 1 || len(report.Outcomes) != 1 || report.Outcomes[0].Source != measurement.SourceTeamCache || report.GrossAvoidedTaskTime.Milliseconds == nil || *report.GrossAvoidedTaskTime.Milliseconds != 5000 {
			t.Fatalf("report=%+v", report)
		}
	}
}

func TestTurboReportEndpointScopesJobsAndRejectsForgedAuthority(t *testing.T) {
	instance, repository, claims := turboReportFixture(t)
	now := time.Now().Add(-time.Second).UTC()
	first := postTurboReport(t, instance, claims, reportSummary(t, "first", "MISS", now))
	if first.Code != 200 {
		t.Fatal(first.Code, first.Body.String())
	}
	other := claims
	other.RunID = "github-actions:acme/widget:123:1:check:789"
	other.WorkspaceID = "github-actions-check:acme/widget:789"
	second := postTurboReport(t, instance, other, reportSummary(t, "second", "HIT", now))
	if second.Code != 200 {
		t.Fatal(second.Code, second.Body.String())
	}
	a, err := repository.RunReport(claims.RunID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := repository.RunReport(other.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Misses != 1 || b.Hits != 1 || b.Outcomes[0].Source != measurement.SourceUnattributed {
		t.Fatalf("job reports: %+v %+v", a, b)
	}
	for name, mutate := range map[string]func(*access.Claims){
		"project":      func(c *access.Claims) { c.Project = "other-project" },
		"integration":  func(c *access.Claims) { c.Integration = "actions" },
		"unscoped-run": func(c *access.Claims) { c.RunID = "github-actions:acme/widget:123:1" },
		"workspace":    func(c *access.Claims) { c.WorkspaceID = "github-actions-check:acme/widget:other" },
		"non-ci":       func(c *access.Claims) { c.Subject = "engineer" },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := claims
			mutate(&invalid)
			response := postTurboReport(t, instance, invalid, reportSummary(t, "invalid", "MISS", now))
			wantStatus := 401
			if name == "project" {
				wantStatus = 404
			}
			if response.Code != wantStatus {
				t.Fatalf("status=%d", response.Code)
			}
		})
	}
	forged := bytes.Replace(reportSummary(t, "forged", "MISS", now), []byte(`{"summaries":`), []byte(`{"runId":"another-run","summaries":`), 1)
	if response := postTurboReport(t, instance, claims, forged); response.Code != 400 {
		t.Fatalf("forged client identity=%d", response.Code)
	}
}

func TestTurboReportEndpointRejectsUnboundedTaskTimingAndOldSummaries(t *testing.T) {
	instance, _, claims := turboReportFixture(t)
	old := reportSummary(t, "old", "MISS", time.Now().Add(-25*time.Hour))
	if response := postTurboReport(t, instance, claims, old); response.Code != 400 {
		t.Fatalf("old=%d", response.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(reportSummary(t, "bad-task", "MISS", time.Now().Add(-time.Second)), &body); err != nil {
		t.Fatal(err)
	}
	summary := body["summaries"].([]any)[0].(map[string]any)
	task := summary["tasks"].([]any)[0].(map[string]any)
	task["execution"].(map[string]any)["startTime"] = float64(1)
	encoded, _ := json.Marshal(body)
	if response := postTurboReport(t, instance, claims, encoded); response.Code != 400 {
		t.Fatalf("unbounded task=%d %s", response.Code, response.Body.String())
	}
}
