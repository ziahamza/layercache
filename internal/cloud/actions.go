package cloud

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/layercache/layercache/internal/actionscache"
	"github.com/layercache/layercache/internal/artifact"
)

// ActionsStorage is the PostgreSQL index and S3 archive adapter for the
// Actions v1 protocol. Reservation metadata and chunks survive process loss;
// the final Actions index and generic artifact reference commit in one
// PostgreSQL transaction.
type ActionsStorage struct {
	store *Store
}

func NewActionsStorage(store *Store) (*ActionsStorage, error) {
	if store == nil || store.database == nil || store.blobs == nil {
		return nil, errors.New("cloud store is required for Actions cache storage")
	}
	return &ActionsStorage{store: store}, nil
}

func (storage *ActionsStorage) Close() error { return nil }

func (storage *ActionsStorage) Reserve(
	ctx context.Context,
	request actionscache.ReserveRequest,
) (actionscache.Reservation, error) {
	if err := storage.validateScope(request.Scope); err != nil {
		return actionscache.Reservation{}, err
	}
	if err := validateIdentity("Actions cache key", request.Key, 512); err != nil {
		return actionscache.Reservation{}, actionscache.ErrInvalidUpload
	}
	if err := validateIdentity("Actions cache version", request.Version, 512); err != nil {
		return actionscache.Reservation{}, actionscache.ErrInvalidUpload
	}
	maximum := request.MaxArtifactBytes
	quota, err := storage.store.Quota(ctx)
	if err != nil {
		return actionscache.Reservation{}, err
	}
	if maximum == 0 || maximum > quota.MaxBytes {
		maximum = quota.MaxBytes
	}
	if maximum <= 0 || request.CacheSize != nil && (*request.CacheSize < 0 || *request.CacheSize > maximum) {
		return actionscache.Reservation{}, actionscache.ErrInvalidUpload
	}
	if err := storage.expireIdentity(ctx, request.Scope, request.Key, request.Version); err != nil {
		return actionscache.Reservation{}, err
	}
	now := storage.store.config.Now().UTC()
	transaction, err := storage.store.database.BeginTx(ctx, nil)
	if err != nil {
		return actionscache.Reservation{}, storage.store.safeError("begin cloud Actions reservation", err)
	}
	defer transaction.Rollback()
	if _, err := storage.store.lockProject(ctx, transaction); err != nil {
		return actionscache.Reservation{}, err
	}
	var exists bool
	if err := transaction.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM layercache_actions_entries_v1
			WHERE project_id = $1 AND repository = $2 AND compatibility = $3
				AND ref_scope = $4 AND cache_key = $5 AND version = $6
		)`, storage.store.config.Project, request.Scope.Repository, request.Scope.Compatibility,
		request.Scope.Ref, request.Key, request.Version).Scan(&exists); err != nil {
		return actionscache.Reservation{}, storage.store.safeError("check cloud Actions cache identity", err)
	}
	if exists {
		return actionscache.Reservation{}, actionscache.ErrAlreadyExists
	}
	var reservationID int64
	err = transaction.QueryRowContext(ctx, `
		INSERT INTO layercache_actions_reservations_v1(
			project_id, repository, compatibility, ref_scope, cache_key, version,
			expected_size, maximum_size, created_at, expires_at
		) VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING reservation_id`, storage.store.config.Project, request.Scope.Repository,
		request.Scope.Compatibility, request.Scope.Ref, request.Key, request.Version,
		nullableInt64(request.CacheSize), maximum, now, now.Add(storage.store.config.StageTTL)).
		Scan(&reservationID)
	if isUniqueViolation(err) {
		return actionscache.Reservation{}, actionscache.ErrAlreadyExists
	}
	if err != nil {
		return actionscache.Reservation{}, storage.store.safeError("create cloud Actions reservation", err)
	}
	if err := transaction.Commit(); err != nil {
		return actionscache.Reservation{}, storage.store.safeError("commit cloud Actions reservation", err)
	}
	return actionscache.Reservation{ID: reservationID}, nil
}

func (storage *ActionsStorage) Upload(ctx context.Context, request actionscache.UploadRequest) error {
	if request.Body == nil || request.Start < 0 || request.End < request.Start || request.End-request.Start == math.MaxInt64 {
		return actionscache.ErrInvalidUpload
	}
	length := request.End - request.Start + 1
	reservation, err := storage.reservation(ctx, request.ReservationID)
	if err != nil {
		return err
	}
	if !actionsScopeMatches(request.Scope, reservation) || request.End >= reservation.maximumSize ||
		reservation.expectedSize.Valid && request.End >= reservation.expectedSize.Int64 ||
		!reservation.expiresAt.After(storage.store.config.Now().UTC()) {
		return actionscache.ErrInvalidUpload
	}
	random, err := randomToken("")
	if err != nil {
		return err
	}
	objectKey := fmt.Sprintf("projects/%s/actions-staging/%d/%020d-%020d-%s",
		storage.store.namespace, request.ReservationID, request.Start, request.End, random)
	staged, err := storage.store.blobs.Stage(ctx, objectKey, request.Body, length, "application/octet-stream")
	if errors.Is(err, ErrQuota) {
		return actionscache.ErrInvalidUpload
	}
	if err != nil {
		return storage.store.safeError("stage cloud Actions chunk", err)
	}
	keepObject := false
	defer func() {
		if !keepObject {
			_ = storage.store.blobs.Delete(context.WithoutCancel(ctx), objectKey)
		}
	}()
	if staged.Size != length {
		return actionscache.ErrInvalidUpload
	}

	transaction, err := storage.store.database.BeginTx(ctx, nil)
	if err != nil {
		return storage.store.safeError("begin cloud Actions chunk", err)
	}
	defer transaction.Rollback()
	reservation, err = storage.reservationTx(ctx, transaction, request.ReservationID, true)
	if err != nil {
		return err
	}
	if !actionsScopeMatches(request.Scope, reservation) || request.End >= reservation.maximumSize ||
		reservation.expectedSize.Valid && request.End >= reservation.expectedSize.Int64 ||
		!reservation.expiresAt.After(storage.store.config.Now().UTC()) {
		return actionscache.ErrInvalidUpload
	}
	var existingEnd, existingSize int64
	var existingDigest string
	err = transaction.QueryRowContext(ctx, `
		SELECT end_offset, digest, size_bytes
		FROM layercache_actions_chunks_v1
		WHERE reservation_id = $1 AND start_offset = $2`, request.ReservationID, request.Start).
		Scan(&existingEnd, &existingDigest, &existingSize)
	if err == nil {
		if existingEnd != request.End || existingSize != staged.Size || existingDigest != staged.Digest {
			return actionscache.ErrInvalidUpload
		}
		if err := transaction.Commit(); err != nil {
			return storage.store.safeError("commit idempotent cloud Actions chunk", err)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return storage.store.safeError("inspect cloud Actions chunk", err)
	}
	var overlap bool
	if err := transaction.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM layercache_actions_chunks_v1
			WHERE reservation_id = $1 AND start_offset <= $2 AND end_offset >= $3
		)`, request.ReservationID, request.End, request.Start).Scan(&overlap); err != nil {
		return storage.store.safeError("check cloud Actions chunk overlap", err)
	}
	if overlap {
		return actionscache.ErrInvalidUpload
	}
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO layercache_actions_chunks_v1(
			project_id, reservation_id, start_offset, end_offset, object_key,
			digest, size_bytes, created_at
		) VALUES($1, $2, $3, $4, $5, $6, $7, $8)`,
		storage.store.config.Project, request.ReservationID, request.Start, request.End,
		objectKey, staged.Digest, staged.Size, storage.store.config.Now().UTC()); err != nil {
		return storage.store.safeError("record cloud Actions chunk", err)
	}
	if err := transaction.Commit(); err != nil {
		// PostgreSQL can report a lost connection after COMMIT reached the
		// server. Keep the staged object until orphan collection can reconcile
		// it; deleting here could leave a committed chunk row pointing at
		// missing bytes.
		keepObject = true
		return storage.store.safeError("commit cloud Actions chunk", err)
	}
	keepObject = true
	return nil
}

func (storage *ActionsStorage) Commit(
	ctx context.Context,
	request actionscache.CommitRequest,
) (actionscache.Entry, error) {
	reservation, err := storage.reservation(ctx, request.ReservationID)
	if err != nil {
		return actionscache.Entry{}, err
	}
	if !actionsScopeMatches(request.Scope, reservation) || request.Size < 0 ||
		reservation.expectedSize.Valid && request.Size != reservation.expectedSize.Int64 ||
		request.Size > reservation.maximumSize || !reservation.expiresAt.After(storage.store.config.Now().UTC()) {
		return actionscache.Entry{}, actionscache.ErrInvalidUpload
	}
	chunks, err := storage.chunks(ctx, request.ReservationID)
	if err != nil {
		return actionscache.Entry{}, err
	}
	if err := validateChunks(chunks, request.Size); err != nil {
		return actionscache.Entry{}, err
	}
	origin, publicMetadata, err := actionscache.NormalizeCommitTrust(request)
	if err != nil {
		return actionscache.Entry{}, err
	}
	publicJSON, err := json.Marshal(publicMetadata)
	if err != nil {
		return actionscache.Entry{}, fmt.Errorf("encode cloud Actions Public metadata: %w", err)
	}
	if publicMetadata == nil {
		publicJSON = nil
	}
	key := storage.artifactKey(reservation)
	reader := &chunkSequenceReader{ctx: ctx, blobs: storage.store.blobs, chunks: chunks}
	defer reader.Close()
	expectedDigest := ""
	if publicMetadata != nil {
		expectedDigest = strings.TrimPrefix(publicMetadata.Digest, "sha256:")
	}
	createdAt := storage.store.config.Now().UTC()
	commitHook := func(ctx context.Context, transaction *sql.Tx, artifactEntry artifact.Entry) error {
		locked, err := storage.reservationTx(ctx, transaction, request.ReservationID, true)
		if err != nil {
			return err
		}
		if locked != reservation || !locked.expiresAt.After(storage.store.config.Now().UTC()) {
			return actionscache.ErrInvalidUpload
		}
		if artifactEntry.Size != request.Size || publicMetadata != nil &&
			(artifactEntry.Digest != expectedDigest || artifactEntry.Size != publicMetadata.Size) {
			return actionscache.ErrInvalidUpload
		}
		_, err = transaction.ExecContext(ctx, `
			INSERT INTO layercache_actions_entries_v1(
				entry_id, project_id, repository, compatibility, ref_scope, cache_key,
				version, artifact_native_key, digest, size_bytes, producer_duration_ns,
				origin, public_metadata_json, created_at
			) VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb, $14)
			ON CONFLICT(project_id, repository, compatibility, ref_scope, cache_key, version) DO NOTHING`,
			request.ReservationID, storage.store.config.Project, reservation.repository,
			reservation.compatibility, reservation.ref, reservation.key, reservation.version,
			key.Native, artifactEntry.Digest, artifactEntry.Size, nullableActionDuration(request.ProducerDuration),
			string(origin), nullableJSON(publicJSON), createdAt)
		if err != nil {
			return storage.store.safeError("record cloud Actions entry", err)
		}
		var storedDigest string
		if err := transaction.QueryRowContext(ctx, `
			SELECT digest FROM layercache_actions_entries_v1
			WHERE project_id = $1 AND repository = $2 AND compatibility = $3
				AND ref_scope = $4 AND cache_key = $5 AND version = $6`,
			storage.store.config.Project, reservation.repository, reservation.compatibility,
			reservation.ref, reservation.key, reservation.version).Scan(&storedDigest); err != nil {
			return storage.store.safeError("confirm cloud Actions entry", err)
		}
		if storedDigest != artifactEntry.Digest {
			return actionscache.ErrAlreadyExists
		}
		if _, err := transaction.ExecContext(ctx, `
			DELETE FROM layercache_actions_reservations_v1
			WHERE project_id = $1 AND reservation_id = $2`,
			storage.store.config.Project, request.ReservationID); err != nil {
			return storage.store.safeError("complete cloud Actions reservation", err)
		}
		return nil
	}
	artifactEntry, _, err := storage.store.putWithCommit(
		ctx, key, artifact.Metadata{Values: map[string]string{"actionsRepository": reservation.repository}},
		reader, expectedDigest, commitHook,
	)
	if errors.Is(err, ErrConflict) {
		return actionscache.Entry{}, actionscache.ErrAlreadyExists
	}
	if err != nil {
		return actionscache.Entry{}, err
	}
	storage.deleteChunkObjects(chunks)
	return actionscache.Entry{
		ID: request.ReservationID, Key: reservation.key, Version: reservation.version,
		Ref: reservation.ref, Size: artifactEntry.Size, CreatedAt: createdAt,
		ProducerDuration: cloneActionDuration(request.ProducerDuration),
		Origin:           origin, Public: actionscache.ClonePublicEntryMetadata(publicMetadata),
	}, nil
}

func (storage *ActionsStorage) Lookup(
	ctx context.Context,
	request actionscache.LookupRequest,
) (actionscache.LookupResult, error) {
	if err := storage.validateScope(request.Scope); err != nil {
		return actionscache.LookupResult{}, err
	}
	if err := validateIdentity("Actions cache version", request.Version, 512); err != nil || len(request.Keys) == 0 {
		return actionscache.LookupResult{}, actionscache.ErrNotFound
	}
	for _, key := range request.Keys {
		if err := validateIdentity("Actions cache lookup key", key, 512); err != nil {
			return actionscache.LookupResult{}, actionscache.ErrNotFound
		}
	}
	refs := []struct {
		value string
		scope actionscache.RefScope
	}{{request.Scope.Ref, actionscache.RefScopeCurrent}}
	if request.Scope.DefaultRef != request.Scope.Ref {
		refs = append(refs, struct {
			value string
			scope actionscache.RefScope
		}{request.Scope.DefaultRef, actionscache.RefScopeDefault})
	}
	for _, ref := range refs {
		for _, requestedKey := range request.Keys {
			entry, found, err := storage.lookupCandidate(ctx, request, ref.value, requestedKey, true)
			if err != nil {
				return actionscache.LookupResult{}, err
			}
			if found {
				return actionscache.LookupResult{
					Entry: entry, Match: actionscache.MatchExact, RequestedKey: requestedKey,
					RefScope: ref.scope, Source: actionscache.SourceTeamCache,
				}, nil
			}
			entry, found, err = storage.lookupCandidate(ctx, request, ref.value, requestedKey, false)
			if err != nil {
				return actionscache.LookupResult{}, err
			}
			if found {
				return actionscache.LookupResult{
					Entry: entry, Match: actionscache.MatchPrefix, RequestedKey: requestedKey,
					RefScope: ref.scope, Source: actionscache.SourceTeamCache,
				}, nil
			}
		}
	}
	return actionscache.LookupResult{}, actionscache.ErrNotFound
}

func (storage *ActionsStorage) Open(
	ctx context.Context,
	request actionscache.OpenRequest,
) (actionscache.Archive, error) {
	if err := storage.validateScope(request.Scope); err != nil {
		return actionscache.Archive{}, err
	}
	entry, nativeKey, err := storage.entryByID(ctx, request)
	if err != nil {
		return actionscache.Archive{}, err
	}
	key := artifact.Key{
		Integration: "actions", Project: storage.store.config.Project,
		Compatibility: request.Scope.Compatibility, Native: nativeKey,
		Version: entry.Version, Ref: entry.Ref,
	}
	artifactEntry, body, err := storage.store.Get(ctx, key)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrCorrupt) {
		_ = storage.InvalidateEntry(ctx, entry.ID)
		return actionscache.Archive{}, actionscache.ErrNotFound
	}
	if err != nil {
		return actionscache.Archive{}, err
	}
	if artifactEntry.Size != entry.Size || entry.Public != nil &&
		(artifactEntry.Digest != strings.TrimPrefix(entry.Public.Digest, "sha256:") || artifactEntry.Size != entry.Public.Size) {
		_ = body.Close()
		_ = storage.InvalidateEntry(ctx, entry.ID)
		return actionscache.Archive{}, actionscache.ErrNotFound
	}
	return actionscache.Archive{Entry: entry, Body: body}, nil
}

func (storage *ActionsStorage) Abort(ctx context.Context, reservationID int64) error {
	return storage.AbortScoped(ctx, reservationID, nil)
}

func (storage *ActionsStorage) AbortScoped(
	ctx context.Context,
	reservationID int64,
	scope *actionscache.Scope,
) error {
	reservation, err := storage.reservation(ctx, reservationID)
	if err != nil {
		return err
	}
	if !actionsScopeMatches(scope, reservation) {
		return actionscache.ErrNotFound
	}
	chunks, err := storage.chunks(ctx, reservationID)
	if err != nil {
		return err
	}
	transaction, err := storage.store.database.BeginTx(ctx, nil)
	if err != nil {
		return storage.store.safeError("begin cloud Actions abort", err)
	}
	defer transaction.Rollback()
	if _, err := storage.reservationTx(ctx, transaction, reservationID, true); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `
		DELETE FROM layercache_actions_reservations_v1
		WHERE project_id = $1 AND reservation_id = $2`, storage.store.config.Project, reservationID); err != nil {
		return storage.store.safeError("abort cloud Actions reservation", err)
	}
	if err := transaction.Commit(); err != nil {
		return storage.store.safeError("commit cloud Actions abort", err)
	}
	storage.deleteChunkObjects(chunks)
	key := storage.artifactKey(reservation)
	if err := storage.store.Delete(context.WithoutCancel(ctx), key); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

func (storage *ActionsStorage) InvalidateEntry(ctx context.Context, id int64) error {
	var reservation cloudActionsReservation
	var nativeKey string
	err := storage.store.database.QueryRowContext(ctx, `
		SELECT repository, compatibility, ref_scope, cache_key, version, artifact_native_key
		FROM layercache_actions_entries_v1
		WHERE project_id = $1 AND entry_id = $2`, storage.store.config.Project, id).
		Scan(&reservation.repository, &reservation.compatibility, &reservation.ref,
			&reservation.key, &reservation.version, &nativeKey)
	if errors.Is(err, sql.ErrNoRows) {
		return actionscache.ErrNotFound
	}
	if err != nil {
		return storage.store.safeError("read cloud Actions invalidation target", err)
	}
	key := storage.artifactKey(reservation)
	key.Native = nativeKey
	if err := storage.store.Delete(ctx, key); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if _, err := storage.store.database.ExecContext(ctx, `
		DELETE FROM layercache_actions_entries_v1 WHERE project_id = $1 AND entry_id = $2`,
		storage.store.config.Project, id); err != nil {
		return storage.store.safeError("invalidate cloud Actions entry", err)
	}
	return nil
}

func (storage *ActionsStorage) UpdatePublicEntryMetadata(
	ctx context.Context,
	id int64,
	metadata *actionscache.PublicEntryMetadata,
) error {
	if metadata == nil {
		return actionscache.ErrInvalidUpload
	}
	var size int64
	if err := storage.store.database.QueryRowContext(ctx, `
		SELECT size_bytes FROM layercache_actions_entries_v1
		WHERE project_id = $1 AND entry_id = $2`, storage.store.config.Project, id).Scan(&size); errors.Is(err, sql.ErrNoRows) {
		return actionscache.ErrNotFound
	} else if err != nil {
		return storage.store.safeError("read cloud Actions trust metadata target", err)
	}
	_, normalized, err := actionscache.NormalizeCommitTrust(actionscache.CommitRequest{
		Size: size, Origin: actionscache.SourcePublicCache, Public: metadata,
	})
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return err
	}
	result, err := storage.store.database.ExecContext(ctx, `
		UPDATE layercache_actions_entries_v1
		SET origin = 'publicCache', public_metadata_json = $1::jsonb
		WHERE project_id = $2 AND entry_id = $3`, string(encoded), storage.store.config.Project, id)
	if err != nil {
		return storage.store.safeError("update cloud Actions trust metadata", err)
	}
	updated, _ := result.RowsAffected()
	if updated == 0 {
		return actionscache.ErrNotFound
	}
	return nil
}

func (storage *ActionsStorage) Maintain(ctx context.Context) (int, error) {
	now := storage.store.config.Now().UTC()
	rows, err := storage.store.database.QueryContext(ctx, `
		SELECT reservation_id FROM layercache_actions_reservations_v1
		WHERE project_id = $1 AND expires_at <= $2
		ORDER BY expires_at LIMIT 256`, storage.store.config.Project, now)
	if err != nil {
		return 0, storage.store.safeError("list expired cloud Actions reservations", err)
	}
	var reservations []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, storage.store.safeError("decode expired cloud Actions reservation", err)
		}
		reservations = append(reservations, id)
	}
	if err := rows.Close(); err != nil {
		return 0, storage.store.safeError("close expired cloud Actions reservations", err)
	}
	removed := 0
	for _, id := range reservations {
		if err := storage.Abort(ctx, id); err == nil {
			removed++
		} else if !errors.Is(err, actionscache.ErrNotFound) {
			return removed, err
		}
	}
	orphans, err := storage.deleteOrphanedChunks(ctx, now.Add(-storage.store.config.StageTTL))
	if err != nil {
		return removed, err
	}
	removed += orphans
	return removed, nil
}

func (storage *ActionsStorage) deleteOrphanedChunks(ctx context.Context, cutoff time.Time) (int, error) {
	prefix := "projects/" + storage.store.namespace + "/actions-staging/"
	removed := 0
	for listed := range storage.store.blobs.List(ctx, prefix) {
		if listed.Err != nil {
			return removed, storage.store.safeError("list orphaned cloud Actions chunks", listed.Err)
		}
		if listed.Info.LastModified.After(cutoff) {
			continue
		}
		transaction, err := storage.store.database.BeginTx(ctx, nil)
		if err != nil {
			return removed, storage.store.safeError("begin cloud Actions orphan collection", err)
		}
		if _, err := storage.store.lockProject(ctx, transaction); err != nil {
			_ = transaction.Rollback()
			return removed, err
		}
		var present bool
		err = transaction.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM layercache_actions_chunks_v1
				WHERE project_id = $1 AND object_key = $2
			)`, storage.store.config.Project, listed.Info.Key).Scan(&present)
		if err != nil {
			_ = transaction.Rollback()
			return removed, storage.store.safeError("check cloud Actions chunk reference", err)
		}
		if present {
			_ = transaction.Rollback()
			continue
		}
		if err := storage.store.blobs.Delete(ctx, listed.Info.Key); err != nil {
			_ = transaction.Rollback()
			return removed, storage.store.safeError("delete orphaned cloud Actions chunk", err)
		}
		if err := transaction.Commit(); err != nil {
			return removed, storage.store.safeError("commit cloud Actions orphan collection", err)
		}
		removed++
	}
	return removed, nil
}

