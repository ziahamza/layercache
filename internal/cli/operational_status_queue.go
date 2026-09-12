package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const stoppedQueueStatsTimeout = 2 * time.Second

type actionsTeamPublicationStats struct {
	PendingJobs  int64
	PendingBytes int64
}

// readStoppedActionsTeamPublicationStats inspects the durable Actions Team
// publication queue without creating or migrating its database. A missing
// database means no Actions archive has been committed on this installation.
func readStoppedActionsTeamPublicationStats(ctx context.Context, path string) (actionsTeamPublicationStats, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return actionsTeamPublicationStats{}, nil
	}
	if err != nil {
		return actionsTeamPublicationStats{}, fmt.Errorf("inspect Actions Team Cache publication queue: %w", err)
	}
	if !info.Mode().IsRegular() {
		return actionsTeamPublicationStats{}, errors.New("Actions Team Cache publication queue is not a regular file")
	}

	dsn := (&url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(path),
		RawQuery: "mode=ro",
	}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return actionsTeamPublicationStats{}, fmt.Errorf("open Actions Team Cache publication queue read-only: %w", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)

	queryContext, cancel := context.WithTimeout(ctx, stoppedQueueStatsTimeout)
	defer cancel()
	var stats actionsTeamPublicationStats
	if err := database.QueryRowContext(queryContext, `SELECT COUNT(*), COALESCE(SUM(size), 0)
		FROM actions_team_publications`).Scan(&stats.PendingJobs, &stats.PendingBytes); err != nil {
		return actionsTeamPublicationStats{}, fmt.Errorf("read Actions Team Cache publication queue: %w", err)
	}
	return stats, nil
}
