package measurement_test

import (
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/measurement"
)

func TestSQLiteRepositoryPersistsReportsAcrossReopen(t *testing.T) {
	t.Parallel()

	databasePath := filepath.Join(t.TempDir(), "measurement.sqlite")
	startedAt := time.Date(2026, time.August, 30, 8, 15, 0, 123456789, time.UTC)
	outcomes := []measurement.FinalOutcome{
		{
			RunID: "run-persisted", WorkID: "compile", Result: measurement.ResultMiss,
			Source: measurement.SourceNone, StartedAt: startedAt,
			FinishedAt:        startedAt.Add(2*time.Second + 987654321*time.Nanosecond),
			ExecutionDuration: duration(1900123456 * time.Nanosecond),
			Timing: measurement.Timing{
				Lookup: 1234567 * time.Nanosecond,
				Upload: 9876543 * time.Nanosecond,
			},
			Bytes: measurement.Bytes{Uploaded: 8192},
		},
		{
			RunID: "run-persisted", WorkID: "link", Dependencies: []string{"compile"},
			ArtifactID: "sha256:artifact", CompatibilityID: "sha256:compatibility",
			Result: measurement.ResultHit, Source: measurement.SourceTeamCache,
			StartedAt:        startedAt.Add(3*time.Second + 7654321*time.Nanosecond),
			FinishedAt:       startedAt.Add(4*time.Second + 135792468*time.Nanosecond),
			ProducerDuration: duration(3200123456 * time.Nanosecond),
			Timing: measurement.Timing{
				Lookup:       1111111 * time.Nanosecond,
				Download:     2222222 * time.Nanosecond,
				Verification: 3333333 * time.Nanosecond,
				Restore:      4444444 * time.Nanosecond,
			},
			Bytes: measurement.Bytes{Downloaded: 16384}, Degraded: true,
		},
	}

	expectedRecorder := measurement.NewRecorder()
	repository, err := measurement.OpenSQLiteRepository(databasePath)
	if err != nil {
		t.Fatalf("open repository: %v", err)
	}
	for _, outcome := range outcomes {
		if err := expectedRecorder.Record(outcome); err != nil {
			t.Fatalf("record expected outcome: %v", err)
		}
		if err := repository.Record(outcome); err != nil {
			t.Fatalf("persist outcome: %v", err)
		}
	}
	if err := repository.Close(); err != nil {
		t.Fatalf("close repository: %v", err)
	}

	repository, err = measurement.OpenSQLiteRepository(databasePath)
	if err != nil {
		t.Fatalf("reopen repository: %v", err)
	}
	t.Cleanup(func() { _ = repository.Close() })

	expectedRun, err := expectedRecorder.RunReport("run-persisted")
	if err != nil {
		t.Fatalf("build expected run report: %v", err)
	}
	actualRun, err := repository.RunReport("run-persisted")
	if err != nil {
		t.Fatalf("load run report: %v", err)
	}
	if !reflect.DeepEqual(actualRun, expectedRun) {
		t.Fatalf("persisted run report differs\nactual:   %#v\nexpected: %#v", actualRun, expectedRun)
	}
	if !actualRun.Outcomes[0].StartedAt.Equal(startedAt) || actualRun.Outcomes[0].StartedAt.Nanosecond() != startedAt.Nanosecond() {
		t.Fatalf("timestamp lost precision: got %s, want %s", actualRun.Outcomes[0].StartedAt, startedAt)
	}
	if actualRun.Outcomes[1].Source != measurement.SourceTeamCache {
		t.Fatalf("source = %q, want %q", actualRun.Outcomes[1].Source, measurement.SourceTeamCache)
	}

	from, to := startedAt.Add(-time.Hour), startedAt.Add(time.Hour)
	expectedPeriod, err := expectedRecorder.PeriodReport(from, to)
	if err != nil {
		t.Fatalf("build expected period report: %v", err)
	}
	actualPeriod, err := repository.PeriodReport(from, to)
	if err != nil {
		t.Fatalf("load period report: %v", err)
	}
	if !reflect.DeepEqual(actualPeriod, expectedPeriod) {
		t.Fatalf("persisted period report differs\nactual:   %#v\nexpected: %#v", actualPeriod, expectedPeriod)
	}
}

