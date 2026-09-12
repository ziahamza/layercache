package publicbuild

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// SQLiteCoordinator is a durable Public Build coordinator. A newly opened
// coordinator recovers interrupted running builds to the queue and invalidates
// their old leases before accepting work.
type SQLiteCoordinator struct {
	database  *sql.DB
	config    Config
	allowlist map[string]struct{}
}

// OpenSQLiteCoordinator opens or creates a durable Public Build coordinator at
// path. The returned coordinator implements Coordinator and must be closed.
func OpenSQLiteCoordinator(path string, config Config) (*SQLiteCoordinator, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("Public Build database path is required")
	}
	allowlist, err := prepareConfig(&config)
	if err != nil {
		return nil, err
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open Public Build database: %w", err)
	}
	// Keeping one connection gives every method a strict serialization point,
	// while WAL still permits readers in other processes and busy_timeout fences
	// concurrent process writers.
	database.SetMaxOpenConns(1)
	coordinator := &SQLiteCoordinator{
		database:  database,
		config:    config,
		allowlist: allowlist,
	}
	if err := coordinator.initialize(context.Background()); err != nil {
		_ = database.Close()
		return nil, err
	}
	return coordinator, nil
}

func (coordinator *SQLiteCoordinator) initialize(ctx context.Context) error {
	statements := []string{
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA journal_mode = WAL`,
		`PRAGMA synchronous = NORMAL`,
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE IF NOT EXISTS public_builds_v1 (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			identity TEXT NOT NULL,
			repository TEXT NOT NULL,
			commit_digest TEXT NOT NULL,
			integration TEXT NOT NULL,
			target TEXT NOT NULL,
			recipe_digest TEXT NOT NULL,
			platform TEXT NOT NULL,
			declared_inputs_json BLOB NOT NULL DEFAULT '[]',
			cpu_millis INTEGER NOT NULL,
			memory_bytes INTEGER NOT NULL,
			disk_bytes INTEGER NOT NULL,
			timeout_ns INTEGER NOT NULL,
			state TEXT NOT NULL CHECK (state IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
			worker_id TEXT NOT NULL DEFAULT '',
			lease_token_hash TEXT NOT NULL DEFAULT '',
			lease_expires_at_ns INTEGER NOT NULL DEFAULT 0,
			publication_token_hash TEXT NOT NULL DEFAULT '',
			requested_at_ns INTEGER NOT NULL,
			started_at_ns INTEGER NOT NULL DEFAULT 0,
			finished_at_ns INTEGER NOT NULL DEFAULT 0,
			producer_duration_ns INTEGER,
			failure TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS public_builds_active_identity_v1
			ON public_builds_v1(identity)
			WHERE state IN ('queued', 'running', 'succeeded')`,
		`CREATE INDEX IF NOT EXISTS public_builds_fifo_v1
			ON public_builds_v1(state, sequence)`,
		`CREATE TABLE IF NOT EXISTS public_build_outputs_v1 (
			build_sequence INTEGER NOT NULL REFERENCES public_builds_v1(sequence) ON DELETE CASCADE,
			ordinal INTEGER NOT NULL,
			name TEXT NOT NULL,
			digest TEXT NOT NULL,
			size_bytes INTEGER NOT NULL,
			media_type TEXT NOT NULL,
			PRIMARY KEY (build_sequence, ordinal),
			UNIQUE (build_sequence, name)
		) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS public_build_logs_v1 (
			build_sequence INTEGER NOT NULL REFERENCES public_builds_v1(sequence) ON DELETE CASCADE,
			sequence INTEGER NOT NULL,
			timestamp_ns INTEGER NOT NULL,
			message TEXT NOT NULL,
			PRIMARY KEY (build_sequence, sequence)
		) WITHOUT ROWID`,
	}
	for _, statement := range statements {
		if _, err := coordinator.database.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize Public Build database: %w", err)
		}
	}
	for name, definition := range map[string]string{
		"lease_expires_at_ns":    `INTEGER NOT NULL DEFAULT 0`,
		"publication_token_hash": `TEXT NOT NULL DEFAULT ''`,
		"declared_inputs_json":   `BLOB NOT NULL DEFAULT '[]'`,
	} {
		if err := ensureBuildColumn(ctx, coordinator.database, name, definition); err != nil {
			return err
		}
	}

	transaction, err := coordinator.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Public Build recovery: %w", err)
	}
	defer transaction.Rollback()
	if _, err := transaction.ExecContext(ctx, `
		UPDATE public_builds_v1
		SET state = ?, worker_id = '', lease_token_hash = '', lease_expires_at_ns = 0,
			publication_token_hash = '', started_at_ns = 0
		WHERE state = ?`, StateQueued, StateRunning); err != nil {
		return fmt.Errorf("recover interrupted Public Builds: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit Public Build recovery: %w", err)
	}
	return nil
}

