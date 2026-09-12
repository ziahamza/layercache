package cloud

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/artifact"
)

type projectUsage struct {
	quota         int64
	metadataQuota int64
	used          int64
	metadataUsed  int64
}

func (store *Store) Put(ctx context.Context, key artifact.Key, metadata artifact.Metadata, body io.Reader) (artifact.Entry, bool, error) {
	return store.putWithCommit(ctx, key, metadata, body, "", nil)
}

func (store *Store) PutVerified(
	ctx context.Context,
	key artifact.Key,
	metadata artifact.Metadata,
	body io.Reader,
	expectedDigest string,
) (artifact.Entry, bool, error) {
	return store.putWithCommit(ctx, key, metadata, body, strings.TrimPrefix(expectedDigest, "sha256:"), nil)
}

type artifactCommitHook func(context.Context, *sql.Tx, artifact.Entry) error

func (store *Store) putWithCommit(
	ctx context.Context,
	key artifact.Key,
	metadata artifact.Metadata,
	body io.Reader,
	expectedDigest string,
	commitHook artifactCommitHook,
) (artifact.Entry, bool, error) {
	if err := store.validateKey(key); err != nil {
		return artifact.Entry{}, false, err
	}
	if expectedDigest != "" && !validDigest(expectedDigest) {
		return artifact.Entry{}, false, ErrCorrupt
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return artifact.Entry{}, false, fmt.Errorf("encode cloud artifact metadata: %w", err)
	}
	if len(metadataJSON) > 64<<10 {
		return artifact.Entry{}, false, errors.New("cloud artifact metadata exceeds 64 KiB")
	}
	mediaType := "application/octet-stream"
	if candidate := metadata.Values["mediaType"]; candidate != "" {
		if err := validateIdentity("artifact media type", candidate, 256); err != nil {
			return artifact.Entry{}, false, err
		}
		mediaType = candidate
	}

	uploadID, err := randomToken("upload-")
	if err != nil {
		return artifact.Entry{}, false, err
	}
	stageKey := "projects/" + store.namespace + "/staging/" + uploadID
	now := store.config.Now().UTC()
	if _, err := store.database.ExecContext(ctx, `
		INSERT INTO layercache_uploads_v1(
			upload_id, project_id, object_key, state, created_at, expires_at
		) VALUES($1, $2, $3, 'uploading', $4, $5)`,
		uploadID, store.config.Project, stageKey, now, now.Add(store.config.StageTTL)); err != nil {
		return artifact.Entry{}, false, store.safeError("record cloud staged upload", err)
	}
	defer store.discardStage(uploadID, stageKey)

	quota, err := store.Quota(ctx)
	if err != nil {
		return artifact.Entry{}, false, err
	}
	staged, err := store.blobs.Stage(ctx, stageKey, body, quota.MaxBytes, mediaType)
	if err != nil {
		if errors.Is(err, ErrQuota) {
			return artifact.Entry{}, false, ErrQuota
		}
		return artifact.Entry{}, false, store.safeError("stage cloud artifact", err)
	}
	if expectedDigest != "" && staged.Digest != expectedDigest {
		return artifact.Entry{}, false, ErrCorrupt
	}
	if _, err := store.database.ExecContext(ctx, `
		UPDATE layercache_uploads_v1
		SET state = 'staged', digest = $1, size_bytes = $2
		WHERE upload_id = $3 AND project_id = $4`,
		staged.Digest, staged.Size, uploadID, store.config.Project); err != nil {
		return artifact.Entry{}, false, store.safeError("finalize cloud staged upload", err)
	}

	transaction, err := store.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return artifact.Entry{}, false, store.safeError("begin cloud artifact publication", err)
	}
	defer transaction.Rollback()
	usage, err := store.lockProject(ctx, transaction)
	if err != nil {
		return artifact.Entry{}, false, err
	}
	existing, err := readEntry(ctx, transaction, key)
	if err == nil {
		if existing.Digest != staged.Digest {
			if auditErr := store.appendAuditTx(ctx, transaction, AuditEvent{
				Project: key.Project, Actor: cloudRequestActor(ctx), Action: "cache.publish",
				Resource: cacheEntryAuditResource(key), Outcome: "rejected",
				Attributes: map[string]string{"integration": key.Integration, "reason": "immutable-conflict"},
				CreatedAt:  store.config.Now().UTC(),
			}); auditErr != nil {
				return artifact.Entry{}, false, auditErr
			}
			if commitErr := transaction.Commit(); commitErr != nil {
				return artifact.Entry{}, false, store.safeError("commit rejected cloud publication audit", commitErr)
			}
			return artifact.Entry{}, false, ErrConflict
		}
		if existing.Size != staged.Size {
			return artifact.Entry{}, false, ErrCorrupt
		}
		// The metadata winner does not prove the canonical object still exists
		// or is healthy. This complete, digest-verified upload repairs it before
		// an idempotent publication succeeds.
		if err := store.blobs.Commit(ctx, staged, store.blobKey(staged.Digest)); err != nil {
			return artifact.Entry{}, false, store.safeError("repair immutable cloud artifact", err)
		}
		if commitHook != nil {
			if err := commitHook(ctx, transaction, existing); err != nil {
				return artifact.Entry{}, false, err
			}
		}
		if err := store.appendAuditTx(ctx, transaction, AuditEvent{
			Project: key.Project, Actor: cloudRequestActor(ctx), Action: "cache.publish",
			Resource: cacheEntryAuditResource(key), Outcome: "allowed",
			Attributes: map[string]string{
				"created": "false", "digest": existing.Digest, "integration": key.Integration,
			}, CreatedAt: store.config.Now().UTC(),
		}); err != nil {
			return artifact.Entry{}, false, err
		}
		if err := transaction.Commit(); err != nil {
			return artifact.Entry{}, false, store.safeError("commit idempotent cloud publication", err)
		}
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return artifact.Entry{}, false, store.safeError("inspect cloud publication winner", err)
	}

	metadataBytes := estimateMetadataBytes(key, metadataJSON)
	if metadataBytes > usage.metadataQuota || staged.Size > usage.quota {
		return artifact.Entry{}, false, ErrQuota
	}
	var artifactSize, references int64
	err = transaction.QueryRowContext(ctx, `
		SELECT size_bytes, ref_count
		FROM layercache_artifacts_v1
		WHERE project_id = $1 AND digest = $2`, key.Project, staged.Digest).
		Scan(&artifactSize, &references)
	blobCharge := staged.Size
	if err == nil {
		if artifactSize != staged.Size {
			return artifact.Entry{}, false, ErrCorrupt
		}
		if references > 0 {
			blobCharge = 0
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return artifact.Entry{}, false, store.safeError("inspect cloud artifact", err)
	}
	if err := store.evictToFit(ctx, transaction, &usage, blobCharge, metadataBytes, staged.Digest); err != nil {
		return artifact.Entry{}, false, err
	}

	objectKey := store.blobKey(staged.Digest)
	if err := store.blobs.Commit(ctx, staged, objectKey); err != nil {
		return artifact.Entry{}, false, store.safeError("commit immutable cloud artifact", err)
	}
	createdAt := store.config.Now().UTC()
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO layercache_artifacts_v1(
			project_id, digest, size_bytes, media_type, object_key, ref_count,
			created_at, last_accessed_at, unreferenced_at
		) VALUES($1, $2, $3, $4, $5, 1, $6, $6, NULL)
		ON CONFLICT(project_id, digest) DO UPDATE
		SET ref_count = layercache_artifacts_v1.ref_count + 1,
			last_accessed_at = EXCLUDED.last_accessed_at,
			unreferenced_at = NULL
		WHERE layercache_artifacts_v1.size_bytes = EXCLUDED.size_bytes`,
		key.Project, staged.Digest, staged.Size, mediaType, objectKey, createdAt); err != nil {
		return artifact.Entry{}, false, store.safeError("record immutable cloud artifact", err)
	}
	result, err := transaction.ExecContext(ctx, `
		INSERT INTO layercache_entries_v1(
			project_id, integration, compatibility, native_key, version, ref_scope,
			digest, size_bytes, metadata_json, metadata_bytes, created_at, last_accessed_at
		) VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $11)
		ON CONFLICT(project_id, integration, compatibility, native_key, version, ref_scope) DO NOTHING`,
		key.Project, key.Integration, key.Compatibility, key.Native, key.Version, key.Ref,
		staged.Digest, staged.Size, string(metadataJSON), metadataBytes, createdAt)
	if err != nil {
		return artifact.Entry{}, false, store.safeError("record cloud cache entry", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return artifact.Entry{}, false, store.safeError("confirm cloud cache publication", err)
	}
	if inserted != 1 {
		return artifact.Entry{}, false, ErrConflict
	}
	committedEntry := artifact.Entry{
		Key: key, Digest: staged.Digest, Size: staged.Size, Metadata: metadata, CreatedAt: createdAt,
	}
	if commitHook != nil {
		if err := commitHook(ctx, transaction, committedEntry); err != nil {
			return artifact.Entry{}, false, err
		}
	}
	if err := store.appendAuditTx(ctx, transaction, AuditEvent{
		Project: key.Project, Actor: cloudRequestActor(ctx), Action: "cache.publish",
		Resource: cacheEntryAuditResource(key), Outcome: "allowed",
		Attributes: map[string]string{
			"created": "true", "digest": committedEntry.Digest, "integration": key.Integration,
		}, CreatedAt: createdAt,
	}); err != nil {
		return artifact.Entry{}, false, err
	}
	if _, err := transaction.ExecContext(ctx, `
		UPDATE layercache_projects_v1
		SET used_bytes = used_bytes + $1,
			metadata_bytes = metadata_bytes + $2,
			updated_at = $3
		WHERE project_id = $4`, blobCharge, metadataBytes, createdAt, key.Project); err != nil {
		return artifact.Entry{}, false, store.safeError("account cloud cache publication", err)
	}
	if err := transaction.Commit(); err != nil {
		return artifact.Entry{}, false, store.safeError("commit cloud cache publication", err)
	}
	return committedEntry, true, nil
}

func (store *Store) Get(ctx context.Context, key artifact.Key) (artifact.Entry, io.ReadCloser, error) {
	entry, leaseID, err := store.readEntryWithLease(ctx, key)
	if err != nil {
		return artifact.Entry{}, nil, err
	}
	body, err := store.blobs.Open(ctx, store.blobKey(entry.Digest), entry.Digest, entry.Size)
	if err != nil {
		store.releaseReadLease(leaseID)
		if errors.Is(err, ErrCorrupt) {
			return artifact.Entry{}, nil, ErrCorrupt
		}
		return artifact.Entry{}, nil, store.safeError("open cloud artifact", err)
	}
	return entry, &leasedReadCloser{
		body: body, store: store, leaseID: leaseID, digest: entry.Digest,
		nextRenewal: store.config.Now().UTC().Add(readLeaseTTL / 2),
	}, nil
}

func (store *Store) Head(ctx context.Context, key artifact.Key) (artifact.Entry, error) {
	if err := store.validateKey(key); err != nil {
		return artifact.Entry{}, err
	}
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return artifact.Entry{}, store.safeError("begin cloud cache lookup", err)
	}
	defer transaction.Rollback()
	if _, err := store.lockProjectShared(ctx, transaction); err != nil {
		return artifact.Entry{}, err
	}
	entry, err := readEntry(ctx, transaction, key)
	if errors.Is(err, sql.ErrNoRows) {
		return artifact.Entry{}, ErrNotFound
	}
	if err != nil {
		return artifact.Entry{}, store.safeError("read cloud cache entry", err)
	}
	now := store.config.Now().UTC()
	if _, err := transaction.ExecContext(ctx, `
		UPDATE layercache_entries_v1 SET last_accessed_at = $1
		WHERE project_id = $2 AND integration = $3 AND compatibility = $4
			AND native_key = $5 AND version = $6 AND ref_scope = $7`,
		now, key.Project, key.Integration, key.Compatibility, key.Native, key.Version, key.Ref); err != nil {
		return artifact.Entry{}, store.safeError("touch cloud cache entry", err)
	}
	if err := transaction.Commit(); err != nil {
		return artifact.Entry{}, store.safeError("commit cloud cache lookup", err)
	}
	return entry, nil
}

func (store *Store) Usage(ctx context.Context) (int64, error) {
	var usage int64
	err := store.database.QueryRowContext(ctx, `
		SELECT used_bytes FROM layercache_projects_v1 WHERE project_id = $1`, store.config.Project).Scan(&usage)
	if err != nil {
		return 0, store.safeError("read cloud cache usage", err)
	}
	return usage, nil
}

func (store *Store) Stats(ctx context.Context) (artifact.Stats, error) {
	var stats artifact.Stats
	if err := store.database.QueryRowContext(ctx, `
		SELECT used_bytes, metadata_bytes FROM layercache_projects_v1 WHERE project_id = $1`, store.config.Project).
		Scan(&stats.UsageBytes, &stats.MetadataBytes); err != nil {
		return artifact.Stats{}, store.safeError("read cloud cache statistics", err)
	}
	if err := store.database.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM layercache_artifacts_v1
		WHERE project_id = $1 AND ref_count > 0`, store.config.Project).Scan(&stats.Artifacts); err != nil {
		return artifact.Stats{}, store.safeError("count cloud artifacts", err)
	}
	if err := store.database.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM layercache_entries_v1 WHERE project_id = $1`, store.config.Project).Scan(&stats.Entries); err != nil {
		return artifact.Stats{}, store.safeError("count cloud cache entries", err)
	}
	return stats, nil
}

func (store *Store) Delete(ctx context.Context, key artifact.Key) error {
	if err := store.validateKey(key); err != nil {
		return err
	}
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return store.safeError("begin cloud cache deletion", err)
	}
	defer transaction.Rollback()
	usage, err := store.lockProject(ctx, transaction)
	if err != nil {
		return err
	}
	removed, _, _, err := store.deleteEntry(ctx, transaction, key)
	if err != nil {
		return err
	}
	if !removed {
		return ErrNotFound
	}
	if err := store.appendAuditTx(ctx, transaction, AuditEvent{
		Project: key.Project, Actor: cloudRequestActor(ctx), Action: "cache.delete",
		Resource: cacheEntryAuditResource(key), Outcome: "allowed",
		Attributes: map[string]string{"integration": key.Integration}, CreatedAt: store.config.Now().UTC(),
	}); err != nil {
		return err
	}
	if err := store.persistUsage(ctx, transaction, usage); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return store.safeError("commit cloud cache deletion", err)
	}
	return nil
}

func cloudRequestActor(ctx context.Context) string {
	claims, ok := access.ClaimsFromContext(ctx)
	if ok && claims.Subject != "" {
		return claims.Subject
	}
	return "cache-gateway"
}

func cacheEntryAuditResource(key artifact.Key) string {
	hash := sha256.New()
	for _, component := range []string{
		key.Project, key.Integration, key.Compatibility, key.Native, key.Version, key.Ref,
	} {
		_, _ = io.WriteString(hash, component)
		_, _ = hash.Write([]byte{0})
	}
	return "cache-entry:" + hex.EncodeToString(hash.Sum(nil))
}

func (store *Store) GC(ctx context.Context, targetBytes int64) error {
	if targetBytes < 0 {
		return errors.New("cloud garbage collection target cannot be negative")
	}
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return store.safeError("begin cloud garbage collection", err)
	}
	defer transaction.Rollback()
	usage, err := store.lockProject(ctx, transaction)
	if err != nil {
		return err
	}
	for usage.used > targetBytes {
		removed, _, _, err := store.evictBlob(ctx, transaction, &usage, "")
		if err != nil {
			return err
		}
		if !removed {
			return ErrQuota
		}
	}
	if err := store.persistUsage(ctx, transaction, usage); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return store.safeError("commit cloud garbage collection", err)
	}
	return nil
}

func (store *Store) lockProject(ctx context.Context, transaction *sql.Tx) (projectUsage, error) {
	var usage projectUsage
	err := transaction.QueryRowContext(ctx, `
		SELECT quota_bytes, metadata_quota_bytes, used_bytes, metadata_bytes
		FROM layercache_projects_v1 WHERE project_id = $1 FOR UPDATE`, store.config.Project).
		Scan(&usage.quota, &usage.metadataQuota, &usage.used, &usage.metadataUsed)
	if err != nil {
		return projectUsage{}, store.safeError("lock cloud project quota", err)
	}
	return usage, nil
}

func (store *Store) lockProjectShared(ctx context.Context, transaction *sql.Tx) (projectUsage, error) {
	var usage projectUsage
	err := transaction.QueryRowContext(ctx, `
		SELECT quota_bytes, metadata_quota_bytes, used_bytes, metadata_bytes
		FROM layercache_projects_v1 WHERE project_id = $1 FOR SHARE`, store.config.Project).
		Scan(&usage.quota, &usage.metadataQuota, &usage.used, &usage.metadataUsed)
	if err != nil {
		return projectUsage{}, store.safeError("lock cloud project for read", err)
	}
	return usage, nil
}

func (store *Store) evictToFit(
	ctx context.Context,
	transaction *sql.Tx,
	usage *projectUsage,
	blobBytes int64,
	metadataBytes int64,
	protectedDigest string,
) error {
	for usage.used+blobBytes > usage.quota || usage.metadataUsed+metadataBytes > usage.metadataQuota {
		// Metadata pressure can remove one unpinned alias even when another
		// alias of the same digest is pinned. Blob pressure ranks whole groups.
		evict := store.evictOldest
		if usage.used+blobBytes > usage.quota {
			evict = store.evictBlob
		}
		removed, _, _, err := evict(ctx, transaction, usage, protectedDigest)
		if err != nil {
			return err
		}
		if !removed {
			return ErrQuota
		}
	}
	return nil
}

func (store *Store) evictOldest(
	ctx context.Context,
	transaction *sql.Tx,
	usage *projectUsage,
	protectedDigest string,
) (bool, string, int64, error) {
	query := `
		SELECT entry.project_id, entry.integration, entry.compatibility,
			entry.native_key, entry.version, entry.ref_scope
		FROM layercache_entries_v1 AS entry
		WHERE entry.project_id = $1
			AND NOT EXISTS (
				SELECT 1 FROM layercache_entry_pins_v1 AS pin WHERE pin.entry_id = entry.entry_id
			)`
	arguments := []any{store.config.Project}
	if protectedDigest != "" {
		query += ` AND entry.digest <> $2`
		arguments = append(arguments, protectedDigest)
	}
	query += ` ORDER BY entry.last_accessed_at, entry.created_at LIMIT 1 FOR UPDATE OF entry SKIP LOCKED`
	var key artifact.Key
	err := transaction.QueryRowContext(ctx, query, arguments...).Scan(
		&key.Project, &key.Integration, &key.Compatibility, &key.Native, &key.Version, &key.Ref,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", 0, nil
	}
	if err != nil {
		return false, "", 0, store.safeError("select cloud LRU victim", err)
	}
	removed, digest, size, err := store.deleteEntry(ctx, transaction, key)
	if err != nil || !removed {
		return removed, digest, size, err
	}
	if err := transaction.QueryRowContext(ctx, `
		SELECT used_bytes, metadata_bytes FROM layercache_projects_v1 WHERE project_id = $1`, store.config.Project).
		Scan(&usage.used, &usage.metadataUsed); err != nil {
		return false, "", 0, store.safeError("reload cloud project usage", err)
	}
	return true, digest, size, nil
}

func (store *Store) deleteEntry(
	ctx context.Context,
	transaction *sql.Tx,
	key artifact.Key,
) (bool, string, int64, error) {
	var digest string
	var size, metadataBytes int64
	err := transaction.QueryRowContext(ctx, `
		DELETE FROM layercache_entries_v1
		WHERE project_id = $1 AND integration = $2 AND compatibility = $3
			AND native_key = $4 AND version = $5 AND ref_scope = $6
		RETURNING digest, size_bytes, metadata_bytes`,
		key.Project, key.Integration, key.Compatibility, key.Native, key.Version, key.Ref).
		Scan(&digest, &size, &metadataBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", 0, nil
	}
	if err != nil {
		return false, "", 0, store.safeError("delete cloud cache entry", err)
	}
	var references int64
	if err := transaction.QueryRowContext(ctx, `
		UPDATE layercache_artifacts_v1
		SET ref_count = ref_count - 1,
			unreferenced_at = CASE WHEN ref_count = 1 THEN $1::timestamptz ELSE NULL END
		WHERE project_id = $2 AND digest = $3 AND ref_count > 0
		RETURNING ref_count`, store.config.Now().UTC(), key.Project, digest).Scan(&references); err != nil {
		return false, "", 0, store.safeError("release cloud artifact reference", err)
	}
	releasedBytes := int64(0)
	if references == 0 {
		releasedBytes = size
	}
	if _, err := transaction.ExecContext(ctx, `
		UPDATE layercache_projects_v1
		SET used_bytes = used_bytes - $1,
			metadata_bytes = metadata_bytes - $2,
			updated_at = $3
		WHERE project_id = $4`,
		releasedBytes, metadataBytes, store.config.Now().UTC(), key.Project); err != nil {
		return false, "", 0, store.safeError("account cloud cache deletion", err)
	}
	return true, digest, size, nil
}

func (store *Store) persistUsage(ctx context.Context, transaction *sql.Tx, usage projectUsage) error {
	// Entry deletion updates counters directly. Reload the authoritative values
	// before the final quota lock is released so callers do not maintain a
	// second accounting implementation in memory.
	err := transaction.QueryRowContext(ctx, `
		SELECT quota_bytes, metadata_quota_bytes, used_bytes, metadata_bytes
		FROM layercache_projects_v1 WHERE project_id = $1`, store.config.Project).
		Scan(&usage.quota, &usage.metadataQuota, &usage.used, &usage.metadataUsed)
	if err != nil {
		return store.safeError("verify cloud project usage", err)
	}
	return nil
}

func readEntry(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, key artifact.Key) (artifact.Entry, error) {
	var entry artifact.Entry
	entry.Key = key
	var metadataJSON []byte
	err := query.QueryRowContext(ctx, `
		SELECT digest, size_bytes, metadata_json, created_at
		FROM layercache_entries_v1
		WHERE project_id = $1 AND integration = $2 AND compatibility = $3
			AND native_key = $4 AND version = $5 AND ref_scope = $6`,
		key.Project, key.Integration, key.Compatibility, key.Native, key.Version, key.Ref).
		Scan(&entry.Digest, &entry.Size, &metadataJSON, &entry.CreatedAt)
	if err != nil {
		return artifact.Entry{}, err
	}
	if err := json.Unmarshal(metadataJSON, &entry.Metadata); err != nil {
		return artifact.Entry{}, fmt.Errorf("decode cloud artifact metadata: %w", err)
	}
	entry.CreatedAt = entry.CreatedAt.UTC()
	return entry, nil
}

func (store *Store) readEntryWithLease(ctx context.Context, key artifact.Key) (artifact.Entry, string, error) {
	if err := store.validateKey(key); err != nil {
		return artifact.Entry{}, "", err
	}
	leaseID, err := randomToken("read-")
	if err != nil {
		return artifact.Entry{}, "", err
	}
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return artifact.Entry{}, "", store.safeError("begin cloud artifact read", err)
	}
	defer transaction.Rollback()
	if _, err := store.lockProjectShared(ctx, transaction); err != nil {
		return artifact.Entry{}, "", err
	}
	entry, err := readEntry(ctx, transaction, key)
	if errors.Is(err, sql.ErrNoRows) {
		return artifact.Entry{}, "", ErrNotFound
	}
	if err != nil {
		return artifact.Entry{}, "", store.safeError("read cloud cache entry", err)
	}
	now := store.config.Now().UTC()
	if _, err := transaction.ExecContext(ctx, `
		INSERT INTO layercache_blob_read_leases_v1(lease_id, project_id, digest, expires_at)
		VALUES($1, $2, $3, $4)`, leaseID, key.Project, entry.Digest, now.Add(readLeaseTTL)); err != nil {
		return artifact.Entry{}, "", store.safeError("lease cloud artifact read", err)
	}
	if _, err := transaction.ExecContext(ctx, `
		UPDATE layercache_entries_v1 SET last_accessed_at = $1
		WHERE project_id = $2 AND integration = $3 AND compatibility = $4
			AND native_key = $5 AND version = $6 AND ref_scope = $7`,
		now, key.Project, key.Integration, key.Compatibility, key.Native, key.Version, key.Ref); err != nil {
		return artifact.Entry{}, "", store.safeError("touch cloud cache entry", err)
	}
	if _, err := transaction.ExecContext(ctx, `
		UPDATE layercache_artifacts_v1 SET last_accessed_at = $1
		WHERE project_id = $2 AND digest = $3`, now, key.Project, entry.Digest); err != nil {
		return artifact.Entry{}, "", store.safeError("touch cloud artifact", err)
	}
	if err := transaction.Commit(); err != nil {
		return artifact.Entry{}, "", store.safeError("commit cloud artifact read", err)
	}
	return entry, leaseID, nil
}

func (store *Store) validateKey(key artifact.Key) error {
	if key.Project != store.config.Project {
		return errors.New("cloud cache key is outside the configured project")
	}
	for name, value := range map[string]string{
		"integration": key.Integration, "project": key.Project, "compatibility": key.Compatibility, "native key": key.Native,
	} {
		if err := validateIdentity(name, value, 1024); err != nil {
			return err
		}
	}
	for name, value := range map[string]string{"version": key.Version, "ref": key.Ref} {
		if len(value) > 1024 || strings.IndexFunc(value, func(character rune) bool { return character < 0x20 || character == 0x7f }) >= 0 {
			return fmt.Errorf("%s is invalid", name)
		}
	}
	return nil
}

func estimateMetadataBytes(key artifact.Key, metadata []byte) int64 {
	return int64(len(key.Integration) + len(key.Project) + len(key.Compatibility) + len(key.Native) +
		len(key.Version) + len(key.Ref) + sha256.Size*2 + len(metadata) + 256)
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && strings.ToLower(value) == value
}

func (store *Store) blobKey(digest string) string {
	return "projects/" + store.namespace + "/blobs/sha256/" + digest
}

func randomToken(prefix string) (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate cloud credential: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(random), nil
}

func (store *Store) discardStage(uploadID, objectKey string) {
	cleanupContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = store.blobs.Delete(cleanupContext, objectKey)
	_, _ = store.database.ExecContext(cleanupContext, `
		DELETE FROM layercache_uploads_v1 WHERE upload_id = $1 AND project_id = $2`,
		uploadID, store.config.Project)
}

type leasedReadCloser struct {
	body        io.ReadCloser
	store       *Store
	leaseID     string
	nextRenewal time.Time
	closed      bool
	digest      string
	countedRead bool
}

func (reader *leasedReadCloser) Read(buffer []byte) (int, error) {
	count, err := reader.body.Read(buffer)
	if errors.Is(err, io.EOF) && !reader.countedRead {
		reader.countedRead = true
		reader.store.recordVerifiedRead(reader.digest)
	}
	if reader.store.config.Now().UTC().After(reader.nextRenewal) {
		reader.renew()
	}
	return count, err
}

func (reader *leasedReadCloser) Close() error {
	if reader.closed {
		return nil
	}
	reader.closed = true
	err := reader.body.Close()
	reader.store.releaseReadLease(reader.leaseID)
	return err
}

func (reader *leasedReadCloser) renew() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	now := reader.store.config.Now().UTC()
	if _, err := reader.store.database.ExecContext(ctx, `
		UPDATE layercache_blob_read_leases_v1 SET expires_at = $1
		WHERE lease_id = $2 AND project_id = $3`,
		now.Add(readLeaseTTL), reader.leaseID, reader.store.config.Project); err == nil {
		reader.nextRenewal = now.Add(readLeaseTTL / 2)
	}
}

func (store *Store) releaseReadLease(leaseID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = store.database.ExecContext(ctx, `
		DELETE FROM layercache_blob_read_leases_v1 WHERE lease_id = $1 AND project_id = $2`,
		leaseID, store.config.Project)
}
