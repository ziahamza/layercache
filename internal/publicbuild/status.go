package publicbuild

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// StatusSummary is the bounded operational view of a Public Build queue. It
// contains no source inputs, logs, worker identities, or credentials.
type StatusSummary struct {
	Queued            int64     `json:"queued"`
	Running           int64     `json:"running"`
	Succeeded         int64     `json:"succeeded"`
	Failed            int64     `json:"failed"`
	Cancelled         int64     `json:"cancelled"`
	LatestBuildID     string    `json:"latestBuildId,omitempty"`
	LatestState       State     `json:"latestState,omitempty"`
	LatestRequestedAt time.Time `json:"latestRequestedAt,omitempty"`
}

// StatusReader is the backend-neutral operational view used by the
// authenticated runtime status endpoint.
type StatusReader interface {
	Status(context.Context) (StatusSummary, error)
}

// Status reads the bounded operational summary from an open SQLite
// coordinator. It does not mutate or recover the queue.
func (coordinator *SQLiteCoordinator) Status(ctx context.Context) (StatusSummary, error) {
	if coordinator == nil || coordinator.database == nil {
		return StatusSummary{}, errors.New("Public Build coordinator is closed")
	}
	return readSQLiteStatus(ctx, coordinator.database)
}

func readSQLiteStatus(ctx context.Context, database *sql.DB) (StatusSummary, error) {
	var result StatusSummary
	err := database.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN state = 'queued' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN state = 'running' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN state = 'succeeded' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN state = 'failed' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN state = 'cancelled' THEN 1 ELSE 0 END), 0)
		FROM public_builds_v1`).Scan(
		&result.Queued,
		&result.Running,
		&result.Succeeded,
		&result.Failed,
		&result.Cancelled,
	)
	if err != nil {
		return StatusSummary{}, fmt.Errorf("read Public Build state counts: %w", err)
	}
	var sequence int64
	var requestedAt int64
	err = database.QueryRowContext(ctx, `
		SELECT sequence, state, requested_at_ns
		FROM public_builds_v1 ORDER BY sequence DESC LIMIT 1`).Scan(
		&sequence,
		&result.LatestState,
		&requestedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return StatusSummary{}, fmt.Errorf("read latest Public Build state: %w", err)
	}
	result.LatestBuildID = formatBuildID(sequence)
	result.LatestRequestedAt = timeFromUnixNano(requestedAt)
	return result, nil
}

var _ StatusReader = (*SQLiteCoordinator)(nil)
