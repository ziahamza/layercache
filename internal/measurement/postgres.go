package measurement

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	postgresOpenTimeout      = 10 * time.Second
	postgresOperationTimeout = 10 * time.Second
	// Serialize first-start DDL across measurement replicas sharing a database.
	// The transaction-scoped lock is released automatically on commit or rollback.
	postgresSchemaMigrationAdvisoryLock int64 = 0x4c617965724d6561
	maximumProjectBytes                       = 256
	maximumRunIDBytes                         = 256
	maximumIdentityBytes                      = 512
	maximumDependencies                       = 256
	maximumDependenciesBytes                  = 64 << 10
)

// PostgresRepository persists cloud report metadata. The project coordinate is
// part of every key, so Team and Public processes can share one database
// without sharing measurements. Stored fields are limited to the bounded
// FinalOutcome and TurboObservation models. Paths, environment values, cache
// keys, headers, and credentials have no column in this schema.
type PostgresRepository struct {
	database    *sql.DB
	postgresURL string
	project     string
}

type postgresOutcomeExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// OpenPostgresRepository opens the cloud measurement repository for project.
func OpenPostgresRepository(ctx context.Context, postgresURL, project string) (*PostgresRepository, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	parsed, err := url.Parse(postgresURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		return nil, errors.New("measurement PostgreSQL URL is invalid")
	}
	if err := validateBoundedText("measurement project", project, maximumProjectBytes, false); err != nil {
		return nil, err
	}

	database, err := sql.Open("pgx", postgresURL)
	if err != nil {
		return nil, redactPostgresError("open measurement PostgreSQL", postgresURL, err)
	}
	database.SetMaxOpenConns(12)
	database.SetMaxIdleConns(3)
	database.SetConnMaxIdleTime(5 * time.Minute)
	database.SetConnMaxLifetime(time.Hour)

	repository := &PostgresRepository{database: database, postgresURL: postgresURL, project: project}
	openContext, cancel := context.WithTimeout(ctx, postgresOpenTimeout)
	defer cancel()
	if err := database.PingContext(openContext); err != nil {
		_ = database.Close()
		return nil, repository.safeError("connect measurement PostgreSQL", err)
	}
	if err := repository.initialize(openContext); err != nil {
		_ = database.Close()
		return nil, err
	}
	return repository, nil
}

