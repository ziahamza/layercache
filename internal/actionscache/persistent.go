package actionscache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/layercache/layercache/internal/artifact"
	_ "modernc.org/sqlite"
)

const (
	actionsReservationTTL     = 24 * time.Hour
	actionsReservationSweep   = time.Minute
	actionsTeamRetryMinimum   = time.Second
	actionsTeamRetryMaximum   = 5 * time.Minute
	actionsTeamAttemptTimeout = 10 * time.Minute
	actionsTeamAbortTimeout   = 5 * time.Second
	actionsTeamIdlePoll       = 5 * time.Second
	actionsTeamShutdownDrain  = 10 * time.Second
	actionsTeamStatsTimeout   = 2 * time.Second
	actionsTeamPinNamespace   = "actions-team-publication"
)

type PersistentStorage struct {
	db        *sql.DB
	artifacts *artifact.Store
	staging   string
	mu        sync.Mutex
	inflight  map[int64]map[int64]*inflightUpload
	leases    map[int64]map[int64]*artifact.StagingLease
	closed    bool

	maintenanceCancel context.CancelFunc
	maintenanceDone   chan struct{}
	team              CacheWriter
	teamCancel        context.CancelFunc
	teamDone          chan struct{}
	teamWake          chan struct{}
}

type teamPublicationJob struct {
	entryID       int64
	repository    string
	compatibility string
	ref           string
	key           string
	version       string
	size          int64
	attempts      int
}

// TeamPublicationStats describes durable Actions archives that have committed
// locally but have not yet finished publishing to Team Cache.
type TeamPublicationStats struct {
	PendingJobs  int64
	PendingBytes int64
}

type inflightUpload struct {
	start        int64
	end          int64
	stagedBytes  int64
	existingPath string
	done         chan struct{}
	lease        *artifact.StagingLease
}

type persistentReservation struct {
	id            int64
	repository    string
	compatibility string
	ref           string
	key           string
	version       string
	expectedSize  sql.NullInt64
	maximumSize   sql.NullInt64
}

func OpenPersistentStorage(ctx context.Context, root string, artifacts *artifact.Store) (*PersistentStorage, error) {
	if artifacts == nil {
		return nil, errors.New("Local Cache artifact backend is required")
	}
	staging := filepath.Join(root, "actions-staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return nil, fmt.Errorf("create Actions cache staging directory: %w", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "actions.db"))
	if err != nil {
		return nil, fmt.Errorf("open Actions cache metadata: %w", err)
	}
	db.SetMaxOpenConns(8)
	for _, statement := range []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS actions_reservations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repository TEXT NOT NULL,
			compatibility TEXT NOT NULL,
			ref_scope TEXT NOT NULL,
			cache_key TEXT NOT NULL,
			version TEXT NOT NULL,
			expected_size INTEGER,
			maximum_size INTEGER,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS actions_identities (
			repository TEXT NOT NULL,
			compatibility TEXT NOT NULL,
			ref_scope TEXT NOT NULL,
			cache_key TEXT NOT NULL,
			version TEXT NOT NULL,
			reservation_id INTEGER NOT NULL,
			committed INTEGER NOT NULL,
			PRIMARY KEY(repository, compatibility, ref_scope, cache_key, version)
		)`,
		`CREATE TABLE IF NOT EXISTS actions_chunks (
			reservation_id INTEGER NOT NULL,
			start_offset INTEGER NOT NULL,
			end_offset INTEGER NOT NULL,
			path TEXT NOT NULL,
			PRIMARY KEY(reservation_id, start_offset)
		)`,
		`CREATE TABLE IF NOT EXISTS actions_entries (
			id INTEGER PRIMARY KEY,
			repository TEXT NOT NULL,
			compatibility TEXT NOT NULL,
			ref_scope TEXT NOT NULL,
			cache_key TEXT NOT NULL,
			version TEXT NOT NULL,
			size INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			producer_duration_ns INTEGER,
			origin TEXT NOT NULL DEFAULT 'localCache',
			public_metadata_json BLOB
		)`,
		`CREATE INDEX IF NOT EXISTS actions_lookup ON actions_entries(repository, compatibility, ref_scope, version, created_at)`,
		`CREATE TABLE IF NOT EXISTS actions_team_publications (
			entry_id INTEGER PRIMARY KEY,
			repository TEXT NOT NULL,
			compatibility TEXT NOT NULL,
			ref_scope TEXT NOT NULL,
			cache_key TEXT NOT NULL,
			version TEXT NOT NULL,
			size INTEGER NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt INTEGER NOT NULL,
			last_error TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS actions_team_publications_due ON actions_team_publications(next_attempt, entry_id)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize Actions cache metadata: %w", err)
		}
	}
	if err := ensureActionsEntryColumn(ctx, db, "origin", `ALTER TABLE actions_entries ADD COLUMN origin TEXT NOT NULL DEFAULT 'localCache'`); err != nil {
		db.Close()
		return nil, err
	}
	if err := ensureActionsEntryColumn(ctx, db, "public_metadata_json", `ALTER TABLE actions_entries ADD COLUMN public_metadata_json BLOB`); err != nil {
		db.Close()
		return nil, err
	}
	if err := ensureActionsEntryColumn(ctx, db, "producer_duration_ns", `ALTER TABLE actions_entries ADD COLUMN producer_duration_ns INTEGER`); err != nil {
		db.Close()
		return nil, err
	}
	if err := ensureActionsReservationColumn(ctx, db, "maximum_size", `ALTER TABLE actions_reservations ADD COLUMN maximum_size INTEGER`); err != nil {
		db.Close()
		return nil, err
	}
	if err := discardIncompleteUploads(ctx, db, staging, artifacts); err != nil {
		db.Close()
		return nil, err
	}
	if err := reconcileActionsTeamPublicationPins(ctx, db, artifacts); err != nil {
		db.Close()
		return nil, err
	}
	storage := &PersistentStorage{
		db: db, artifacts: artifacts, staging: staging,
		inflight:        make(map[int64]map[int64]*inflightUpload),
		leases:          make(map[int64]map[int64]*artifact.StagingLease),
		maintenanceDone: make(chan struct{}),
		teamWake:        make(chan struct{}, 1),
	}
	maintenanceContext, cancel := context.WithCancel(context.Background())
	storage.maintenanceCancel = cancel
	go storage.runReservationMaintenance(maintenanceContext)
	return storage, nil
}

