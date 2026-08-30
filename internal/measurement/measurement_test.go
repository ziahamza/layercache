package measurement_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/measurement"
)

func TestRunReportKeepsNegativeNetSavings(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, time.August, 30, 8, 0, 0, 0, time.UTC)
	recorder := measurement.NewRecorder()
	err := recorder.Record(measurement.FinalOutcome{
		RunID:            "run-negative",
		WorkID:           "compile",
		Result:           measurement.ResultHit,
		Source:           measurement.SourceTeamCache,
		StartedAt:        start,
		FinishedAt:       start.Add(130 * time.Millisecond),
		ProducerDuration: duration(100 * time.Millisecond),
		Timing: measurement.Timing{
			Lookup:   20 * time.Millisecond,
			Download: 80 * time.Millisecond,
			Restore:  30 * time.Millisecond,
		},
		Bytes: measurement.Bytes{Downloaded: 4096},
	})
	if err != nil {
		t.Fatalf("record final outcome: %v", err)
	}

	report, err := recorder.RunReport("run-negative")
	if err != nil {
		t.Fatalf("build run report: %v", err)
	}

	assertEstimate(t, report.GrossAvoidedTaskTime, 100, "producerDuration", measurement.ConfidenceHigh, 1, 1)
	assertEstimate(t, report.NetEstimatedBuildTimeSaved, -30, "criticalPath", measurement.ConfidenceMedium, 1, 1)
	if report.HitRate != 1 {
		t.Fatalf("hit rate = %v, want 1", report.HitRate)
	}
	if report.Bytes.Downloaded != 4096 {
		t.Fatalf("downloaded bytes = %d, want 4096", report.Bytes.Downloaded)
	}
	if len(report.Sources) != 1 || report.Sources[0].Source != measurement.SourceTeamCache || report.Sources[0].Hits != 1 {
		t.Fatalf("source report = %#v, want one Team Cache hit", report.Sources)
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal JSON-ready report: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("decode report JSON: %v", err)
	}
	if document["schemaVersion"] != measurement.SchemaVersion {
		t.Fatalf("schema version = %#v, want %q", document["schemaVersion"], measurement.SchemaVersion)
	}
}

func TestRunReportUsesTheTaskGraphCriticalPath(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, time.August, 30, 9, 0, 0, 0, time.UTC)
	recorder := measurement.NewRecorder()
	for _, outcome := range []measurement.FinalOutcome{
		{
			RunID: "parallel", WorkID: "compile-a", Result: measurement.ResultHit,
			Source: measurement.SourceLocalCache, StartedAt: start, FinishedAt: start.Add(10 * time.Millisecond),
			ProducerDuration: duration(100 * time.Millisecond), Timing: measurement.Timing{Restore: 10 * time.Millisecond},
		},
		{
			RunID: "parallel", WorkID: "compile-b", Result: measurement.ResultHit,
			Source: measurement.SourceTeamCache, StartedAt: start, FinishedAt: start.Add(20 * time.Millisecond),
			ProducerDuration: duration(80 * time.Millisecond), Timing: measurement.Timing{Restore: 20 * time.Millisecond},
		},
		{
			RunID: "parallel", WorkID: "link", Dependencies: []string{"compile-a", "compile-b"}, Result: measurement.ResultMiss,
			Source: measurement.SourceNone, StartedAt: start.Add(20 * time.Millisecond), FinishedAt: start.Add(80 * time.Millisecond),
			ExecutionDuration: duration(50 * time.Millisecond), Timing: measurement.Timing{Lookup: 5 * time.Millisecond, Upload: 5 * time.Millisecond},
		},
	} {
		if err := recorder.Record(outcome); err != nil {
			t.Fatalf("record %s: %v", outcome.WorkID, err)
		}
	}

	report, err := recorder.RunReport("parallel")
	if err != nil {
		t.Fatalf("build run report: %v", err)
	}
	assertEstimate(t, report.GrossAvoidedTaskTime, 180, "producerDuration", measurement.ConfidenceHigh, 2, 2)
	// Cold critical path: max(100, 80) + 50 = 150ms.
	// Observed critical path: max(10, 20) + (50 + 10) = 80ms.
	assertEstimate(t, report.NetEstimatedBuildTimeSaved, 70, "criticalPath", measurement.ConfidenceMedium, 3, 3)
	if report.HitRate != float64(2)/3 {
		t.Fatalf("hit rate = %v, want 2/3", report.HitRate)
	}
}