func ensureBuildColumn(ctx context.Context, database *sql.DB, name, definition string) error {
	rows, err := database.QueryContext(ctx, `PRAGMA table_info(public_builds_v1)`)
	if err != nil {
		return fmt.Errorf("inspect Public Build database schema: %w", err)
	}
	found := false
	for rows.Next() {
		var cid int
		var column, columnType string
		var notNull int
		var defaultValue any
		var primaryKey int
		if err := rows.Scan(&cid, &column, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return fmt.Errorf("inspect Public Build database column: %w", err)
		}
		if column == name {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close Public Build database schema: %w", err)
	}
	if found {
		return nil
	}
	if _, err := database.ExecContext(ctx, `ALTER TABLE public_builds_v1 ADD COLUMN `+name+` `+definition); err != nil {
		return fmt.Errorf("add Public Build database column %s: %w", name, err)
	}
	return nil
}

// Close releases the SQLite database connection.
func (coordinator *SQLiteCoordinator) Close() error {
	if err := coordinator.database.Close(); err != nil {
		return fmt.Errorf("close Public Build database: %w", err)
	}
	return nil
}

func (coordinator *SQLiteCoordinator) Request(ctx context.Context, request BuildRequest) (RequestResult, error) {
	if err := ctx.Err(); err != nil {
		return RequestResult{}, err
	}
	normalized, err := admit(ctx, request, coordinator.allowlist, coordinator.config)
	if err != nil {
		return RequestResult{}, err
	}
	publicationMissing := false
	publicationUnavailable := false
	if coordinator.config.Publications != nil {
		existing, lookupErr := coordinator.config.Publications.Find(ctx, normalized)
		if lookupErr == nil {
			build, err := buildFromExistingPublication(normalized, existing, coordinator.config.Now().UTC())
			if err != nil {
				return RequestResult{}, err
			}
			return RequestResult{Build: build, Reused: true}, nil
		}
		if !errors.Is(lookupErr, ErrPublicationNotFound) {
			return RequestResult{}, fmt.Errorf("find existing Public Cache publication: %w", lookupErr)
		}
		publicationMissing = true
		publicationUnavailable = errors.Is(lookupErr, ErrPublicationUnavailable)
	}
	identity := durableBuildIdentity(normalized)
	encodedInputs, err := encodeDeclaredInputs(normalized.Inputs)
	if err != nil {
		return RequestResult{}, err
	}
	now := coordinator.config.Now().UTC()

	transaction, err := coordinator.database.BeginTx(ctx, nil)
	if err != nil {
		return RequestResult{}, fmt.Errorf("begin Public Build request: %w", err)
	}
	defer transaction.Rollback()
	if publicationMissing {
		if _, err := transaction.ExecContext(ctx, `
			UPDATE public_builds_v1
			SET state = ?, publication_token_hash = '', failure = ?
			WHERE identity = ? AND state = ? AND (? OR finished_at_ns <= ?)`,
			StateFailed, "Public Cache publication is unavailable", identity, StateSucceeded,
			publicationUnavailable, now.Add(-publicationRegistrationGrace).UnixNano(),
		); err != nil {
			return RequestResult{}, fmt.Errorf("retire unavailable Public Build publication: %w", err)
		}
	}
	result, err := transaction.ExecContext(ctx, `
		INSERT OR IGNORE INTO public_builds_v1 (
			identity, repository, commit_digest, integration, target, recipe_digest, platform, declared_inputs_json,
			cpu_millis, memory_bytes, disk_bytes, timeout_ns, state, requested_at_ns
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		identity,
		normalized.Repository,
		normalized.Commit,
		normalized.Integration,
		normalized.Target,
		normalized.RecipeDigest,
		normalized.Platform,
		encodedInputs,
		normalized.Resources.CPUMillis,
		normalized.Resources.MemoryBytes,
		normalized.Resources.DiskBytes,
		int64(normalized.Resources.Timeout),
		StateQueued,
		now.UnixNano(),
	)
	if err != nil {
		return RequestResult{}, fmt.Errorf("queue Public Build: %w", err)
	}
	written, err := result.RowsAffected()
	if err != nil {
		return RequestResult{}, fmt.Errorf("confirm Public Build request: %w", err)
	}
	var sequence int64
	reused := written == 0
	if reused {
		err = transaction.QueryRowContext(ctx, `
			SELECT sequence FROM public_builds_v1
			WHERE identity = ? AND state IN (?, ?, ?)
			ORDER BY sequence LIMIT 1`, identity, StateQueued, StateRunning, StateSucceeded).Scan(&sequence)
		if errors.Is(err, sql.ErrNoRows) {
			return RequestResult{}, errors.New("Public Build identity changed during request")
		}
		if err != nil {
			return RequestResult{}, fmt.Errorf("load reused Public Build identity: %w", err)
		}
	} else {
		sequence, err = result.LastInsertId()
		if err != nil {
			return RequestResult{}, fmt.Errorf("read Public Build sequence: %w", err)
		}
	}
	build, err := loadSQLiteBuild(ctx, transaction, sequence)
	if err != nil {
		return RequestResult{}, err
	}
	if publicationMissing && build.State == StateSucceeded {
		return RequestResult{}, ErrPublicationPending
	}
	if err := transaction.Commit(); err != nil {
		return RequestResult{}, fmt.Errorf("commit Public Build request: %w", err)
	}
	return RequestResult{Build: build, Reused: reused}, nil
}

func (coordinator *SQLiteCoordinator) Inspect(ctx context.Context, id string) (Build, error) {
	if err := ctx.Err(); err != nil {
		return Build{}, err
	}
	sequence, err := parseBuildID(id)
	if err != nil {
		return Build{}, ErrNotFound
	}
	build, err := loadSQLiteBuild(ctx, coordinator.database, sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return Build{}, ErrNotFound
	}
	if err != nil {
		return Build{}, err
	}
	return build, nil
}

func (coordinator *SQLiteCoordinator) Logs(ctx context.Context, id string) ([]LogEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sequence, err := parseBuildID(id)
	if err != nil {
		return nil, ErrNotFound
	}
	var exists bool
	if err := coordinator.database.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM public_builds_v1 WHERE sequence = ?)`, sequence).Scan(&exists); err != nil {
		return nil, fmt.Errorf("find Public Build logs: %w", err)
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := coordinator.database.QueryContext(ctx, `
		SELECT sequence, timestamp_ns, message
		FROM public_build_logs_v1 WHERE build_sequence = ? ORDER BY sequence`, sequence)
	if err != nil {
		return nil, fmt.Errorf("load Public Build logs: %w", err)
	}
	defer rows.Close()
	var logs []LogEntry
	for rows.Next() {
		var entry LogEntry
		var timestamp int64
		if err := rows.Scan(&entry.Sequence, &timestamp, &entry.Message); err != nil {
			return nil, fmt.Errorf("decode Public Build log: %w", err)
		}
		entry.Timestamp = timeFromUnixNano(timestamp)
		logs = append(logs, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load Public Build logs: %w", err)
	}
	return logs, nil
}

func (coordinator *SQLiteCoordinator) LeaseNext(ctx context.Context, worker Worker) (Lease, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	if worker == nil {
		return Lease{}, reject("worker is required")
	}
	workerID := strings.TrimSpace(worker.ID())
	if workerID == "" {
		return Lease{}, reject("worker ID is required")
	}
	capabilities := worker.Capabilities()
	if (len(capabilities.Recipes) == 0 && len(capabilities.Integrations) == 0) || len(capabilities.Platforms) == 0 {
		return Lease{}, ErrNoWork
	}
	token, err := newLeaseToken()
	if err != nil {
		return Lease{}, err
	}
	now := coordinator.config.Now().UTC()
	expiresAt := now.Add(coordinator.config.LeaseDuration)

	transaction, err := coordinator.database.BeginTx(ctx, nil)
	if err != nil {
		return Lease{}, fmt.Errorf("begin Public Build lease: %w", err)
	}
	defer transaction.Rollback()
	if err := coordinator.requeueExpired(ctx, transaction, now); err != nil {
		return Lease{}, err
	}
	arguments := []any{StateRunning, workerID, hashLeaseToken(token), now.UnixNano(), expiresAt.UnixNano(), StateQueued}
	recipePredicate := "integration IN (" + placeholders(len(capabilities.Integrations)) + ")"
	if len(capabilities.Recipes) > 0 {
		predicates := make([]string, 0, len(capabilities.Recipes))
		for _, recipe := range capabilities.Recipes {
			if recipe.Target == "*" {
				predicates = append(predicates, "(integration = ? AND recipe_digest = ?)")
				arguments = append(arguments, recipe.Integration, recipe.RecipeDigest)
				continue
			}
			predicates = append(predicates, "(integration = ? AND target = ? AND recipe_digest = ?)")
			arguments = append(arguments, recipe.Integration, recipe.Target, recipe.RecipeDigest)
		}
		recipePredicate = "(" + strings.Join(predicates, " OR ") + ")"
	} else {
		for _, integration := range capabilities.Integrations {
			arguments = append(arguments, integration)
		}
	}
	for _, platform := range capabilities.Platforms {
		arguments = append(arguments, platform)
	}
	arguments = append(arguments, StateQueued)
	claim := fmt.Sprintf(`
		UPDATE public_builds_v1
		SET state = ?, worker_id = ?, lease_token_hash = ?, started_at_ns = ?, lease_expires_at_ns = ?
		WHERE sequence = (
			SELECT sequence FROM public_builds_v1
			WHERE state = ? AND %s AND platform IN (%s)
			ORDER BY sequence LIMIT 1
		) AND state = ?
		RETURNING sequence`, recipePredicate, placeholders(len(capabilities.Platforms)))
	var selected int64
	if err := transaction.QueryRowContext(ctx, claim, arguments...).Scan(&selected); errors.Is(err, sql.ErrNoRows) {
		return Lease{}, ErrNoWork
	} else if err != nil {
		return Lease{}, fmt.Errorf("claim queued Public Build: %w", err)
	}
	build, err := loadSQLiteBuild(ctx, transaction, selected)
	if err != nil {
		return Lease{}, err
	}
	if err := transaction.Commit(); err != nil {
		return Lease{}, fmt.Errorf("commit Public Build lease: %w", err)
	}
	return Lease{Token: token, WorkerID: workerID, Build: build, LeasedAt: now, ExpiresAt: expiresAt}, nil
}

func (coordinator *SQLiteCoordinator) requeueExpired(ctx context.Context, transaction *sql.Tx, now time.Time) error {
	if _, err := transaction.ExecContext(ctx, `
		UPDATE public_builds_v1
		SET state = ?, worker_id = '', lease_token_hash = '', lease_expires_at_ns = 0,
			publication_token_hash = '', started_at_ns = 0
		WHERE state = ? AND lease_expires_at_ns <= ?`, StateQueued, StateRunning, now.UnixNano()); err != nil {
		return fmt.Errorf("recover expired Public Build leases: %w", err)
	}
	return nil
}

func (coordinator *SQLiteCoordinator) Renew(ctx context.Context, lease Lease) (Lease, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	sequence, err := parseBuildID(lease.Build.ID)
	if err != nil {
		return Lease{}, ErrNotFound
	}
	now := coordinator.config.Now().UTC()
	expiresAt := now.Add(coordinator.config.LeaseDuration)
	transaction, err := coordinator.database.BeginTx(ctx, nil)
	if err != nil {
		return Lease{}, fmt.Errorf("begin Public Build lease renewal: %w", err)
	}
	defer transaction.Rollback()
	var renewed int64
	err = transaction.QueryRowContext(ctx, `
		UPDATE public_builds_v1 SET lease_expires_at_ns = ?
		WHERE sequence = ? AND state = ? AND worker_id = ? AND lease_token_hash = ?
			AND lease_expires_at_ns > ? AND publication_token_hash = ''
		RETURNING sequence`, expiresAt.UnixNano(), sequence, StateRunning, lease.WorkerID,
		hashLeaseToken(lease.Token), now.UnixNano()).Scan(&renewed)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, sqliteLeaseError(ctx, transaction, sequence)
	}
	if err != nil {
		return Lease{}, fmt.Errorf("renew Public Build lease: %w", err)
	}
	build, err := loadSQLiteBuild(ctx, transaction, renewed)
	if err != nil {
		return Lease{}, err
	}
	if err := transaction.Commit(); err != nil {
		return Lease{}, fmt.Errorf("commit Public Build lease renewal: %w", err)
	}
	return Lease{Token: lease.Token, WorkerID: lease.WorkerID, Build: build, LeasedAt: now, ExpiresAt: expiresAt}, nil
}

func (coordinator *SQLiteCoordinator) AppendLog(ctx context.Context, lease Lease, message string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sanitized := sanitizePublicBuildLog(coordinator.config, message, lease.Token)
	now := coordinator.config.Now().UTC()
	transaction, err := coordinator.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Public Build log append: %w", err)
	}
	defer transaction.Rollback()
	sequence, err := parseBuildID(lease.Build.ID)
	if err != nil {
		return ErrNotFound
	}
	var logSequence uint64
	err = transaction.QueryRowContext(ctx, `
		INSERT INTO public_build_logs_v1(build_sequence, sequence, timestamp_ns, message)
		SELECT build.sequence,
			COALESCE((SELECT MAX(log.sequence) FROM public_build_logs_v1 AS log WHERE log.build_sequence = build.sequence), 0) + 1,
			?, ?
		FROM public_builds_v1 AS build
		WHERE build.sequence = ? AND build.state = ? AND build.worker_id = ? AND build.lease_token_hash = ?
			AND build.lease_expires_at_ns > ? AND build.publication_token_hash = ''
		RETURNING sequence`,
		now.UnixNano(), sanitized, sequence, StateRunning, lease.WorkerID,
		hashLeaseToken(lease.Token), now.UnixNano()).Scan(&logSequence)
	if errors.Is(err, sql.ErrNoRows) {
		return sqliteLeaseError(ctx, transaction, sequence)
	}
	if err != nil {
		return fmt.Errorf("append Public Build log: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit Public Build log: %w", err)
	}
	return nil
}

func (coordinator *SQLiteCoordinator) Complete(ctx context.Context, lease Lease, publication Publication) (Build, error) {
	if err := ctx.Err(); err != nil {
		return Build{}, err
	}
	if err := validateCredentialFreePublication(publication, lease.Token); err != nil {
		return Build{}, reject(err.Error())
	}
	publication = clonePublication(publication)
	now := coordinator.config.Now().UTC()
	transaction, err := coordinator.database.BeginTx(ctx, nil)
	if err != nil {
		return Build{}, fmt.Errorf("begin Public Build completion: %w", err)
	}
	defer transaction.Rollback()
	sequence, err := parseBuildID(lease.Build.ID)
	if err != nil {
		return Build{}, ErrNotFound
	}
	err = transaction.QueryRowContext(ctx, `
		UPDATE public_builds_v1
		SET state = ?, finished_at_ns = ?, producer_duration_ns = ?, lease_token_hash = '', lease_expires_at_ns = 0
		WHERE sequence = ? AND state = ? AND worker_id = ? AND lease_token_hash = ?
			AND lease_expires_at_ns > ? AND publication_token_hash = ''
		RETURNING sequence`,
		StateSucceeded,
		now.UnixNano(),
		int64(publication.ProducerDuration),
		sequence,
		StateRunning,
		lease.WorkerID,
		hashLeaseToken(lease.Token),
		now.UnixNano(),
	).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return Build{}, sqliteLeaseError(ctx, transaction, sequence)
	}
	if err != nil {
		return Build{}, fmt.Errorf("complete Public Build: %w", err)
	}
	if err := insertSQLiteOutputs(ctx, transaction, sequence, publication.Outputs); err != nil {
		return Build{}, err
	}
	build, err := loadSQLiteBuild(ctx, transaction, sequence)
	if err != nil {
		return Build{}, err
	}
	if err := transaction.Commit(); err != nil {
		return Build{}, fmt.Errorf("commit Public Build completion: %w", err)
	}
	return build, nil
}