func discardIncompleteUploads(ctx context.Context, db *sql.DB, staging string, artifacts *artifact.Store) error {
	rows, err := db.QueryContext(ctx, `SELECT repository, compatibility, ref_scope, cache_key, version FROM actions_reservations`)
	if err != nil {
		return fmt.Errorf("inspect incomplete Actions cache uploads: %w", err)
	}
	var orphaned []artifact.Key
	for rows.Next() {
		var key artifact.Key
		key.Integration = "actions"
		if err := rows.Scan(&key.Project, &key.Compatibility, &key.Ref, &key.Native, &key.Version); err != nil {
			rows.Close()
			return fmt.Errorf("read incomplete Actions cache upload: %w", err)
		}
		orphaned = append(orphaned, key)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read incomplete Actions cache uploads: %w", err)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, key := range orphaned {
		if err := artifacts.Delete(ctx, key); err != nil && !errors.Is(err, artifact.ErrNotFound) {
			return fmt.Errorf("discard orphaned Actions cache artifact: %w", err)
		}
	}
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("discard incomplete Actions cache staging: %w", err)
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return fmt.Errorf("recreate Actions cache staging directory: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin incomplete Actions cache cleanup: %w", err)
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`DELETE FROM actions_chunks`,
		`DELETE FROM actions_identities WHERE committed = 0`,
		`DELETE FROM actions_reservations`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("discard incomplete Actions cache metadata: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit incomplete Actions cache cleanup: %w", err)
	}
	return nil
}

func (storage *PersistentStorage) Close() error {
	storage.mu.Lock()
	if storage.closed {
		storage.mu.Unlock()
		return nil
	}
	storage.closed = true
	maintenanceCancel := storage.maintenanceCancel
	teamCancel := storage.teamCancel
	maintenanceDone := storage.maintenanceDone
	teamDone := storage.teamDone
	storage.mu.Unlock()
	maintenanceCancel()
	if teamCancel != nil {
		teamCancel()
	}
	<-maintenanceDone
	if teamDone != nil {
		<-teamDone
		drainContext, cancelDrain := context.WithTimeout(context.Background(), actionsTeamShutdownDrain)
		_ = storage.drainDueTeamJobs(drainContext)
		cancelDrain()
	}

	storage.mu.Lock()
	defer storage.mu.Unlock()
	cleanupErr := discardIncompleteUploads(context.Background(), storage.db, storage.staging, storage.artifacts)
	if cleanupErr == nil {
		for reservationID := range storage.leases {
			storage.releaseReservationLeasesLocked(reservationID)
		}
	}
	pinErr := reconcileActionsTeamPublicationPins(context.Background(), storage.db, storage.artifacts)
	return errors.Join(cleanupErr, pinErr, storage.db.Close())
}

func (storage *PersistentStorage) runReservationMaintenance(ctx context.Context) {
	defer close(storage.maintenanceDone)
	ticker := time.NewTicker(actionsReservationSweep)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			_ = storage.expireReservations(ctx, now.UTC().Add(-actionsReservationTTL))
		}
	}
}

// TeamPublicationStats returns a bounded snapshot of the durable publication
// queue. Every row in actions_team_publications is pending until the Team Cache
// accepts its immutable archive identity and the publisher deletes the row.
func (storage *PersistentStorage) TeamPublicationStats(ctx context.Context) (TeamPublicationStats, error) {
	queryContext, cancel := context.WithTimeout(ctx, actionsTeamStatsTimeout)
	defer cancel()

	var stats TeamPublicationStats
	err := storage.db.QueryRowContext(queryContext, `SELECT COUNT(*), COALESCE(SUM(size), 0)
		FROM actions_team_publications`).Scan(&stats.PendingJobs, &stats.PendingBytes)
	if err != nil {
		return TeamPublicationStats{}, fmt.Errorf("read Actions Team Cache publication stats: %w", err)
	}
	return stats, nil
}

// EnableTeamPublication starts the durable Team Cache publisher. Commits add a
// queue row in the same SQLite transaction that exposes their Actions entry.
// The publisher retries until the Team Cache accepts the immutable identity.
func (storage *PersistentStorage) EnableTeamPublication(team CacheWriter) error {
	if team == nil {
		return errors.New("Team Cache writer is required")
	}
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if storage.closed {
		return errors.New("Actions cache storage is closed")
	}
	if storage.team != nil {
		return errors.New("Actions Team Cache publisher is already configured")
	}
	storage.team = team
	teamContext, cancel := context.WithCancel(context.Background())
	storage.teamCancel = cancel
	storage.teamDone = make(chan struct{})
	go storage.runTeamPublisher(teamContext)
	select {
	case storage.teamWake <- struct{}{}:
	default:
	}
	return nil
}

func (storage *PersistentStorage) runTeamPublisher(ctx context.Context) {
	defer close(storage.teamDone)
	ticker := time.NewTicker(actionsTeamIdlePoll)
	defer ticker.Stop()
	for {
		if _, err := storage.publishNextTeamJob(ctx, true); err != nil && ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-storage.teamWake:
		case <-ticker.C:
		}
	}
}

func (storage *PersistentStorage) drainDueTeamJobs(ctx context.Context) error {
	for {
		attempted, err := storage.publishNextTeamJob(ctx, false)
		if err != nil || !attempted {
			return err
		}
	}
}

