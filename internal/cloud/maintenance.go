package cloud

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Maintain expires abandoned uploads and leases, then removes unreferenced or
// orphaned objects after their configured grace periods. It is safe for
// several cloud process replicas to call this concurrently.
func (store *Store) Maintain(ctx context.Context) (MaintenanceResult, error) {
	var result MaintenanceResult
	now := store.config.Now().UTC()
	var err error
	result.PrunedEntries, err = store.pruneExpiredEntries(ctx, now)
	if err != nil {
		return result, err
	}
	if store.config.SoftBytes > 0 {
		if err := store.GC(ctx, store.config.SoftBytes); err != nil && !errors.Is(err, ErrQuota) {
			return result, err
		}
	}
	if _, err := store.database.ExecContext(ctx, `
		DELETE FROM layercache_blob_read_leases_v1 WHERE expires_at <= $1`, now); err != nil {
		return result, store.safeError("expire cloud read leases", err)
	}

	expiredUploads, err := store.expiredUploads(ctx, now, 256)
	if err != nil {
		return result, err
	}
	for _, upload := range expiredUploads {
		if err := store.blobs.Delete(ctx, upload.objectKey); err != nil {
			return result, store.safeError("delete expired cloud staged object", err)
		}
		deleted, err := store.database.ExecContext(ctx, `
			DELETE FROM layercache_uploads_v1
			WHERE upload_id = $1 AND project_id = $2 AND expires_at <= $3`,
			upload.id, store.config.Project, now)
		if err != nil {
			return result, store.safeError("delete expired cloud upload record", err)
		}
		count, _ := deleted.RowsAffected()
		result.ExpiredUploads += int(count)
	}

	result.ExpiredLeases, err = store.expirePromotionLeases(ctx, now)
	if err != nil {
		return result, err
	}
	deleted, err := store.deleteUnreferencedBlobs(ctx, now.Add(-store.config.BlobGrace), 256)
	if err != nil {
		return result, err
	}
	result.DeletedBlobs += deleted
	orphaned, err := store.deleteOrphanedObjects(ctx, now)
	if err != nil {
		return result, err
	}
	result.DeletedBlobs += orphaned
	return result, nil
}

// RunMaintenance runs one pass immediately and repeats until ctx is cancelled.
// The caller owns this lifecycle so shutdown waits for no hidden goroutine.
func (store *Store) RunMaintenance(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return errors.New("cloud maintenance interval must be positive")
	}
	if _, err := store.Maintain(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if _, err := store.Maintain(ctx); err != nil {
				return err
			}
		}
	}
}

type expiredUpload struct {
	id        string
	objectKey string
}

func (store *Store) expiredUploads(ctx context.Context, now time.Time, limit int) ([]expiredUpload, error) {
	rows, err := store.database.QueryContext(ctx, `
		SELECT upload_id, object_key
		FROM layercache_uploads_v1
		WHERE project_id = $1 AND expires_at <= $2
		ORDER BY expires_at
		LIMIT $3`, store.config.Project, now, limit)
	if err != nil {
		return nil, store.safeError("list expired cloud uploads", err)
	}
	defer rows.Close()
	var uploads []expiredUpload
	for rows.Next() {
		var upload expiredUpload
		if err := rows.Scan(&upload.id, &upload.objectKey); err != nil {
			return nil, store.safeError("decode expired cloud upload", err)
		}
		uploads = append(uploads, upload)
	}
	if err := rows.Err(); err != nil {
		return nil, store.safeError("iterate expired cloud uploads", err)
	}
	return uploads, nil
}

func (store *Store) expirePromotionLeases(ctx context.Context, now time.Time) (int, error) {
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return 0, store.safeError("begin promotion lease expiry", err)
	}
	defer transaction.Rollback()
	rows, err := transaction.QueryContext(ctx, `
		DELETE FROM layercache_promotion_leases_v1
		WHERE project_id = $1 AND expires_at <= $2
		RETURNING reference, owner`, store.config.Project, now)
	if err != nil {
		return 0, store.safeError("expire BuildKit promotion leases", err)
	}
	type expiredLease struct{ reference, owner string }
	var expired []expiredLease
	for rows.Next() {
		var lease expiredLease
		if err := rows.Scan(&lease.reference, &lease.owner); err != nil {
			_ = rows.Close()
			return 0, store.safeError("decode expired BuildKit promotion lease", err)
		}
		expired = append(expired, lease)
	}
	if err := rows.Close(); err != nil {
		return 0, store.safeError("close expired BuildKit promotion leases", err)
	}
	for _, lease := range expired {
		if err := store.appendAuditTx(ctx, transaction, AuditEvent{
			Project: store.config.Project, Actor: "cloud-maintenance",
			Action: "buildkit.promotion.expire", Resource: "oci:" + lease.reference,
			Outcome: "expired", Attributes: map[string]string{"owner": lease.owner}, CreatedAt: now,
		}); err != nil {
			return 0, err
		}
	}
	if err := transaction.Commit(); err != nil {
		return 0, store.safeError("commit promotion lease expiry", err)
	}
	return len(expired), nil
}

