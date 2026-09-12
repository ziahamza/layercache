package cloud

import (
	"context"
	"time"

	"github.com/layercache/layercache/internal/artifact"
)

// Prune uses whole digest groups. A fresh alias, pin or live read lease protects
// the whole blob. Project locking serializes this decision with publication.
func (store *Store) pruneExpiredEntries(ctx context.Context, now time.Time) (int, error) {
	if store.config.IdleTTL <= 0 && store.config.UnreusedTTL <= 0 {
		return 0, nil
	}
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return 0, store.safeError("begin proactive pruning", err)
	}
	defer tx.Rollback()
	if _, err := store.lockProject(ctx, tx); err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `
	 SELECT e.integration, e.compatibility, e.native_key, e.version, e.ref_scope
	 FROM layercache_entries_v1 e WHERE e.project_id = $1 AND e.digest IN (
	  SELECT candidate.digest FROM layercache_entries_v1 candidate
	  LEFT JOIN layercache_artifact_access_v1 a ON a.project_id=candidate.project_id AND a.digest=candidate.digest
	  WHERE candidate.project_id=$1
	  AND NOT EXISTS (SELECT 1 FROM layercache_entry_pins_v1 p
	    JOIN layercache_entries_v1 pinned ON pinned.entry_id=p.entry_id
	    WHERE pinned.project_id=candidate.project_id AND pinned.digest=candidate.digest)
	  AND NOT EXISTS (SELECT 1 FROM layercache_blob_read_leases_v1 l
	    WHERE l.project_id=candidate.project_id AND l.digest=candidate.digest AND l.expires_at>$2)
	  GROUP BY candidate.digest
	  HAVING ($3 AND MAX(candidate.last_accessed_at)<=$4)
	    OR ($5 AND MAX(COALESCE(a.read_count,0))=0 AND MAX(candidate.created_at)<=$6)
	  ORDER BY MAX(candidate.last_accessed_at), candidate.digest LIMIT 256
	 )`, store.config.Project, now, store.config.IdleTTL > 0, now.Add(-store.config.IdleTTL),
		store.config.UnreusedTTL > 0, now.Add(-store.config.UnreusedTTL))
	if err != nil {
		return 0, store.safeError("select proactive prune candidates", err)
	}
	var keys []artifact.Key
	for rows.Next() {
		key := artifact.Key{Project: store.config.Project}
		if err := rows.Scan(&key.Integration, &key.Compatibility, &key.Native, &key.Version, &key.Ref); err != nil {
			rows.Close()
			return 0, store.safeError("read prune candidate", err)
		}
		keys = append(keys, key)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, store.safeError("iterate prune candidates", err)
	}
	for _, key := range keys {
		if _, _, _, err := store.deleteEntry(ctx, tx, key); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, store.safeError("commit proactive prune", err)
	}
	return len(keys), nil
}
