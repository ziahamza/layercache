package measurement

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

var ErrFinalOutcomeAlreadyRecorded = errors.New("measurement final outcome already recorded")

// SQLiteRepository persists the bounded, non-sensitive FinalOutcome telemetry
// model. Its schema deliberately has no fields for paths, environment values,
// credentials, or cache keys.
type SQLiteRepository struct {
	database *sql.DB
}

type outcomeExecutor interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// OpenSQLiteRepository opens or creates a measurement event database at path.
func OpenSQLiteRepository(path string) (*SQLiteRepository, error) {
	if path == "" {
		return nil, errors.New("measurement database path is required")
	}

	database, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open measurement database: %w", err)
	}
	// A single connection also makes :memory: useful and keeps PRAGMA settings
	// consistent. SQLite still serializes writes across processes.
	database.SetMaxOpenConns(1)

	repository := &SQLiteRepository{database: database}
	if err := repository.initialize(); err != nil {
		_ = database.Close()
		return nil, err
	}
	return repository, nil
}

func (repository *SQLiteRepository) initialize() error {
	statements := []string{
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA journal_mode = WAL`,
		`PRAGMA synchronous = NORMAL`,
		`CREATE TABLE IF NOT EXISTS measurement_final_outcomes_v1 (
			run_id TEXT NOT NULL,
			workspace_id TEXT NOT NULL DEFAULT '',
			integration TEXT NOT NULL DEFAULT '',
			work_id TEXT NOT NULL,
			dependencies_json BLOB NOT NULL,
			artifact_id TEXT NOT NULL,
			compatibility_id TEXT NOT NULL,
			result TEXT NOT NULL,
			source TEXT NOT NULL,
			started_at BLOB NOT NULL,
			finished_at BLOB NOT NULL,
			producer_duration_ns INTEGER,
			execution_duration_ns INTEGER,
			lookup_ns INTEGER NOT NULL,
			download_ns INTEGER NOT NULL,
			verification_ns INTEGER NOT NULL,
			restore_ns INTEGER NOT NULL,
			upload_ns INTEGER NOT NULL,
			downloaded_bytes INTEGER NOT NULL,
			uploaded_bytes INTEGER NOT NULL,
			degraded INTEGER NOT NULL CHECK (degraded IN (0, 1)),
			eligible_units INTEGER NOT NULL DEFAULT 0 CHECK (eligible_units >= 0),
			hit_units INTEGER NOT NULL DEFAULT 0 CHECK (hit_units >= 0 AND hit_units <= eligible_units),
			PRIMARY KEY (run_id, work_id)
		) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS measurement_turbo_observations_v1 (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id TEXT NOT NULL,
			artifact_id TEXT NOT NULL,
			result TEXT NOT NULL,
			source TEXT NOT NULL,
			started_at BLOB NOT NULL,
			finished_at BLOB NOT NULL,
			lookup_ns INTEGER NOT NULL,
			download_ns INTEGER NOT NULL,
			verification_ns INTEGER NOT NULL,
			restore_ns INTEGER NOT NULL,
			upload_ns INTEGER NOT NULL,
			downloaded_bytes INTEGER NOT NULL,
			uploaded_bytes INTEGER NOT NULL,
			degraded INTEGER NOT NULL CHECK (degraded IN (0, 1))
		)`,
		`CREATE INDEX IF NOT EXISTS measurement_turbo_observations_run_v1
			ON measurement_turbo_observations_v1 (run_id, artifact_id, sequence)`,
	}
	for _, statement := range statements {
		if _, err := repository.database.Exec(statement); err != nil {
			return fmt.Errorf("initialize measurement database: %w", err)
		}
	}
	if err := repository.ensureFinalOutcomeColumns(); err != nil {
		return err
	}
	return nil
}

func (repository *SQLiteRepository) ensureFinalOutcomeColumns() error {
	rows, err := repository.database.Query(`PRAGMA table_info(measurement_final_outcomes_v1)`)
	if err != nil {
		return fmt.Errorf("inspect measurement database columns: %w", err)
	}
	columns := make(map[string]bool)
	for rows.Next() {
		var sequence int
		var name, dataType string
		var notNull int
		var defaultValue any
		var primaryKey int
		if err := rows.Scan(&sequence, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return fmt.Errorf("inspect measurement database column: %w", err)
		}
		columns[name] = true
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close measurement database column inspection: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect measurement database columns: %w", err)
	}
	additions := []struct {
		name       string
		definition string
	}{
		{name: "workspace_id", definition: `TEXT NOT NULL DEFAULT ''`},
		{name: "integration", definition: `TEXT NOT NULL DEFAULT ''`},
		{name: "eligible_units", definition: `INTEGER NOT NULL DEFAULT 0 CHECK (eligible_units >= 0)`},
		{name: "hit_units", definition: `INTEGER NOT NULL DEFAULT 0 CHECK (hit_units >= 0 AND hit_units <= eligible_units)`},
	}
	for _, addition := range additions {
		if columns[addition.name] {
			continue
		}
		if _, err := repository.database.Exec(`ALTER TABLE measurement_final_outcomes_v1 ADD COLUMN ` + addition.name + ` ` + addition.definition); err != nil {
			return fmt.Errorf("add measurement database column %s: %w", addition.name, err)
		}
	}
	return nil
}

// Close releases the database connection.
func (repository *SQLiteRepository) Close() error {
	if err := repository.database.Close(); err != nil {
		return fmt.Errorf("close measurement database: %w", err)
	}
	return nil
}

// Record persists one immutable final outcome. The primary-key conflict check
// and insert are a single SQLite statement, so concurrent writers cannot both
// accept the same run/work pair.
func (repository *SQLiteRepository) Record(outcome FinalOutcome) error {
	if err := validateOutcome(outcome); err != nil {
		return err
	}
	return insertFinalOutcome(repository.database, outcome, true)
}

// EnrichActionsMiss atomically adds producer and upload measurements to one
// existing Actions lookup miss.
func (repository *SQLiteRepository) EnrichActionsMiss(completion ActionsMissCompletion) error {
	if err := validateActionsMissCompletion(completion); err != nil {
		return err
	}
	transaction, err := repository.database.Begin()
	if err != nil {
		return fmt.Errorf("begin Actions miss completion: %w", err)
	}
	defer transaction.Rollback()

	var encodedLookupFinish []byte
	err = transaction.QueryRow(`
		SELECT finished_at
		FROM measurement_final_outcomes_v1
		WHERE run_id = ? AND work_id = ? AND integration = 'actions' AND result = 'miss'`,
		completion.RunID, completion.WorkID,
	).Scan(&encodedLookupFinish)
	if errors.Is(err, sql.ErrNoRows) {
		return actionsMissNotFound(completion)
	}
	if err != nil {
		return fmt.Errorf("load Actions miss outcome: %w", err)
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
	result, err := transaction.Exec(`
		UPDATE measurement_final_outcomes_v1
		SET finished_at = ?, execution_duration_ns = ?, upload_ns = ?, uploaded_bytes = ?
		WHERE run_id = ? AND work_id = ? AND integration = 'actions' AND result = 'miss'
			AND finished_at = ?`,
		encodedCompletionFinish,
		int64(completion.ExecutionDuration),
		int64(completion.UploadDuration),
		completion.UploadedBytes,
		completion.RunID,
		completion.WorkID,
		encodedLookupFinish,
	)
	if err != nil {
		return fmt.Errorf("enrich Actions miss outcome: %w", err)
	}
	written, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("confirm Actions miss completion: %w", err)
	}
	if written != 1 {
		return actionsMissNotFound(completion)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit Actions miss completion: %w", err)
	}
	return nil
}

// LatestBuildkitBaseline returns the most recent observed cold execution for
// the same stable BuildKit graph. A warm build uses it as producer duration;
// absence remains unknown rather than being estimated from the warm solve.
func (repository *SQLiteRepository) LatestBuildkitBaseline(artifactID, compatibilityID string) (*time.Duration, error) {
	var nanoseconds int64
	err := repository.database.QueryRow(`
		SELECT execution_duration_ns
		FROM measurement_final_outcomes_v1
		WHERE integration = 'buildkit' AND artifact_id = ? AND compatibility_id = ?
			AND result = 'miss' AND execution_duration_ns IS NOT NULL
		ORDER BY started_at DESC LIMIT 1`, artifactID, compatibilityID).Scan(&nanoseconds)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read BuildKit cold baseline: %w", err)
	}
	duration := time.Duration(nanoseconds)
	return &duration, nil
}

func insertFinalOutcome(executor outcomeExecutor, outcome FinalOutcome, ignoreConflict bool) error {
	if err := validateOutcome(outcome); err != nil {
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
		INSERT INTO measurement_final_outcomes_v1 (
			run_id, workspace_id, integration, work_id, dependencies_json, artifact_id, compatibility_id,
			result, source, started_at, finished_at,
			producer_duration_ns, execution_duration_ns,
			lookup_ns, download_ns, verification_ns, restore_ns, upload_ns,
			downloaded_bytes, uploaded_bytes, degraded, eligible_units, hit_units
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if ignoreConflict {
		statement += ` ON CONFLICT (run_id, work_id) DO NOTHING`
	}
	result, err := executor.Exec(statement,
		outcome.RunID,
		outcome.WorkspaceID,
		outcome.Integration,
		outcome.WorkID,
		dependencies,
		outcome.ArtifactID,
		outcome.CompatibilityID,
		outcome.Result,
		outcome.Source,
		startedAt,
		finishedAt,
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

// RunReport replays the stored events through Recorder so persistent and
// in-memory reporting always share one calculation path.
func (repository *SQLiteRepository) RunReport(runID string) (RunReport, error) {
	recorder, count, err := repository.replay(" WHERE run_id = ?", runID)
	if err != nil {
		return RunReport{}, err
	}
	if count == 0 {
		return RunReport{}, ErrRunNotFound
	}
	return recorder.RunReport(runID)
}

// PeriodReport replays all stored events through Recorder and applies the
// Recorder's existing half-open period semantics.
func (repository *SQLiteRepository) PeriodReport(from, to time.Time) (PeriodReport, error) {
	recorder, _, err := repository.replay("")
	if err != nil {
		return PeriodReport{}, err
	}
	return recorder.PeriodReport(from, to)
}

const selectOutcomes = `
	SELECT
		run_id, workspace_id, integration, work_id, dependencies_json, artifact_id, compatibility_id,
		result, source, started_at, finished_at,
		producer_duration_ns, execution_duration_ns,
		lookup_ns, download_ns, verification_ns, restore_ns, upload_ns,
		downloaded_bytes, uploaded_bytes, degraded, eligible_units, hit_units
	FROM measurement_final_outcomes_v1`

func (repository *SQLiteRepository) replay(filter string, arguments ...any) (*Recorder, int, error) {
	rows, err := repository.database.Query(selectOutcomes+filter+" ORDER BY run_id, work_id", arguments...)
	if err != nil {
		return nil, 0, fmt.Errorf("load measurement final outcomes: %w", err)
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
		return nil, 0, fmt.Errorf("load measurement final outcomes: %w", err)
	}
	return recorder, count, nil
}

type rowScanner interface {
	Scan(destinations ...any) error
}

func scanFinalOutcome(row rowScanner) (FinalOutcome, error) {
	var outcome FinalOutcome
	var dependencies []byte
	var startedAt []byte
	var finishedAt []byte
	var producerDuration sql.NullInt64
	var executionDuration sql.NullInt64
	var lookup int64
	var download int64
	var verification int64
	var restore int64
	var upload int64
	var degraded bool

	if err := row.Scan(
		&outcome.RunID,
		&outcome.WorkspaceID,
		&outcome.Integration,
		&outcome.WorkID,
		&dependencies,
		&outcome.ArtifactID,
		&outcome.CompatibilityID,
		&outcome.Result,
		&outcome.Source,
		&startedAt,
		&finishedAt,
		&producerDuration,
		&executionDuration,
		&lookup,
		&download,
		&verification,
		&restore,
		&upload,
		&outcome.Bytes.Downloaded,
		&outcome.Bytes.Uploaded,
		&degraded,
		&outcome.EligibleUnits,
		&outcome.HitUnits,
	); err != nil {
		return FinalOutcome{}, fmt.Errorf("decode measurement final outcome: %w", err)
	}
	if err := json.Unmarshal(dependencies, &outcome.Dependencies); err != nil {
		return FinalOutcome{}, fmt.Errorf("decode measurement dependencies: %w", err)
	}
	if err := outcome.StartedAt.UnmarshalBinary(startedAt); err != nil {
		return FinalOutcome{}, fmt.Errorf("decode measurement start: %w", err)
	}
	if err := outcome.FinishedAt.UnmarshalBinary(finishedAt); err != nil {
		return FinalOutcome{}, fmt.Errorf("decode measurement finish: %w", err)
	}
	outcome.ProducerDuration = durationFromNull(producerDuration)
	outcome.ExecutionDuration = durationFromNull(executionDuration)
	outcome.Timing = Timing{
		Lookup:       time.Duration(lookup),
		Download:     time.Duration(download),
		Verification: time.Duration(verification),
		Restore:      time.Duration(restore),
		Upload:       time.Duration(upload),
	}
	outcome.Degraded = degraded
	return outcome, nil
}

func nullableDuration(duration *time.Duration) any {
	if duration == nil {
		return nil
	}
	return int64(*duration)
}

func durationFromNull(duration sql.NullInt64) *time.Duration {
	if !duration.Valid {
		return nil
	}
	value := time.Duration(duration.Int64)
	return &value
}