func (repository *PostgresRepository) initialize(ctx context.Context) error {
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return repository.safeError("begin measurement PostgreSQL migration", err)
	}
	defer transaction.Rollback()
	if _, err := transaction.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock($1)`, postgresSchemaMigrationAdvisoryLock); err != nil {
		return repository.safeError("lock measurement PostgreSQL migration", err)
	}

	statements := []string{
		`SET LOCAL lock_timeout = '5s'`,
		`SET LOCAL statement_timeout = '10s'`,
		`CREATE TABLE IF NOT EXISTS layercache_measurement_final_outcomes_v1 (
			project_id TEXT NOT NULL CHECK (octet_length(project_id) BETWEEN 1 AND 256),
			run_id TEXT NOT NULL CHECK (octet_length(run_id) BETWEEN 1 AND 256),
			workspace_id TEXT NOT NULL DEFAULT '' CHECK (octet_length(workspace_id) <= 512),
			integration TEXT NOT NULL DEFAULT '' CHECK (integration IN ('', 'turbo', 'actions', 'buildkit')),
			work_id TEXT NOT NULL CHECK (octet_length(work_id) BETWEEN 1 AND 512),
			dependencies_json JSONB NOT NULL CONSTRAINT layercache_measurement_dependencies_shape_v1 CHECK (
				jsonb_typeof(dependencies_json) IN ('array', 'null')
				AND pg_column_size(dependencies_json) <= 131072
			),
			artifact_id TEXT NOT NULL CHECK (octet_length(artifact_id) <= 512),
			compatibility_id TEXT NOT NULL CHECK (octet_length(compatibility_id) <= 512),
			result TEXT NOT NULL CHECK (result IN ('hit', 'miss', 'unknown')),
			source TEXT NOT NULL CHECK (source IN ('none', 'localCache', 'teamCache', 'publicCache', 'unattributed')),
			started_at BYTEA NOT NULL CHECK (octet_length(started_at) BETWEEN 1 AND 64),
			finished_at BYTEA NOT NULL CHECK (octet_length(finished_at) BETWEEN 1 AND 64),
			started_at_unix_seconds BIGINT NOT NULL,
			started_at_nanosecond INTEGER NOT NULL CHECK (started_at_nanosecond BETWEEN 0 AND 999999999),
			finished_at_unix_seconds BIGINT NOT NULL,
			finished_at_nanosecond INTEGER NOT NULL CHECK (finished_at_nanosecond BETWEEN 0 AND 999999999),
			producer_duration_ns BIGINT CHECK (producer_duration_ns >= 0),
			execution_duration_ns BIGINT CHECK (execution_duration_ns >= 0),
			lookup_ns BIGINT NOT NULL CHECK (lookup_ns >= 0),
			download_ns BIGINT NOT NULL CHECK (download_ns >= 0),
			verification_ns BIGINT NOT NULL CHECK (verification_ns >= 0),
			restore_ns BIGINT NOT NULL CHECK (restore_ns >= 0),
			upload_ns BIGINT NOT NULL CHECK (upload_ns >= 0),
			downloaded_bytes BIGINT NOT NULL CHECK (downloaded_bytes >= 0),
			uploaded_bytes BIGINT NOT NULL CHECK (uploaded_bytes >= 0),
			degraded BOOLEAN NOT NULL,
			eligible_units INTEGER NOT NULL DEFAULT 0 CHECK (eligible_units >= 0),
			hit_units INTEGER NOT NULL DEFAULT 0 CHECK (hit_units >= 0 AND hit_units <= eligible_units),
			PRIMARY KEY (project_id, run_id, work_id),
			CHECK ((result = 'miss' AND source = 'none') OR (result = 'hit' AND source <> 'none') OR
				(result = 'unknown' AND source = 'unattributed')),
			CHECK ((finished_at_unix_seconds, finished_at_nanosecond) >=
				(started_at_unix_seconds, started_at_nanosecond))
		)`,
		`ALTER TABLE layercache_measurement_final_outcomes_v1
			ADD COLUMN IF NOT EXISTS workspace_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE layercache_measurement_final_outcomes_v1
			ADD COLUMN IF NOT EXISTS integration TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE layercache_measurement_final_outcomes_v1
			ADD COLUMN IF NOT EXISTS eligible_units INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE layercache_measurement_final_outcomes_v1
			ADD COLUMN IF NOT EXISTS hit_units INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE layercache_measurement_final_outcomes_v1
			DROP CONSTRAINT IF EXISTS layercache_measurement_final_outcomes_v1_result_check`,
		`ALTER TABLE layercache_measurement_final_outcomes_v1
			ADD CONSTRAINT layercache_measurement_final_outcomes_v1_result_check
			CHECK (result IN ('hit', 'miss', 'unknown'))`,
		`ALTER TABLE layercache_measurement_final_outcomes_v1
			DROP CONSTRAINT IF EXISTS layercache_measurement_final_outcomes_v1_check`,
		`ALTER TABLE layercache_measurement_final_outcomes_v1
			ADD CONSTRAINT layercache_measurement_final_outcomes_v1_check CHECK (
				(result = 'miss' AND source = 'none') OR
				(result = 'hit' AND source <> 'none') OR
				(result = 'unknown' AND source = 'unattributed')
			)`,
		`ALTER TABLE layercache_measurement_final_outcomes_v1
			DROP CONSTRAINT IF EXISTS layercache_measurement_final_outcomes_v_dependencies_json_check`,
		`DO $$
		BEGIN
			IF NOT EXISTS (
				SELECT 1 FROM pg_constraint
				WHERE conname = 'layercache_measurement_dependencies_shape_v1'
					AND conrelid = 'layercache_measurement_final_outcomes_v1'::regclass
			) THEN
				ALTER TABLE layercache_measurement_final_outcomes_v1
					ADD CONSTRAINT layercache_measurement_dependencies_shape_v1 CHECK (
						jsonb_typeof(dependencies_json) IN ('array', 'null')
						AND pg_column_size(dependencies_json) <= 131072
					);
			END IF;
		END;
		$$`,
		`CREATE INDEX IF NOT EXISTS layercache_measurement_final_period_v1
			ON layercache_measurement_final_outcomes_v1 (
				project_id, started_at_unix_seconds, started_at_nanosecond, run_id
			)`,
		`CREATE TABLE IF NOT EXISTS layercache_measurement_turbo_observations_v1 (
			sequence BIGSERIAL PRIMARY KEY,
			project_id TEXT NOT NULL CHECK (octet_length(project_id) BETWEEN 1 AND 256),
			run_id TEXT NOT NULL CHECK (octet_length(run_id) BETWEEN 1 AND 256),
			artifact_id TEXT NOT NULL CHECK (octet_length(artifact_id) BETWEEN 1 AND 512),
			result TEXT NOT NULL CHECK (result IN ('hit', 'miss')),
			source TEXT NOT NULL CHECK (source IN ('none', 'localCache', 'teamCache', 'publicCache', 'unattributed')),
			started_at BYTEA NOT NULL CHECK (octet_length(started_at) BETWEEN 1 AND 64),
			finished_at BYTEA NOT NULL CHECK (octet_length(finished_at) BETWEEN 1 AND 64),
			started_at_unix_seconds BIGINT NOT NULL,
			started_at_nanosecond INTEGER NOT NULL CHECK (started_at_nanosecond BETWEEN 0 AND 999999999),
			finished_at_unix_seconds BIGINT NOT NULL,
			finished_at_nanosecond INTEGER NOT NULL CHECK (finished_at_nanosecond BETWEEN 0 AND 999999999),
			lookup_ns BIGINT NOT NULL CHECK (lookup_ns >= 0),
			download_ns BIGINT NOT NULL CHECK (download_ns >= 0),
			verification_ns BIGINT NOT NULL CHECK (verification_ns >= 0),
			restore_ns BIGINT NOT NULL CHECK (restore_ns >= 0),
			upload_ns BIGINT NOT NULL CHECK (upload_ns >= 0),
			downloaded_bytes BIGINT NOT NULL CHECK (downloaded_bytes >= 0),
			uploaded_bytes BIGINT NOT NULL CHECK (uploaded_bytes >= 0),
			degraded BOOLEAN NOT NULL,
			CHECK ((result = 'miss' AND source = 'none') OR (result = 'hit' AND source <> 'none')),
			CHECK ((finished_at_unix_seconds, finished_at_nanosecond) >=
				(started_at_unix_seconds, started_at_nanosecond))
		)`,
		`CREATE INDEX IF NOT EXISTS layercache_measurement_turbo_run_v1
			ON layercache_measurement_turbo_observations_v1 (project_id, run_id, artifact_id, sequence)`,
	}
	for _, statement := range statements {
		if _, err := transaction.ExecContext(ctx, statement); err != nil {
			return repository.safeError("migrate measurement PostgreSQL", err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return repository.safeError("commit measurement PostgreSQL migration", err)
	}
	return nil
}

// Close releases the PostgreSQL connection pool.
func (repository *PostgresRepository) Close() error {
	if repository == nil || repository.database == nil {
		return nil
	}
	if err := repository.database.Close(); err != nil {
		return repository.safeError("close measurement PostgreSQL", err)
	}
	return nil
}

// Record persists one immutable final outcome within the configured project.
func (repository *PostgresRepository) Record(outcome FinalOutcome) error {
	if err := validateStoredOutcome(outcome); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresOperationTimeout)
	defer cancel()
	err := insertPostgresFinalOutcome(ctx, repository.database, repository.project, outcome, true)
	if errors.Is(err, ErrFinalOutcomeAlreadyRecorded) {
		return err
	}
	if err != nil {
		return repository.safeError("record measurement final outcome", err)
	}
	return nil
}

// EnrichActionsMiss atomically adds producer and upload measurements to one
// existing project-scoped Actions lookup miss.
func (repository *PostgresRepository) EnrichActionsMiss(completion ActionsMissCompletion) error {
	if err := validateStoredActionsMissCompletion(completion); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresOperationTimeout)
	defer cancel()
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return repository.safeError("begin Actions miss completion", err)
	}
	defer transaction.Rollback()

	var encodedLookupFinish []byte
	err = transaction.QueryRowContext(ctx, `
		SELECT finished_at
		FROM layercache_measurement_final_outcomes_v1
		WHERE project_id = $1 AND run_id = $2 AND work_id = $3
			AND integration = 'actions' AND result = 'miss'
		FOR UPDATE`, repository.project, completion.RunID, completion.WorkID).Scan(&encodedLookupFinish)
	if errors.Is(err, sql.ErrNoRows) {
		return actionsMissNotFound(completion)
	}
	if err != nil {
		return repository.safeError("load Actions miss outcome", err)
	}
	var lookupFinishedAt time.Time
	if err := lookupFinishedAt.UnmarshalBinary(encodedLookupFinish); err != nil {
		return fmt.Errorf("decode Actions miss lookup finish: %w", err)
	}
	if err := validateActionsMissCompletionOrder(FinalOutcome{FinishedAt: lookupFinishedAt}, completion); err != nil {
		return err
	}
	encodedCompletionFinish, err := completion.FinishedAt.MarshalBinary()
	if err != nil {
		return fmt.Errorf("encode Actions miss completion finish: %w", err)
	}
	result, err := transaction.ExecContext(ctx, `
		UPDATE layercache_measurement_final_outcomes_v1
		SET finished_at = $1,
			finished_at_unix_seconds = $2,
			finished_at_nanosecond = $3,
			execution_duration_ns = $4,
			upload_ns = $5,
			uploaded_bytes = $6
		WHERE project_id = $7 AND run_id = $8 AND work_id = $9
			AND integration = 'actions' AND result = 'miss'`,
		encodedCompletionFinish,
		completion.FinishedAt.Unix(),
		completion.FinishedAt.Nanosecond(),
		int64(completion.ExecutionDuration),
		int64(completion.UploadDuration),
		completion.UploadedBytes,
		repository.project,
		completion.RunID,
		completion.WorkID,
	)
	if err != nil {
		return repository.safeError("enrich Actions miss outcome", err)
	}
	written, err := result.RowsAffected()
	if err != nil {
		return repository.safeError("confirm Actions miss completion", err)
	}
	if written != 1 {
		return actionsMissNotFound(completion)
	}
	if err := transaction.Commit(); err != nil {
		return repository.safeError("commit Actions miss completion", err)
	}
	return nil
}

func (repository *PostgresRepository) LatestBuildkitBaseline(artifactID, compatibilityID string) (*time.Duration, error) {
	if err := validateBoundedText("measurement artifact ID", artifactID, maximumIdentityBytes, false); err != nil {
		return nil, err
	}
	if err := validateBoundedText("measurement compatibility ID", compatibilityID, maximumIdentityBytes, false); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresOperationTimeout)
	defer cancel()
	var nanoseconds int64
	err := repository.database.QueryRowContext(ctx, `
		SELECT execution_duration_ns
		FROM layercache_measurement_final_outcomes_v1
		WHERE project_id = $1 AND integration = 'buildkit' AND artifact_id = $2 AND compatibility_id = $3
			AND result = 'miss' AND execution_duration_ns IS NOT NULL
		ORDER BY started_at_unix_seconds DESC, started_at_nanosecond DESC LIMIT 1`,
		repository.project, artifactID, compatibilityID,
	).Scan(&nanoseconds)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, repository.safeError("read BuildKit cold baseline", err)
	}
	duration := time.Duration(nanoseconds)
	return &duration, nil
}

func insertPostgresFinalOutcome(
	ctx context.Context,
	executor postgresOutcomeExecutor,
	project string,
	outcome FinalOutcome,
	ignoreConflict bool,
) error {
	if err := validateStoredOutcome(outcome); err != nil {
		return err
	}
	dependencies, err := json.Marshal(outcome.Dependencies)
	if err != nil {
		return fmt.Errorf("encode measurement dependencies: %w", err)
	}
	startedAt, err := outcome.StartedAt.MarshalBinary()
	if err != nil {
		return fmt.Errorf("encode measurement start: %w", err)
	}
	finishedAt, err := outcome.FinishedAt.MarshalBinary()
	if err != nil {
		return fmt.Errorf("encode measurement finish: %w", err)
	}

	statement := `
		INSERT INTO layercache_measurement_final_outcomes_v1 (
			project_id, run_id, workspace_id, integration, work_id, dependencies_json, artifact_id, compatibility_id,
			result, source, started_at, finished_at,
			started_at_unix_seconds, started_at_nanosecond,
			finished_at_unix_seconds, finished_at_nanosecond,
			producer_duration_ns, execution_duration_ns,
			lookup_ns, download_ns, verification_ns, restore_ns, upload_ns,
			downloaded_bytes, uploaded_bytes, degraded, eligible_units, hit_units
		) VALUES (
			$1, $2, $3, $4, $5, $6::TEXT::JSONB, $7, $8, $9, $10, $11, $12,
			$13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24,
			$25, $26, $27, $28
		)`
	if ignoreConflict {
		statement += ` ON CONFLICT (project_id, run_id, work_id) DO NOTHING`
	}
	result, err := executor.ExecContext(ctx, statement,
		project,
		outcome.RunID,
		outcome.WorkspaceID,
		string(outcome.Integration),
		outcome.WorkID,
		string(dependencies),
		outcome.ArtifactID,
		outcome.CompatibilityID,
		string(outcome.Result),
		string(outcome.Source),
		startedAt,
		finishedAt,
		outcome.StartedAt.Unix(),
		outcome.StartedAt.Nanosecond(),
		outcome.FinishedAt.Unix(),
		outcome.FinishedAt.Nanosecond(),
		nullableDuration(outcome.ProducerDuration),
		nullableDuration(outcome.ExecutionDuration),
		int64(outcome.Timing.Lookup),
		int64(outcome.Timing.Download),
		int64(outcome.Timing.Verification),
		int64(outcome.Timing.Restore),
		int64(outcome.Timing.Upload),
		outcome.Bytes.Downloaded,
		outcome.Bytes.Uploaded,
		outcome.Degraded,
		outcome.EligibleUnits,
		outcome.HitUnits,
	)
	if err != nil {
		return fmt.Errorf("record measurement final outcome: %w", err)
	}
	written, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("confirm measurement final outcome: %w", err)
	}
	if ignoreConflict && written == 0 {
		return fmt.Errorf("%w: run %q work %q", ErrFinalOutcomeAlreadyRecorded, outcome.RunID, outcome.WorkID)
	}
	return nil
}

// RunReport replays project-scoped records through the shared report
// calculator.
func (repository *PostgresRepository) RunReport(runID string) (RunReport, error) {
	if err := validateBoundedText("measurement run ID", runID, maximumRunIDBytes, false); err != nil {
		return RunReport{}, ErrRunNotFound
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresOperationTimeout)
	defer cancel()
	recorder, count, err := repository.replay(ctx, postgresSelectOutcomes+`
		WHERE project_id = $1 AND run_id = $2
		ORDER BY run_id, work_id`, repository.project, runID)
	if err != nil {
		return RunReport{}, err
	}
	if count == 0 {
		return RunReport{}, ErrRunNotFound
	}
	return recorder.RunReport(runID)
}

// PeriodReport includes runs whose first final outcome starts in [from, to).
func (repository *PostgresRepository) PeriodReport(from, to time.Time) (PeriodReport, error) {
	if from.IsZero() || to.IsZero() || !to.After(from) {
		return PeriodReport{}, errors.New("measurement period needs an ordered start and finish")
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresOperationTimeout)
	defer cancel()
	query := `
		WITH run_starts AS (
			SELECT DISTINCT ON (run_id)
				run_id, started_at_unix_seconds, started_at_nanosecond
			FROM layercache_measurement_final_outcomes_v1
			WHERE project_id = $1
			ORDER BY run_id, started_at_unix_seconds, started_at_nanosecond
		), selected_runs AS (
			SELECT run_id
			FROM run_starts
			WHERE (started_at_unix_seconds, started_at_nanosecond) >= ($2::BIGINT, $3::INTEGER)
				AND (started_at_unix_seconds, started_at_nanosecond) < ($4::BIGINT, $5::INTEGER)
		)
	` + postgresSelectOutcomes + ` AS outcomes
		INNER JOIN selected_runs USING (run_id)
		WHERE outcomes.project_id = $1
		ORDER BY outcomes.run_id, outcomes.work_id`
	recorder, _, err := repository.replay(ctx, query,
		repository.project,
		from.Unix(), from.Nanosecond(),
		to.Unix(), to.Nanosecond(),
	)
	if err != nil {
		return PeriodReport{}, err
	}
	return recorder.PeriodReport(from, to)
}

const postgresSelectOutcomes = `
	SELECT
		run_id, workspace_id, integration, work_id, dependencies_json, artifact_id, compatibility_id,
		result, source, started_at, finished_at,
		producer_duration_ns, execution_duration_ns,
		lookup_ns, download_ns, verification_ns, restore_ns, upload_ns,
		downloaded_bytes, uploaded_bytes, degraded, eligible_units, hit_units
	FROM layercache_measurement_final_outcomes_v1`

func (repository *PostgresRepository) replay(
	ctx context.Context,
	query string,
	arguments ...any,
) (*Recorder, int, error) {
	rows, err := repository.database.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, 0, repository.safeError("load measurement final outcomes", err)
	}
	defer rows.Close()

	recorder := NewRecorder()
	count := 0
	for rows.Next() {
		outcome, err := scanFinalOutcome(rows)
		if err != nil {
			return nil, 0, err
		}
		if err := recorder.Record(outcome); err != nil {
			return nil, 0, fmt.Errorf("replay measurement final outcome: %w", err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return nil, 0, repository.safeError("load measurement final outcomes", err)
	}
	return recorder, count, nil
}

// ObserveTurbo records project-scoped gateway evidence. It takes the same
// per-run advisory lock as reconciliation so a committed observation is never
// deleted without first being considered by the reconciler.
func (repository *PostgresRepository) ObserveTurbo(observation TurboObservation) error {
	if err := validateTurboObservation(observation); err != nil {
		return err
	}
	if err := validateBoundedText("measurement run ID", observation.RunID, maximumRunIDBytes, false); err != nil {
		return err
	}
	if err := validateBoundedText("Turbo observation artifact ID", observation.ArtifactID, maximumIdentityBytes, false); err != nil {
		return err
	}
	startedAt, err := observation.StartedAt.MarshalBinary()
	if err != nil {
		return fmt.Errorf("encode Turbo observation start: %w", err)
	}
	finishedAt, err := observation.FinishedAt.MarshalBinary()
	if err != nil {
		return fmt.Errorf("encode Turbo observation finish: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), postgresOperationTimeout)
	defer cancel()
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return repository.safeError("begin Turbo measurement observation", err)
	}
	defer transaction.Rollback()
	if err := repository.lockRun(ctx, transaction, observation.RunID); err != nil {
		return err
	}
	_, err = transaction.ExecContext(ctx, `
		INSERT INTO layercache_measurement_turbo_observations_v1 (
			project_id, run_id, artifact_id, result, source, started_at, finished_at,
			started_at_unix_seconds, started_at_nanosecond,
			finished_at_unix_seconds, finished_at_nanosecond,
			lookup_ns, download_ns, verification_ns, restore_ns, upload_ns,
			downloaded_bytes, uploaded_bytes, degraded
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
			$11, $12, $13, $14, $15, $16, $17, $18, $19
		)`,
		repository.project,
		observation.RunID,
		observation.ArtifactID,
		string(observation.Result),
		string(observation.Source),
		startedAt,
		finishedAt,
		observation.StartedAt.Unix(),
		observation.StartedAt.Nanosecond(),
		observation.FinishedAt.Unix(),
		observation.FinishedAt.Nanosecond(),
		int64(observation.Timing.Lookup),
		int64(observation.Timing.Download),
		int64(observation.Timing.Verification),
		int64(observation.Timing.Restore),
		int64(observation.Timing.Upload),
		observation.Bytes.Downloaded,
		observation.Bytes.Uploaded,
		observation.Degraded,
	)
	if err != nil {
		return repository.safeError("record Turbo transport observation", err)
	}
	if err := transaction.Commit(); err != nil {
		return repository.safeError("commit Turbo transport observation", err)
	}
	return nil
}

// ReconcileTurboSummaries atomically replaces provisional traffic with final
// task outcomes for one project-scoped run.
func (repository *PostgresRepository) ReconcileTurboSummaries(
	runID string,
	options TurboReconcileOptions,
	documents [][]byte,
) (int, error) {
	summaries, err := parseTurboSummaries(runID, options, documents)
	if err != nil {
		return 0, err
	}
	if err := validateBoundedText("measurement run ID", runID, maximumRunIDBytes, false); err != nil {
		return 0, err
	}
	if err := validateBoundedText("Turbo reconciliation project", options.Project, maximumProjectBytes, false); err != nil {
		return 0, err
	}
	if err := validateBoundedText("Turbo reconciliation compatibility", options.CompatibilityID, maximumIdentityBytes, false); err != nil {
		return 0, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), postgresOperationTimeout)
	defer cancel()
	transaction, err := repository.database.BeginTx(ctx, nil)
	if err != nil {
		return 0, repository.safeError("begin Turbo measurement reconciliation", err)
	}
	defer transaction.Rollback()
	if err := repository.lockRun(ctx, transaction, runID); err != nil {
		return 0, err
	}

	observations, err := repository.loadTurboObservations(ctx, transaction, runID)
	if err != nil {
		return 0, err
	}
	outcomes, err := reconcileTurboOutcomes(runID, options, summaries, observations)
	if err != nil {
		return 0, err
	}
	for _, outcome := range outcomes {
		if err := validateStoredOutcome(outcome); err != nil {
			return 0, err
		}
	}
	if _, err := transaction.ExecContext(ctx, `
		DELETE FROM layercache_measurement_final_outcomes_v1
		WHERE project_id = $1 AND run_id = $2 AND integration IN ('', 'turbo')`, repository.project, runID); err != nil {
		return 0, repository.safeError("clear provisional Turbo outcomes", err)
	}
	for _, outcome := range outcomes {
		if err := insertPostgresFinalOutcome(ctx, transaction, repository.project, outcome, false); err != nil {
			return 0, repository.safeError("store reconciled Turbo outcome", err)
		}
	}
	if _, err := transaction.ExecContext(ctx, `
		DELETE FROM layercache_measurement_turbo_observations_v1
		WHERE project_id = $1 AND run_id = $2`, repository.project, runID); err != nil {
		return 0, repository.safeError("clear reconciled Turbo observations", err)
	}
	if err := transaction.Commit(); err != nil {
		return 0, repository.safeError("commit Turbo measurement reconciliation", err)
	}
	return len(outcomes), nil
}

func (repository *PostgresRepository) lockRun(ctx context.Context, transaction *sql.Tx, runID string) error {
	_, err := transaction.ExecContext(ctx, `
		SELECT pg_advisory_xact_lock(
			hashtextextended($1, hashtextextended($2, 78234723))
		)`, repository.project, runID)
	if err != nil {
		return repository.safeError("lock Turbo measurement run", err)
	}
	return nil
}

func (repository *PostgresRepository) loadTurboObservations(
	ctx context.Context,
	transaction *sql.Tx,
	runID string,
) ([]turboObservation, error) {
	rows, err := transaction.QueryContext(ctx, `
		SELECT
			artifact_id, result, source, started_at, finished_at,
			lookup_ns, download_ns, verification_ns, restore_ns, upload_ns,
			downloaded_bytes, uploaded_bytes, degraded
		FROM layercache_measurement_turbo_observations_v1
		WHERE project_id = $1 AND run_id = $2
		ORDER BY sequence`, repository.project, runID)
	if err != nil {
		return nil, repository.safeError("load Turbo transport observations", err)
	}
	observations := make([]turboObservation, 0)
	for rows.Next() {
		observation, err := scanTurboObservation(rows, runID)
		if err != nil {
			rows.Close()
			return nil, err
		}
		observations = append(observations, turboObservation{FinalOutcome: observation})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, repository.safeError("load Turbo transport observations", err)
	}
	if err := rows.Close(); err != nil {
		return nil, repository.safeError("close Turbo transport observations", err)
	}
	if len(observations) > 0 {
		return observations, nil
	}

	legacyRows, err := transaction.QueryContext(ctx, postgresSelectOutcomes+`
		WHERE project_id = $1 AND run_id = $2 AND integration IN ('', 'turbo')
		ORDER BY started_at_unix_seconds, started_at_nanosecond, work_id`, repository.project, runID)
	if err != nil {
		return nil, repository.safeError("load legacy Turbo observations", err)
	}
	for legacyRows.Next() {
		outcome, err := scanFinalOutcome(legacyRows)
		if err != nil {
			legacyRows.Close()
			return nil, err
		}
		observations = append(observations, turboObservation{FinalOutcome: outcome})
	}
	if err := legacyRows.Err(); err != nil {
		legacyRows.Close()
		return nil, repository.safeError("load legacy Turbo observations", err)
	}
	if err := legacyRows.Close(); err != nil {
		return nil, repository.safeError("close legacy Turbo observations", err)
	}
	return observations, nil
}

func validateStoredOutcome(outcome FinalOutcome) error {
	if err := validateOutcome(outcome); err != nil {
		return err
	}
	if err := validateBoundedText("measurement run ID", outcome.RunID, maximumRunIDBytes, false); err != nil {
		return err
	}
	if err := validateBoundedText("measurement work ID", outcome.WorkID, maximumIdentityBytes, false); err != nil {
		return err
	}
	if err := validateBoundedText("measurement workspace ID", outcome.WorkspaceID, maximumIdentityBytes, true); err != nil {
		return err
	}
	if err := validateBoundedText("measurement artifact ID", outcome.ArtifactID, maximumIdentityBytes, true); err != nil {
		return err
	}
	if err := validateBoundedText("measurement compatibility ID", outcome.CompatibilityID, maximumIdentityBytes, true); err != nil {
		return err
	}
	if len(outcome.Dependencies) > maximumDependencies {
		return fmt.Errorf("measurement dependencies exceed %d entries", maximumDependencies)
	}
	for _, dependency := range outcome.Dependencies {
		if err := validateBoundedText("measurement dependency", dependency, maximumIdentityBytes, false); err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(outcome.Dependencies)
	if err != nil {
		return fmt.Errorf("encode measurement dependencies: %w", err)
	}
	if len(encoded) > maximumDependenciesBytes {
		return fmt.Errorf("measurement dependencies exceed %d encoded bytes", maximumDependenciesBytes)
	}
	return nil
}

func validateStoredActionsMissCompletion(completion ActionsMissCompletion) error {
	return validateActionsMissCompletion(completion)
}

func validateBoundedText(name, value string, maximum int, allowEmpty bool) error {
	if value == "" && allowEmpty {
		return nil
	}
	if value == "" || strings.TrimSpace(value) != value {
		return fmt.Errorf("%s must be non-empty and trimmed", name)
	}
	if len(value) > maximum {
		return fmt.Errorf("%s exceeds %d bytes", name, maximum)
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%s contains control characters", name)
	}
	return nil
}

func (repository *PostgresRepository) safeError(operation string, err error) error {
	return redactPostgresError(operation, repository.postgresURL, err)
}

func redactPostgresError(operation, postgresURL string, err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	if postgresURL != "" {
		message = strings.ReplaceAll(message, postgresURL, "[redacted]")
		message = strings.ReplaceAll(message, url.QueryEscape(postgresURL), "[redacted]")
	}
	if parsed, parseErr := url.Parse(postgresURL); parseErr == nil && parsed.User != nil {
		if password, found := parsed.User.Password(); found && password != "" {
			message = strings.ReplaceAll(message, password, "[redacted]")
			message = strings.ReplaceAll(message, url.QueryEscape(password), "[redacted]")
			message = strings.ReplaceAll(message, url.PathEscape(password), "[redacted]")
		}
	}
	return fmt.Errorf("%s: %s", operation, message)
}
