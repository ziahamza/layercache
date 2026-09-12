package cli

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/layercache/layercache/internal/config"
)

func TestReadStoppedActionsTeamPublicationStatsDoesNotCreateMissingDatabase(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "actions.db")
	stats, err := readStoppedActionsTeamPublicationStats(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if stats.PendingJobs != 0 || stats.PendingBytes != 0 {
		t.Fatalf("missing Actions Team Cache publication queue stats = %#v", stats)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read-only status created missing Actions database: %v", err)
	}
}

func TestReadStoppedActionsTeamPublicationStatsRejectsNonRegularDatabase(t *testing.T) {
	t.Parallel()

	if _, err := readStoppedActionsTeamPublicationStats(context.Background(), t.TempDir()); err == nil {
		t.Fatal("accepted directory as Actions Team Cache publication database")
	}
}

func TestInspectOperationalStatusAggregatesStoppedTeamQueues(t *testing.T) {
	t.Parallel()

	configPath, dataDir := setupLocalConfig(t)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}

	actionsDatabase := openStatusFixtureDatabase(t, filepath.Join(dataDir, "actions.db"))
	if _, err := actionsDatabase.Exec(`CREATE TABLE actions_team_publications (size INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := actionsDatabase.Exec(`INSERT INTO actions_team_publications(size) VALUES (13), (17)`); err != nil {
		t.Fatal(err)
	}
	if err := actionsDatabase.Close(); err != nil {
		t.Fatal(err)
	}

	turboDatabase := openStatusFixtureDatabase(t, filepath.Join(dataDir, "team-uploads.db"))
	if _, err := turboDatabase.Exec(`CREATE TABLE team_upload_jobs_v1 (
		expected_size INTEGER NOT NULL,
		state TEXT NOT NULL,
		next_attempt_at INTEGER NOT NULL,
		lease_expires_at INTEGER
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := turboDatabase.Exec(`INSERT INTO team_upload_jobs_v1(
		expected_size, state, next_attempt_at, lease_expires_at
	) VALUES (19, 'pending', 0, NULL), (23, 'leased', 0, 0), (29, 'completed', 0, NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := turboDatabase.Close(); err != nil {
		t.Fatal(err)
	}

	status := inspectOperationalStatus(context.Background(), cfg)
	if status.Running {
		t.Fatal("fixture runtime unexpectedly reported running")
	}
	if status.PendingUploads != 4 || status.PendingUploadBytes != 72 {
		t.Fatalf("stopped Team Cache queue status = %d jobs, %d bytes, want 4 jobs and 72 bytes",
			status.PendingUploads, status.PendingUploadBytes)
	}
}

func openStatusFixtureDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	return database
}
