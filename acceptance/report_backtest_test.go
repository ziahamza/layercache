package acceptance_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/measurement"
)

func TestInstalledCLIReportsPersistedHistoricalROI(t *testing.T) {
	root := t.TempDir()
	binary := buildLayerCache(t)
	address := availableAddress(t)
	configPath := filepath.Join(root, "config.json")
	dataDir := filepath.Join(root, "cache")
	runBinary(t, binary,
		"setup", "--config", configPath, "--data-dir", dataDir, "--listen", address,
		"--project", "github.com/acme/widgets", "--actions-repository", "acme/widgets",
		"--non-interactive", "--json",
	)
	repository, err := measurement.OpenSQLiteRepository(filepath.Join(dataDir, "measurements.db"))
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, time.August, 30, 0, 0, 0, 0, time.UTC)
	producer := 100 * time.Millisecond
	for _, outcome := range []measurement.FinalOutcome{
		{
			RunID: "negative", WorkID: "build", Result: measurement.ResultHit, Source: measurement.SourceTeamCache,
			StartedAt: day.Add(8 * time.Hour), FinishedAt: day.Add(8*time.Hour + 130*time.Millisecond),
			ProducerDuration: &producer,
			Timing:           measurement.Timing{Lookup: 20 * time.Millisecond, Download: 80 * time.Millisecond, Restore: 30 * time.Millisecond},
		},
		{
			RunID: "unknown", WorkID: "build", Result: measurement.ResultHit, Source: measurement.SourceUnattributed,
			StartedAt: day.Add(9 * time.Hour), FinishedAt: day.Add(9*time.Hour + 10*time.Millisecond),
			Timing: measurement.Timing{Restore: 10 * time.Millisecond},
		},
		{
			RunID: "outside", WorkID: "build", Result: measurement.ResultMiss, Source: measurement.SourceNone,
			StartedAt: day.Add(12 * time.Hour), FinishedAt: day.Add(12*time.Hour + time.Millisecond),
		},
	} {
		if err := repository.Record(outcome); err != nil {
			repository.Close()
			t.Fatal(err)
		}
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	runtime := startLayerCache(t, binary, configPath, address)
	defer runtime.stop(t)

	output := runBinary(t, binary,
		"report", "--config", configPath,
		"--from", day.Add(8*time.Hour).Format(time.RFC3339),
		"--to", day.Add(10*time.Hour).Format(time.RFC3339), "--json",
	)
	var report measurement.PeriodReport
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatalf("decode period report: %v\n%s", err, output)
	}
	if report.Runs != 2 || report.Hits != 2 || report.Misses != 0 {
		t.Fatalf("period counts = %#v", report)
	}
	net := report.NetEstimatedBuildTimeSaved
	if net.Milliseconds == nil || *net.Milliseconds != -30 || net.Known != 1 || net.Total != 2 || net.Confidence != measurement.ConfidenceLow {
		t.Fatalf("period net estimate = %#v, want -30ms with 1/2 coverage", net)
	}
}

func TestInstalledCLIBacktestIsCausalAndKeepsUnknownAndNegativeROI(t *testing.T) {
	binary := buildLayerCache(t)
	inputPath := filepath.Join(t.TempDir(), "history.json")
	const input = `{
  "schemaVersion": "1",
  "runs": [
    {"runId":"future-listed-first","startedAt":"2026-08-30T12:00:00Z","finishedAt":"2026-08-30T12:00:00.100Z","work":[{"workId":"build","artifactId":"a","compatibilityId":"linux-amd64","executionDurationMs":100}]},
    {"runId":"early-producer","startedAt":"2026-08-30T10:00:00Z","finishedAt":"2026-08-30T10:00:00.100Z","work":[{"workId":"build","artifactId":"a","compatibilityId":"linux-amd64","executionDurationMs":100}]},
    {"runId":"starts-before-artifact-exists","startedAt":"2026-08-30T10:00:00.050Z","finishedAt":"2026-08-30T10:00:00.150Z","work":[{"workId":"build","artifactId":"a","compatibilityId":"linux-amd64","executionDurationMs":100}]},
    {"runId":"negative-hit","startedAt":"2026-08-30T11:00:00Z","finishedAt":"2026-08-30T11:00:00.150Z","work":[{"workId":"build","artifactId":"a","compatibilityId":"linux-amd64","hitTiming":{"lookupMs":80,"downloadMs":50,"restoreMs":20}}]},
    {"runId":"unknown-producer","startedAt":"2026-08-30T09:00:00Z","finishedAt":"2026-08-30T09:00:00.100Z","work":[{"workId":"build","artifactId":"b","compatibilityId":"linux-amd64"}]},
    {"runId":"unknown-hit","startedAt":"2026-08-30T09:30:00Z","finishedAt":"2026-08-30T09:30:00.010Z","work":[{"workId":"build","artifactId":"b","compatibilityId":"linux-amd64","hitTiming":{"restoreMs":10}}]}
  ]
}`
	if err := os.WriteFile(inputPath, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	output := runBinary(t, binary,
		"backtest", "--input", inputPath, "--retention", "24h", "--source", "publicCache", "--json",
	)
	var report measurement.BacktestReport
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatalf("decode backtest report: %v\n%s", err, output)
	}
	if len(report.Runs) != 6 || report.Runs[0].RunID != "unknown-producer" {
		t.Fatalf("backtest run order = %#v", report.Runs)
	}
	byID := make(map[string]measurement.RunReport, len(report.Runs))
	for _, run := range report.Runs {
		byID[run.RunID] = run
	}
	if byID["starts-before-artifact-exists"].Outcomes[0].Result != measurement.ResultMiss {
		t.Fatalf("future artifact leaked into replay: %#v", byID["starts-before-artifact-exists"])
	}
	negative := byID["negative-hit"].NetEstimatedBuildTimeSaved
	if negative.Milliseconds == nil || *negative.Milliseconds != -50 {
		t.Fatalf("negative ROI = %#v, want -50ms", negative)
	}
	unknown := byID["unknown-hit"].NetEstimatedBuildTimeSaved
	if unknown.Milliseconds != nil || unknown.Confidence != measurement.ConfidenceUnknown || unknown.Known != 0 || unknown.Total != 1 {
		t.Fatalf("unknown timing coverage = %#v", unknown)
	}
}