func TestRunReportLeavesSavingsUnknownWhenProducerTimingIsMissing(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, time.August, 30, 10, 0, 0, 0, time.UTC)
	recorder := measurement.NewRecorder()
	if err := recorder.Record(measurement.FinalOutcome{
		RunID: "unknown", WorkID: "test", Result: measurement.ResultHit,
		Source: measurement.SourceUnattributed, StartedAt: start, FinishedAt: start.Add(10 * time.Millisecond),
		Timing: measurement.Timing{Lookup: 2 * time.Millisecond, Restore: 8 * time.Millisecond},
		Bytes:  measurement.Bytes{Downloaded: 512},
	}); err != nil {
		t.Fatalf("record final outcome: %v", err)
	}

	report, err := recorder.RunReport("unknown")
	if err != nil {
		t.Fatalf("build run report: %v", err)
	}
	for name, estimate := range map[string]measurement.Estimate{
		"gross": report.GrossAvoidedTaskTime,
		"net":   report.NetEstimatedBuildTimeSaved,
	} {
		if estimate.Milliseconds != nil || estimate.Confidence != measurement.ConfidenceUnknown || estimate.Known != 0 || estimate.Total != 1 {
			t.Fatalf("%s estimate = %#v, want unknown with 0/1 coverage", name, estimate)
		}
	}
	if report.Timing.LookupMS != 2 || report.Timing.RestoreMS != 8 || report.Bytes.Downloaded != 512 {
		t.Fatalf("observable costs were lost: timing=%#v bytes=%#v", report.Timing, report.Bytes)
	}
}

func TestPeriodReportAggregatesKnownSavingsAndShowsPartialCoverage(t *testing.T) {
	t.Parallel()

	day := time.Date(2026, time.August, 30, 0, 0, 0, 0, time.UTC)
	recorder := measurement.NewRecorder()
	for _, outcome := range []measurement.FinalOutcome{
		{
			RunID: "known-hit", WorkID: "build", Result: measurement.ResultHit, Source: measurement.SourceLocalCache,
			StartedAt: day.Add(8 * time.Hour), FinishedAt: day.Add(8*time.Hour + 10*time.Millisecond),
			ProducerDuration: duration(100 * time.Millisecond), Timing: measurement.Timing{Restore: 10 * time.Millisecond},
			Bytes: measurement.Bytes{Downloaded: 1000},
		},
		{
			RunID: "known-miss", WorkID: "build", Result: measurement.ResultMiss, Source: measurement.SourceNone,
			StartedAt: day.Add(9 * time.Hour), FinishedAt: day.Add(9*time.Hour + 60*time.Millisecond),
			ExecutionDuration: duration(50 * time.Millisecond), Timing: measurement.Timing{Lookup: 5 * time.Millisecond, Upload: 5 * time.Millisecond},
			Bytes: measurement.Bytes{Uploaded: 200}, Degraded: true,
		},
		{
			RunID: "unknown-hit", WorkID: "build", Result: measurement.ResultHit, Source: measurement.SourceUnattributed,
			StartedAt: day.Add(10 * time.Hour), FinishedAt: day.Add(10*time.Hour + 5*time.Millisecond),
			Timing: measurement.Timing{Restore: 5 * time.Millisecond}, Bytes: measurement.Bytes{Downloaded: 500},
		},
		{
			RunID: "outside", WorkID: "build", Result: measurement.ResultMiss, Source: measurement.SourceNone,
			StartedAt: day.Add(12 * time.Hour), FinishedAt: day.Add(12*time.Hour + 10*time.Millisecond),
			ExecutionDuration: duration(10 * time.Millisecond),
		},
	} {
		if err := recorder.Record(outcome); err != nil {
			t.Fatalf("record %s: %v", outcome.RunID, err)
		}
	}

	report, err := recorder.PeriodReport(day.Add(8*time.Hour), day.Add(11*time.Hour))
	if err != nil {
		t.Fatalf("build period report: %v", err)
	}
	if report.Runs != 3 || report.Eligible != 3 || report.Hits != 2 || report.Misses != 1 || report.HitRate != float64(2)/3 {
		t.Fatalf("period counts = %#v", report)
	}
	assertEstimate(t, report.GrossAvoidedTaskTime, 100, "producerDuration", measurement.ConfidenceLow, 1, 2)
	// Known run estimates are +90ms for the hit and -10ms for the miss.
	assertEstimate(t, report.NetEstimatedBuildTimeSaved, 80, "criticalPath", measurement.ConfidenceLow, 2, 3)
	if report.Bytes != (measurement.Bytes{Downloaded: 1500, Uploaded: 200}) {
		t.Fatalf("period bytes = %#v", report.Bytes)
	}
	if !report.Degraded {
		t.Fatal("period lost degraded state")
	}
}

