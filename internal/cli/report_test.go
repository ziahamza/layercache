package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
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

func TestHumanReportExplainsSourcesBytesEstimatesAndOverhead(t *testing.T) {
	t.Parallel()

	grossMilliseconds := int64(250)
	netMilliseconds := int64(-50)
	report := measurement.RunReport{
		RunID: "run-explained", Eligible: 3, Hits: 2, Misses: 1, HitRate: 2.0 / 3.0,
		Bytes: measurement.Bytes{Downloaded: 1536, Uploaded: 512},
		Sources: []measurement.SourceReport{
			{Source: measurement.SourceLocalCache, Hits: 1, Downloaded: 1024},
			{Source: measurement.SourcePublicCache, Hits: 1, Downloaded: 512},
		},
		GrossAvoidedTaskTime: measurement.Estimate{
			Milliseconds: &grossMilliseconds, Method: "producerDuration", Confidence: measurement.ConfidenceHigh,
			Known: 2, Total: 3,
		},
		Timing: measurement.TimingReport{LookupMS: 10, DownloadMS: 20, VerificationMS: 5, RestoreMS: 15, UploadMS: 50},
		NetEstimatedBuildTimeSaved: measurement.Estimate{
			Milliseconds: &netMilliseconds, Method: "criticalPath", Confidence: measurement.ConfidenceMedium,
			Known: 3, Total: 3,
		},
		Degraded: true,
	}
	var output bytes.Buffer
	if err := printRunReport(&output, report); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Layer Cache run run-explained",
		"Cache outcomes: 2/3 hits (67%); misses: 1",
		"Bytes: 1.5 KiB downloaded; 512 B uploaded",
		"Local Cache: hits: 1; 1.0 KiB downloaded; 0 B uploaded",
		"Public Cache: hits: 1; 512 B downloaded; 0 B uploaded",
		"Gross avoided task time: +250ms (producerDuration; high confidence; 2/3 known)",
		"Measured cache overhead: +100ms (lookup 10ms; download 20ms; verification 5ms; restore 15ms; upload 50ms)",
		"Net estimated build time saved: -50ms (criticalPath; medium confidence; 3/3 known)",
		"Degraded: yes",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("human report omitted %q:\n%s", want, output.String())
		}
	}

	output.Reset()
	period := measurement.PeriodReport{
		From: time.Date(2026, time.August, 30, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, time.August, 31, 0, 0, 0, 0, time.UTC),
		Runs: 4,
	}
	if err := printPeriodReport(&output, period); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Layer Cache period 2026-08-30T00:00:00Z to 2026-08-31T00:00:00Z (4 runs)") {
		t.Fatalf("period report omitted range and run count:\n%s", output.String())
	}
}