func TestSQLiteRepositoryAtomicallyRejectsDuplicateFinalOutcomes(t *testing.T) {
	t.Parallel()

	repository, err := measurement.OpenSQLiteRepository(filepath.Join(t.TempDir(), "measurement.sqlite"))
	if err != nil {
		t.Fatalf("open repository: %v", err)
	}
	t.Cleanup(func() { _ = repository.Close() })

	startedAt := time.Date(2026, time.August, 30, 9, 0, 0, 0, time.UTC)
	outcome := measurement.FinalOutcome{
		RunID: "run-once", WorkID: "build", Result: measurement.ResultMiss,
		Source: measurement.SourceNone, StartedAt: startedAt, FinishedAt: startedAt.Add(time.Second),
		ExecutionDuration: duration(time.Second),
	}

	const writers = 24
	start := make(chan struct{})
	var successes atomic.Int32
	var unexpected atomic.Int32
	var wait sync.WaitGroup
	wait.Add(writers)
	for range writers {
		go func() {
			defer wait.Done()
			<-start
			err := repository.Record(outcome)
			switch {
			case err == nil:
				successes.Add(1)
			case !errors.Is(err, measurement.ErrFinalOutcomeAlreadyRecorded):
				unexpected.Add(1)
			}
		}()
	}
	close(start)
	wait.Wait()

	if successes.Load() != 1 || unexpected.Load() != 0 {
		t.Fatalf("concurrent records: %d successes, %d unexpected errors", successes.Load(), unexpected.Load())
	}
	report, err := repository.RunReport("run-once")
	if err != nil {
		t.Fatalf("load run report: %v", err)
	}
	if len(report.Outcomes) != 1 {
		t.Fatalf("stored outcomes = %d, want 1", len(report.Outcomes))
	}
}

