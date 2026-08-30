// Package uploadqueue persists Team Cache publication work independently from
// the Local Cache artifact lifecycle. The queue records references to complete
// artifacts; it never owns or deletes their bytes.
package uploadqueue

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

var (
	ErrNoDueJob  = errors.New("no Team Cache upload is due")
	ErrNotFound  = errors.New("Team Cache upload job not found")
	ErrQueueFull = errors.New("Team Cache upload queue byte quota exceeded")
	ErrConflict  = errors.New("Team Cache upload identity already has different artifact metadata")
	ErrLeaseLost = errors.New("Team Cache upload lease is no longer current")
)

type Adapter string

const (
	AdapterTurbo    Adapter = "turbo"
	AdapterActions  Adapter = "actions"
	AdapterBuildKit Adapter = "buildkit"
)

type Scope string

const ScopeTeam Scope = "team"

type State string

const (
	StatePending   State = "pending"
	StateLeased    State = "leased"
	StateCompleted State = "completed"
)

type Outcome string

const OutcomeUploaded Outcome = "uploaded"

type Target struct {
	Scope    Scope
	Endpoint string
	Project  string
}

type EnqueueRequest struct {
	Adapter  Adapter
	Target   Target
	Identity string
	Digest   string
	Size     int64
}

type Job struct {
	ID              string
	Adapter         Adapter
	Target          Target
	Identity        string
	Digest          string
	Size            int64
	State           State
	Attempts        int
	LastError       string
	NextAttemptAt   time.Time
	LeaseGeneration int64
	LeaseExpiresAt  *time.Time
	Outcome         Outcome
	CreatedAt       time.Time
	UpdatedAt       time.Time
	CompletedAt     *time.Time
}

type EnqueueResult struct {
	Job     Job
	Created bool
}

type Lease struct {
	Job        Job
	Generation int64
	ExpiresAt  time.Time
}

type Stats struct {
	MaxQueuedBytes int64
	QueuedBytes    int64
	QueuedJobs     int64
	PendingJobs    int64
	LeasedJobs     int64
	DueJobs        int64
	CompletedJobs  int64
}

type Config struct {
	Path           string
	MaxQueuedBytes int64
	LeaseDuration  time.Duration
	BaseBackoff    time.Duration
	MaxBackoff     time.Duration
	Now            func() time.Time
}

type Queue struct {
	database       *sql.DB
	maxQueuedBytes int64
	leaseDuration  time.Duration
	baseBackoff    time.Duration
	maxBackoff     time.Duration
	now            func() time.Time
}

const (
	defaultLeaseDuration = time.Minute
	defaultBaseBackoff   = time.Second
	defaultMaxBackoff    = time.Hour
)

func Open(config Config) (*Queue, error) {
	if strings.TrimSpace(config.Path) == "" {
		return nil, errors.New("upload queue database path is required")
	}
	if config.MaxQueuedBytes <= 0 {
		return nil, errors.New("upload queue byte quota must be positive")
	}
	if config.LeaseDuration == 0 {
		config.LeaseDuration = defaultLeaseDuration
	}
	if config.BaseBackoff == 0 {
		config.BaseBackoff = defaultBaseBackoff
	}
	if config.MaxBackoff == 0 {
		config.MaxBackoff = defaultMaxBackoff
	}
	if config.LeaseDuration < 0 {
		return nil, errors.New("upload queue lease duration must be positive")
	}
	if config.BaseBackoff < 0 {
		return nil, errors.New("upload queue base backoff must be positive")
	}
	if config.MaxBackoff < config.BaseBackoff {
		return nil, errors.New("upload queue maximum backoff must be at least its base backoff")
	}
	if config.Now == nil {
		config.Now = time.Now
	}

	if config.Path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(config.Path), 0o700); err != nil {
			return nil, fmt.Errorf("create upload queue directory: %w", err)
		}
	}
	database, err := sql.Open("sqlite", config.Path)
	if err != nil {
		return nil, fmt.Errorf("open upload queue: %w", err)
	}
	// Queue operations are short. One connection gives deterministic SQLite
	// locking while remaining safe for concurrent goroutines.
	database.SetMaxOpenConns(1)
	queue := &Queue{
		database:       database,
		maxQueuedBytes: config.MaxQueuedBytes,
		leaseDuration:  config.LeaseDuration,
		baseBackoff:    config.BaseBackoff,
		maxBackoff:     config.MaxBackoff,
		now:            config.Now,
	}
	if err := queue.initialize(context.Background()); err != nil {
		_ = database.Close()
		return nil, err
	}
	return queue, nil
}

