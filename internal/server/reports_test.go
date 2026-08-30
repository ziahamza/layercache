package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/measurement"
)

func TestPeriodReportEndpointReturnsPersistedHalfOpenPeriod(t *testing.T) {
	t.Parallel()

	repository, err := measurement.OpenSQLiteRepository(filepath.Join(t.TempDir(), "measurements.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	day := time.Date(2026, time.August, 30, 0, 0, 0, 0, time.UTC)
	execution := 50 * time.Millisecond
	for _, outcome := range []measurement.FinalOutcome{
		{
			RunID: "inside", WorkID: "build", Result: measurement.ResultMiss, Source: measurement.SourceNone,
			StartedAt: day.Add(time.Hour), FinishedAt: day.Add(time.Hour + 55*time.Millisecond),
			ExecutionDuration: &execution, Timing: measurement.Timing{Lookup: 5 * time.Millisecond},
		},
		{
			RunID: "at-exclusive-end", WorkID: "build", Result: measurement.ResultMiss, Source: measurement.SourceNone,
			StartedAt: day.Add(2 * time.Hour), FinishedAt: day.Add(2*time.Hour + 55*time.Millisecond),
			ExecutionDuration: &execution, Timing: measurement.Timing{Lookup: 5 * time.Millisecond},
		},
	} {
		if err := repository.Record(outcome); err != nil {
			t.Fatal(err)
		}
	}
	instance := &Server{
		config:       config.Config{Role: "local", LocalToken: "report-secret"},
		measurements: repository,
		mux:          http.NewServeMux(),
	}
	instance.routes()

	target := "/v1/reports?from=" + day.Format(time.RFC3339) + "&to=" + day.Add(2*time.Hour).Format(time.RFC3339)
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("Authorization", "Bearer report-secret")
	response := httptest.NewRecorder()
	instance.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("period report status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var report measurement.PeriodReport
	if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
		t.Fatal(err)
	}
	if report.Runs != 1 || report.Misses != 1 || report.From != day || report.To != day.Add(2*time.Hour) {
		t.Fatalf("period report = %#v", report)
	}
}

func TestPeriodReportEndpointRejectsInvalidBounds(t *testing.T) {
	t.Parallel()

	repository, err := measurement.OpenSQLiteRepository(filepath.Join(t.TempDir(), "measurements.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	instance := &Server{
		config:       config.Config{Role: "local", LocalToken: "report-secret"},
		measurements: repository,
		mux:          http.NewServeMux(),
	}
	instance.routes()
	request := httptest.NewRequest(http.MethodGet, "/v1/reports?from=not-a-time&to=2026-08-30T12:00:00Z", nil)
	request.Header.Set("Authorization", "Bearer report-secret")
	response := httptest.NewRecorder()
	instance.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid period status = %d, want 400: %s", response.Code, response.Body.String())
	}
}