func insertSQLiteOutputs(ctx context.Context, transaction *sql.Tx, sequence int64, outputs []OutputDescriptor) error {
	for ordinal, output := range outputs {
		if _, err := transaction.ExecContext(ctx, `
			INSERT INTO public_build_outputs_v1 (
				build_sequence, ordinal, name, digest, size_bytes, media_type
			) VALUES (?, ?, ?, ?, ?, ?)`,
			sequence, ordinal, output.Name, output.Digest, output.SizeBytes, output.MediaType); err != nil {
			return fmt.Errorf("stage Public Build publication: %w", err)
		}
	}
	return nil
}

func (coordinator *SQLiteCoordinator) BeginLeasedPublication(
	ctx context.Context,
	lease Lease,
) (PublicationPermit, error) {
	if err := ctx.Err(); err != nil {
		return PublicationPermit{}, err
	}
	sequence, err := parseBuildID(lease.Build.ID)
	if err != nil {
		return PublicationPermit{}, ErrNotFound
	}
	if lease.Token == "" || lease.WorkerID == "" {
		return PublicationPermit{}, ErrPublicationLost
	}
	token, err := newLeaseToken()
	if err != nil {
		return PublicationPermit{}, err
	}
	now := coordinator.config.Now().UTC()
	transaction, err := coordinator.database.BeginTx(ctx, nil)
	if err != nil {
		return PublicationPermit{}, fmt.Errorf("begin leased Public Build publication: %w", err)
	}
	defer transaction.Rollback()
	var selected int64
	err = transaction.QueryRowContext(ctx, `
		UPDATE public_builds_v1
		SET publication_token_hash = ?, lease_expires_at_ns = ?
		WHERE sequence = ? AND state = ? AND worker_id = ? AND lease_token_hash = ?
			AND lease_expires_at_ns > ? AND publication_token_hash = ''
		RETURNING sequence`, hashLeaseToken(token), now.Add(coordinator.config.PublicationDuration).UnixNano(),
		sequence, StateRunning, lease.WorkerID, hashLeaseToken(lease.Token), now.UnixNano()).Scan(&selected)
	if errors.Is(err, sql.ErrNoRows) {
		publicationErr := sqlitePublicationError(ctx, transaction, sequence)
		if errors.Is(publicationErr, ErrNotFound) {
			return PublicationPermit{}, publicationErr
		}
		return PublicationPermit{}, ErrPublicationLost
	}
	if err != nil {
		return PublicationPermit{}, fmt.Errorf("claim leased Public Build publication: %w", err)
	}
	build, err := loadSQLiteBuild(ctx, transaction, selected)
	if err != nil {
		return PublicationPermit{}, err
	}
	if err := transaction.Commit(); err != nil {
		return PublicationPermit{}, fmt.Errorf("commit leased Public Build publication claim: %w", err)
	}
	return PublicationPermit{Token: token, Build: build}, nil
}

