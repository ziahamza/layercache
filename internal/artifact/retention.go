package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/layercache/layercache/internal/retention"
)

func (store *Store) SetEvictionPolicy(policy retention.Policy) error {
	validated, err := retention.Normalize(policy)
	if err != nil {
		return err
	}
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	store.evictionPolicy = validated
	return nil
}

// evictImpact deletes a whole unpinned digest group, so its size is actual blob
// capacity released rather than a sum of duplicate cache-key sizes. The caller
// holds writeMu, preserving the same writer and active-reader guarantees as LRU.
func (store *Store) evictImpact(ctx context.Context) (bool, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT entry.digest, entry.size, entry.metadata_json,
		entry.last_accessed_at, entry.created_at, COALESCE(access.read_count, 0)
		FROM cache_entries AS entry
		LEFT JOIN artifact_access AS access ON access.digest = entry.digest
		WHERE NOT EXISTS (SELECT 1 FROM cache_entry_pins AS pin WHERE pin.digest = entry.digest)
		ORDER BY entry.digest`)
	if err != nil {
		return false, fmt.Errorf("select Local Cache impact candidates: %w", err)
	}
	var candidates []retention.Candidate
	for rows.Next() {
		var digest string
		var size, accessed, created, reads int64
		var encoded []byte
		if err := rows.Scan(&digest, &size, &encoded, &accessed, &created, &reads); err != nil {
			rows.Close()
			return false, err
		}
		var metadata Metadata
		if err := json.Unmarshal(encoded, &metadata); err != nil {
			rows.Close()
			return false, fmt.Errorf("decode Local Cache retention metadata: %w", err)
		}
		candidate := retention.Candidate{
			ID: digest, Bytes: size, ProducerDurationMS: metadata.DurationMS,
			DurationKnown: metadata.DurationMS > 0, AccessCount: reads,
			LastAccess: time.Unix(0, accessed), CreatedAt: time.Unix(0, created),
		}
		if len(candidates) == 0 || candidates[len(candidates)-1].ID != digest {
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
		return false, err
	}
	victim, _ := retention.Choose(retention.Impact, candidates, time.Now().UTC())
	if victim < 0 {
		return false, nil
	}
	digest := candidates[victim].ID
	if _, err := tx.ExecContext(ctx, `DELETE FROM cache_entries WHERE digest = ?`, digest); err != nil {
		return false, fmt.Errorf("evict Local Cache digest group: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM artifacts WHERE digest = ?`, digest); err != nil {
		return false, fmt.Errorf("release Local Cache digest group: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if err := os.Remove(store.blobPath(digest)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("delete Local Cache digest group bytes: %w", err)
	}
	return true, nil
}