func TestBacktestIsCausalAndExpiresArtifactsAfterTheRetentionWindow(t *testing.T) {
	t.Parallel()

	day := time.Date(2026, time.August, 30, 0, 0, 0, 0, time.UTC)
	run := func(runID, artifact string, startedAt time.Time, execution time.Duration) measurement.HistoricalRun {
		return measurement.HistoricalRun{
			RunID: runID, StartedAt: startedAt, FinishedAt: startedAt.Add(execution),
			Work: []measurement.HistoricalWork{{
				WorkID: "build", ArtifactID: artifact, CompatibilityID: "linux-amd64",
				ExecutionDuration: duration(execution), ArtifactBytes: 1024,
				HitTiming:  measurement.Timing{Lookup: 5 * time.Millisecond, Download: 5 * time.Millisecond, Restore: 10 * time.Millisecond},
				MissTiming: measurement.Timing{Lookup: 5 * time.Millisecond, Upload: 5 * time.Millisecond},
			}},
		}
	}

	result, err := measurement.Backtest([]measurement.HistoricalRun{
		// Deliberately first in the input. It cannot seed the earlier run.
		run("late-a", "artifact-a", day.Add(13*time.Hour), 100*time.Millisecond),
		run("early-a", "artifact-a", day.Add(10*time.Hour), 100*time.Millisecond),
		run("produce-b", "artifact-b", day.Add(10*time.Hour+30*time.Minute), 200*time.Millisecond),
		run("touch-b", "artifact-b", day.Add(11*time.Hour), 200*time.Millisecond),
		// This remains a hit only if the 11:00 restore refreshed retention.
		run("keep-b", "artifact-b", day.Add(12*time.Hour+45*time.Minute), 200*time.Millisecond),
		run("expired-b", "artifact-b", day.Add(15*time.Hour), 200*time.Millisecond),
	}, measurement.BacktestPolicy{Retention: 2 * time.Hour, HitSource: measurement.SourceTeamCache})
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}
	if result.Method != "causalReplay" || result.RetentionMS != int64((2*time.Hour)/time.Millisecond) {
		t.Fatalf("backtest method/retention = %q/%dms", result.Method, result.RetentionMS)
	}

	if len(result.Runs) != 6 || result.Runs[0].RunID != "early-a" || result.Runs[0].Outcomes[0].Result != measurement.ResultMiss {
		t.Fatalf("backtest did not replay in timestamp order: %#v", result.Runs)
	}
	if result.Period.Hits != 2 || result.Period.Misses != 4 {
		t.Fatalf("backtest outcomes = %d hits/%d misses, want 2/4", result.Period.Hits, result.Period.Misses)
	}
	assertEstimate(t, result.Period.GrossAvoidedTaskTime, 400, "producerDuration", measurement.ConfidenceHigh, 2, 2)
	// Four misses each add 10ms overhead. Each hit avoids 200ms and costs 20ms.
	assertEstimate(t, result.Period.NetEstimatedBuildTimeSaved, 320, "criticalPath", measurement.ConfidenceMedium, 6, 6)
	if len(result.Period.Sources) != 1 || result.Period.Sources[0].Source != measurement.SourceTeamCache || result.Period.Sources[0].Downloaded != 2048 {
		t.Fatalf("backtest source report = %#v", result.Period.Sources)
	}
}

func TestBacktestReportsUnknownTimingInsteadOfZeroSavings(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, time.August, 30, 15, 0, 0, 0, time.UTC)
	history := []measurement.HistoricalRun{
		{
			RunID: "unknown-producer", StartedAt: start, FinishedAt: start.Add(time.Minute),
			Work: []measurement.HistoricalWork{{
				WorkID: "build", ArtifactID: "artifact", CompatibilityID: "linux-amd64",
				MissTiming: measurement.Timing{Lookup: time.Millisecond},
			}},
		},
		{
			RunID: "unknown-consumer", StartedAt: start.Add(time.Hour), FinishedAt: start.Add(time.Hour + time.Minute),
			Work: []measurement.HistoricalWork{{
				WorkID: "build", ArtifactID: "artifact", CompatibilityID: "linux-amd64",
				HitTiming: measurement.Timing{Restore: time.Millisecond},
			}},
		},
	}
	result, err := measurement.Backtest(history, measurement.BacktestPolicy{})
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}
	if result.Period.Hits != 1 || result.Period.Misses != 1 {
		t.Fatalf("backtest outcomes = %d hits/%d misses", result.Period.Hits, result.Period.Misses)
	}
	for name, estimate := range map[string]measurement.Estimate{
		"gross": result.Period.GrossAvoidedTaskTime,
		"net":   result.Period.NetEstimatedBuildTimeSaved,
	} {
		if estimate.Milliseconds != nil || estimate.Confidence != measurement.ConfidenceUnknown || estimate.Known != 0 {
			t.Fatalf("%s estimate = %#v, want unknown", name, estimate)
		}
	}
}