func (storage *PersistentStorage) publishNextTeamJob(ctx context.Context, recordRetry bool) (bool, error) {
	job, found, err := storage.nextTeamJob(ctx, time.Now().UTC())
	if err != nil || !found {
		return false, err
	}
	attemptContext, cancel := context.WithTimeout(ctx, actionsTeamAttemptTimeout)
	err = storage.publishTeamJob(attemptContext, job)
	cancel()
	if err == nil || errors.Is(err, ErrAlreadyExists) {
		_, deleteErr := storage.db.ExecContext(ctx, `DELETE FROM actions_team_publications WHERE entry_id = ?`, job.entryID)
		if deleteErr != nil {
			return true, deleteErr
		}
		unpinErr := storage.artifacts.Unpin(ctx, actionsTeamPinNamespace, actionsTeamPublicationOwner(job.entryID))
		select {
		case storage.teamWake <- struct{}{}:
		default:
		}
		return true, unpinErr
	}
	if !recordRetry {
		return true, err
	}
	delay := actionsTeamRetryMinimum
	for attempt := 0; attempt < job.attempts && delay < actionsTeamRetryMaximum; attempt++ {
		delay *= 2
		if delay > actionsTeamRetryMaximum {
			delay = actionsTeamRetryMaximum
		}
	}
	message := err.Error()
	if len(message) > 4<<10 {
		message = message[:4<<10]
	}
	_, updateErr := storage.db.ExecContext(ctx, `UPDATE actions_team_publications
		SET attempts = attempts + 1, next_attempt = ?, last_error = ? WHERE entry_id = ?`,
		time.Now().UTC().Add(delay).UnixNano(), message, job.entryID)
	return true, updateErr
}

func (storage *PersistentStorage) nextTeamJob(ctx context.Context, now time.Time) (teamPublicationJob, bool, error) {
	var job teamPublicationJob
	err := storage.db.QueryRowContext(ctx, `SELECT entry_id, repository, compatibility, ref_scope, cache_key, version, size, attempts
		FROM actions_team_publications WHERE next_attempt <= ? ORDER BY next_attempt, entry_id LIMIT 1`, now.UnixNano()).Scan(
		&job.entryID, &job.repository, &job.compatibility, &job.ref, &job.key, &job.version, &job.size, &job.attempts,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return teamPublicationJob{}, false, nil
	}
	if err != nil {
		return teamPublicationJob{}, false, err
	}
	return job, true, nil
}