func (coordinator *SQLiteCoordinator) CommitPublication(
	ctx context.Context,
	permit PublicationPermit,
	publication Publication,
) (Build, error) {
	if err := ctx.Err(); err != nil {
		return Build{}, err
	}
	if err := validateCredentialFreePublication(publication, permit.Token); err != nil {
		return Build{}, reject(err.Error())
	}
	sequence, err := parseBuildID(permit.Build.ID)
	if err != nil {
		return Build{}, ErrNotFound
	}
	now := coordinator.config.Now().UTC()
	transaction, err := coordinator.database.BeginTx(ctx, nil)
	if err != nil {
		return Build{}, fmt.Errorf("begin trusted Public Build publication: %w", err)
	}
	defer transaction.Rollback()
	err = transaction.QueryRowContext(ctx, `
		UPDATE public_builds_v1
		SET state = ?, finished_at_ns = ?, producer_duration_ns = ?, lease_token_hash = '',
			lease_expires_at_ns = 0, publication_token_hash = ''
		WHERE sequence = ? AND state = ? AND publication_token_hash = ? AND lease_expires_at_ns > ?
		RETURNING sequence`, StateSucceeded, now.UnixNano(),
		int64(publication.ProducerDuration), sequence, StateRunning, hashLeaseToken(permit.Token),
		now.UnixNano()).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return Build{}, sqlitePublicationError(ctx, transaction, sequence)
	}
	if err != nil {
		return Build{}, fmt.Errorf("commit trusted Public Build publication state: %w", err)
	}
	if err := insertSQLiteOutputs(ctx, transaction, sequence, publication.Outputs); err != nil {
		return Build{}, err
	}
	build, err := loadSQLiteBuild(ctx, transaction, sequence)
	if err != nil {
		return Build{}, err
	}
	if err := transaction.Commit(); err != nil {
		return Build{}, fmt.Errorf("commit trusted Public Build publication: %w", err)
	}
	return build, nil
}