func TestRunReportHasAStableJSONContract(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, time.August, 30, 16, 0, 0, 0, time.UTC)
	recorder := measurement.NewRecorder()
	if err := recorder.Record(measurement.FinalOutcome{
		RunID: "json", WorkID: "build", Result: measurement.ResultMiss, Source: measurement.SourceNone,
		StartedAt: start, FinishedAt: start.Add(55 * time.Millisecond),
		ExecutionDuration: duration(50 * time.Millisecond), Timing: measurement.Timing{Lookup: 5 * time.Millisecond},
		Bytes: measurement.Bytes{Uploaded: 123},
	}); err != nil {
		t.Fatalf("record final outcome: %v", err)
	}
	report, err := recorder.RunReport("json")
	if err != nil {
		t.Fatalf("build run report: %v", err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal run report: %v", err)
	}
	const want = `{"schemaVersion":"1","runId":"json","startedAt":"2026-08-30T16:00:00Z","finishedAt":"2026-08-30T16:00:00.055Z","outcomes":[{"workId":"build","result":"miss","source":"none","startedAt":"2026-08-30T16:00:00Z","finishedAt":"2026-08-30T16:00:00.055Z","producerDurationMs":null,"executionDurationMs":50,"timing":{"lookupMs":5,"downloadMs":0,"verificationMs":0,"restoreMs":0,"uploadMs":0},"bytes":{"downloaded":0,"uploaded":123},"degraded":false}],"eligible":1,"hits":0,"misses":1,"hitRate":0,"grossAvoidedTaskTime":{"milliseconds":0,"method":"producerDuration","confidence":"high","known":0,"total":0},"netEstimatedBuildTimeSaved":{"milliseconds":-5,"method":"criticalPath","confidence":"medium","known":1,"total":1},"timing":{"lookupMs":5,"downloadMs":0,"verificationMs":0,"restoreMs":0,"uploadMs":0},"bytes":{"downloaded":0,"uploaded":123},"sources":[],"degraded":false}`
	if string(encoded) != want {
		t.Fatalf("run report JSON changed\n got: %s\nwant: %s", encoded, want)
	}
}

func TestPeriodReportDoesNotInventCriticalPathSavingsForAPartiallyTimedRun(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, time.August, 30, 17, 0, 0, 0, time.UTC)
	recorder := measurement.NewRecorder()
	for _, outcome := range []measurement.FinalOutcome{
		{
			RunID: "partial", WorkID: "known", Result: measurement.ResultHit, Source: measurement.SourceLocalCache,
			StartedAt: start, FinishedAt: start.Add(10 * time.Millisecond),
			ProducerDuration: duration(100 * time.Millisecond), Timing: measurement.Timing{Restore: 10 * time.Millisecond},
		},
		{
			RunID: "partial", WorkID: "unknown", Result: measurement.ResultHit, Source: measurement.SourceLocalCache,
			StartedAt: start, FinishedAt: start.Add(10 * time.Millisecond), Timing: measurement.Timing{Restore: 10 * time.Millisecond},
		},
	} {
		if err := recorder.Record(outcome); err != nil {
			t.Fatalf("record %s: %v", outcome.WorkID, err)
		}
	}
	report, err := recorder.PeriodReport(start.Add(-time.Minute), start.Add(time.Minute))
	if err != nil {
		t.Fatalf("build period report: %v", err)
	}
	if report.NetEstimatedBuildTimeSaved.Milliseconds != nil || report.NetEstimatedBuildTimeSaved.Confidence != measurement.ConfidenceUnknown || report.NetEstimatedBuildTimeSaved.Known != 0 || report.NetEstimatedBuildTimeSaved.Total != 2 {
		t.Fatalf("net estimate = %#v, want unavailable with 0/2 usable critical-path coverage", report.NetEstimatedBuildTimeSaved)
	}
}

