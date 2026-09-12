package cloud

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/retention"
)

func (store *Store) recordVerifiedRead(digest string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Only an EOF from the digest-verifying S3 reader counts. Failed or
	// abandoned streams and HEAD probes cannot inflate retention frequency.
	_, _ = store.database.ExecContext(ctx, `INSERT INTO layercache_artifact_access_v1(project_id, digest, read_count)
		SELECT project_id, digest, 1 FROM layercache_artifacts_v1 WHERE project_id = $1 AND digest = $2
		ON CONFLICT(project_id, digest) DO UPDATE SET read_count = LEAST(layercache_artifact_access_v1.read_count + 1, 32)`,
		store.config.Project, digest)
}

func (store *Store) evictBlob(ctx context.Context, tx *sql.Tx, usage *projectUsage, protectedDigest string) (bool, string, int64, error) {
	if store.config.EvictionPolicy != retention.Impact {
		return store.evictOldest(ctx, tx, usage, protectedDigest)
	}
	// The caller holds the project quota row FOR UPDATE. Publication, pinning,
	// deletion and read leases acquire that same project lock before entry rows.
	rows, err := tx.QueryContext(ctx, `SELECT entry.digest, entry.size_bytes, entry.metadata_json,
		entry.last_accessed_at, entry.created_at, COALESCE(access.read_count, 0),
		entry.integration, entry.compatibility, entry.native_key, entry.version, entry.ref_scope
		FROM layercache_entries_v1 AS entry
		LEFT JOIN layercache_artifact_access_v1 AS access ON access.project_id = entry.project_id AND access.digest = entry.digest
		WHERE entry.project_id = $1 AND entry.digest <> $2
		AND NOT EXISTS (
			SELECT 1 FROM layercache_entry_pins_v1 AS pin
			JOIN layercache_entries_v1 AS pinned ON pinned.entry_id = pin.entry_id
			WHERE pinned.project_id = entry.project_id AND pinned.digest = entry.digest
		)
		ORDER BY entry.digest FOR UPDATE OF entry`, store.config.Project, protectedDigest)
	if err != nil {
		return false, "", 0, store.safeError("select cloud impact candidates", err)
	}
	var candidates []retention.Candidate
	keys := make(map[string][]artifact.Key)
	for rows.Next() {
		var candidate retention.Candidate
		var encoded []byte
		key := artifact.Key{Project: store.config.Project}
		if err := rows.Scan(&candidate.ID, &candidate.Bytes, &encoded, &candidate.LastAccess, &candidate.CreatedAt,
			&candidate.AccessCount, &key.Integration, &key.Compatibility, &key.Native, &key.Version, &key.Ref); err != nil {
			rows.Close()
			return false, "", 0, store.safeError("read cloud impact candidate", err)
		}
		var metadata artifact.Metadata
		if err := json.Unmarshal(encoded, &metadata); err != nil {
			rows.Close()
			return false, "", 0, store.safeError("decode cloud retention metadata", err)
		}
		candidate.ProducerDurationMS = metadata.DurationMS
		candidate.DurationKnown = metadata.DurationMS > 0
		keys[candidate.ID] = append(keys[candidate.ID], key)
		if len(candidates) == 0 || candidates[len(candidates)-1].ID != candidate.ID {
			candidates = append(candidates, candidate)
			continue
		}
		group := &candidates[len(candidates)-1]
		group.ProducerDurationMS = max(group.ProducerDurationMS, candidate.ProducerDurationMS)
		group.DurationKnown = group.DurationKnown || candidate.DurationKnown
		if candidate.LastAccess.After(group.LastAccess) {
			group.LastAccess = candidate.LastAccess
		}
		if candidate.CreatedAt.Before(group.CreatedAt) {
			group.CreatedAt = candidate.CreatedAt
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, "", 0, store.safeError("read cloud impact candidates", err)
	}
	victim, _ := retention.Choose(retention.Impact, candidates, store.config.Now().UTC())
	if victim < 0 {
		return false, "", 0, nil
	}
	digest := candidates[victim].ID
	for _, key := range keys[digest] {
		if _, _, _, err := store.deleteEntry(ctx, tx, key); err != nil {
			return false, "", 0, err
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT used_bytes, metadata_bytes FROM layercache_projects_v1 WHERE project_id = $1`, store.config.Project).
		Scan(&usage.used, &usage.metadataUsed); err != nil {
		return false, "", 0, store.safeError("reload cloud impact usage", err)
	}
	return true, digest, candidates[victim].Bytes, nil
}
