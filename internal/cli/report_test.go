package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/measurement"
)

func TestReportCLIRequestsPersistedPeriodAsJSON(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	day := time.Date(2026, time.August, 30, 0, 0, 0, 0, time.UTC)
	want := measurement.PeriodReport{
		SchemaVersion: measurement.SchemaVersion, From: day, To: day.Add(24 * time.Hour),
		Runs: 2, Eligible: 3, Hits: 2, Misses: 1, HitRate: 2.0 / 3.0,
		Sources: []measurement.SourceReport{},
	}
	requestSeen := make(chan struct{}, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/reports" || request.URL.Query().Get("from") != day.Format(time.RFC3339) ||
			request.URL.Query().Get("to") != day.Add(24*time.Hour).Format(time.RFC3339) {
			t.Errorf("report request = %s", request.URL.String())
		}
		if request.Header.Get("Authorization") != "Bearer report-token" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		requestSeen <- struct{}{}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(want)
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	configPath, _ := setupConfigWithAddress(t, listener.Addr().String(), "report-token")

	var stdout, stderr bytes.Buffer
	err = runReport(context.Background(), []string{
		"--config", configPath, "--from", day.Format(time.RFC3339),
		"--to", day.Add(24 * time.Hour).Format(time.RFC3339), "--json",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("period report command: %v\n%s", err, stderr.String())
	}
	select {
	case <-requestSeen:
	case <-time.After(time.Second):
		t.Fatal("runtime did not receive period report request")
	}
	var got measurement.PeriodReport
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode report output: %v\n%s", err, stdout.String())
	}
	if got.Runs != want.Runs || got.Hits != want.Hits || got.From != want.From || got.To != want.To {
		t.Fatalf("period report = %#v", got)
	}
}

func TestReportCLIRequiresExactlyOneRunOrPeriodSelector(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{},
		{"--run", "run-1", "--from", "2026-08-30T00:00:00Z", "--to", "2026-08-31T00:00:00Z"},
		{"--from", "2026-08-30T00:00:00Z"},
	} {
		var stdout, stderr bytes.Buffer
		if err := runReport(context.Background(), args, &stdout, &stderr); err == nil {
			t.Fatalf("runReport(%v) succeeded", args)
		}
	}
}
