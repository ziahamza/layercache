package measurement_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/measurement"
)

// TestConcurrentPostgresRepositoryInitializationAgainstPostgres proves that
// replicas can all start against the same empty schema. PostgreSQL's
// CREATE TABLE IF NOT EXISTS does not itself serialize concurrent catalog
// creation, so this specifically exercises the migration advisory lock.
func TestConcurrentPostgresRepositoryInitializationAgainstPostgres(t *testing.T) {
	postgresURL := os.Getenv("LAYERCACHE_MEASUREMENT_POSTGRES_URL")
	if postgresURL == "" {
		t.Skip("set LAYERCACHE_MEASUREMENT_POSTGRES_URL to run the PostgreSQL measurement integration test")
	}

	parsed, err := url.Parse(postgresURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("layercache_measurement_migration_qa_%d", time.Now().UnixNano())
	cleanupDatabase, err := sql.Open("pgx", postgresURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cleanupDatabase.ExecContext(context.Background(), fmt.Sprintf(`CREATE SCHEMA "%s"`, schema)); err != nil {
		_ = cleanupDatabase.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = cleanupDatabase.ExecContext(ctx, fmt.Sprintf(`DROP SCHEMA "%s" CASCADE`, schema))
		_ = cleanupDatabase.Close()
	})
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	isolatedURL := parsed.String()

	const replicas = 12
	start := make(chan struct{})
	results := make(chan error, replicas)
	var wait sync.WaitGroup
	wait.Add(replicas)
	for replica := range replicas {
		go func() {
			defer wait.Done()
			<-start
			repository, err := measurement.OpenPostgresRepository(
				context.Background(), isolatedURL, fmt.Sprintf("migration-qa-%d", replica),
			)
			if err == nil {
				err = repository.Close()
			}
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent measurement repository initialization: %v", err)
		}
	}
}

// TestPostgresRepositoryAgainstPostgres exercises the real pgx/database/sql
// adapter. It is opt-in because it creates project-scoped rows in the supplied
// database. The test deletes those rows before returning.
func TestPostgresRepositoryAgainstPostgres(t *testing.T) {
	postgresURL := os.Getenv("LAYERCACHE_MEASUREMENT_POSTGRES_URL")
	if postgresURL == "" {
		t.Skip("set LAYERCACHE_MEASUREMENT_POSTGRES_URL to run the PostgreSQL measurement integration test")
	}

	project := fmt.Sprintf("measurement-qa-%d", time.Now().UnixNano())
	isolationProject := project + "-isolated"
	cleanupDatabase, err := sql.Open("pgx", postgresURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = cleanupDatabase.ExecContext(ctx,
			`DELETE FROM layercache_measurement_turbo_observations_v1 WHERE project_id IN ($1, $2)`,
			project, isolationProject,
		)
		_, _ = cleanupDatabase.ExecContext(ctx,
			`DELETE FROM layercache_measurement_final_outcomes_v1 WHERE project_id IN ($1, $2)`,
			project, isolationProject,
		)
		_ = cleanupDatabase.Close()
	})

	repository, err := measurement.OpenPostgresRepository(context.Background(), postgresURL, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })

	base := time.Date(2026, time.August, 31, 14, 0, 0, 123456789, time.UTC)
	execution := 1800*time.Millisecond + 123*time.Nanosecond
	lookupDuration := 2*time.Millisecond + 12*time.Nanosecond
	actionsExecution := 1700*time.Millisecond + 100*time.Nanosecond
	actionsUpload := execution - lookupDuration - actionsExecution
	producer := 2400*time.Millisecond + 456*time.Nanosecond
	outcomes := []measurement.FinalOutcome{
		{
			RunID: "run-persisted", WorkspaceID: "sha256:workspace", Integration: measurement.IntegrationActions,
			WorkID: "compile", Result: measurement.ResultMiss,
			Source: measurement.SourceNone, StartedAt: base,
			FinishedAt: base.Add(lookupDuration),
			Timing:     measurement.Timing{Lookup: lookupDuration},
		},
		{
			RunID: "run-persisted", WorkspaceID: "sha256:workspace", Integration: measurement.IntegrationBuildkit,
			WorkID: "link", Dependencies: []string{"compile"},
			ArtifactID: "sha256:artifact", CompatibilityID: "linux-amd64-schema1",
			Result: measurement.ResultHit, Source: measurement.SourceTeamCache,
			StartedAt:        base.Add(2*time.Second + 17*time.Nanosecond),
			FinishedAt:       base.Add(2100*time.Millisecond + 29*time.Nanosecond),
			ProducerDuration: &producer,
			Timing: measurement.Timing{
				Lookup: 3 * time.Millisecond, Download: 20 * time.Millisecond,
				Verification: time.Millisecond, Restore: 5 * time.Millisecond,
			},
			Bytes: measurement.Bytes{Downloaded: 4096}, Degraded: true, EligibleUnits: 4, HitUnits: 2,
		},
		{
			RunID: "run-persisted", WorkspaceID: "sha256:workspace", Integration: measurement.IntegrationBuildkit,
			WorkID: "unknown-progress", Result: measurement.ResultUnknown, Source: measurement.SourceUnattributed,
			StartedAt: base.Add(3 * time.Second), FinishedAt: base.Add(3100 * time.Millisecond), Degraded: true,
		},
	}
	expected := measurement.NewRecorder()
	for _, outcome := range outcomes {
		if err := expected.Record(outcome); err != nil {
			t.Fatal(err)
		}
		if err := repository.Record(outcome); err != nil {
			t.Fatal(err)
		}
	}
	completion := measurement.ActionsMissCompletion{
		RunID: "run-persisted", WorkID: "compile", FinishedAt: base.Add(execution),
		ExecutionDuration: actionsExecution, UploadDuration: actionsUpload, UploadedBytes: 2048,
	}
	if err := expected.EnrichActionsMiss(completion); err != nil {
		t.Fatal(err)
	}
	if err := repository.EnrichActionsMiss(completion); err != nil {
		t.Fatal(err)
	}
	missingCompletion := completion
	missingCompletion.WorkID = "missing"
	if err := repository.EnrichActionsMiss(missingCompletion); !errors.Is(err, measurement.ErrActionsMissNotFound) {
		t.Fatalf("missing PostgreSQL Actions miss error = %v, want ErrActionsMissNotFound", err)
	}
	expectedReport, err := expected.RunReport("run-persisted")
	if err != nil {
		t.Fatal(err)
	}
	actualReport, err := repository.RunReport("run-persisted")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actualReport, expectedReport) {
		t.Fatalf("PostgreSQL report differs\nactual:   %#v\nexpected: %#v", actualReport, expectedReport)
	}
	if !actualReport.StartedAt.Equal(base) || actualReport.StartedAt.Nanosecond() != base.Nanosecond() {
		t.Fatalf("PostgreSQL timestamp lost precision: got %s, want %s", actualReport.StartedAt, base)
	}

	inside, err := repository.PeriodReport(base, base.Add(10*time.Second))
	if err != nil || inside.Runs != 1 {
		t.Fatalf("inclusive period report = %#v, %v", inside, err)
	}
	outside, err := repository.PeriodReport(base.Add(time.Nanosecond), base.Add(10*time.Second))
	if err != nil || outside.Runs != 0 {
		t.Fatalf("nanosecond-exclusive period report = %#v, %v", outside, err)
	}

	duplicate := measurement.FinalOutcome{
		RunID: "run-concurrent", WorkID: "build", Result: measurement.ResultMiss,
		Source: measurement.SourceNone, StartedAt: base, FinishedAt: base.Add(time.Second),
	}
	const writers = 16
	start := make(chan struct{})
	var successes atomic.Int32
	var unexpected atomic.Int32
	var wait sync.WaitGroup
	wait.Add(writers)
	for range writers {
		go func() {
			defer wait.Done()
			<-start
			err := repository.Record(duplicate)
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

	isolation, err := measurement.OpenPostgresRepository(context.Background(), postgresURL, isolationProject)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := isolation.RunReport("run-persisted"); !errors.Is(err, measurement.ErrRunNotFound) {
		t.Fatalf("other project read = %v, want ErrRunNotFound", err)
	}
	if err := isolation.EnrichActionsMiss(completion); !errors.Is(err, measurement.ErrActionsMissNotFound) {
		t.Fatalf("other project completion = %v, want ErrActionsMissNotFound", err)
	}
	if err := isolation.Close(); err != nil {
		t.Fatal(err)
	}

	turboRun := "run-turbo"
	turboProject := "github.com/acme/widget"
	compatibility := "linux-amd64-schema1"
	artifactID := measurement.TurboArtifactIdentity(turboProject, compatibility, "hash-build")
	if err := repository.ObserveTurbo(measurement.TurboObservation{
		RunID: turboRun, ArtifactID: artifactID,
		Result: measurement.ResultHit, Source: measurement.SourceTeamCache,
		StartedAt: base.Add(20 * time.Second), FinishedAt: base.Add(20250 * time.Millisecond),
		Timing: measurement.Timing{Download: 25 * time.Millisecond},
		Bytes:  measurement.Bytes{Downloaded: 8192},
	}); err != nil {
		t.Fatal(err)
	}
	document := turboSummaryDocument(t, map[string]any{
		"id": "postgres-summary", "version": "1",
		"execution": map[string]any{
			"startTime": base.Add(20 * time.Second).UnixMilli(),
			"endTime":   base.Add(21 * time.Second).UnixMilli(),
		},
		"tasks": []any{
			turboTask("pkg#build", "hash-build", nil, true, "HIT", false, true, "REMOTE", 900,
				base.Add(20*time.Second), base.Add(20040*time.Millisecond), 0),
		},
	})
	count, err := repository.ReconcileTurboSummaries(turboRun, measurement.TurboReconcileOptions{
		Project: turboProject, CompatibilityID: compatibility,
	}, [][]byte{document})
	if err != nil || count != 1 {
		t.Fatalf("reconcile PostgreSQL Turbo summary = %d, %v", count, err)
	}
	turboReport, err := repository.RunReport(turboRun)
	if err != nil {
		t.Fatal(err)
	}
	if turboReport.Hits != 1 || turboReport.Outcomes[0].Source != measurement.SourceTeamCache ||
		turboReport.Bytes.Downloaded != 8192 || turboReport.Outcomes[0].Timing.DownloadMS != 25 {
		t.Fatalf("reconciled PostgreSQL Turbo report = %#v", turboReport)
	}
	count, err = repository.ReconcileTurboSummaries(turboRun, measurement.TurboReconcileOptions{
		Project: turboProject, CompatibilityID: compatibility,
	}, [][]byte{document})
	if err != nil || count != 1 {
		t.Fatalf("repeat PostgreSQL reconciliation = %d, %v", count, err)
	}
	repeated, err := repository.RunReport(turboRun)
	if err != nil || !reflect.DeepEqual(repeated, turboReport) {
		t.Fatalf("repeat PostgreSQL report changed: %#v, %v", repeated, err)
	}

	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository, err = measurement.OpenPostgresRepository(context.Background(), postgresURL, project)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := repository.RunReport("run-persisted")
	if err != nil || !reflect.DeepEqual(reopened, expectedReport) {
		t.Fatalf("reopened PostgreSQL report = %#v, %v", reopened, err)
	}
}
