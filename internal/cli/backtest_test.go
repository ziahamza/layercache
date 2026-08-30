package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/layercache/layercache/internal/measurement"
)

func TestBacktestCLIReplaysDocumentedHistoryCausally(t *testing.T) {
	t.Parallel()

	inputPath := filepath.Join(t.TempDir(), "history.json")
	const input = `{
  "schemaVersion": "1",
  "runs": [
    {
      "runId": "future-listed-first",
      "startedAt": "2026-08-30T12:00:00Z",
      "finishedAt": "2026-08-30T12:00:00.100Z",
      "work": [{"workId":"build","artifactId":"a","compatibilityId":"linux-amd64","executionDurationMs":100,"artifactBytes":1000}]
    },
    {
      "runId": "early-producer",
      "startedAt": "2026-08-30T10:00:00Z",
      "finishedAt": "2026-08-30T10:00:00.100Z",
      "work": [{"workId":"build","artifactId":"a","compatibilityId":"linux-amd64","executionDurationMs":100,"artifactBytes":1000}]
    },
    {
      "runId": "starts-before-artifact-exists",
      "startedAt": "2026-08-30T10:00:00.050Z",
      "finishedAt": "2026-08-30T10:00:00.150Z",
      "work": [{"workId":"build","artifactId":"a","compatibilityId":"linux-amd64","executionDurationMs":100,"artifactBytes":1000}]
    },
    {
      "runId": "negative-hit",
      "startedAt": "2026-08-30T11:00:00Z",
      "finishedAt": "2026-08-30T11:00:00.150Z",
      "work": [{
        "workId":"build","artifactId":"a","compatibilityId":"linux-amd64","artifactBytes":1000,
        "hitTiming":{"lookupMs":80,"downloadMs":50,"restoreMs":20}
      }]
    },
    {
      "runId": "unknown-producer",
      "startedAt": "2026-08-30T09:00:00Z",
      "finishedAt": "2026-08-30T09:00:00.100Z",
      "work": [{"workId":"build","artifactId":"b","compatibilityId":"linux-amd64"}]
    },
    {
      "runId": "unknown-hit",
      "startedAt": "2026-08-30T09:30:00Z",
      "finishedAt": "2026-08-30T09:30:00.010Z",
      "work": [{"workId":"build","artifactId":"b","compatibilityId":"linux-amd64","hitTiming":{"restoreMs":10}}]
    }
  ]
}`
	if err := os.WriteFile(inputPath, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := Run(context.Background(), []string{
		"backtest", "--input", inputPath, "--retention", "24h", "--source", "publicCache", "--json",
	}, &stdout, &stderr); err != nil {
		t.Fatalf("backtest command: %v\n%s", err, stderr.String())
	}
	var report measurement.BacktestReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode backtest: %v\n%s", err, stdout.String())
	}
	if len(report.Runs) != 6 || report.Runs[0].RunID != "unknown-producer" {
		t.Fatalf("causal run order = %#v", report.Runs)
	}
	byID := make(map[string]measurement.RunReport, len(report.Runs))
	for _, run := range report.Runs {
		byID[run.RunID] = run
	}
	if byID["starts-before-artifact-exists"].Outcomes[0].Result != measurement.ResultMiss {
		t.Fatalf("future artifact was used: %#v", byID["starts-before-artifact-exists"])
	}
	negative := byID["negative-hit"].NetEstimatedBuildTimeSaved
	if negative.Milliseconds == nil || *negative.Milliseconds != -50 {
		t.Fatalf("negative savings = %#v, want -50ms", negative)
	}
	unknown := byID["unknown-hit"].NetEstimatedBuildTimeSaved
	if unknown.Milliseconds != nil || unknown.Confidence != measurement.ConfidenceUnknown || unknown.Known != 0 || unknown.Total != 1 {
		t.Fatalf("unknown coverage = %#v", unknown)
	}
	if byID["negative-hit"].Sources[0].Source != measurement.SourcePublicCache {
		t.Fatalf("backtest source = %#v", byID["negative-hit"].Sources)
	}
}