func reconcileActionsTeamPublicationPins(ctx context.Context, db *sql.DB, artifacts *artifact.Store) error {
	rows, err := db.QueryContext(ctx, `SELECT entry_id, repository, compatibility, ref_scope, cache_key, version, size, attempts
		FROM actions_team_publications ORDER BY entry_id`)
	if err != nil {
		return fmt.Errorf("read queued Actions Team Cache publications for pin reconciliation: %w", err)
	}
	var pins []artifact.Pin
	for rows.Next() {
		var job teamPublicationJob
		if err := rows.Scan(&job.entryID, &job.repository, &job.compatibility, &job.ref,
			&job.key, &job.version, &job.size, &job.attempts); err != nil {
			rows.Close()
			return fmt.Errorf("read queued Actions Team Cache publication for pin reconciliation: %w", err)
		}
		key := actionsTeamPublicationArtifactKey(job)
		entry, err := artifacts.Head(ctx, key)
		if errors.Is(err, artifact.ErrNotFound) {
			// Keep the durable queue row. The publisher will record and retry the
			// missing-artifact failure instead of declaring the job complete.
			continue
		}
		if err != nil {
			rows.Close()
			return fmt.Errorf("read queued Actions Team Cache artifact for pin reconciliation: %w", err)
		}
		pins = append(pins, artifact.Pin{
			Owner: actionsTeamPublicationOwner(job.entryID), Key: key,
			Digest: entry.Digest, Size: entry.Size,
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read queued Actions Team Cache publications for pin reconciliation: %w", err)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if _, err := artifacts.ReconcilePins(ctx, actionsTeamPinNamespace, pins); err != nil {
		return fmt.Errorf("reconcile queued Actions Team Cache artifact pins: %w", err)
	}
	return nil
}

func actionsTeamPublicationArtifactKey(job teamPublicationJob) artifact.Key {
	return artifact.Key{
		Integration: "actions", Project: job.repository, Compatibility: job.compatibility,
		Native: job.key, Version: job.version, Ref: job.ref,
	}
}

func actionsTeamPublicationOwner(entryID int64) string {
	return fmt.Sprintf("entry-%d", entryID)
}

func (storage *PersistentStorage) publishTeamJob(ctx context.Context, job teamPublicationJob) error {
	scope := Scope{
		Repository: job.repository, Compatibility: job.compatibility,
		Ref: job.ref, DefaultRef: job.ref,
	}
	archive, err := storage.Open(ctx, OpenRequest{Scope: scope, ID: job.entryID})
	if err != nil {
		return err
	}
	defer archive.Body.Close()
	reservation, err := storage.team.Reserve(ctx, ReserveRequest{
		Scope: scope, Key: job.key, Version: job.version, CacheSize: &job.size,
	})
	if errors.Is(err, ErrAlreadyExists) {
		reader, ok := storage.team.(CacheReader)
		if !ok {
			return err
		}
		result, lookupErr := reader.Lookup(ctx, LookupRequest{Scope: scope, Keys: []string{job.key}, Version: job.version})
		if lookupErr == nil && result.Match == MatchExact && result.Entry.Key == job.key {
			return ErrAlreadyExists
		}
		if lookupErr != nil && !errors.Is(lookupErr, ErrNotFound) {
			return lookupErr
		}
		return err
	}
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if aborter, ok := storage.team.(reservationAborter); ok {
			abortContext, cancelAbort := context.WithTimeout(context.Background(), actionsTeamAbortTimeout)
			_ = aborter.Abort(abortContext, reservation.ID)
			cancelAbort()
		}
	}()
	if _, err := uploadArchive(ctx, storage.team, reservation.ID, &scope, archive.Body, job.size); err != nil {
		return err
	}
	if _, err := storage.team.Commit(ctx, CommitRequest{
		ReservationID: reservation.ID, Scope: &scope, Size: job.size,
		ProducerDuration: cloneDuration(archive.Entry.ProducerDuration),
	}); err != nil {
		return err
	}
	committed = true
	return nil
}

func (storage *PersistentStorage) expireReservations(ctx context.Context, cutoff time.Time) error {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if storage.closed {
		return nil
	}
	rows, err := storage.db.QueryContext(ctx, `SELECT id FROM actions_reservations WHERE created_at <= ? ORDER BY id`, cutoff.UnixNano())
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	var cleanupErr error
	for _, id := range ids {
		if len(storage.inflight[id]) != 0 {
			continue
		}
		cleanupErr = errors.Join(cleanupErr, storage.removeReservationLocked(ctx, id))
	}
	return cleanupErr
}

// ArtifactStore exposes the Local Cache admission backend to integrations that
// must stage verified remote archives under the same aggregate byte limits.
func (storage *PersistentStorage) ArtifactStore() *artifact.Store { return storage.artifacts }

func (storage *PersistentStorage) Lookup(ctx context.Context, request LookupRequest) (LookupResult, error) {
	refs := []struct {
		value string
		scope RefScope
	}{{request.Scope.Ref, RefScopeCurrent}}
	if request.Scope.DefaultRef != request.Scope.Ref {
		refs = append(refs, struct {
			value string
			scope RefScope
		}{request.Scope.DefaultRef, RefScopeDefault})
	}
	for _, ref := range refs {
		entries, err := storage.entriesFor(ctx, request, ref.value)
		if err != nil {
			return LookupResult{}, err
		}
		for _, requestedKey := range request.Keys {
			for _, entry := range entries {
				if entry.Key == requestedKey &&
					(entry.Public == nil || requestedKey == request.Keys[0]) {
					return LookupResult{Entry: entry, Match: MatchExact, RequestedKey: requestedKey, RefScope: ref.scope, Source: SourceLocalCache}, nil
				}
			}
			for _, entry := range entries {
				if entry.Public == nil && strings.HasPrefix(entry.Key, requestedKey) {
					return LookupResult{Entry: entry, Match: MatchPrefix, RequestedKey: requestedKey, RefScope: ref.scope, Source: SourceLocalCache}, nil
				}
			}
		}
	}
	return LookupResult{}, ErrNotFound
}

func (storage *PersistentStorage) Reserve(ctx context.Context, request ReserveRequest) (Reservation, error) {
	maximumSize := request.MaxArtifactBytes
	if maximumSize < 0 {
		return Reservation{}, ErrInvalidUpload
	}
	if maximumSize == 0 {
		maximumSize = storage.artifacts.MaxBytes()
	}
	if maximumSize > storage.artifacts.MaxBytes() {
		maximumSize = storage.artifacts.MaxBytes()
	}
	if request.CacheSize != nil && (*request.CacheSize < 0 || *request.CacheSize > maximumSize) {
		return Reservation{}, ErrInvalidUpload
	}
	storage.mu.Lock()
	defer storage.mu.Unlock()
	tx, err := storage.db.BeginTx(ctx, nil)
	if err != nil {
		return Reservation{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO actions_reservations(repository, compatibility, ref_scope, cache_key, version, expected_size, maximum_size, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, request.Scope.Repository, request.Scope.Compatibility, request.Scope.Ref,
		request.Key, request.Version, nullableSize(request.CacheSize), maximumSize, time.Now().UTC().UnixNano())
	if err != nil {
		return Reservation{}, fmt.Errorf("create Actions cache reservation: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Reservation{}, fmt.Errorf("read Actions cache reservation ID: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO actions_identities(repository, compatibility, ref_scope, cache_key, version, reservation_id, committed)
		VALUES(?, ?, ?, ?, ?, ?, 0)`, request.Scope.Repository, request.Scope.Compatibility, request.Scope.Ref,
		request.Key, request.Version, id)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Reservation{}, ErrAlreadyExists
		}
		return Reservation{}, fmt.Errorf("claim Actions cache identity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Reservation{}, fmt.Errorf("commit Actions cache reservation: %w", err)
	}
	return Reservation{ID: id}, nil
}

func (storage *PersistentStorage) Upload(ctx context.Context, request UploadRequest) error {
	if request.Body == nil || request.Start < 0 || request.End < request.Start {
		return ErrInvalidUpload
	}
	length := request.End - request.Start
	if length == math.MaxInt64 {
		return ErrInvalidUpload
	}
	length++

	reservation, upload, err := storage.beginUpload(ctx, request)
	if err != nil {
		return err
	}
	defer storage.finishUpload(request.ReservationID, upload)
	if upload.existingPath != "" {
		return fileMatchesExactBody(upload.existingPath, request.Body, length)
	}
	upload.lease, err = storage.artifacts.ReserveUploadStaging(upload.stagedBytes)
	if err != nil {
		return err
	}

	dir := filepath.Join(storage.staging, fmt.Sprint(request.ReservationID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	defer os.Remove(dir)
	tmp, err := os.CreateTemp(dir, "chunk-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := streamExactLength(storage.artifacts.SpaceCheckedWriter(ctx, tmp), request.Body, length); err != nil {
		tmp.Close()
		return err
	}
	if err := storage.artifacts.SyncStaged(ctx, tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	storage.mu.Lock()
	defer storage.mu.Unlock()
	reservation, err = storage.reservation(ctx, request.ReservationID)
	if err != nil {
		return err
	}
	if !reservationScopeMatches(request.Scope, reservation.repository, reservation.compatibility, reservation.ref) {
		return ErrNotFound
	}
	if reservation.expectedSize.Valid && request.End >= reservation.expectedSize.Int64 {
		return ErrInvalidUpload
	}
	var existingEnd int64
	var existingPath string
	err = storage.db.QueryRowContext(ctx, `SELECT end_offset, path FROM actions_chunks WHERE reservation_id = ? AND start_offset = ?`, request.ReservationID, request.Start).Scan(&existingEnd, &existingPath)
	if err == nil {
		if existingEnd != request.End {
			return ErrInvalidUpload
		}
		equal, compareErr := filesHaveSameContents(existingPath, tmpPath)
		if compareErr != nil {
			return compareErr
		}
		if equal {
			return nil
		}
		return ErrInvalidUpload
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var overlapping int
	if err := storage.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM actions_chunks
		WHERE reservation_id = ? AND start_offset <= ? AND end_offset >= ?`,
		request.ReservationID, request.End, request.Start).Scan(&overlapping); err != nil {
		return err
	}
	if overlapping != 0 {
		return ErrInvalidUpload
	}
	chunkPath := filepath.Join(dir, fmt.Sprintf("%020d", request.Start))
	if err := os.Rename(tmpPath, chunkPath); err != nil {
		return err
	}
	if _, err := storage.db.ExecContext(ctx, `INSERT INTO actions_chunks(reservation_id, start_offset, end_offset, path) VALUES(?, ?, ?, ?)`,
		request.ReservationID, request.Start, request.End, chunkPath); err != nil {
		_ = os.Remove(chunkPath)
		return err
	}
	if storage.leases[request.ReservationID] == nil {
		storage.leases[request.ReservationID] = make(map[int64]*artifact.StagingLease)
	}
	storage.leases[request.ReservationID][request.Start] = upload.lease
	upload.lease = nil
	return nil
}

func (storage *PersistentStorage) beginUpload(
	ctx context.Context,
	request UploadRequest,
) (persistentReservation, *inflightUpload, error) {
	for {
		storage.mu.Lock()
		reservation, err := storage.reservation(ctx, request.ReservationID)
		if err != nil {
			storage.mu.Unlock()
			return persistentReservation{}, nil, err
		}
		if !reservationScopeMatches(request.Scope, reservation.repository, reservation.compatibility, reservation.ref) {
			storage.mu.Unlock()
			return persistentReservation{}, nil, ErrNotFound
		}
		limit, err := reservation.uploadLimit()
		if err != nil || request.End >= limit {
			storage.mu.Unlock()
			return persistentReservation{}, nil, ErrInvalidUpload
		}

		var existingEnd int64
		var existingPath string
		err = storage.db.QueryRowContext(ctx, `SELECT end_offset, path FROM actions_chunks
			WHERE reservation_id = ? AND start_offset = ?`, request.ReservationID, request.Start).Scan(&existingEnd, &existingPath)
		existing := err == nil
		if err == nil && existingEnd != request.End {
			storage.mu.Unlock()
			return persistentReservation{}, nil, ErrInvalidUpload
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			storage.mu.Unlock()
			return persistentReservation{}, nil, err
		}
		if errors.Is(err, sql.ErrNoRows) {
			var overlapping int
			if err := storage.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM actions_chunks
				WHERE reservation_id = ? AND start_offset <= ? AND end_offset >= ?`,
				request.ReservationID, request.End, request.Start).Scan(&overlapping); err != nil {
				storage.mu.Unlock()
				return persistentReservation{}, nil, err
			}
			if overlapping != 0 {
				storage.mu.Unlock()
				return persistentReservation{}, nil, ErrInvalidUpload
			}
		}

		var retry <-chan struct{}
		var inflightBytes int64
		for _, active := range storage.inflight[request.ReservationID] {
			if rangesOverlap(active.start, active.end, request.Start, request.End) {
				if active.start == request.Start && active.end == request.End {
					retry = active.done
					break
				}
				storage.mu.Unlock()
				return persistentReservation{}, nil, ErrInvalidUpload
			}
			if active.stagedBytes < 0 || inflightBytes > limit-active.stagedBytes {
				storage.mu.Unlock()
				return persistentReservation{}, nil, ErrInvalidUpload
			}
			inflightBytes += active.stagedBytes
		}
		if retry != nil {
			storage.mu.Unlock()
			select {
			case <-ctx.Done():
				return persistentReservation{}, nil, ctx.Err()
			case <-retry:
				continue
			}
		}

		var persistedBytes int64
		if err := storage.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(end_offset - start_offset + 1), 0)
			FROM actions_chunks WHERE reservation_id = ?`, request.ReservationID).Scan(&persistedBytes); err != nil {
			storage.mu.Unlock()
			return persistentReservation{}, nil, err
		}
		if persistedBytes < 0 || persistedBytes > limit || inflightBytes > limit-persistedBytes {
			storage.mu.Unlock()
			return persistentReservation{}, nil, ErrInvalidUpload
		}
		stagedBytes := int64(0)
		if !existing {
			stagedBytes = request.End - request.Start + 1
			if stagedBytes > limit-persistedBytes-inflightBytes {
				storage.mu.Unlock()
				return persistentReservation{}, nil, ErrInvalidUpload
			}
		}

		upload := &inflightUpload{
			start: request.Start, end: request.End, stagedBytes: stagedBytes,
			existingPath: existingPath, done: make(chan struct{}),
		}
		if storage.inflight[request.ReservationID] == nil {
			storage.inflight[request.ReservationID] = make(map[int64]*inflightUpload)
		}
		storage.inflight[request.ReservationID][request.Start] = upload
		storage.mu.Unlock()
		return reservation, upload, nil
	}
}

func (storage *PersistentStorage) finishUpload(reservationID int64, upload *inflightUpload) {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if upload.lease != nil {
		upload.lease.Release()
		upload.lease = nil
	}
	active := storage.inflight[reservationID]
	if active[upload.start] != upload {
		return
	}
	delete(active, upload.start)
	if len(active) == 0 {
		delete(storage.inflight, reservationID)
	}
	close(upload.done)
}

// Abort removes an incomplete reservation and releases all of its retained
// staging bytes. It is intended for internal transfers that cannot be retried.
func (storage *PersistentStorage) Abort(ctx context.Context, reservationID int64) error {
	return storage.AbortScoped(ctx, reservationID, nil)
}

func (storage *PersistentStorage) AbortScoped(ctx context.Context, reservationID int64, scope *Scope) error {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	if len(storage.inflight[reservationID]) != 0 {
		return ErrInvalidUpload
	}
	reservation, err := storage.reservation(ctx, reservationID)
	if err != nil {
		return err
	}
	if !reservationScopeMatches(scope, reservation.repository, reservation.compatibility, reservation.ref) {
		return ErrNotFound
	}
	return storage.removeReservationLocked(ctx, reservationID)
}

func (storage *PersistentStorage) removeReservationLocked(ctx context.Context, reservationID int64) error {
	removeErr := os.RemoveAll(filepath.Join(storage.staging, fmt.Sprint(reservationID)))
	if removeErr == nil {
		defer storage.releaseReservationLeasesLocked(reservationID)
	}
	tx, err := storage.db.BeginTx(ctx, nil)
	if err != nil {
		return errors.Join(removeErr, err)
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`DELETE FROM actions_chunks WHERE reservation_id = ?`,
		`DELETE FROM actions_identities WHERE reservation_id = ? AND committed = 0`,
		`DELETE FROM actions_reservations WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, statement, reservationID); err != nil {
			return errors.Join(removeErr, err)
		}
	}
	commitErr := tx.Commit()
	return errors.Join(removeErr, commitErr)
}

func (storage *PersistentStorage) releaseReservationLeasesLocked(reservationID int64) {
	for _, lease := range storage.leases[reservationID] {
		lease.Release()
	}
	delete(storage.leases, reservationID)
}

func rangesOverlap(firstStart, firstEnd, secondStart, secondEnd int64) bool {
	return firstStart <= secondEnd && secondStart <= firstEnd
}

func (reservation persistentReservation) uploadLimit() (int64, error) {
	maximum := defaultMaxArtifactBytes
	if reservation.maximumSize.Valid {
		maximum = reservation.maximumSize.Int64
	}
	if maximum <= 0 {
		return 0, ErrInvalidUpload
	}
	if !reservation.expectedSize.Valid {
		return maximum, nil
	}
	if reservation.expectedSize.Int64 < 0 || reservation.expectedSize.Int64 > maximum {
		return 0, ErrInvalidUpload
	}
	return reservation.expectedSize.Int64, nil
}

func streamExactLength(destination io.Writer, source io.Reader, length int64) error {
	written, err := io.CopyN(destination, source, length)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return ErrInvalidUpload
		}
		return err
	}
	if written != length {
		return ErrInvalidUpload
	}
	var extra [1]byte
	read, err := io.ReadFull(source, extra[:])
	if read != 0 || err == nil {
		return ErrInvalidUpload
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func fileMatchesExactBody(path string, body io.Reader, length int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != length {
		return ErrInvalidUpload
	}
	existingHash := sha256.New()
	if _, err := io.Copy(existingHash, file); err != nil {
		return err
	}
	uploadHash := sha256.New()
	if err := streamExactLength(uploadHash, body, length); err != nil {
		return err
	}
	if !bytes.Equal(existingHash.Sum(nil), uploadHash.Sum(nil)) {
		return ErrInvalidUpload
	}
	return nil
}

func filesHaveSameContents(firstPath, secondPath string) (bool, error) {
	first, err := os.Open(firstPath)
	if err != nil {
		return false, err
	}
	defer first.Close()
	second, err := os.Open(secondPath)
	if err != nil {
		return false, err
	}
	defer second.Close()
	firstHash := sha256.New()
	if _, err := io.Copy(firstHash, first); err != nil {
		return false, err
	}
	secondHash := sha256.New()
	if _, err := io.Copy(secondHash, second); err != nil {
		return false, err
	}
	return bytes.Equal(firstHash.Sum(nil), secondHash.Sum(nil)), nil
}

func (storage *PersistentStorage) Commit(ctx context.Context, request CommitRequest) (Entry, error) {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	reservation, err := storage.reservation(ctx, request.ReservationID)
	if err != nil {
		return Entry{}, err
	}
	if !reservationScopeMatches(request.Scope, reservation.repository, reservation.compatibility, reservation.ref) {
		return Entry{}, ErrNotFound
	}
	if request.Size < 0 || reservation.expectedSize.Valid && request.Size != reservation.expectedSize.Int64 {
		return Entry{}, ErrInvalidUpload
	}
	rows, err := storage.db.QueryContext(ctx, `SELECT start_offset, end_offset, path FROM actions_chunks WHERE reservation_id = ? ORDER BY start_offset`, request.ReservationID)
	if err != nil {
		return Entry{}, err
	}
	type chunk struct {
		start int64
		end   int64
		path  string
	}
	var chunks []chunk
	for rows.Next() {
		var value chunk
		if err := rows.Scan(&value.start, &value.end, &value.path); err != nil {
			rows.Close()
			return Entry{}, err
		}
		chunks = append(chunks, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Entry{}, fmt.Errorf("read Actions cache upload chunks: %w", err)
	}
	if err := rows.Close(); err != nil {
		return Entry{}, fmt.Errorf("close Actions cache upload chunks: %w", err)
	}
	var offset int64
	readers := make([]io.Reader, 0, len(chunks))
	files := make([]*os.File, 0, len(chunks))
	defer func() {
		for _, file := range files {
			file.Close()
		}
	}()
	for _, chunk := range chunks {
		if chunk.start != offset || chunk.end < chunk.start {
			return Entry{}, ErrIncompleteUpload
		}
		file, err := os.Open(chunk.path)
		if err != nil {
			return Entry{}, ErrIncompleteUpload
		}
		files = append(files, file)
		readers = append(readers, file)
		offset = chunk.end + 1
	}
	if offset != request.Size {
		return Entry{}, ErrIncompleteUpload
	}
	origin, publicMetadata, err := normalizedCommitTrust(request)
	if err != nil {
		return Entry{}, err
	}
	queueTeamPublication := storage.team != nil && origin == SourceLocalCache
	var publicMetadataJSON []byte
	if publicMetadata != nil {
		publicMetadataJSON, err = json.Marshal(publicMetadata)
		if err != nil {
			return Entry{}, fmt.Errorf("encode Actions Public Cache metadata: %w", err)
		}
	}
	key := artifact.Key{
		Integration: "actions", Project: reservation.repository,
		Compatibility: reservation.compatibility, Native: reservation.key,
		Version: reservation.version, Ref: reservation.ref,
	}
	var artifactEntry artifact.Entry
	if publicMetadata != nil {
		artifactEntry, _, err = storage.artifacts.PutVerified(
			ctx, key, artifact.Metadata{}, io.MultiReader(readers...), publicMetadata.Digest,
		)
	} else {
		artifactEntry, _, err = storage.artifacts.Put(ctx, key, artifact.Metadata{}, io.MultiReader(readers...))
	}
	if errors.Is(err, artifact.ErrConflict) {
		return Entry{}, ErrAlreadyExists
	}
	if err != nil {
		return Entry{}, err
	}
	metadataCommitted := false
	defer func() {
		if !metadataCommitted {
			_ = storage.artifacts.Delete(context.Background(), key)
		}
	}()
	if publicMetadata != nil && (artifactEntry.Digest != publicMetadata.Digest || artifactEntry.Size != publicMetadata.Size) {
		return Entry{}, ErrInvalidUpload
	}
	createdAt := time.Now().UTC()
	tx, err := storage.db.BeginTx(ctx, nil)
	if err != nil {
		return Entry{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO actions_entries(
		id, repository, compatibility, ref_scope, cache_key, version, size, created_at,
		producer_duration_ns, origin, public_metadata_json
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, reservation.id, reservation.repository, reservation.compatibility,
		reservation.ref, reservation.key, reservation.version, request.Size, createdAt.UnixNano(),
		durationNanoseconds(request.ProducerDuration), origin, publicMetadataJSON); err != nil {
		return Entry{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM actions_chunks WHERE reservation_id = ?`, reservation.id); err != nil {
		return Entry{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM actions_reservations WHERE id = ?`, reservation.id); err != nil {
		return Entry{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE actions_identities SET committed = 1 WHERE reservation_id = ?`, reservation.id); err != nil {
		return Entry{}, err
	}
	if queueTeamPublication {
		now := time.Now().UTC().UnixNano()
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO actions_team_publications(
			entry_id, repository, compatibility, ref_scope, cache_key, version, size, attempts, next_attempt, last_error, created_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, 0, ?, '', ?)`, reservation.id, reservation.repository,
			reservation.compatibility, reservation.ref, reservation.key, reservation.version, request.Size, now, now); err != nil {
			return Entry{}, fmt.Errorf("queue Actions Team Cache publication: %w", err)
		}
		pin := artifact.Pin{
			Owner:  actionsTeamPublicationOwner(reservation.id),
			Key:    key,
			Digest: artifactEntry.Digest,
			Size:   artifactEntry.Size,
		}
		if err := storage.artifacts.Pin(ctx, actionsTeamPinNamespace, pin); err != nil {
			return Entry{}, fmt.Errorf("pin queued Actions Team Cache artifact: %w", err)
		}
		defer func() {
			if !metadataCommitted {
				_ = storage.artifacts.Unpin(context.Background(), actionsTeamPinNamespace, pin.Owner)
			}
		}()
	}
	if err := tx.Commit(); err != nil {
		return Entry{}, err
	}
	metadataCommitted = true
	_ = os.RemoveAll(filepath.Join(storage.staging, fmt.Sprint(reservation.id)))
	storage.releaseReservationLeasesLocked(reservation.id)
	if queueTeamPublication {
		select {
		case storage.teamWake <- struct{}{}:
		default:
		}
	}
	return Entry{
		ID: reservation.id, Key: reservation.key, Version: reservation.version, Ref: reservation.ref,
		Size: request.Size, CreatedAt: createdAt, ProducerDuration: cloneDuration(request.ProducerDuration),
		Origin: origin, Public: clonePublicEntryMetadata(publicMetadata),
	}, nil
}

func (storage *PersistentStorage) Open(ctx context.Context, request OpenRequest) (Archive, error) {
	var entry Entry
	var compatibility, repository string
	var createdAt int64
	var producerDuration sql.NullInt64
	var publicMetadataJSON []byte
	err := storage.db.QueryRowContext(ctx, `SELECT id, cache_key, version, ref_scope, size, created_at, repository, compatibility,
		producer_duration_ns, origin, public_metadata_json
		FROM actions_entries WHERE id = ? AND repository = ? AND compatibility = ?
		AND (ref_scope = ? OR ref_scope = ?)`, request.ID, request.Scope.Repository, request.Scope.Compatibility,
		request.Scope.Ref, request.Scope.DefaultRef).Scan(
		&entry.ID, &entry.Key, &entry.Version, &entry.Ref, &entry.Size, &createdAt, &repository, &compatibility,
		&producerDuration, &entry.Origin, &publicMetadataJSON,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Archive{}, ErrNotFound
	}
	if err != nil {
		return Archive{}, err
	}
	entry.CreatedAt = time.Unix(0, createdAt).UTC()
	entry.ProducerDuration = durationFromNullInt64(producerDuration)
	if err := decodePublicEntryMetadata(publicMetadataJSON, &entry, repository, compatibility); err != nil {
		return Archive{}, err
	}
	key := artifact.Key{
		Integration: "actions", Project: repository, Compatibility: compatibility,
		Native: entry.Key, Version: entry.Version, Ref: entry.Ref,
	}
	artifactEntry, file, err := storage.artifacts.Get(ctx, key)
	if errors.Is(err, artifact.ErrNotFound) || errors.Is(err, artifact.ErrCorrupt) {
		_ = storage.InvalidateEntry(ctx, entry.ID)
		return Archive{}, ErrNotFound
	}
	if err != nil {
		return Archive{}, err
	}
	if entry.Public != nil && (artifactEntry.Digest != entry.Public.Digest || artifactEntry.Size != entry.Public.Size) {
		file.Close()
		_ = storage.InvalidateEntry(ctx, entry.ID)
		return Archive{}, ErrNotFound
	}
	return Archive{Entry: entry, Body: file}, nil
}

func (storage *PersistentStorage) entriesFor(ctx context.Context, request LookupRequest, ref string) ([]Entry, error) {
	rows, err := storage.db.QueryContext(ctx, `SELECT id, cache_key, version, ref_scope, size, created_at,
		producer_duration_ns, origin, public_metadata_json
		FROM actions_entries WHERE repository = ? AND compatibility = ? AND ref_scope = ? AND version = ?
		ORDER BY created_at DESC, id DESC`, request.Scope.Repository, request.Scope.Compatibility, ref, request.Version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []Entry
	for rows.Next() {
		var entry Entry
		var createdAt int64
		var producerDuration sql.NullInt64
		var publicMetadataJSON []byte
		if err := rows.Scan(&entry.ID, &entry.Key, &entry.Version, &entry.Ref, &entry.Size, &createdAt,
			&producerDuration, &entry.Origin, &publicMetadataJSON); err != nil {
			return nil, err
		}
		entry.CreatedAt = time.Unix(0, createdAt).UTC()
		entry.ProducerDuration = durationFromNullInt64(producerDuration)
		if err := decodePublicEntryMetadata(publicMetadataJSON, &entry, request.Scope.Repository, request.Scope.Compatibility); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	available := entries[:0]
	for _, entry := range entries {
		key := artifact.Key{
			Integration: "actions", Project: request.Scope.Repository, Compatibility: request.Scope.Compatibility,
			Native: entry.Key, Version: entry.Version, Ref: entry.Ref,
		}
		artifactEntry, file, err := storage.artifacts.Get(ctx, key)
		if file != nil {
			_ = file.Close()
		}
		if errors.Is(err, artifact.ErrNotFound) || errors.Is(err, artifact.ErrCorrupt) {
			if invalidateErr := storage.InvalidateEntry(ctx, entry.ID); invalidateErr != nil && !errors.Is(invalidateErr, ErrNotFound) {
				return nil, invalidateErr
			}
			continue
		} else if err != nil {
			return nil, err
		}
		if entry.Public != nil && (artifactEntry.Digest != entry.Public.Digest || artifactEntry.Size != entry.Public.Size) {
			if invalidateErr := storage.InvalidateEntry(ctx, entry.ID); invalidateErr != nil && !errors.Is(invalidateErr, ErrNotFound) {
				return nil, invalidateErr
			}
			continue
		}
		available = append(available, entry)
	}
	return available, nil
}

func (storage *PersistentStorage) InvalidateEntry(ctx context.Context, id int64) error {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	var queued int
	if err := storage.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM actions_team_publications WHERE entry_id = ?
	)`, id).Scan(&queued); err != nil {
		return err
	}
	if queued != 0 {
		return artifact.ErrPinned
	}
	var key artifact.Key
	err := storage.db.QueryRowContext(ctx, `SELECT repository, compatibility, cache_key, version, ref_scope
		FROM actions_entries WHERE id = ?`, id).Scan(&key.Project, &key.Compatibility, &key.Native, &key.Version, &key.Ref)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	key.Integration = "actions"
	tx, err := storage.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM actions_entries WHERE id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM actions_identities WHERE reservation_id = ?`, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := storage.artifacts.Delete(ctx, key); err != nil && !errors.Is(err, artifact.ErrNotFound) {
		return err
	}
	return nil
}

func (storage *PersistentStorage) UpdatePublicEntryMetadata(ctx context.Context, id int64, metadata *PublicEntryMetadata) error {
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	result, err := storage.db.ExecContext(ctx, `UPDATE actions_entries SET origin = ?, public_metadata_json = ? WHERE id = ?`,
		SourcePublicCache, encoded, id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (storage *PersistentStorage) reservation(ctx context.Context, id int64) (persistentReservation, error) {
	var reservation persistentReservation
	err := storage.db.QueryRowContext(ctx, `SELECT id, repository, compatibility, ref_scope, cache_key, version, expected_size, maximum_size
		FROM actions_reservations WHERE id = ?`, id).Scan(&reservation.id, &reservation.repository,
		&reservation.compatibility, &reservation.ref, &reservation.key, &reservation.version,
		&reservation.expectedSize, &reservation.maximumSize)
	if errors.Is(err, sql.ErrNoRows) {
		return persistentReservation{}, ErrNotFound
	}
	if err != nil {
		return persistentReservation{}, err
	}
	return reservation, nil
}

func nullableSize(size *int64) any {
	if size == nil {
		return nil
	}
	return *size
}

func decodePublicEntryMetadata(encoded []byte, entry *Entry, repository, compatibility string) error {
	if len(encoded) == 0 {
		if entry.Origin == SourcePublicCache {
			return errors.New("Actions Public Cache origin is missing signed metadata")
		}
		if entry.Origin == "" {
			entry.Origin = SourceLocalCache
		}
		return nil
	}
	var metadata PublicEntryMetadata
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		return fmt.Errorf("decode Actions Public Cache metadata: %w", err)
	}
	entry.Public = &metadata
	if entry.Origin != SourcePublicCache {
		return errors.New("Actions Public Cache metadata has a non-Public origin")
	}
	if metadata.Request.CacheIdentity == "" || metadata.Envelope.Payload == "" || metadata.PublicIdentity == "" ||
		metadata.Digest == "" || metadata.ExpiresAt.IsZero() {
		return errors.New("Actions Public Cache origin has incomplete signed metadata")
	}
	request := metadata.Request
	if request.Repository != repository || request.Compatibility != compatibility ||
		request.Ref != entry.Ref || request.Key != entry.Key || request.Version != entry.Version ||
		metadata.Size != entry.Size {
		return errors.New("Actions Public Cache metadata does not match its Local Cache identity")
	}
	return nil
}

func ensureActionsEntryColumn(ctx context.Context, db *sql.DB, name, statement string) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(actions_entries)`)
	if err != nil {
		return fmt.Errorf("inspect Actions cache metadata schema: %w", err)
	}
	found := false
	for rows.Next() {
		var sequence int
		var columnName, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&sequence, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("read Actions cache metadata schema: %w", err)
		}
		if columnName == name {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read Actions cache metadata schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := db.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("migrate Actions cache metadata column %s: %w", name, err)
	}
	return nil
}

func durationFromNullInt64(value sql.NullInt64) *time.Duration {
	if !value.Valid || value.Int64 < 0 {
		return nil
	}
	duration := time.Duration(value.Int64)
	return &duration
}

func ensureActionsReservationColumn(ctx context.Context, db *sql.DB, name, statement string) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(actions_reservations)`)
	if err != nil {
		return fmt.Errorf("inspect Actions cache reservation schema: %w", err)
	}
	found := false
	for rows.Next() {
		var sequence int
		var columnName, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&sequence, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("read Actions cache reservation schema: %w", err)
		}
		if columnName == name {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read Actions cache reservation schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := db.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("migrate Actions cache reservation column %s: %w", name, err)
	}
	return nil
}

var _ StorageIndex = (*PersistentStorage)(nil)
