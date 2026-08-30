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
			PRIMARY KEY (run_id, work_id)
		) WITHOUT ROWID`,
	}
	for _, statement := range statements {
		if _, err := repository.database.Exec(statement); err != nil {
			return fmt.Errorf("initialize measurement database: %w", err)
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

	result, err := repository.database.Exec(`
		INSERT INTO measurement_final_outcomes_v1 (
			run_id, work_id, dependencies_json, artifact_id, compatibility_id,
			result, source, started_at, finished_at,
			producer_duration_ns, execution_duration_ns,
			lookup_ns, download_ns, verification_ns, restore_ns, upload_ns,
			downloaded_bytes, uploaded_bytes, degraded
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (run_id, work_id) DO NOTHING`,
		outcome.RunID,
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
	)
	if err != nil {
		return fmt.Errorf("record measurement final outcome: %w", err)
	}
	written, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("confirm measurement final outcome: %w", err)
	}
	if written == 0 {
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
		run_id, work_id, dependencies_json, artifact_id, compatibility_id,
		result, source, started_at, finished_at,
		producer_duration_ns, execution_duration_ns,
		lookup_ns, download_ns, verification_ns, restore_ns, upload_ns,
		downloaded_bytes, uploaded_bytes, degraded
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
