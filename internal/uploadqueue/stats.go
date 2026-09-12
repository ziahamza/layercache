package uploadqueue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"time"
)

// ReadPersistedStats reads a stopped runtime's upload queue without creating
// or migrating it. Missing queue state is an empty queue.
func ReadPersistedStats(ctx context.Context, path string, maxQueuedBytes int64) (Stats, error) {
	stats := Stats{MaxQueuedBytes: maxQueuedBytes}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return stats, nil
	}
	if err != nil {
		return Stats{}, fmt.Errorf("inspect upload queue: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Stats{}, errors.New("upload queue is not a regular file")
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return Stats{}, fmt.Errorf("open upload queue read-only: %w", err)
	}
	defer database.Close()
	now := time.Now().UTC().UnixNano()
	err = database.QueryRowContext(ctx, `
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
		return Stats{}, fmt.Errorf("read persisted upload queue stats: %w", err)
	}
	return stats, nil
}