func (coordinator *SQLiteCoordinator) AbortPublication(
	ctx context.Context,
	permit PublicationPermit,
	reason string,
) (Build, error) {
	if err := ctx.Err(); err != nil {
		return Build{}, err
	}
	if strings.TrimSpace(reason) == "" {
		return Build{}, reject("publication failure reason is required")
	}
	sequence, err := parseBuildID(permit.Build.ID)
	if err != nil {
		return Build{}, ErrNotFound
	}
	now := coordinator.config.Now().UTC()
	transaction, err := coordinator.database.BeginTx(ctx, nil)
	if err != nil {
		return Build{}, fmt.Errorf("begin Public Build publication failure: %w", err)
	}
	defer transaction.Rollback()
	err = transaction.QueryRowContext(ctx, `
		UPDATE public_builds_v1
		SET state = ?, finished_at_ns = ?, failure = ?, lease_token_hash = '',
			lease_expires_at_ns = 0, publication_token_hash = ''
		WHERE sequence = ? AND state = ? AND publication_token_hash = ? AND lease_expires_at_ns > ?
		RETURNING sequence`, StateFailed, now.UnixNano(),
		sanitizePublicBuildLog(coordinator.config, reason, permit.Token), sequence, StateRunning, hashLeaseToken(permit.Token),
		now.UnixNano()).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return Build{}, sqlitePublicationError(ctx, transaction, sequence)
	}
	if err != nil {
		return Build{}, fmt.Errorf("fail Public Build publication: %w", err)
	}
	build, err := loadSQLiteBuild(ctx, transaction, sequence)
	if err != nil {
		return Build{}, err
	}
	if err := transaction.Commit(); err != nil {
		return Build{}, fmt.Errorf("commit Public Build publication failure: %w", err)
	}
	return build, nil
}

