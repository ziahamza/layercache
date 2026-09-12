package measurement_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/measurement"
)

func TestTurboRunSummaryReplacesProbesWithFinalTaskGraph(t *testing.T) {
	t.Parallel()

	repository, err := measurement.OpenSQLiteRepository(filepath.Join(t.TempDir(), "measurements.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })

	runID := "run-summary"
	project := "github.com/acme/widget"
	compatibility := "linux-amd64-glibc2.39-node@24-schema1"
	base := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	hitArtifact := measurement.TurboArtifactIdentity(project, compatibility, "hash-build")
	missArtifact := measurement.TurboArtifactIdentity(project, compatibility, "hash-test")
	if err := repository.ObserveTurbo(measurement.TurboObservation{
		RunID: runID, ArtifactID: hitArtifact, Result: measurement.ResultHit,
		Source: measurement.SourceTeamCache, StartedAt: base.Add(100 * time.Millisecond),
		FinishedAt: base.Add(125 * time.Millisecond),
		Timing:     measurement.Timing{Download: 20 * time.Millisecond},
		Bytes:      measurement.Bytes{Downloaded: 1234}, Degraded: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ObserveTurbo(measurement.TurboObservation{
		RunID: runID, ArtifactID: missArtifact, Result: measurement.ResultMiss,
		Source: measurement.SourceNone, StartedAt: base.Add(145 * time.Millisecond),
		FinishedAt: base.Add(150 * time.Millisecond),
		Timing:     measurement.Timing{Lookup: 5 * time.Millisecond}, Degraded: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ObserveTurbo(measurement.TurboObservation{
		RunID: runID, ArtifactID: missArtifact, Result: measurement.ResultMiss,
		Source: measurement.SourceNone, StartedAt: base.Add(150 * time.Millisecond),
		FinishedAt: base.Add(375 * time.Millisecond),
		Timing:     measurement.Timing{Upload: 25 * time.Millisecond},
		Bytes:      measurement.Bytes{Uploaded: 4321},
	}); err != nil {
		t.Fatal(err)
	}
	// This is how old runtimes represented a raw probe. Reconciliation must
	// remove it rather than count it as another cache-eligible task.
	if err := repository.Record(measurement.FinalOutcome{
		RunID: runID, WorkID: "legacy-probe", ArtifactID: "legacy-probe",
		Result: measurement.ResultMiss, Source: measurement.SourceNone,
		StartedAt: base, FinishedAt: base.Add(time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}

	document := turboSummaryDocument(t, map[string]any{
		"id":      "turbo-summary-one",
		"version": "1",
		"execution": map[string]any{
			"startTime": base.UnixMilli(), "endTime": base.Add(time.Second).UnixMilli(),
		},
		"tasks": []any{
			turboTask("pkg#build", "hash-build", nil, true, "HIT", false, true, "REMOTE", 1000,
				base.Add(100*time.Millisecond), base.Add(140*time.Millisecond), 0),
			turboTask("pkg#test", "hash-test", []string{"pkg#build"}, true, "MISS", false, false, "", 0,
				base.Add(150*time.Millisecond), base.Add(350*time.Millisecond), 0),
			turboTask("pkg#uncached", "hash-uncached", nil, false, "MISS", false, false, "", 0,
				base.Add(400*time.Millisecond), base.Add(500*time.Millisecond), 0),
			turboTask("pkg#failed", "hash-failed", nil, true, "MISS", false, false, "", 0,
				base.Add(600*time.Millisecond), base.Add(700*time.Millisecond), 1),
		},
	})

	count, err := repository.ReconcileTurboSummaries(runID, measurement.TurboReconcileOptions{
		Project: project, CompatibilityID: compatibility,
		RunStartedAt: base.Add(-time.Second), RunFinishedAt: base.Add(2 * time.Second),
	}, [][]byte{document})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("reconciled tasks = %d, want 2", count)
	}

	report, err := repository.RunReport(runID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Eligible != 2 || report.Hits != 1 || report.Misses != 1 || report.HitRate != 0.5 {
		t.Fatalf("final summary counts = %#v", report)
	}
	if len(report.Sources) != 1 || report.Sources[0].Source != measurement.SourceTeamCache || report.Sources[0].Hits != 1 {
		t.Fatalf("sources = %#v", report.Sources)
	}
	if report.Bytes != (measurement.Bytes{Downloaded: 1234, Uploaded: 4321}) {
		t.Fatalf("bytes = %#v", report.Bytes)
	}
	if report.GrossAvoidedTaskTime.Milliseconds == nil || *report.GrossAvoidedTaskTime.Milliseconds != 1000 {
		t.Fatalf("gross estimate = %#v", report.GrossAvoidedTaskTime)
	}
	// Baseline critical path is 1000ms + 200ms. Observed is a 40ms hit
	// restore + (5ms lookup + 200ms execution + 25ms upload), yielding 930ms net.
	if report.NetEstimatedBuildTimeSaved.Milliseconds == nil || *report.NetEstimatedBuildTimeSaved.Milliseconds != 930 {
		t.Fatalf("net estimate = %#v", report.NetEstimatedBuildTimeSaved)
	}

	var hit, miss measurement.OutcomeReport
	for _, outcome := range report.Outcomes {
		if outcome.WorkID == "legacy-probe" || strings.Contains(outcome.WorkID, "pkg#") {
			t.Fatalf("stored raw task identity %q", outcome.WorkID)
		}
		switch outcome.Result {
		case measurement.ResultHit:
			hit = outcome
		case measurement.ResultMiss:
			miss = outcome
		}
	}
	if hit.ArtifactID != hitArtifact || hit.Source != measurement.SourceTeamCache || hit.Timing.DownloadMS != 20 || hit.Timing.RestoreMS != 20 || !hit.Degraded {
		t.Fatalf("hit outcome = %#v", hit)
	}
	if miss.ArtifactID != missArtifact || miss.ExecutionDurationMS == nil || *miss.ExecutionDurationMS != 200 ||
		miss.Timing.LookupMS != 5 || miss.Timing.UploadMS != 25 || !miss.Degraded {
		t.Fatalf("miss outcome = %#v", miss)
	}
	if len(miss.Dependencies) != 1 || miss.Dependencies[0] != hit.WorkID {
		t.Fatalf("reconciled dependency = %#v, hit work = %q", miss.Dependencies, hit.WorkID)
	}

	count, err = repository.ReconcileTurboSummaries(runID, measurement.TurboReconcileOptions{
		Project: project, CompatibilityID: compatibility,
		RunStartedAt: base.Add(-time.Second), RunFinishedAt: base.Add(2 * time.Second),
	}, [][]byte{document})
	if err != nil || count != 2 {
		t.Fatalf("idempotent reconciliation = %d, %v", count, err)
	}
	repeated, err := repository.RunReport(runID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(repeated, report) {
		t.Fatalf("repeated reconciliation changed report\nfirst:  %#v\nsecond: %#v", report, repeated)
	}
}

func TestTurboRunSummaryNeverInventsRemoteSource(t *testing.T) {
	t.Parallel()

	repository, err := measurement.OpenSQLiteRepository(filepath.Join(t.TempDir(), "measurements.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	base := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	document := turboSummaryDocument(t, map[string]any{
		"id": "summary-unattributed", "version": "1",
		"execution": map[string]any{"startTime": base.UnixMilli(), "endTime": base.Add(time.Second).UnixMilli()},
		"tasks": []any{
			turboTask("pkg#build", "remote-without-evidence", nil, true, "HIT", false, true, "REMOTE", 500,
				base.Add(10*time.Millisecond), base.Add(30*time.Millisecond), 0),
		},
	})
	if _, err := repository.ReconcileTurboSummaries("run-unattributed", measurement.TurboReconcileOptions{
		Project: "github.com/acme/widget", CompatibilityID: "linux-amd64-schema1",
	}, [][]byte{document}); err != nil {
		t.Fatal(err)
	}
	report, err := repository.RunReport("run-unattributed")
	if err != nil {
		t.Fatal(err)
	}
	if got := report.Outcomes[0].Source; got != measurement.SourceUnattributed {
		t.Fatalf("source = %q, want unattributed", got)
	}
	if report.Outcomes[0].Timing.RestoreMS != 20 {
		t.Fatalf("restore timing = %#v", report.Outcomes[0].Timing)
	}
}

func TestTurboReconciliationPreservesOtherIntegrationOutcomesInTheRun(t *testing.T) {
	t.Parallel()

	repository, err := measurement.OpenSQLiteRepository(filepath.Join(t.TempDir(), "measurements.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	base := time.Date(2026, time.August, 31, 14, 0, 0, 0, time.UTC)
	if err := repository.Record(measurement.FinalOutcome{
		RunID: "run-mixed", WorkspaceID: "sha256:workspace", Integration: measurement.IntegrationActions,
		WorkID: "actions-save", Result: measurement.ResultMiss, Source: measurement.SourceNone,
		StartedAt: base, FinishedAt: base.Add(10 * time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	document := turboSummaryDocument(t, map[string]any{
		"id": "summary-mixed", "version": "1",
		"execution": map[string]any{"startTime": base.UnixMilli(), "endTime": base.Add(time.Second).UnixMilli()},
		"tasks": []any{
			turboTask("pkg#build", "hash-mixed", nil, true, "MISS", false, false, "", 0,
				base.Add(20*time.Millisecond), base.Add(120*time.Millisecond), 0),
		},
	})
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := repository.ReconcileTurboSummaries("run-mixed", measurement.TurboReconcileOptions{
			Project: "github.com/acme/widget", WorkspaceID: "sha256:workspace",
			CompatibilityID: "linux-amd64-schema1",
		}, [][]byte{document}); err != nil {
			t.Fatal(err)
		}
	}
	report, err := repository.RunReport("run-mixed")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Outcomes) != 2 || report.Groups != 2 {
		t.Fatalf("mixed report = %#v", report)
	}
	integrations := map[measurement.Integration]bool{}
	for _, outcome := range report.Outcomes {
		integrations[outcome.Integration] = true
		if outcome.WorkspaceID != "sha256:workspace" {
			t.Fatalf("workspace correlation was lost: %#v", outcome)
		}
	}
	if !integrations[measurement.IntegrationTurbo] || !integrations[measurement.IntegrationActions] {
		t.Fatalf("integrations = %#v", integrations)
	}
}

func TestTurboRunSummaryValidationIsAtomic(t *testing.T) {
	t.Parallel()

	repository, err := measurement.OpenSQLiteRepository(filepath.Join(t.TempDir(), "measurements.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	base := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	existing := measurement.FinalOutcome{
		RunID: "run-atomic", WorkID: "existing", Result: measurement.ResultMiss,
		Source: measurement.SourceNone, StartedAt: base, FinishedAt: base.Add(time.Second),
	}
	if err := repository.Record(existing); err != nil {
		t.Fatal(err)
	}
	bad := []byte(fmt.Sprintf(`{
		"id":"outside","version":"1",
		"execution":{"startTime":%d,"endTime":%d},"tasks":[]
	}`, base.Add(-time.Hour).UnixMilli(), base.Add(-time.Hour+time.Second).UnixMilli()))
	_, err = repository.ReconcileTurboSummaries("run-atomic", measurement.TurboReconcileOptions{
		Project: "github.com/acme/widget", CompatibilityID: "linux-amd64-schema1",
		RunStartedAt: base, RunFinishedAt: base.Add(time.Minute),
	}, [][]byte{bad})
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("reconcile error = %v", err)
	}
	report, err := repository.RunReport("run-atomic")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Outcomes) != 1 || report.Outcomes[0].WorkID != "existing" {
		t.Fatalf("existing outcome changed after rejected summary: %#v", report.Outcomes)
	}
	if !errors.Is(repository.Record(existing), measurement.ErrFinalOutcomeAlreadyRecorded) {
		t.Fatal("existing final outcome was not preserved")
	}
}

func TestConcurrentTurboReconciliationsRetrySQLiteLockUpgrade(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "measurements.db")
	base := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	const workers = 12
	repositories := make([]*measurement.SQLiteRepository, workers)
	for index := range repositories {
		repository, err := measurement.OpenSQLiteRepository(databasePath)
		if err != nil {
			t.Fatal(err)
		}
		repositories[index] = repository
		t.Cleanup(func() { _ = repository.Close() })
	}

	start := make(chan struct{})
	errorsByWorker := make([]error, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for index, repository := range repositories {
		go func() {
			defer wait.Done()
			<-start
			runID := fmt.Sprintf("run-concurrent-%d", index)
			document := turboSummaryDocument(t, map[string]any{
				"id": fmt.Sprintf("summary-concurrent-%d", index), "version": "1",
				"execution": map[string]any{"startTime": base.UnixMilli(), "endTime": base.Add(time.Second).UnixMilli()},
				"tasks": []any{
					turboTask(fmt.Sprintf("pkg#task-%d", index), fmt.Sprintf("hash-%d", index), nil, true,
						"HIT", true, false, "LOCAL", 100, base.Add(time.Millisecond), base.Add(2*time.Millisecond), 0),
				},
			})
			_, errorsByWorker[index] = repository.ReconcileTurboSummaries(runID, measurement.TurboReconcileOptions{
				Project: "github.com/acme/widget", CompatibilityID: "linux-amd64-schema1",
			}, [][]byte{document})
		}()
	}
	close(start)
	wait.Wait()
	for index, err := range errorsByWorker {
		if err != nil {
			t.Fatalf("worker %d: %v", index, err)
		}
		report, err := repositories[index].RunReport(fmt.Sprintf("run-concurrent-%d", index))
		if err != nil || report.Eligible != 0 || report.Hits != 0 || report.Misses != 0 ||
			len(report.Outcomes) != 1 || report.Outcomes[0].Result != measurement.ResultUnknown ||
			report.Outcomes[0].Source != measurement.SourceUnattributed {
			t.Fatalf("worker %d report = %#v, %v", index, report, err)
		}
	}
}

func turboSummaryDocument(t *testing.T, value map[string]any) []byte {
	t.Helper()
	document, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func turboTask(
	taskID string,
	hash string,
	dependencies []string,
	cacheable bool,
	status string,
	local bool,
	remote bool,
	source string,
	timeSaved int64,
	startedAt time.Time,
	finishedAt time.Time,
	exitCode int,
) map[string]any {
	return map[string]any{
		"taskId": taskID, "hash": hash, "dependencies": dependencies,
		"cache": map[string]any{
			"local": local, "remote": remote, "status": status, "source": source, "timeSaved": timeSaved,
		},
		"execution": map[string]any{
			"startTime": startedAt.UnixMilli(), "endTime": finishedAt.UnixMilli(), "exitCode": exitCode,
		},
		"resolvedTaskDefinition": map[string]any{"cache": cacheable},
	}
}