func (store *Store) deleteUnreferencedBlobs(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	deleted := 0
	for deleted < limit {
		transaction, err := store.database.BeginTx(ctx, nil)
		if err != nil {
			return deleted, store.safeError("begin cloud blob collection", err)
		}
		usage, err := store.lockProject(ctx, transaction)
		if err != nil {
			_ = transaction.Rollback()
			return deleted, err
		}
		_ = usage
		var digest, objectKey string
		err = transaction.QueryRowContext(ctx, `
			SELECT artifact.digest, artifact.object_key
			FROM layercache_artifacts_v1 AS artifact
			WHERE artifact.project_id = $1
				AND artifact.ref_count = 0
				AND artifact.unreferenced_at <= $2
				AND NOT EXISTS (
					SELECT 1 FROM layercache_blob_read_leases_v1 AS lease
					WHERE lease.project_id = artifact.project_id
						AND lease.digest = artifact.digest
						AND lease.expires_at > $3
				)
			ORDER BY artifact.unreferenced_at
			LIMIT 1
			FOR UPDATE OF artifact SKIP LOCKED`, store.config.Project, cutoff, store.config.Now().UTC()).
			Scan(&digest, &objectKey)
		if errors.Is(err, sql.ErrNoRows) {
			_ = transaction.Rollback()
			break
		}
		if err != nil {
			_ = transaction.Rollback()
			return deleted, store.safeError("select unreferenced cloud blob", err)
		}
		if err := store.blobs.Delete(ctx, objectKey); err != nil {
			_ = transaction.Rollback()
			return deleted, store.safeError("delete unreferenced cloud blob", err)
		}
		if _, err := transaction.ExecContext(ctx, `
			DELETE FROM layercache_artifacts_v1
			WHERE project_id = $1 AND digest = $2 AND ref_count = 0`, store.config.Project, digest); err != nil {
			_ = transaction.Rollback()
			return deleted, store.safeError("delete unreferenced cloud blob record", err)
		}
		if err := transaction.Commit(); err != nil {
			return deleted, store.safeError("commit unreferenced cloud blob deletion", err)
		}
		deleted++
	}
	return deleted, nil
}

func (store *Store) deleteOrphanedObjects(ctx context.Context, now time.Time) (int, error) {
	deleted := 0
	prefixes := []struct {
		prefix string
		cutoff time.Time
		table  string
	}{
		{prefix: "projects/" + store.namespace + "/staging/", cutoff: now.Add(-store.config.StageTTL), table: "staging"},
		{prefix: "projects/" + store.namespace + "/blobs/sha256/", cutoff: now.Add(-store.config.BlobGrace), table: "blobs"},
	}
	for _, candidate := range prefixes {
		for listed := range store.blobs.List(ctx, candidate.prefix) {
			if listed.Err != nil {
				return deleted, store.safeError("list cloud objects for orphan collection", listed.Err)
			}
			if listed.Info.LastModified.After(candidate.cutoff) {
				continue
			}
			removed, err := store.deleteOrphanedObject(ctx, candidate.table, listed.Info.Key)
			if err != nil {
				return deleted, err
			}
			if removed {
				deleted++
			}
		}
	}
	return deleted, nil
}

func (store *Store) deleteOrphanedObject(ctx context.Context, kind, objectKey string) (bool, error) {
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return false, store.safeError("begin orphaned cloud object collection", err)
	}
	defer transaction.Rollback()
	if _, err := store.lockProject(ctx, transaction); err != nil {
		return false, err
	}
	var present bool
	switch kind {
	case "staging":
		err = transaction.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM layercache_uploads_v1 WHERE project_id = $1 AND object_key = $2
			)`, store.config.Project, objectKey).Scan(&present)
	case "blobs":
		err = transaction.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM layercache_artifacts_v1 WHERE project_id = $1 AND object_key = $2
			)`, store.config.Project, objectKey).Scan(&present)
	default:
		return false, fmt.Errorf("unknown cloud object kind %q", kind)
	}
	if err != nil {
		return false, store.safeError("check cloud object references", err)
	}
	if present {
		return false, nil
	}
	if err := store.blobs.Delete(ctx, objectKey); err != nil {
		return false, store.safeError("delete orphaned cloud object", err)
	}
	if err := transaction.Commit(); err != nil {
		return false, store.safeError("commit orphaned cloud object deletion", err)
	}
	return true, nil
}