func (queue *Queue) initialize(ctx context.Context) error {
	statements := []string{
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA journal_mode = WAL`,
		`PRAGMA synchronous = NORMAL`,
		`CREATE TABLE IF NOT EXISTS team_upload_jobs_v1 (
			id TEXT PRIMARY KEY,
			adapter TEXT NOT NULL,
			target_scope TEXT NOT NULL CHECK (target_scope = 'team'),
			target_endpoint TEXT NOT NULL,
			target_project TEXT NOT NULL,
			artifact_identity TEXT NOT NULL,
			expected_digest TEXT NOT NULL,
			expected_size INTEGER NOT NULL CHECK (expected_size >= 0),
			state TEXT NOT NULL CHECK (state IN ('pending', 'leased', 'completed')),
			attempts INTEGER NOT NULL CHECK (attempts >= 0),
			last_error_code TEXT NOT NULL,
			next_attempt_at INTEGER NOT NULL,
			lease_generation INTEGER NOT NULL CHECK (lease_generation >= 0),
			lease_expires_at INTEGER,
			final_outcome TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			completed_at INTEGER,
			UNIQUE (adapter, target_scope, target_endpoint, target_project, artifact_identity)
		) WITHOUT ROWID`,
		`CREATE INDEX IF NOT EXISTS team_upload_jobs_due_v1
			ON team_upload_jobs_v1(state, next_attempt_at, lease_expires_at, created_at)`,
	}
	for _, statement := range statements {
		if _, err := queue.database.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize upload queue: %w", err)
		}
	}
	return nil
}

func (queue *Queue) Close() error {
	if err := queue.database.Close(); err != nil {
		return fmt.Errorf("close upload queue: %w", err)
	}
	return nil
}

// RequestID returns the durable identity Enqueue will use. Callers that must
// retain the referenced artifact can establish that retention before the
// queue transaction lands.
func RequestID(request EnqueueRequest) (string, error) {
	normalized, err := normalizeRequest(request)
	if err != nil {
		return "", err
	}
	return jobID(normalized), nil
}

func (queue *Queue) Enqueue(ctx context.Context, request EnqueueRequest) (EnqueueResult, error) {
	normalized, err := normalizeRequest(request)
	if err != nil {
		return EnqueueResult{}, err
	}
	now := queue.now().UTC()
	id := jobID(normalized)
	remainingBytes := queue.maxQueuedBytes - normalized.Size
	result, err := queue.database.ExecContext(ctx, `
		INSERT INTO team_upload_jobs_v1 (
			id, adapter, target_scope, target_endpoint, target_project,
			artifact_identity, expected_digest, expected_size,
			state, attempts, last_error_code, next_attempt_at,
			lease_generation, final_outcome, created_at, updated_at
		)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, 'pending', 0, '', ?, 0, '', ?, ?
		WHERE (
			SELECT COALESCE(SUM(expected_size), 0)
			FROM team_upload_jobs_v1
			WHERE state IN ('pending', 'leased')
		) <= ?
		ON CONFLICT (id) DO NOTHING`,
		id,
		normalized.Adapter,
		normalized.Target.Scope,
		normalized.Target.Endpoint,
		normalized.Target.Project,
		normalized.Identity,
		normalized.Digest,
		normalized.Size,
		now.UnixNano(),
		now.UnixNano(),
		now.UnixNano(),
		remainingBytes,
	)
	if err != nil {
		return EnqueueResult{}, fmt.Errorf("enqueue Team Cache upload: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return EnqueueResult{}, fmt.Errorf("confirm Team Cache upload enqueue: %w", err)
	}
	job, getErr := queue.Get(ctx, id)
	if inserted == 0 && errors.Is(getErr, ErrNotFound) {
		return EnqueueResult{}, ErrQueueFull
	}
	if getErr != nil {
		return EnqueueResult{}, getErr
	}
	if job.Digest != normalized.Digest || job.Size != normalized.Size {
		return EnqueueResult{}, ErrConflict
	}
	return EnqueueResult{Job: job, Created: inserted == 1}, nil
}

func (queue *Queue) Get(ctx context.Context, id string) (Job, error) {
	if !isLowerHex(id, sha256.Size*2) {
		return Job{}, ErrNotFound
	}
	job, err := scanJob(queue.database.QueryRowContext(ctx, selectJob+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("read Team Cache upload job: %w", err)
	}
	return job, nil
}

func (queue *Queue) Stats(ctx context.Context) (Stats, error) {
	now := queue.now().UTC().UnixNano()
	stats := Stats{MaxQueuedBytes: queue.maxQueuedBytes}
	err := queue.database.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN state IN ('pending', 'leased') THEN expected_size ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN state IN ('pending', 'leased') THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN state = 'pending' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN state = 'leased' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE
				WHEN state = 'pending' AND next_attempt_at <= ? THEN 1
				WHEN state = 'leased' AND lease_expires_at <= ? THEN 1
				ELSE 0
			END), 0),
			COALESCE(SUM(CASE WHEN state = 'completed' THEN 1 ELSE 0 END), 0)
		FROM team_upload_jobs_v1`, now, now).Scan(
		&stats.QueuedBytes,
		&stats.QueuedJobs,
		&stats.PendingJobs,
		&stats.LeasedJobs,
		&stats.DueJobs,
		&stats.CompletedJobs,
	)
	if err != nil {
		return Stats{}, fmt.Errorf("read Team Cache upload queue stats: %w", err)
	}
	return stats, nil
}