type cloudActionsReservation struct {
	id            int64
	repository    string
	compatibility string
	ref           string
	key           string
	version       string
	expectedSize  sql.NullInt64
	maximumSize   int64
	createdAt     time.Time
	expiresAt     time.Time
}

type cloudActionsChunk struct {
	start, end int64
	objectKey  string
	digest     string
	size       int64
}

func (storage *ActionsStorage) reservation(ctx context.Context, id int64) (cloudActionsReservation, error) {
	return storage.reservationQuery(ctx, storage.store.database, id, false)
}

func (storage *ActionsStorage) reservationTx(
	ctx context.Context,
	transaction *sql.Tx,
	id int64,
	lock bool,
) (cloudActionsReservation, error) {
	return storage.reservationQuery(ctx, transaction, id, lock)
}

type actionsReservationQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (storage *ActionsStorage) reservationQuery(
	ctx context.Context,
	query actionsReservationQueryer,
	id int64,
	lock bool,
) (cloudActionsReservation, error) {
	statement := `
		SELECT reservation_id, repository, compatibility, ref_scope, cache_key,
			version, expected_size, maximum_size, created_at, expires_at
		FROM layercache_actions_reservations_v1
		WHERE project_id = $1 AND reservation_id = $2`
	if lock {
		statement += ` FOR UPDATE`
	}
	var reservation cloudActionsReservation
	err := query.QueryRowContext(ctx, statement, storage.store.config.Project, id).Scan(
		&reservation.id, &reservation.repository, &reservation.compatibility, &reservation.ref,
		&reservation.key, &reservation.version, &reservation.expectedSize, &reservation.maximumSize,
		&reservation.createdAt, &reservation.expiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return cloudActionsReservation{}, actionscache.ErrNotFound
	}
	if err != nil {
		return cloudActionsReservation{}, storage.store.safeError("read cloud Actions reservation", err)
	}
	reservation.createdAt = reservation.createdAt.UTC()
	reservation.expiresAt = reservation.expiresAt.UTC()
	return reservation, nil
}