func (coordinator *SQLiteCoordinator) Fail(ctx context.Context, lease Lease, reason string) (Build, error) {
	if err := ctx.Err(); err != nil {
		return Build{}, err
	}
	if strings.TrimSpace(reason) == "" {
		return Build{}, reject("failure reason is required")
	}
	sanitized := sanitizePublicBuildLog(coordinator.config, reason, lease.Token)
	now := coordinator.config.Now().UTC()
	transaction, err := coordinator.database.BeginTx(ctx, nil)
	if err != nil {
		return Build{}, fmt.Errorf("begin Public Build failure: %w", err)
	}
	defer transaction.Rollback()
	sequence, err := parseBuildID(lease.Build.ID)
	if err != nil {
		return Build{}, ErrNotFound
	}
	err = transaction.QueryRowContext(ctx, `
		UPDATE public_builds_v1
		SET state = ?, finished_at_ns = ?, failure = ?, lease_token_hash = '', lease_expires_at_ns = 0
		WHERE sequence = ? AND state = ? AND worker_id = ? AND lease_token_hash = ?
			AND lease_expires_at_ns > ? AND publication_token_hash = ''
		RETURNING sequence`,
		StateFailed,
		now.UnixNano(),
		sanitized,
		sequence,
		StateRunning,
		lease.WorkerID,
		hashLeaseToken(lease.Token),
		now.UnixNano(),
	).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return Build{}, sqliteLeaseError(ctx, transaction, sequence)
	}
	if err != nil {
		return Build{}, fmt.Errorf("fail Public Build: %w", err)
	}
	build, err := loadSQLiteBuild(ctx, transaction, sequence)
	if err != nil {
		return Build{}, err
	}
	if err := transaction.Commit(); err != nil {
		return Build{}, fmt.Errorf("commit Public Build failure: %w", err)
	}
	return build, nil
}

