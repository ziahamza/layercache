package actionscache_test

import (
	"bytes"
	"context"
	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/actionscache"
	"github.com/layercache/layercache/internal/artifact"
)

func TestActionsProducerDurationPersistsAcrossMemorySQLiteAndCacheChain(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main", DefaultRef: "refs/heads/main",
		Compatibility: "linux-amd64-node24",
	}
	duration := 37 * time.Second
	payload := []byte("producer-duration")

	memory := actionscache.NewMemoryStorage()
	commitArchiveWithProducerDuration(t, memory, scope, "memory", "v1", payload, duration)
	assertProducerDuration(t, memory, scope, "memory", "v1", duration)

	root := t.TempDir()
	artifacts, err := artifact.Open(ctx, root, 64<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	persistent, err := actionscache.OpenPersistentStorage(ctx, root, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	commitArchiveWithProducerDuration(t, persistent, scope, "sqlite", "v1", payload, duration)
	if err := persistent.Close(); err != nil {
		t.Fatal(err)
	}
	if err := artifacts.Close(); err != nil {
		t.Fatal(err)
	}
	artifacts, err = artifact.Open(ctx, root, 64<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = artifacts.Close() })
	persistent, err = actionscache.OpenPersistentStorage(ctx, root, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = persistent.Close() })
	assertProducerDuration(t, persistent, scope, "sqlite", "v1", duration)

	local := actionscache.NewMemoryStorage()
	team := actionscache.NewMemoryStorage()
	chain, err := actionscache.NewCacheChain(local, team)
	if err != nil {
		t.Fatal(err)
	}
	commitArchiveWithProducerDuration(t, chain, scope, "chain", "v1", payload, duration)
	assertProducerDuration(t, local, scope, "chain", "v1", duration)
	assertProducerDuration(t, team, scope, "chain", "v1", duration)
}

func TestRemoteActionsProducerDurationRoundTripsThroughProtocol(t *testing.T) {
	t.Parallel()

	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main", DefaultRef: "refs/heads/main",
		Compatibility: "linux-amd64-node24",
	}
	handler, err := actionscache.NewHandler(actionscache.Config{
		Repository: scope.Repository, Ref: scope.Ref, DefaultRef: scope.DefaultRef,
		Compatibility: scope.Compatibility,
	}, actionscache.NewMemoryStorage())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	remote, err := actionscache.NewRemoteStorage(actionscache.RemoteStorageConfig{
		Endpoint: server.URL, Token: "remote-team-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	duration := 91 * time.Second
	commitArchiveWithProducerDuration(t, remote, scope, "remote", "v1", []byte("remote-duration"), duration)
	assertProducerDuration(t, remote, scope, "remote", "v1", duration)
}

func TestPersistentActionsProducerDurationMigratesExistingIndex(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	database, err := sql.Open("sqlite", filepath.Join(root, "actions.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TABLE actions_entries (
		id INTEGER PRIMARY KEY,
		repository TEXT NOT NULL,
		compatibility TEXT NOT NULL,
		ref_scope TEXT NOT NULL,
		cache_key TEXT NOT NULL,
		version TEXT NOT NULL,
		size INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		origin TEXT NOT NULL DEFAULT 'localCache',
		public_metadata_json BLOB
	)`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifact.Open(ctx, root, 64<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = artifacts.Close() })
	storage, err := actionscache.OpenPersistentStorage(ctx, root, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main", DefaultRef: "refs/heads/main",
		Compatibility: "linux-amd64-node24",
	}
	duration := 11 * time.Second
	commitArchiveWithProducerDuration(t, storage, scope, "migrated", "v1", []byte("migrated"), duration)
	assertProducerDuration(t, storage, scope, "migrated", "v1", duration)
}

func commitArchiveWithProducerDuration(
	t *testing.T,
	storage actionscache.StorageIndex,
	scope actionscache.Scope,
	key, version string,
	payload []byte,
	duration time.Duration,
) actionscache.Entry {
	t.Helper()
	ctx := context.Background()
	size := int64(len(payload))
	reservation, err := storage.Reserve(ctx, actionscache.ReserveRequest{
		Scope: scope, Key: key, Version: version, CacheSize: &size,
	})
	if err != nil {
		t.Fatal(err)
	}
	if size > 0 {
		if err := storage.Upload(ctx, actionscache.UploadRequest{
			ReservationID: reservation.ID, Scope: &scope, Start: 0, End: size - 1,
			Body: bytes.NewReader(payload),
		}); err != nil {
			t.Fatal(err)
		}
	}
	entry, err := storage.Commit(ctx, actionscache.CommitRequest{
		ReservationID: reservation.ID, Scope: &scope, Size: size, ProducerDuration: &duration,
	})
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func assertProducerDuration(
	t *testing.T,
	storage actionscache.CacheReader,
	scope actionscache.Scope,
	key, version string,
	want time.Duration,
) {
	t.Helper()
	result, err := storage.Lookup(context.Background(), actionscache.LookupRequest{
		Scope: scope, Keys: []string{key}, Version: version,
	})
	if err != nil {
		t.Fatalf("lookup %s producer duration: %v", key, err)
	}
	if result.Entry.ProducerDuration == nil || *result.Entry.ProducerDuration != want {
		t.Fatalf("%s producer duration = %v, want %s", key, result.Entry.ProducerDuration, want)
	}
	archive, err := storage.Open(context.Background(), actionscache.OpenRequest{Scope: scope, ID: result.Entry.ID})
	if err != nil {
		t.Fatalf("open %s producer duration: %v", key, err)
	}
	_ = archive.Body.Close()
	if archive.Entry.ProducerDuration == nil || *archive.Entry.ProducerDuration != want {
		t.Fatalf("%s open producer duration = %v, want %s", key, archive.Entry.ProducerDuration, want)
	}
	if result.Entry.Key != key {
		t.Fatalf("lookup key = %q, want %q", result.Entry.Key, key)
	}
}