func TestSQLiteRepositoryEnrichesOneExistingActionsMiss(t *testing.T) {
	t.Parallel()

	databasePath := filepath.Join(t.TempDir(), "measurement.sqlite")
	repository, err := measurement.OpenSQLiteRepository(databasePath)
	if err != nil {
		t.Fatalf("open repository: %v", err)
	}
	startedAt := time.Date(2026, time.August, 31, 10, 0, 0, 0, time.UTC)
	lookupFinishedAt := startedAt.Add(20 * time.Millisecond)
	if err := repository.Record(measurement.FinalOutcome{
		RunID: "run-actions", Integration: measurement.IntegrationActions, WorkID: "restore-1",
		Result: measurement.ResultMiss, Source: measurement.SourceNone,
		StartedAt: startedAt, FinishedAt: lookupFinishedAt,
		Timing: measurement.Timing{Lookup: 20 * time.Millisecond},
	}); err != nil {
		t.Fatalf("record Actions miss: %v", err)
	}
	completion := measurement.ActionsMissCompletion{
		RunID: "run-actions", WorkID: "restore-1", FinishedAt: startedAt.Add(3 * time.Second),
		ExecutionDuration: 2 * time.Second,
		UploadDuration:    980 * time.Millisecond,
		UploadedBytes:     8192,
	}
	if err := repository.EnrichActionsMiss(completion); err != nil {
		t.Fatalf("enrich Actions miss: %v", err)
	}
	if err := repository.Close(); err != nil {
		t.Fatalf("close repository: %v", err)
	}

	repository, err = measurement.OpenSQLiteRepository(databasePath)
	if err != nil {
		t.Fatalf("reopen repository: %v", err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	report, err := repository.RunReport(completion.RunID)
	if err != nil {
		t.Fatalf("read enriched run: %v", err)
	}
	if len(report.Outcomes) != 1 {
		t.Fatalf("outcomes = %d, want one enriched outcome", len(report.Outcomes))
	}
	outcome := report.Outcomes[0]
	if !outcome.FinishedAt.Equal(completion.FinishedAt) || outcome.ExecutionDurationMS == nil ||
		*outcome.ExecutionDurationMS != 2000 || outcome.Timing.LookupMS != 20 || outcome.Timing.UploadMS != 980 ||
		outcome.Bytes.Uploaded != 8192 {
		t.Fatalf("persisted enriched outcome = %#v", outcome)
	}
}

func TestSQLiteRepositoryRejectsUnknownOrOutOfOrderActionsMissCompletion(t *testing.T) {
	t.Parallel()

	repository, err := measurement.OpenSQLiteRepository(filepath.Join(t.TempDir(), "measurement.sqlite"))
	if err != nil {
		t.Fatalf("open repository: %v", err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	startedAt := time.Date(2026, time.August, 31, 11, 0, 0, 0, time.UTC)
	if err := repository.Record(measurement.FinalOutcome{
		RunID: "run-actions", Integration: measurement.IntegrationActions, WorkID: "restore-1",
		Result: measurement.ResultMiss, Source: measurement.SourceNone,
		StartedAt: startedAt, FinishedAt: startedAt.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}

	missing := measurement.ActionsMissCompletion{
		RunID: "run-actions", WorkID: "missing", FinishedAt: startedAt.Add(2 * time.Second),
	}
	if err := repository.EnrichActionsMiss(missing); !errors.Is(err, measurement.ErrActionsMissNotFound) {
		t.Fatalf("unknown Actions miss error = %v, want ErrActionsMissNotFound", err)
	}
	outOfOrder := missing
	outOfOrder.WorkID = "restore-1"
	outOfOrder.FinishedAt = startedAt
	if err := repository.EnrichActionsMiss(outOfOrder); err == nil {
		t.Fatal("out-of-order Actions miss completion was accepted")
	}

	report, err := repository.RunReport("run-actions")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Outcomes) != 1 || report.Outcomes[0].ExecutionDurationMS != nil ||
		!report.Outcomes[0].FinishedAt.Equal(startedAt.Add(time.Second)) {
		t.Fatalf("rejected completion changed outcomes: %#v", report.Outcomes)
	}
}

func TestSQLiteRepositoryReturnsBuildkitColdBaseline(t *testing.T) {
	t.Parallel()
	repository, err := measurement.OpenSQLiteRepository(filepath.Join(t.TempDir(), "measurements.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	start := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	duration := 3 * time.Second
	if err := repository.Record(measurement.FinalOutcome{
		RunID: "cold", Integration: measurement.IntegrationBuildkit, WorkID: "build",
		ArtifactID: "graph", CompatibilityID: "linux-amd64",
		Result: measurement.ResultMiss, Source: measurement.SourceNone,
		StartedAt: start, FinishedAt: start.Add(duration), ExecutionDuration: &duration,
	}); err != nil {
		t.Fatal(err)
	}
	baseline, err := repository.LatestBuildkitBaseline("graph", "linux-amd64")
	if err != nil || baseline == nil || *baseline != duration {
		t.Fatalf("BuildKit baseline = %v, error = %v", baseline, err)
	}
	missing, err := repository.LatestBuildkitBaseline("other", "linux-amd64")
	if err != nil || missing != nil {
		t.Fatalf("missing BuildKit baseline = %v, error = %v", missing, err)
	}
}

func TestSQLiteRepositoryUsesRecorderValidationAndNotFoundBehavior(t *testing.T) {
	t.Parallel()

	repository, err := measurement.OpenSQLiteRepository(filepath.Join(t.TempDir(), "measurement.sqlite"))
	if err != nil {
		t.Fatalf("open repository: %v", err)
	}
	t.Cleanup(func() { _ = repository.Close() })

	if err := repository.Record(measurement.FinalOutcome{}); err == nil {
		t.Fatal("invalid outcome was persisted")
	}
	if _, err := repository.RunReport("missing"); !errors.Is(err, measurement.ErrRunNotFound) {
		t.Fatalf("missing run error = %v, want ErrRunNotFound", err)
	}
	from := time.Date(2026, time.August, 30, 0, 0, 0, 0, time.UTC)
	period, err := repository.PeriodReport(from, from.Add(time.Hour))
	if err != nil {
		t.Fatalf("empty period report: %v", err)
	}
	if period.Runs != 0 || period.Eligible != 0 {
		t.Fatalf("empty period = %#v", period)
	}
}
