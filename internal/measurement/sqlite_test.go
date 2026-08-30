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
