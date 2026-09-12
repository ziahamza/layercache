package publicbuild

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	postgresCoordinatorOpenTimeout = 10 * time.Second
	maximumPublicBuildProjectBytes = 256
)

// PostgresCoordinator persists the Public Build control plane for one project.
// Every mutable transition is fenced in PostgreSQL, so independent server
// processes may safely share a queue.
type PostgresCoordinator struct {
	database    *sql.DB
	postgresURL string
	project     string
	config      Config
	allowlist   map[string]struct{}
}

// OpenPostgresCoordinator opens the project-scoped cloud Public Build queue.
func OpenPostgresCoordinator(
	ctx context.Context,
	postgresURL string,
	project string,
	config Config,
) (*PostgresCoordinator, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	parsed, err := url.Parse(postgresURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		return nil, errors.New("Public Build PostgreSQL URL is invalid")
	}
	if project == "" || strings.TrimSpace(project) != project || len(project) > maximumPublicBuildProjectBytes ||
		strings.IndexFunc(project, unicode.IsControl) >= 0 {
		return nil, errors.New("Public Build PostgreSQL project must be non-empty, trimmed, and at most 256 bytes")
	}
	allowlist, err := prepareConfig(&config)
	if err != nil {
		return nil, err
	}
	database, err := sql.Open("pgx", postgresURL)
	if err != nil {
		return nil, redactPostgresCoordinatorError("open Public Build PostgreSQL", postgresURL, err)
	}
	database.SetMaxOpenConns(8)
	database.SetMaxIdleConns(2)
	database.SetConnMaxIdleTime(5 * time.Minute)
	database.SetConnMaxLifetime(time.Hour)
	coordinator := &PostgresCoordinator{
		database: database, postgresURL: postgresURL, project: project,
		config: config, allowlist: allowlist,
	}
	openContext, cancel := context.WithTimeout(ctx, postgresCoordinatorOpenTimeout)
	defer cancel()
	if err := database.PingContext(openContext); err != nil {
		_ = database.Close()
		return nil, coordinator.safeError("connect Public Build PostgreSQL", err)
	}
	if err := coordinator.initialize(openContext); err != nil {
		_ = database.Close()
		return nil, err
	}
	return coordinator, nil
}