func TestRunReportOrdersOutcomesAndDependenciesStably(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, time.August, 30, 18, 0, 0, 0, time.UTC)
	recorder := measurement.NewRecorder()
	for _, outcome := range []measurement.FinalOutcome{
		{RunID: "order", WorkID: "b", Dependencies: []string{"c", "a"}, Result: measurement.ResultMiss, Source: measurement.SourceNone, StartedAt: start, FinishedAt: start.Add(time.Millisecond), ExecutionDuration: duration(time.Millisecond)},
		{RunID: "order", WorkID: "c", Result: measurement.ResultMiss, Source: measurement.SourceNone, StartedAt: start, FinishedAt: start.Add(time.Millisecond), ExecutionDuration: duration(time.Millisecond)},
		{RunID: "order", WorkID: "a", Result: measurement.ResultMiss, Source: measurement.SourceNone, StartedAt: start, FinishedAt: start.Add(time.Millisecond), ExecutionDuration: duration(time.Millisecond)},
	} {
		if err := recorder.Record(outcome); err != nil {
			t.Fatalf("record %s: %v", outcome.WorkID, err)
		}
	}
	report, err := recorder.RunReport("order")
	if err != nil {
		t.Fatalf("build run report: %v", err)
	}
	if got := []string{report.Outcomes[0].WorkID, report.Outcomes[1].WorkID, report.Outcomes[2].WorkID}; got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("outcome order = %v", got)
	}
	if dependencies := report.Outcomes[1].Dependencies; len(dependencies) != 2 || dependencies[0] != "a" || dependencies[1] != "c" {
		t.Fatalf("dependency order = %v, want [a c]", dependencies)
	}
}

func TestEmptyBacktestStillReturnsACompleteJSONShape(t *testing.T) {
	t.Parallel()

	result, err := measurement.Backtest(nil, measurement.BacktestPolicy{})
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal backtest: %v", err)
	}
	var document struct {
		Runs   []json.RawMessage `json:"runs"`
		Period struct {
			Sources []json.RawMessage    `json:"sources"`
			Gross   measurement.Estimate `json:"grossAvoidedTaskTime"`
			Net     measurement.Estimate `json:"netEstimatedBuildTimeSaved"`
		} `json:"period"`
	}
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("decode backtest: %v", err)
	}
	if document.Runs == nil || document.Period.Sources == nil {
		t.Fatalf("empty collections encoded as null: %s", encoded)
	}
	assertEstimate(t, document.Period.Gross, 0, "producerDuration", measurement.ConfidenceHigh, 0, 0)
	assertEstimate(t, document.Period.Net, 0, "criticalPath", measurement.ConfidenceMedium, 0, 0)
}

func TestNetSavingsSumsSubMillisecondOverheadBeforeRounding(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, time.August, 30, 19, 0, 0, 0, time.UTC)
	recorder := measurement.NewRecorder()
	if err := recorder.Record(measurement.FinalOutcome{
		RunID: "rounding", WorkID: "build", Result: measurement.ResultHit, Source: measurement.SourceLocalCache,
		StartedAt: start, FinishedAt: start.Add(1500 * time.Microsecond),
		ProducerDuration: duration(2 * time.Millisecond),
		Timing:           measurement.Timing{Lookup: 750 * time.Microsecond, Restore: 750 * time.Microsecond},
	}); err != nil {
		t.Fatalf("record final outcome: %v", err)
	}
	report, err := recorder.RunReport("rounding")
	if err != nil {
		t.Fatalf("build run report: %v", err)
	}
	assertEstimate(t, report.NetEstimatedBuildTimeSaved, 1, "criticalPath", measurement.ConfidenceMedium, 1, 1)
}

func duration(value time.Duration) *time.Duration {
	return &value
}

func assertEstimate(t *testing.T, got measurement.Estimate, milliseconds int64, method string, confidence measurement.Confidence, known, total int) {
	t.Helper()
	if got.Milliseconds == nil || *got.Milliseconds != milliseconds {
		t.Fatalf("estimate milliseconds = %v, want %d", got.Milliseconds, milliseconds)
	}
	if got.Method != method || got.Confidence != confidence || got.Known != known || got.Total != total {
		t.Fatalf("estimate metadata = %#v, want method=%q confidence=%q coverage=%d/%d", got, method, confidence, known, total)
	}
}
