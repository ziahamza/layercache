package measurement_test

import (
	"testing"
	"time"

	"github.com/layercache/layercache/internal/measurement"
	"github.com/layercache/layercache/internal/retention"
)

func TestBacktestComparesImpactWithLRUWithoutFutureTiming(t *testing.T) {
	start := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	run := func(id, artifact string, minute int, producerMS int64) measurement.HistoricalRun {
		execution := time.Duration(producerMS) * time.Millisecond
		at := start.Add(time.Duration(minute) * time.Minute)
		return measurement.HistoricalRun{RunID: id, StartedAt: at, FinishedAt: at.Add(10 * time.Second), Work: []measurement.HistoricalWork{{
			WorkID: "build", ArtifactID: artifact, CompatibilityID: "linux-amd64", ArtifactBytes: 4,
			ExecutionDuration: &execution, HitTiming: measurement.Timing{Restore: time.Millisecond},
		}}}
	}
	history := []measurement.HistoricalRun{
		run("build-expensive", "a", 0, 6000), run("build-cheap", "b", 1, 10),
		run("build-new", "c", 2, 10), run("reuse-expensive", "a", 3, 6000),
	}
	for _, policy := range []retention.Policy{retention.LRU, retention.Impact} {
		report, err := measurement.Backtest(history, measurement.BacktestPolicy{MaxBytes: 8, EvictionPolicy: policy})
		if err != nil {
			t.Fatal(err)
		}
		wantHits, wantMS := 0, int64(0)
		if policy == retention.Impact {
			wantHits, wantMS = 1, 5999
		}
		if report.Period.Hits != wantHits || report.Period.NetEstimatedBuildTimeSaved.Milliseconds == nil || *report.Period.NetEstimatedBuildTimeSaved.Milliseconds != wantMS {
			t.Fatalf("%s replay hits/savings = %d/%v, expected %d/%d", policy, report.Period.Hits, report.Period.NetEstimatedBuildTimeSaved.Milliseconds, wantHits, wantMS)
		}
		if policy == retention.Impact && report.ImpactEvictions != 1 {
			t.Fatalf("impact decisions = %d", report.ImpactEvictions)
		}
	}
	// Without the first producer's timing, the later expensive run cannot
	// retroactively influence the eviction. Replay must choose LRU and miss.
	history[0].Work[0].ExecutionDuration = nil
	report, err := measurement.Backtest(history, measurement.BacktestPolicy{MaxBytes: 8, EvictionPolicy: retention.Impact})
	if err != nil {
		t.Fatal(err)
	}
	if report.Period.Hits != 0 || report.LRUFallbackEvictions != 1 || report.ImpactEvictions != 0 {
		t.Fatalf("unknown timing borrowed future value: %+v", report)
	}
}

func TestBacktestUnknownCostFallbackPreservesLRUTies(t *testing.T) {
	start := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	work := func(artifact string, known bool) measurement.HistoricalWork {
		result := measurement.HistoricalWork{WorkID: artifact, ArtifactID: artifact, CompatibilityID: "linux-amd64", ArtifactBytes: 4}
		if known {
			duration := time.Second
			result.ExecutionDuration = &duration
		}
		return result
	}
	run := func(id string, minute int, work ...measurement.HistoricalWork) measurement.HistoricalRun {
		at := start.Add(time.Duration(minute) * time.Minute)
		return measurement.HistoricalRun{RunID: id, StartedAt: at, FinishedAt: at.Add(10 * time.Second), Work: work}
	}
	history := []measurement.HistoricalRun{
		run("old-z", 0, work("z", false)),
		run("new-a", 1, work("a", true)),
		run("touch-both", 2, work("z", false), work("a", true)),
		run("publish-c", 3, work("c", true)),
		run("reuse-z", 4, work("z", false)),
	}
	for _, policy := range []retention.Policy{retention.LRU, retention.Impact} {
		report, err := measurement.Backtest(history, measurement.BacktestPolicy{MaxBytes: 8, EvictionPolicy: policy})
		if err != nil {
			t.Fatal(err)
		}
		if report.Runs[4].Outcomes[0].Result != measurement.ResultHit {
			t.Fatalf("%s changed LRU's equal-recency key ordering", policy)
		}
		if policy == retention.Impact && report.LRUFallbackEvictions != 1 {
			t.Fatalf("fallback decisions = %d", report.LRUFallbackEvictions)
		}
	}
}