func (storage *ActionsStorage) chunks(ctx context.Context, reservationID int64) ([]cloudActionsChunk, error) {
	rows, err := storage.store.database.QueryContext(ctx, `
		SELECT start_offset, end_offset, object_key, digest, size_bytes
		FROM layercache_actions_chunks_v1
		WHERE project_id = $1 AND reservation_id = $2
		ORDER BY start_offset`, storage.store.config.Project, reservationID)
	if err != nil {
		return nil, storage.store.safeError("read cloud Actions chunks", err)
	}
	defer rows.Close()
	var chunks []cloudActionsChunk
	for rows.Next() {
		var chunk cloudActionsChunk
		if err := rows.Scan(&chunk.start, &chunk.end, &chunk.objectKey, &chunk.digest, &chunk.size); err != nil {
			return nil, storage.store.safeError("decode cloud Actions chunk", err)
		}
		chunks = append(chunks, chunk)
	}
	if err := rows.Err(); err != nil {
		return nil, storage.store.safeError("iterate cloud Actions chunks", err)
	}
	return chunks, nil
}

type cloudActionsEntry struct {
	entry     actionscache.Entry
	nativeKey string
}

func (storage *ActionsStorage) lookupCandidate(
	ctx context.Context,
	request actionscache.LookupRequest,
	ref string,
	requestedKey string,
	exact bool,
) (actionscache.Entry, bool, error) {
	for {
		candidates, found, err := storage.queryCandidates(ctx, request, ref, requestedKey, exact, 64)
		if err != nil || !found {
			return actionscache.Entry{}, false, err
		}
		for _, candidate := range candidates {
			available, err := storage.actionsEntryAvailable(ctx, request, candidate)
			if err != nil {
				return actionscache.Entry{}, false, err
			}
			if available {
				return candidate.entry, true, nil
			}
		}
		// Every selected candidate was invalidated. Query again so a corrupt
		// newest prefix cannot hide the next healthy match.
	}
}