func (coordinator *SQLiteCoordinator) Cancel(ctx context.Context, id string) (Build, error) {
	if err := ctx.Err(); err != nil {
		return Build{}, err
	}
	sequence, err := parseBuildID(id)
	if err != nil {
		return Build{}, ErrNotFound
	}
	transaction, err := coordinator.database.BeginTx(ctx, nil)
	if err != nil {
		return Build{}, fmt.Errorf("begin Public Build cancellation: %w", err)
	}
	defer transaction.Rollback()
	var cancelledSequence int64
	err = transaction.QueryRowContext(ctx, `
		UPDATE public_builds_v1
		SET state = ?, finished_at_ns = ?, lease_token_hash = '', lease_expires_at_ns = 0
		WHERE sequence = ? AND state IN (?, ?) AND publication_token_hash = ''
		RETURNING sequence`,
		StateCancelled, coordinator.config.Now().UTC().UnixNano(), sequence, StateQueued, StateRunning).Scan(&cancelledSequence)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Build{}, fmt.Errorf("cancel Public Build: %w", err)
	}
	build, loadErr := loadSQLiteBuild(ctx, transaction, sequence)
	if errors.Is(loadErr, sql.ErrNoRows) {
		return Build{}, ErrNotFound
	}
	if loadErr != nil {
		return Build{}, loadErr
	}
	if errors.Is(err, sql.ErrNoRows) && build.State != StateCancelled {
		return Build{}, fmt.Errorf("%w: cannot cancel build in %s state", ErrInvalidTransition, build.State)
	}
	if err := transaction.Commit(); err != nil {
		return Build{}, fmt.Errorf("commit Public Build cancellation: %w", err)
	}
	return build, nil
}

type sqliteQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const sqliteBuildSelect = `
	SELECT sequence, repository, commit_digest, integration, target, recipe_digest, platform, declared_inputs_json,
		cpu_millis, memory_bytes, disk_bytes, timeout_ns, state, worker_id,
		requested_at_ns, started_at_ns, finished_at_ns, producer_duration_ns, failure
	FROM public_builds_v1 WHERE sequence = ?`