func (coordinator *PostgresCoordinator) initialize(ctx context.Context) error {
	transaction, err := coordinator.database.BeginTx(ctx, nil)
	if err != nil {
		return coordinator.safeError("begin Public Build PostgreSQL migration", err)
	}
	defer transaction.Rollback()
	if _, err := transaction.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('layercache-public-build-schema-v1', 0))`); err != nil {
		return coordinator.safeError("lock Public Build PostgreSQL migration", err)
	}
	statements := []string{
		`SET LOCAL lock_timeout = '5s'`,
		`SET LOCAL statement_timeout = '10s'`,
		`CREATE TABLE IF NOT EXISTS layercache_public_builds_v1 (
			project_id TEXT NOT NULL CHECK (octet_length(project_id) BETWEEN 1 AND 256),
			sequence BIGSERIAL NOT NULL,
			identity TEXT NOT NULL CHECK (identity ~ '^[0-9a-f]{64}$'),
			repository TEXT NOT NULL,
			commit_digest TEXT NOT NULL,
			integration TEXT NOT NULL CHECK (integration IN ('turbo', 'buildkit', 'actions')),
			target TEXT NOT NULL,
			recipe_digest TEXT NOT NULL,
			platform TEXT NOT NULL CHECK (platform IN ('linux/amd64', 'linux/arm64')),
			declared_inputs_json JSONB NOT NULL DEFAULT '[]'::jsonb,
			cpu_millis BIGINT NOT NULL CHECK (cpu_millis > 0),
			memory_bytes BIGINT NOT NULL CHECK (memory_bytes > 0),
			disk_bytes BIGINT NOT NULL CHECK (disk_bytes > 0),
			timeout_ns BIGINT NOT NULL CHECK (timeout_ns > 0),
			state TEXT NOT NULL CHECK (state IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
			worker_id TEXT NOT NULL DEFAULT '',
			lease_token_hash TEXT NOT NULL DEFAULT '',
			lease_expires_at_ns BIGINT NOT NULL DEFAULT 0,
			publication_token_hash TEXT NOT NULL DEFAULT '',
			requested_at_ns BIGINT NOT NULL,
			started_at_ns BIGINT NOT NULL DEFAULT 0,
			finished_at_ns BIGINT NOT NULL DEFAULT 0,
			producer_duration_ns BIGINT,
			failure TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (project_id, sequence),
			CHECK (lease_token_hash = '' OR lease_token_hash ~ '^[0-9a-f]{64}$'),
			CHECK (publication_token_hash = '' OR publication_token_hash ~ '^[0-9a-f]{64}$'),
			CHECK (producer_duration_ns IS NULL OR producer_duration_ns >= 0)
		)`,
		`ALTER TABLE layercache_public_builds_v1
			ADD COLUMN IF NOT EXISTS declared_inputs_json JSONB NOT NULL DEFAULT '[]'::jsonb`,
		`CREATE UNIQUE INDEX IF NOT EXISTS layercache_public_builds_active_identity_v1
			ON layercache_public_builds_v1(project_id, identity)
			WHERE state IN ('queued', 'running', 'succeeded')`,
		`CREATE INDEX IF NOT EXISTS layercache_public_builds_fifo_v1
			ON layercache_public_builds_v1(project_id, state, sequence)`,
		`CREATE INDEX IF NOT EXISTS layercache_public_builds_expiry_v1
			ON layercache_public_builds_v1(project_id, lease_expires_at_ns)
			WHERE state = 'running'`,
		`CREATE TABLE IF NOT EXISTS layercache_public_build_outputs_v1 (
			project_id TEXT NOT NULL,
			build_sequence BIGINT NOT NULL,
			ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
			name TEXT NOT NULL,
			digest TEXT NOT NULL,
			size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
			media_type TEXT NOT NULL,
			PRIMARY KEY (project_id, build_sequence, ordinal),
			UNIQUE (project_id, build_sequence, name),
			FOREIGN KEY (project_id, build_sequence)
				REFERENCES layercache_public_builds_v1(project_id, sequence) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS layercache_public_build_logs_v1 (
			project_id TEXT NOT NULL,
			build_sequence BIGINT NOT NULL,
			sequence BIGINT NOT NULL CHECK (sequence > 0),
			timestamp_ns BIGINT NOT NULL,
			message TEXT NOT NULL,
			PRIMARY KEY (project_id, build_sequence, sequence),
			FOREIGN KEY (project_id, build_sequence)
				REFERENCES layercache_public_builds_v1(project_id, sequence) ON DELETE CASCADE
		)`,
	}
	for _, statement := range statements {
		if _, err := transaction.ExecContext(ctx, statement); err != nil {
			return coordinator.safeError("migrate Public Build PostgreSQL", err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return coordinator.safeError("commit Public Build PostgreSQL migration", err)
	}
	return nil
}

// Close releases the PostgreSQL connection pool.
func (coordinator *PostgresCoordinator) Close() error {
	if coordinator == nil || coordinator.database == nil {
		return nil
	}
	if err := coordinator.database.Close(); err != nil {
		return coordinator.safeError("close Public Build PostgreSQL", err)
	}
	return nil
}

func (coordinator *PostgresCoordinator) Request(ctx context.Context, request BuildRequest) (RequestResult, error) {
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
		return RequestResult{}, coordinator.safeError("begin Public Build request", err)
	}
	defer transaction.Rollback()
	// The partial identity index prevents duplicates. The advisory transaction
	// lock also makes the insert-or-reuse decision stable when a failed or
	// cancelled attempt is concurrently being retried.
	if _, err := transaction.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		fmt.Sprintf("%d:%s:%s", len(coordinator.project), coordinator.project, identity)); err != nil {
		return RequestResult{}, coordinator.safeError("lock Public Build identity", err)
	}
	if publicationMissing {
		if _, err := transaction.ExecContext(ctx, `
			UPDATE layercache_public_builds_v1
			SET state = $1, publication_token_hash = '', failure = $2
			WHERE project_id = $3 AND identity = $4 AND state = $5
				AND ($6 OR finished_at_ns <= $7)`,
			StateFailed, "Public Cache publication is unavailable", coordinator.project, identity,
			StateSucceeded, publicationUnavailable, now.Add(-publicationRegistrationGrace).UnixNano(),
		); err != nil {
			return RequestResult{}, coordinator.safeError("retire unavailable Public Build publication", err)
		}
	}
	var sequence int64
	err = transaction.QueryRowContext(ctx, `
		SELECT sequence FROM layercache_public_builds_v1
		WHERE project_id = $1 AND identity = $2 AND state IN ($3, $4, $5)
		ORDER BY sequence LIMIT 1 FOR UPDATE`,
		coordinator.project, identity, StateQueued, StateRunning, StateSucceeded).Scan(&sequence)
	reused := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return RequestResult{}, coordinator.safeError("load reused Public Build identity", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		err = transaction.QueryRowContext(ctx, `
			INSERT INTO layercache_public_builds_v1 (
				project_id, identity, repository, commit_digest, integration, target, recipe_digest, platform, declared_inputs_json,
				cpu_millis, memory_bytes, disk_bytes, timeout_ns, state, requested_at_ns
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $12, $13, $14, $15)
			RETURNING sequence`,
			coordinator.project, identity, normalized.Repository, normalized.Commit, normalized.Integration,
			normalized.Target, normalized.RecipeDigest, normalized.Platform, string(encodedInputs), normalized.Resources.CPUMillis,
			normalized.Resources.MemoryBytes, normalized.Resources.DiskBytes, int64(normalized.Resources.Timeout),
			StateQueued, now.UnixNano()).Scan(&sequence)
		if err != nil {
			return RequestResult{}, coordinator.safeError("queue Public Build", err)
		}
	}
	build, err := coordinator.loadBuild(ctx, transaction, sequence)
	if err != nil {
		return RequestResult{}, err
	}
	if publicationMissing && build.State == StateSucceeded {
		return RequestResult{}, ErrPublicationPending
	}
	if err := transaction.Commit(); err != nil {
		return RequestResult{}, coordinator.safeError("commit Public Build request", err)
	}
	return RequestResult{Build: build, Reused: reused}, nil
}

func (coordinator *PostgresCoordinator) Inspect(ctx context.Context, id string) (Build, error) {
	if err := ctx.Err(); err != nil {
		return Build{}, err
	}
	sequence, err := parseBuildID(id)
	if err != nil {
		return Build{}, ErrNotFound
	}
	build, err := coordinator.loadBuild(ctx, coordinator.database, sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return Build{}, ErrNotFound
	}
	return build, err
}

func (coordinator *PostgresCoordinator) Logs(ctx context.Context, id string) ([]LogEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sequence, err := parseBuildID(id)
	if err != nil {
		return nil, ErrNotFound
	}
	exists, err := coordinator.buildExists(ctx, coordinator.database, sequence)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := coordinator.database.QueryContext(ctx, `
		SELECT sequence, timestamp_ns, message
		FROM layercache_public_build_logs_v1
		WHERE project_id = $1 AND build_sequence = $2 ORDER BY sequence`, coordinator.project, sequence)
	if err != nil {
		return nil, coordinator.safeError("load Public Build logs", err)
	}
	defer rows.Close()
	var logs []LogEntry
	for rows.Next() {
		var storedSequence int64
		var timestamp int64
		var entry LogEntry
		if err := rows.Scan(&storedSequence, &timestamp, &entry.Message); err != nil {
			return nil, coordinator.safeError("decode Public Build log", err)
		}
		entry.Sequence = uint64(storedSequence)
		entry.Timestamp = timeFromUnixNano(timestamp)
		logs = append(logs, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, coordinator.safeError("load Public Build logs", err)
	}
	return logs, nil
}

func (coordinator *PostgresCoordinator) LeaseNext(ctx context.Context, worker Worker) (Lease, error) {
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
	if len(workerID) > 128 || strings.IndexFunc(workerID, unicode.IsControl) >= 0 {
		return Lease{}, reject("worker ID must be at most 128 bytes and contain no control characters")
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
		return Lease{}, coordinator.safeError("begin Public Build lease", err)
	}
	defer transaction.Rollback()
	if err := coordinator.requeueExpired(ctx, transaction, now); err != nil {
		return Lease{}, err
	}
	arguments := []any{coordinator.project, StateQueued}
	addArgument := func(value any) string {
		arguments = append(arguments, value)
		return fmt.Sprintf("$%d", len(arguments))
	}
	var capabilityPredicate string
	if len(capabilities.Recipes) > 0 {
		predicates := make([]string, 0, len(capabilities.Recipes))
		for _, recipe := range capabilities.Recipes {
			integration := addArgument(recipe.Integration)
			if recipe.Target == "*" {
				predicates = append(predicates, "(integration = "+integration+" AND recipe_digest = "+addArgument(recipe.RecipeDigest)+")")
				continue
			}
			predicates = append(predicates, "(integration = "+integration+" AND target = "+addArgument(recipe.Target)+
				" AND recipe_digest = "+addArgument(recipe.RecipeDigest)+")")
		}
		capabilityPredicate = "(" + strings.Join(predicates, " OR ") + ")"
	} else {
		placeholders := make([]string, 0, len(capabilities.Integrations))
		for _, integration := range capabilities.Integrations {
			placeholders = append(placeholders, addArgument(integration))
		}
		capabilityPredicate = "integration IN (" + strings.Join(placeholders, ",") + ")"
	}
	platforms := make([]string, 0, len(capabilities.Platforms))
	for _, platform := range capabilities.Platforms {
		platforms = append(platforms, addArgument(platform))
	}
	workerArgument := addArgument(workerID)
	tokenArgument := addArgument(hashLeaseToken(token))
	startedArgument := addArgument(now.UnixNano())
	expiryArgument := addArgument(expiresAt.UnixNano())
	claim := fmt.Sprintf(`
		WITH candidate AS (
			SELECT sequence FROM layercache_public_builds_v1
			WHERE project_id = $1 AND state = $2 AND %s AND platform IN (%s)
			ORDER BY sequence FOR UPDATE SKIP LOCKED LIMIT 1
		)
		UPDATE layercache_public_builds_v1 AS build
		SET state = 'running', worker_id = %s, lease_token_hash = %s,
			started_at_ns = %s, lease_expires_at_ns = %s
		FROM candidate
		WHERE build.project_id = $1 AND build.sequence = candidate.sequence AND build.state = 'queued'
		RETURNING build.sequence`, capabilityPredicate, strings.Join(platforms, ","),
		workerArgument, tokenArgument, startedArgument, expiryArgument)
	var selected int64
	if err := transaction.QueryRowContext(ctx, claim, arguments...).Scan(&selected); errors.Is(err, sql.ErrNoRows) {
		return Lease{}, ErrNoWork
	} else if err != nil {
		return Lease{}, coordinator.safeError("claim queued Public Build", err)
	}
	build, err := coordinator.loadBuild(ctx, transaction, selected)
	if err != nil {
		return Lease{}, err
	}
	if err := transaction.Commit(); err != nil {
		return Lease{}, coordinator.safeError("commit Public Build lease", err)
	}
	return Lease{Token: token, WorkerID: workerID, Build: build, LeasedAt: now, ExpiresAt: expiresAt}, nil
}

func (coordinator *PostgresCoordinator) requeueExpired(ctx context.Context, transaction *sql.Tx, now time.Time) error {
	if _, err := transaction.ExecContext(ctx, `
		UPDATE layercache_public_builds_v1
		SET state = $1, worker_id = '', lease_token_hash = '', lease_expires_at_ns = 0,
			publication_token_hash = '', started_at_ns = 0
		WHERE project_id = $2 AND state = $3 AND lease_expires_at_ns <= $4`,
		StateQueued, coordinator.project, StateRunning, now.UnixNano()); err != nil {
		return coordinator.safeError("recover expired Public Build leases", err)
	}
	return nil
}

func (coordinator *PostgresCoordinator) Renew(ctx context.Context, lease Lease) (Lease, error) {
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
		return Lease{}, coordinator.safeError("begin Public Build lease renewal", err)
	}
	defer transaction.Rollback()
	var renewed int64
	err = transaction.QueryRowContext(ctx, `
		UPDATE layercache_public_builds_v1 SET lease_expires_at_ns = $1
		WHERE project_id = $2 AND sequence = $3 AND state = $4 AND worker_id = $5
			AND lease_token_hash = $6 AND lease_expires_at_ns > $7 AND publication_token_hash = ''
		RETURNING sequence`, expiresAt.UnixNano(), coordinator.project, sequence, StateRunning,
		lease.WorkerID, hashLeaseToken(lease.Token), now.UnixNano()).Scan(&renewed)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, coordinator.leaseError(ctx, transaction, sequence)
	}
	if err != nil {
		return Lease{}, coordinator.safeError("renew Public Build lease", err)
	}
	build, err := coordinator.loadBuild(ctx, transaction, renewed)
	if err != nil {
		return Lease{}, err
	}
	if err := transaction.Commit(); err != nil {
		return Lease{}, coordinator.safeError("commit Public Build lease renewal", err)
	}
	return Lease{Token: lease.Token, WorkerID: lease.WorkerID, Build: build, LeasedAt: now, ExpiresAt: expiresAt}, nil
}

func (coordinator *PostgresCoordinator) AppendLog(ctx context.Context, lease Lease, message string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sequence, err := parseBuildID(lease.Build.ID)
	if err != nil {
		return ErrNotFound
	}
	now := coordinator.config.Now().UTC()
	transaction, err := coordinator.database.BeginTx(ctx, nil)
	if err != nil {
		return coordinator.safeError("begin Public Build log append", err)
	}
	defer transaction.Rollback()
	// Locking the build row serializes the per-build sequence calculation across
	// all coordinator instances without introducing a separate counter record.
	var locked int64
	err = transaction.QueryRowContext(ctx, `
		SELECT sequence FROM layercache_public_builds_v1
		WHERE project_id = $1 AND sequence = $2 AND state = $3 AND worker_id = $4
			AND lease_token_hash = $5 AND lease_expires_at_ns > $6 AND publication_token_hash = ''
		FOR UPDATE`, coordinator.project, sequence, StateRunning, lease.WorkerID,
		hashLeaseToken(lease.Token), now.UnixNano()).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return coordinator.leaseError(ctx, transaction, sequence)
	}
	if err != nil {
		return coordinator.safeError("lock Public Build log sequence", err)
	}
	var logSequence int64
	err = transaction.QueryRowContext(ctx, `
		INSERT INTO layercache_public_build_logs_v1 (
			project_id, build_sequence, sequence, timestamp_ns, message
		) VALUES (
			$1, $2,
			COALESCE((SELECT MAX(sequence) FROM layercache_public_build_logs_v1
				WHERE project_id = $1 AND build_sequence = $2), 0) + 1,
			$3, $4
		) RETURNING sequence`, coordinator.project, sequence, now.UnixNano(),
		sanitizePublicBuildLog(coordinator.config, message, lease.Token)).Scan(&logSequence)
	if err != nil {
		return coordinator.safeError("append Public Build log", err)
	}
	if err := transaction.Commit(); err != nil {
		return coordinator.safeError("commit Public Build log", err)
	}
	return nil
}

func (coordinator *PostgresCoordinator) Complete(
	ctx context.Context,
	lease Lease,
	publication Publication,
) (Build, error) {
	if err := ctx.Err(); err != nil {
		return Build{}, err
	}
	if err := validateCredentialFreePublication(publication, lease.Token); err != nil {
		return Build{}, reject(err.Error())
	}
	sequence, err := parseBuildID(lease.Build.ID)
	if err != nil {
		return Build{}, ErrNotFound
	}
	now := coordinator.config.Now().UTC()
	transaction, err := coordinator.database.BeginTx(ctx, nil)
	if err != nil {
		return Build{}, coordinator.safeError("begin Public Build completion", err)
	}
	defer transaction.Rollback()
	err = transaction.QueryRowContext(ctx, `
		UPDATE layercache_public_builds_v1
		SET state = $1, finished_at_ns = $2, producer_duration_ns = $3,
			lease_token_hash = '', lease_expires_at_ns = 0
		WHERE project_id = $4 AND sequence = $5 AND state = $6 AND worker_id = $7
			AND lease_token_hash = $8 AND lease_expires_at_ns > $9 AND publication_token_hash = ''
		RETURNING sequence`, StateSucceeded, now.UnixNano(), int64(publication.ProducerDuration),
		coordinator.project, sequence, StateRunning, lease.WorkerID, hashLeaseToken(lease.Token),
		now.UnixNano()).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return Build{}, coordinator.leaseError(ctx, transaction, sequence)
	}
	if err != nil {
		return Build{}, coordinator.safeError("complete Public Build", err)
	}
	if err := coordinator.insertOutputs(ctx, transaction, sequence, publication.Outputs); err != nil {
		return Build{}, err
	}
	build, err := coordinator.loadBuild(ctx, transaction, sequence)
	if err != nil {
		return Build{}, err
	}
	if err := transaction.Commit(); err != nil {
		return Build{}, coordinator.safeError("commit Public Build completion", err)
	}
	return build, nil
}

func (coordinator *PostgresCoordinator) insertOutputs(
	ctx context.Context,
	transaction *sql.Tx,
	sequence int64,
	outputs []OutputDescriptor,
) error {
	for ordinal, output := range outputs {
		if _, err := transaction.ExecContext(ctx, `
			INSERT INTO layercache_public_build_outputs_v1 (
				project_id, build_sequence, ordinal, name, digest, size_bytes, media_type
			) VALUES ($1, $2, $3, $4, $5, $6, $7)`, coordinator.project, sequence, ordinal,
			output.Name, output.Digest, output.SizeBytes, output.MediaType); err != nil {
			return coordinator.safeError("stage Public Build publication", err)
		}
	}
	return nil
}

func (coordinator *PostgresCoordinator) BeginLeasedPublication(
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
		return PublicationPermit{}, coordinator.safeError("begin leased Public Build publication", err)
	}
	defer transaction.Rollback()
	var selected int64
	err = transaction.QueryRowContext(ctx, `
		UPDATE layercache_public_builds_v1
		SET publication_token_hash = $1, lease_expires_at_ns = $2
		WHERE project_id = $3 AND sequence = $4 AND state = $5 AND worker_id = $6
			AND lease_token_hash = $7 AND lease_expires_at_ns > $8 AND publication_token_hash = ''
		RETURNING sequence`, hashLeaseToken(token), now.Add(coordinator.config.PublicationDuration).UnixNano(),
		coordinator.project, sequence, StateRunning, lease.WorkerID, hashLeaseToken(lease.Token),
		now.UnixNano()).Scan(&selected)
	if errors.Is(err, sql.ErrNoRows) {
		publicationErr := coordinator.publicationError(ctx, transaction, sequence)
		if errors.Is(publicationErr, ErrNotFound) {
			return PublicationPermit{}, publicationErr
		}
		return PublicationPermit{}, ErrPublicationLost
	}
	if err != nil {
		return PublicationPermit{}, coordinator.safeError("claim leased Public Build publication", err)
	}
	build, err := coordinator.loadBuild(ctx, transaction, selected)
	if err != nil {
		return PublicationPermit{}, err
	}
	if err := transaction.Commit(); err != nil {
		return PublicationPermit{}, coordinator.safeError("commit leased Public Build publication claim", err)
	}
	return PublicationPermit{Token: token, Build: build}, nil
}

func (coordinator *PostgresCoordinator) CommitPublication(
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
		return Build{}, coordinator.safeError("begin trusted Public Build publication", err)
	}
	defer transaction.Rollback()
	err = transaction.QueryRowContext(ctx, `
		UPDATE layercache_public_builds_v1
		SET state = $1, finished_at_ns = $2, producer_duration_ns = $3,
			lease_token_hash = '', lease_expires_at_ns = 0, publication_token_hash = ''
		WHERE project_id = $4 AND sequence = $5 AND state = $6
			AND publication_token_hash = $7 AND lease_expires_at_ns > $8
		RETURNING sequence`, StateSucceeded, now.UnixNano(), int64(publication.ProducerDuration),
		coordinator.project, sequence, StateRunning, hashLeaseToken(permit.Token), now.UnixNano()).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return Build{}, coordinator.publicationError(ctx, transaction, sequence)
	}
	if err != nil {
		return Build{}, coordinator.safeError("commit trusted Public Build publication state", err)
	}
	if err := coordinator.insertOutputs(ctx, transaction, sequence, publication.Outputs); err != nil {
		return Build{}, err
	}
	build, err := coordinator.loadBuild(ctx, transaction, sequence)
	if err != nil {
		return Build{}, err
	}
	if err := transaction.Commit(); err != nil {
		return Build{}, coordinator.safeError("commit trusted Public Build publication", err)
	}
	return build, nil
}

func (coordinator *PostgresCoordinator) AbortPublication(
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
		return Build{}, coordinator.safeError("begin Public Build publication failure", err)
	}
	defer transaction.Rollback()
	err = transaction.QueryRowContext(ctx, `
		UPDATE layercache_public_builds_v1
		SET state = $1, finished_at_ns = $2, failure = $3,
			lease_token_hash = '', lease_expires_at_ns = 0, publication_token_hash = ''
		WHERE project_id = $4 AND sequence = $5 AND state = $6
			AND publication_token_hash = $7 AND lease_expires_at_ns > $8
		RETURNING sequence`, StateFailed, now.UnixNano(), sanitizePublicBuildLog(coordinator.config, reason, permit.Token),
		coordinator.project, sequence, StateRunning, hashLeaseToken(permit.Token), now.UnixNano()).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return Build{}, coordinator.publicationError(ctx, transaction, sequence)
	}
	if err != nil {
		return Build{}, coordinator.safeError("fail Public Build publication", err)
	}
	build, err := coordinator.loadBuild(ctx, transaction, sequence)
	if err != nil {
		return Build{}, err
	}
	if err := transaction.Commit(); err != nil {
		return Build{}, coordinator.safeError("commit Public Build publication failure", err)
	}
	return build, nil
}

func (coordinator *PostgresCoordinator) Fail(ctx context.Context, lease Lease, reason string) (Build, error) {
	if err := ctx.Err(); err != nil {
		return Build{}, err
	}
	if strings.TrimSpace(reason) == "" {
		return Build{}, reject("failure reason is required")
	}
	sequence, err := parseBuildID(lease.Build.ID)
	if err != nil {
		return Build{}, ErrNotFound
	}
	now := coordinator.config.Now().UTC()
	transaction, err := coordinator.database.BeginTx(ctx, nil)
	if err != nil {
		return Build{}, coordinator.safeError("begin Public Build failure", err)
	}
	defer transaction.Rollback()
	err = transaction.QueryRowContext(ctx, `
		UPDATE layercache_public_builds_v1
		SET state = $1, finished_at_ns = $2, failure = $3,
			lease_token_hash = '', lease_expires_at_ns = 0
		WHERE project_id = $4 AND sequence = $5 AND state = $6 AND worker_id = $7
			AND lease_token_hash = $8 AND lease_expires_at_ns > $9 AND publication_token_hash = ''
		RETURNING sequence`, StateFailed, now.UnixNano(), sanitizePublicBuildLog(coordinator.config, reason, lease.Token),
		coordinator.project, sequence, StateRunning, lease.WorkerID, hashLeaseToken(lease.Token),
		now.UnixNano()).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return Build{}, coordinator.leaseError(ctx, transaction, sequence)
	}
	if err != nil {
		return Build{}, coordinator.safeError("fail Public Build", err)
	}
	build, err := coordinator.loadBuild(ctx, transaction, sequence)
	if err != nil {
		return Build{}, err
	}
	if err := transaction.Commit(); err != nil {
		return Build{}, coordinator.safeError("commit Public Build failure", err)
	}
	return build, nil
}

func (coordinator *PostgresCoordinator) Cancel(ctx context.Context, id string) (Build, error) {
	if err := ctx.Err(); err != nil {
		return Build{}, err
	}
	sequence, err := parseBuildID(id)
	if err != nil {
		return Build{}, ErrNotFound
	}
	transaction, err := coordinator.database.BeginTx(ctx, nil)
	if err != nil {
		return Build{}, coordinator.safeError("begin Public Build cancellation", err)
	}
	defer transaction.Rollback()
	var cancelledSequence int64
	err = transaction.QueryRowContext(ctx, `
		UPDATE layercache_public_builds_v1
		SET state = $1, finished_at_ns = $2, lease_token_hash = '', lease_expires_at_ns = 0
		WHERE project_id = $3 AND sequence = $4 AND state IN ($5, $6) AND publication_token_hash = ''
		RETURNING sequence`, StateCancelled, coordinator.config.Now().UTC().UnixNano(), coordinator.project,
		sequence, StateQueued, StateRunning).Scan(&cancelledSequence)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Build{}, coordinator.safeError("cancel Public Build", err)
	}
	build, loadErr := coordinator.loadBuild(ctx, transaction, sequence)
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
		return Build{}, coordinator.safeError("commit Public Build cancellation", err)
	}
	return build, nil
}

type postgresBuildQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const postgresBuildSelect = `
	SELECT sequence, repository, commit_digest, integration, target, recipe_digest, platform, declared_inputs_json,
		cpu_millis, memory_bytes, disk_bytes, timeout_ns, state, worker_id,
		requested_at_ns, started_at_ns, finished_at_ns, producer_duration_ns, failure
	FROM layercache_public_builds_v1 WHERE project_id = $1 AND sequence = $2`

func (coordinator *PostgresCoordinator) loadBuild(
	ctx context.Context,
	queryer postgresBuildQueryer,
	sequence int64,
) (Build, error) {
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
	if err := queryer.QueryRowContext(ctx, postgresBuildSelect, coordinator.project, sequence).Scan(
		&storedSequence, &build.Request.Repository, &build.Request.Commit, &integration,
		&build.Request.Target, &build.Request.RecipeDigest, &platform,
		&encodedInputs,
		&build.Request.Resources.CPUMillis, &build.Request.Resources.MemoryBytes,
		&build.Request.Resources.DiskBytes, &timeout, &state, &build.WorkerID,
		&requestedAt, &startedAt, &finishedAt, &producerDuration, &build.Failure,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Build{}, err
		}
		return Build{}, coordinator.safeError("load Public Build", err)
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
	if build.State != StateSucceeded {
		return build, nil
	}
	if !producerDuration.Valid {
		return Build{}, errors.New("corrupt succeeded Public Build has no producer duration")
	}
	rows, err := queryer.QueryContext(ctx, `
		SELECT name, digest, size_bytes, media_type
		FROM layercache_public_build_outputs_v1
		WHERE project_id = $1 AND build_sequence = $2 ORDER BY ordinal`, coordinator.project, sequence)
	if err != nil {
		return Build{}, coordinator.safeError("load Public Build publication", err)
	}
	publication := Publication{ProducerDuration: time.Duration(producerDuration.Int64)}
	for rows.Next() {
		var output OutputDescriptor
		if err := rows.Scan(&output.Name, &output.Digest, &output.SizeBytes, &output.MediaType); err != nil {
			_ = rows.Close()
			return Build{}, coordinator.safeError("decode Public Build publication", err)
		}
		publication.Outputs = append(publication.Outputs, output)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return Build{}, coordinator.safeError("load Public Build publication", err)
	}
	if err := rows.Close(); err != nil {
		return Build{}, coordinator.safeError("close Public Build publication", err)
	}
	if len(publication.Outputs) == 0 {
		return Build{}, errors.New("corrupt succeeded Public Build has no outputs")
	}
	build.Publication = &publication
	return build, nil
}

// Status returns a bounded operational queue summary for this project.
func (coordinator *PostgresCoordinator) Status(ctx context.Context) (StatusSummary, error) {
	if err := ctx.Err(); err != nil {
		return StatusSummary{}, err
	}
	var result StatusSummary
	var sequence int64
	var requestedAt int64
	err := coordinator.database.QueryRowContext(ctx, `
		WITH scoped AS MATERIALIZED (
			SELECT sequence, state, requested_at_ns
			FROM layercache_public_builds_v1 WHERE project_id = $1
		), latest AS (
			SELECT sequence, state, requested_at_ns FROM scoped ORDER BY sequence DESC LIMIT 1
		)
		SELECT
			COUNT(*) FILTER (WHERE state = 'queued'),
			COUNT(*) FILTER (WHERE state = 'running'),
			COUNT(*) FILTER (WHERE state = 'succeeded'),
			COUNT(*) FILTER (WHERE state = 'failed'),
			COUNT(*) FILTER (WHERE state = 'cancelled'),
			COALESCE((SELECT sequence FROM latest), 0),
			COALESCE((SELECT state FROM latest), ''),
			COALESCE((SELECT requested_at_ns FROM latest), 0)
		FROM scoped`, coordinator.project).Scan(
		&result.Queued, &result.Running, &result.Succeeded, &result.Failed, &result.Cancelled,
		&sequence, &result.LatestState, &requestedAt,
	)
	if err != nil {
		return StatusSummary{}, coordinator.safeError("read Public Build state counts", err)
	}
	if sequence == 0 {
		return result, nil
	}
	result.LatestBuildID = formatBuildID(sequence)
	result.LatestRequestedAt = timeFromUnixNano(requestedAt)
	return result, nil
}

func (coordinator *PostgresCoordinator) buildExists(
	ctx context.Context,
	queryer postgresBuildQueryer,
	sequence int64,
) (bool, error) {
	var exists bool
	if err := queryer.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM layercache_public_builds_v1 WHERE project_id = $1 AND sequence = $2
		)`, coordinator.project, sequence).Scan(&exists); err != nil {
		return false, coordinator.safeError("find Public Build", err)
	}
	return exists, nil
}

func (coordinator *PostgresCoordinator) leaseError(
	ctx context.Context,
	transaction *sql.Tx,
	sequence int64,
) error {
	exists, err := coordinator.buildExists(ctx, transaction, sequence)
	if err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	return ErrLeaseLost
}

func (coordinator *PostgresCoordinator) publicationError(
	ctx context.Context,
	transaction *sql.Tx,
	sequence int64,
) error {
	exists, err := coordinator.buildExists(ctx, transaction, sequence)
	if err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	return ErrPublicationLost
}

func (coordinator *PostgresCoordinator) safeError(operation string, err error) error {
	return redactPostgresCoordinatorError(operation, coordinator.postgresURL, err)
}

func redactPostgresCoordinatorError(operation, postgresURL string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", operation, err)
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
		}
	}
	for _, secret := range []string{
		os.Getenv("PGPASSWORD"), os.Getenv("PGPASSFILE"), os.Getenv("PGSERVICEFILE"),
	} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
			message = strings.ReplaceAll(message, url.QueryEscape(secret), "[redacted]")
		}
	}
	return fmt.Errorf("%s: %s", operation, message)
}

var _ Coordinator = (*PostgresCoordinator)(nil)
var _ StatusReader = (*PostgresCoordinator)(nil)