// Unfinished returns the durable work that still needs its referenced Local
// Cache artifact. Both pending jobs and jobs with a live or expired worker
// lease remain unfinished until Complete records the upload outcome.
func (queue *Queue) Unfinished(ctx context.Context) ([]Job, error) {
	rows, err := queue.database.QueryContext(ctx, selectJob+`
		WHERE state IN ('pending', 'leased')
		ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list unfinished Team Cache uploads: %w", err)
	}
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scan unfinished Team Cache upload: %w", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unfinished Team Cache uploads: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close unfinished Team Cache uploads: %w", err)
	}
	return jobs, nil
}

func (queue *Queue) Claim(ctx context.Context) (Lease, error) {
	now := queue.now().UTC()
	expiresAt := now.Add(queue.leaseDuration)
	job, err := scanJob(queue.database.QueryRowContext(ctx, `
		UPDATE team_upload_jobs_v1
		SET state = 'leased',
			attempts = attempts + 1,
			lease_generation = lease_generation + 1,
			lease_expires_at = ?,
			updated_at = ?
		WHERE id = (
			SELECT id
			FROM team_upload_jobs_v1
			WHERE (state = 'pending' AND next_attempt_at <= ?)
			   OR (state = 'leased' AND lease_expires_at <= ?)
			ORDER BY
				CASE state WHEN 'leased' THEN lease_expires_at ELSE next_attempt_at END,
				created_at,
				id
			LIMIT 1
		)
		RETURNING `+jobColumns,
		expiresAt.UnixNano(),
		now.UnixNano(),
		now.UnixNano(),
		now.UnixNano(),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, ErrNoDueJob
	}
	if err != nil {
		return Lease{}, fmt.Errorf("claim Team Cache upload: %w", err)
	}
	return Lease{
		Job:        job,
		Generation: job.LeaseGeneration,
		ExpiresAt:  expiresAt,
	}, nil
}

// Complete marks a claimed job as uploaded. Repeating completion with the
// lease that finalized the job returns the same final job.
func (queue *Queue) Complete(ctx context.Context, lease Lease) (Job, error) {
	if lease.Job.ID == "" || lease.Generation <= 0 {
		return Job{}, ErrLeaseLost
	}
	now := queue.now().UTC()
	job, err := scanJob(queue.database.QueryRowContext(ctx, `
		UPDATE team_upload_jobs_v1
		SET state = 'completed',
			lease_expires_at = NULL,
			final_outcome = 'uploaded',
			updated_at = ?,
			completed_at = ?
		WHERE id = ? AND state = 'leased' AND lease_generation = ?
		RETURNING `+jobColumns,
		now.UnixNano(),
		now.UnixNano(),
		lease.Job.ID,
		lease.Generation,
	))
	if err == nil {
		return job, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Job{}, fmt.Errorf("complete Team Cache upload: %w", err)
	}
	existing, getErr := queue.Get(ctx, lease.Job.ID)
	if getErr != nil {
		if errors.Is(getErr, ErrNotFound) {
			return Job{}, ErrLeaseLost
		}
		return Job{}, getErr
	}
	if existing.State == StateCompleted &&
		existing.Outcome == OutcomeUploaded &&
		existing.LeaseGeneration == lease.Generation {
		return existing, nil
	}
	return Job{}, ErrLeaseLost
}

// Retry releases a lease and schedules the next claim. failureCode is a short
// machine-readable category such as "network_timeout" or "http_503". The
// restricted format keeps request data and credentials out of queue metadata.
func (queue *Queue) Retry(ctx context.Context, lease Lease, failureCode string) (Job, error) {
	if lease.Job.ID == "" || lease.Generation <= 0 {
		return Job{}, ErrLeaseLost
	}
	if !failureCodePattern.MatchString(failureCode) {
		return Job{}, errors.New("upload failure code must be a non-sensitive lowercase identifier")
	}
	current, err := queue.Get(ctx, lease.Job.ID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Job{}, ErrLeaseLost
		}
		return Job{}, err
	}
	if current.State == StatePending &&
		current.LeaseGeneration == lease.Generation &&
		current.LastError == failureCode {
		return current, nil
	}
	if current.State != StateLeased || current.LeaseGeneration != lease.Generation {
		return Job{}, ErrLeaseLost
	}
	now := queue.now().UTC()
	nextAttemptAt := now.Add(queue.retryDelay(current.Attempts))
	job, err := scanJob(queue.database.QueryRowContext(ctx, `
		UPDATE team_upload_jobs_v1
		SET state = 'pending',
			last_error_code = ?,
			next_attempt_at = ?,
			lease_expires_at = NULL,
			updated_at = ?
		WHERE id = ? AND state = 'leased' AND lease_generation = ?
		RETURNING `+jobColumns,
		failureCode,
		nextAttemptAt.UnixNano(),
		now.UnixNano(),
		lease.Job.ID,
		lease.Generation,
	))
	if err == nil {
		return job, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Job{}, fmt.Errorf("schedule Team Cache upload retry: %w", err)
	}
	existing, getErr := queue.Get(ctx, lease.Job.ID)
	if getErr != nil {
		if errors.Is(getErr, ErrNotFound) {
			return Job{}, ErrLeaseLost
		}
		return Job{}, getErr
	}
	if existing.State == StatePending && existing.LeaseGeneration == lease.Generation && existing.LastError == failureCode {
		return existing, nil
	}
	return Job{}, ErrLeaseLost
}

func (queue *Queue) retryDelay(attempt int) time.Duration {
	delay := queue.baseBackoff
	for current := 1; current < attempt && delay < queue.maxBackoff; current++ {
		if delay > queue.maxBackoff/2 {
			return queue.maxBackoff
		}
		delay *= 2
	}
	if delay > queue.maxBackoff {
		return queue.maxBackoff
	}
	return delay
}

const jobColumns = `
	id, adapter, target_scope, target_endpoint, target_project,
	artifact_identity, expected_digest, expected_size,
	state, attempts, last_error_code, next_attempt_at,
	lease_generation, lease_expires_at, final_outcome,
	created_at, updated_at, completed_at`

const selectJob = `SELECT ` + jobColumns + ` FROM team_upload_jobs_v1`

type rowScanner interface {
	Scan(destinations ...any) error
}

func scanJob(row rowScanner) (Job, error) {
	var job Job
	var nextAttemptAt int64
	var leaseExpiresAt sql.NullInt64
	var createdAt int64
	var updatedAt int64
	var completedAt sql.NullInt64
	if err := row.Scan(
		&job.ID,
		&job.Adapter,
		&job.Target.Scope,
		&job.Target.Endpoint,
		&job.Target.Project,
		&job.Identity,
		&job.Digest,
		&job.Size,
		&job.State,
		&job.Attempts,
		&job.LastError,
		&nextAttemptAt,
		&job.LeaseGeneration,
		&leaseExpiresAt,
		&job.Outcome,
		&createdAt,
		&updatedAt,
		&completedAt,
	); err != nil {
		return Job{}, err
	}
	job.NextAttemptAt = fromUnixNano(nextAttemptAt)
	job.CreatedAt = fromUnixNano(createdAt)
	job.UpdatedAt = fromUnixNano(updatedAt)
	if leaseExpiresAt.Valid {
		value := fromUnixNano(leaseExpiresAt.Int64)
		job.LeaseExpiresAt = &value
	}
	if completedAt.Valid {
		value := fromUnixNano(completedAt.Int64)
		job.CompletedAt = &value
	}
	return job, nil
}

func normalizeRequest(request EnqueueRequest) (EnqueueRequest, error) {
	switch request.Adapter {
	case AdapterTurbo, AdapterActions, AdapterBuildKit:
	default:
		return EnqueueRequest{}, fmt.Errorf("unsupported upload adapter %q", request.Adapter)
	}
	if request.Target.Scope != ScopeTeam {
		return EnqueueRequest{}, errors.New("upload destination must be Team Cache")
	}
	endpoint, err := normalizeEndpoint(request.Target.Endpoint)
	if err != nil {
		return EnqueueRequest{}, err
	}
	project := strings.TrimSpace(request.Target.Project)
	if err := validateOpaque("Team Cache project", project, 256); err != nil {
		return EnqueueRequest{}, err
	}
	if looksLikeWorkspacePath(project) {
		return EnqueueRequest{}, errors.New("Team Cache project cannot be an absolute workspace path")
	}
	identity := strings.TrimSpace(request.Identity)
	if err := validateOpaque("artifact identity", identity, 2048); err != nil {
		return EnqueueRequest{}, err
	}
	if looksLikeWorkspacePath(identity) {
		return EnqueueRequest{}, errors.New("artifact identity cannot be an absolute workspace path")
	}
	digest := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(request.Digest)), "sha256:")
	if !isLowerHex(digest, sha256.Size*2) {
		return EnqueueRequest{}, errors.New("artifact digest must be a SHA-256 digest")
	}
	if request.Size < 0 {
		return EnqueueRequest{}, errors.New("artifact size cannot be negative")
	}
	request.Target.Endpoint = endpoint
	request.Target.Project = project
	request.Identity = identity
	request.Digest = digest
	return request, nil
}

func normalizeEndpoint(raw string) (string, error) {
	endpoint, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return "", errors.New("Team Cache endpoint must be an absolute HTTP or HTTPS URL")
	}
	if endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return "", errors.New("Team Cache endpoint cannot contain credentials, a query, or a fragment")
	}
	if endpoint.Scheme == "http" && !isLoopbackHost(endpoint.Hostname()) {
		return "", errors.New("Team Cache endpoint must use HTTPS except on loopback")
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/")
	return endpoint.String(), nil
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func validateOpaque(name, value string, maximum int) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > maximum {
		return fmt.Errorf("%s exceeds %d bytes", name, maximum)
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%s contains control characters", name)
	}
	return nil
}

var windowsAbsolutePath = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

var failureCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

func looksLikeWorkspacePath(value string) bool {
	lower := strings.ToLower(value)
	return strings.HasPrefix(value, "/") ||
		strings.HasPrefix(value, `\\`) ||
		windowsAbsolutePath.MatchString(value) ||
		strings.HasPrefix(lower, "file://")
}

func jobID(request EnqueueRequest) string {
	hash := sha256.New()
	for _, part := range []string{
		string(request.Adapter),
		string(request.Target.Scope),
		request.Target.Endpoint,
		request.Target.Project,
		request.Identity,
	} {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func isLowerHex(value string, length int) bool {
	if len(value) != length || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func fromUnixNano(value int64) time.Time {
	return time.Unix(0, value).UTC()
}