func loadSQLiteBuild(ctx context.Context, queryer sqliteQueryer, sequence int64) (Build, error) {
	var storedSequence int64
	var integration string
	var platform string
	var encodedInputs []byte
	var timeout int64
	var state string
	var requestedAt int64
	var startedAt int64
	var finishedAt int64
	var producerDuration sql.NullInt64
	var build Build
	if err := queryer.QueryRowContext(ctx, sqliteBuildSelect, sequence).Scan(
		&storedSequence,
		&build.Request.Repository,
		&build.Request.Commit,
		&integration,
		&build.Request.Target,
		&build.Request.RecipeDigest,
		&platform,
		&encodedInputs,
		&build.Request.Resources.CPUMillis,
		&build.Request.Resources.MemoryBytes,
		&build.Request.Resources.DiskBytes,
		&timeout,
		&state,
		&build.WorkerID,
		&requestedAt,
		&startedAt,
		&finishedAt,
		&producerDuration,
		&build.Failure,
	); err != nil {
		return Build{}, err
	}
	build.ID = formatBuildID(storedSequence)
	build.Request.Integration = Integration(integration)
	build.Request.Platform = Platform(platform)
	inputs, err := decodeDeclaredInputs(encodedInputs)
	if err != nil {
		return Build{}, err
	}
	build.Request.Inputs = inputs
	build.Request.Resources.Timeout = time.Duration(timeout)
	build.State = State(state)
	build.RequestedAt = timeFromUnixNano(requestedAt)
	build.StartedAt = timeFromUnixNano(startedAt)
	build.FinishedAt = timeFromUnixNano(finishedAt)
	if build.State == StateSucceeded {
		if !producerDuration.Valid {
			return Build{}, errors.New("corrupt succeeded Public Build has no producer duration")
		}
		rows, err := queryer.QueryContext(ctx, `
			SELECT name, digest, size_bytes, media_type
			FROM public_build_outputs_v1 WHERE build_sequence = ? ORDER BY ordinal`, sequence)
		if err != nil {
			return Build{}, fmt.Errorf("load Public Build publication: %w", err)
		}
		publication := Publication{ProducerDuration: time.Duration(producerDuration.Int64)}
		for rows.Next() {
			var output OutputDescriptor
			if err := rows.Scan(&output.Name, &output.Digest, &output.SizeBytes, &output.MediaType); err != nil {
				rows.Close()
				return Build{}, fmt.Errorf("decode Public Build publication: %w", err)
			}
			publication.Outputs = append(publication.Outputs, output)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return Build{}, fmt.Errorf("load Public Build publication: %w", err)
		}
		if err := rows.Close(); err != nil {
			return Build{}, fmt.Errorf("close Public Build publication: %w", err)
		}
		if len(publication.Outputs) == 0 {
			return Build{}, errors.New("corrupt succeeded Public Build has no outputs")
		}
		build.Publication = &publication
	}
	return build, nil
}

func sqliteLeaseError(ctx context.Context, transaction *sql.Tx, sequence int64) error {
	var exists bool
	if err := transaction.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM public_builds_v1 WHERE sequence = ?)`, sequence).Scan(&exists); err != nil {
		return fmt.Errorf("check Public Build lease: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return ErrLeaseLost
}

func sqlitePublicationError(ctx context.Context, transaction *sql.Tx, sequence int64) error {
	var exists bool
	if err := transaction.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM public_builds_v1 WHERE sequence = ?)`, sequence).Scan(&exists); err != nil {
		return fmt.Errorf("check Public Build publication permit: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return ErrPublicationLost
}

func durableBuildIdentity(request BuildRequest) string {
	digest := sha256.Sum256([]byte(buildIdentity(request)))
	return hex.EncodeToString(digest[:])
}

func newLeaseToken() (string, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate Public Build lease: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(token[:]), nil
}

func hashLeaseToken(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func formatBuildID(sequence int64) string {
	return "public-build-" + strconv.FormatInt(sequence, 10)
}

func placeholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func parseBuildID(id string) (int64, error) {
	value := strings.TrimPrefix(id, "public-build-")
	if value == id || value == "" || strings.HasPrefix(value, "+") || strings.HasPrefix(value, "0") {
		return 0, ErrNotFound
	}
	sequence, err := strconv.ParseInt(value, 10, 64)
	if err != nil || sequence <= 0 || formatBuildID(sequence) != id {
		return 0, ErrNotFound
	}
	return sequence, nil
}

func timeFromUnixNano(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, value).UTC()
}

var _ Coordinator = (*SQLiteCoordinator)(nil)