func (storage *ActionsStorage) queryCandidates(
	ctx context.Context,
	request actionscache.LookupRequest,
	ref string,
	requestedKey string,
	exact bool,
	limit int,
) ([]cloudActionsEntry, bool, error) {
	keyPredicate := `cache_key = $6`
	if !exact {
		keyPredicate = `LEFT(cache_key, LENGTH($6)) = $6`
	}
	allowPublic := exact && len(request.Keys) > 0 && requestedKey == request.Keys[0]
	rows, err := storage.store.database.QueryContext(ctx, `
		SELECT entry_id, cache_key, version, ref_scope, artifact_native_key,
			size_bytes, created_at, producer_duration_ns, origin, public_metadata_json
		FROM layercache_actions_entries_v1
		WHERE project_id = $1 AND repository = $2 AND compatibility = $3
			AND ref_scope = $4 AND version = $5 AND `+keyPredicate+`
			AND (public_metadata_json IS NULL OR $8)
		ORDER BY created_at DESC, entry_id DESC
		LIMIT $7`, storage.store.config.Project, request.Scope.Repository,
		request.Scope.Compatibility, ref, request.Version, requestedKey, limit, allowPublic)
	if err != nil {
		return nil, false, storage.store.safeError("lookup cloud Actions entries", err)
	}
	defer rows.Close()
	var entries []cloudActionsEntry
	for rows.Next() {
		var candidate cloudActionsEntry
		var origin string
		var producerDuration sql.NullInt64
		var publicJSON []byte
		if err := rows.Scan(
			&candidate.entry.ID, &candidate.entry.Key, &candidate.entry.Version, &candidate.entry.Ref,
			&candidate.nativeKey, &candidate.entry.Size, &candidate.entry.CreatedAt, &producerDuration, &origin, &publicJSON,
		); err != nil {
			return nil, false, storage.store.safeError("decode cloud Actions entry", err)
		}
		candidate.entry.Origin = actionscache.CacheSource(origin)
		candidate.entry.CreatedAt = candidate.entry.CreatedAt.UTC()
		candidate.entry.ProducerDuration = actionDurationFromNull(producerDuration)
		if len(publicJSON) > 0 {
			var metadata actionscache.PublicEntryMetadata
			if err := json.Unmarshal(publicJSON, &metadata); err != nil {
				return nil, false, fmt.Errorf("decode cloud Actions Public metadata: %w", err)
			}
			candidate.entry.Public = &metadata
		}
		entries = append(entries, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, false, storage.store.safeError("iterate cloud Actions entries", err)
	}
	if err := rows.Close(); err != nil {
		return nil, false, storage.store.safeError("close cloud Actions entries", err)
	}
	return entries, len(entries) > 0, nil
}

func (storage *ActionsStorage) actionsEntryAvailable(
	ctx context.Context,
	request actionscache.LookupRequest,
	candidate cloudActionsEntry,
) (bool, error) {
	entry := candidate.entry
	key := artifact.Key{
		Integration: "actions", Project: storage.store.config.Project,
		Compatibility: request.Scope.Compatibility, Native: candidate.nativeKey,
		Version: entry.Version, Ref: entry.Ref,
	}
	artifactEntry, body, err := storage.store.Get(ctx, key)
	if err == nil {
		_, err = io.Copy(io.Discard, body)
		closeErr := body.Close()
		if err == nil {
			err = closeErr
		}
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrCorrupt) {
		if invalidateErr := storage.InvalidateEntry(ctx, entry.ID); invalidateErr != nil &&
			!errors.Is(invalidateErr, actionscache.ErrNotFound) {
			return false, invalidateErr
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if entry.Public != nil && (artifactEntry.Digest != strings.TrimPrefix(entry.Public.Digest, "sha256:") || artifactEntry.Size != entry.Public.Size) {
		if invalidateErr := storage.InvalidateEntry(ctx, entry.ID); invalidateErr != nil &&
			!errors.Is(invalidateErr, actionscache.ErrNotFound) {
			return false, invalidateErr
		}
		return false, nil
	}
	return true, nil
}

func (storage *ActionsStorage) entryByID(
	ctx context.Context,
	request actionscache.OpenRequest,
) (actionscache.Entry, string, error) {
	var entry actionscache.Entry
	var repository, compatibility, nativeKey, origin string
	var producerDuration sql.NullInt64
	var publicJSON []byte
	err := storage.store.database.QueryRowContext(ctx, `
		SELECT entry_id, repository, compatibility, cache_key, version, ref_scope,
			artifact_native_key, size_bytes, created_at, producer_duration_ns, origin, public_metadata_json
		FROM layercache_actions_entries_v1
		WHERE project_id = $1 AND entry_id = $2 AND repository = $3 AND compatibility = $4
			AND (ref_scope = $5 OR ref_scope = $6)`, storage.store.config.Project,
		request.ID, request.Scope.Repository, request.Scope.Compatibility,
		request.Scope.Ref, request.Scope.DefaultRef).Scan(
		&entry.ID, &repository, &compatibility, &entry.Key, &entry.Version, &entry.Ref,
		&nativeKey, &entry.Size, &entry.CreatedAt, &producerDuration, &origin, &publicJSON,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return actionscache.Entry{}, "", actionscache.ErrNotFound
	}
	if err != nil {
		return actionscache.Entry{}, "", storage.store.safeError("open cloud Actions entry", err)
	}
	entry.Origin = actionscache.CacheSource(origin)
	entry.CreatedAt = entry.CreatedAt.UTC()
	entry.ProducerDuration = actionDurationFromNull(producerDuration)
	if len(publicJSON) > 0 {
		var metadata actionscache.PublicEntryMetadata
		if err := json.Unmarshal(publicJSON, &metadata); err != nil {
			return actionscache.Entry{}, "", fmt.Errorf("decode cloud Actions Public metadata: %w", err)
		}
		entry.Public = &metadata
	}
	return entry, nativeKey, nil
}

func (storage *ActionsStorage) artifactKey(reservation cloudActionsReservation) artifact.Key {
	digest := sha256.Sum256([]byte(reservation.repository + "\x00" + reservation.key))
	return artifact.Key{
		Integration: "actions", Project: storage.store.config.Project,
		Compatibility: reservation.compatibility, Native: "actions-" + hex.EncodeToString(digest[:]),
		Version: reservation.version, Ref: reservation.ref,
	}
}

func (storage *ActionsStorage) validateScope(scope actionscache.Scope) error {
	for name, value := range map[string]string{
		"Actions repository":    scope.Repository,
		"Actions compatibility": scope.Compatibility,
		"Actions ref":           scope.Ref,
		"Actions default ref":   scope.DefaultRef,
	} {
		if err := validateIdentity(name, value, 1024); err != nil {
			return actionscache.ErrInvalidUpload
		}
	}
	return nil
}

func (storage *ActionsStorage) expireIdentity(
	ctx context.Context,
	scope actionscache.Scope,
	key, version string,
) error {
	var reservationID int64
	err := storage.store.database.QueryRowContext(ctx, `
		SELECT reservation_id FROM layercache_actions_reservations_v1
		WHERE project_id = $1 AND repository = $2 AND compatibility = $3
			AND ref_scope = $4 AND cache_key = $5 AND version = $6 AND expires_at <= $7`,
		storage.store.config.Project, scope.Repository, scope.Compatibility, scope.Ref,
		key, version, storage.store.config.Now().UTC()).Scan(&reservationID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return storage.store.safeError("inspect expired cloud Actions identity", err)
	}
	return storage.Abort(ctx, reservationID)
}

func (storage *ActionsStorage) deleteChunkObjects(chunks []cloudActionsChunk) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, chunk := range chunks {
		_ = storage.store.blobs.Delete(ctx, chunk.objectKey)
	}
}

func actionsScopeMatches(scope *actionscache.Scope, reservation cloudActionsReservation) bool {
	return scope == nil || scope.Repository == reservation.repository &&
		scope.Compatibility == reservation.compatibility && scope.Ref == reservation.ref
}

func validateChunks(chunks []cloudActionsChunk, size int64) error {
	if size == 0 && len(chunks) == 0 {
		return nil
	}
	offset := int64(0)
	for _, chunk := range chunks {
		if chunk.start != offset || chunk.end < chunk.start || chunk.size != chunk.end-chunk.start+1 {
			return actionscache.ErrIncompleteUpload
		}
		offset = chunk.end + 1
	}
	if offset != size {
		return actionscache.ErrIncompleteUpload
	}
	return nil
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableActionDuration(value *time.Duration) any {
	if value == nil {
		return nil
	}
	return int64(*value)
}

func cloneActionDuration(value *time.Duration) *time.Duration {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func actionDurationFromNull(value sql.NullInt64) *time.Duration {
	if !value.Valid || value.Int64 < 0 {
		return nil
	}
	duration := time.Duration(value.Int64)
	return &duration
}

func nullableJSON(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return string(value)
}

func isUniqueViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}

type chunkSequenceReader struct {
	ctx     context.Context
	blobs   blobStore
	chunks  []cloudActionsChunk
	index   int
	current io.ReadCloser
}

func (reader *chunkSequenceReader) Read(buffer []byte) (int, error) {
	for {
		if reader.current == nil {
			if reader.index >= len(reader.chunks) {
				return 0, io.EOF
			}
			chunk := reader.chunks[reader.index]
			body, err := reader.blobs.Open(reader.ctx, chunk.objectKey, chunk.digest, chunk.size)
			if err != nil {
				return 0, err
			}
			reader.current = body
		}
		count, err := reader.current.Read(buffer)
		if errors.Is(err, io.EOF) {
			closeErr := reader.current.Close()
			reader.current = nil
			reader.index++
			if count > 0 {
				return count, closeErr
			}
			if closeErr != nil {
				return 0, closeErr
			}
			continue
		}
		return count, err
	}
}

func (reader *chunkSequenceReader) Close() error {
	if reader.current == nil {
		return nil
	}
	err := reader.current.Close()
	reader.current = nil
	return err
}

var _ actionscache.StorageIndex = (*ActionsStorage)(nil)
