package artifact_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/layercache/layercache/internal/artifact"
)

func TestPinnedEntrySurvivesRestartAndLRUEvictionUntilReleased(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := artifact.Open(ctx, root, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	key := artifact.Key{
		Integration: "turbo", Project: "github.com/acme/widget",
		Compatibility: "linux-amd64-schema1", Native: "queued",
	}
	entry, _, err := store.Put(ctx, key, artifact.Metadata{}, bytes.NewReader([]byte("keep")))
	if err != nil {
		t.Fatal(err)
	}
	pin := artifact.Pin{Owner: "job-1", Key: key, Digest: entry.Digest, Size: entry.Size}
	if err := store.Pin(ctx, "team-upload", pin); err != nil {
		t.Fatalf("pin queued artifact: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = artifact.Open(ctx, root, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	replacementKey := key
	replacementKey.Native = "replacement"
	if _, _, err := store.Put(ctx, replacementKey, artifact.Metadata{}, bytes.NewReader([]byte("drop"))); !errors.Is(err, artifact.ErrQuota) {
		t.Fatalf("Put while queued artifact is pinned = %v, want ErrQuota", err)
	}
	if _, file, err := store.Get(ctx, key); err != nil {
		t.Fatalf("Get pinned queued artifact: %v", err)
	} else {
		_ = file.Close()
	}

	if err := store.Unpin(ctx, "team-upload", pin.Owner); err != nil {
		t.Fatalf("release queued artifact pin: %v", err)
	}
	if _, _, err := store.Put(ctx, replacementKey, artifact.Metadata{}, bytes.NewReader([]byte("drop"))); err != nil {
		t.Fatalf("Put after pin release: %v", err)
	}
	if _, _, err := store.Get(ctx, key); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("Get released LRU entry = %v, want ErrNotFound", err)
	}
}

func TestReconcilePinsReleasesCompletedOwnersAndKeepsUnfinishedOwners(t *testing.T) {
	ctx := context.Background()
	store, err := artifact.Open(ctx, t.TempDir(), 8, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	staleKey := artifact.Key{
		Integration: "turbo", Project: "github.com/acme/widget",
		Compatibility: "linux-amd64-schema1", Native: "completed",
	}
	staleEntry, _, err := store.Put(ctx, staleKey, artifact.Metadata{}, bytes.NewReader([]byte("old!")))
	if err != nil {
		t.Fatal(err)
	}
	activeKey := staleKey
	activeKey.Native = "unfinished"
	activeEntry, _, err := store.Put(ctx, activeKey, artifact.Metadata{}, bytes.NewReader([]byte("live")))
	if err != nil {
		t.Fatal(err)
	}
	stalePin := artifact.Pin{Owner: "completed-job", Key: staleKey, Digest: staleEntry.Digest, Size: staleEntry.Size}
	activePin := artifact.Pin{Owner: "unfinished-job", Key: activeKey, Digest: activeEntry.Digest, Size: activeEntry.Size}
	for _, pin := range []artifact.Pin{stalePin, activePin} {
		if err := store.Pin(ctx, "team-upload", pin); err != nil {
			t.Fatalf("pin %s: %v", pin.Owner, err)
		}
	}

	pinned, err := store.ReconcilePins(ctx, "team-upload", []artifact.Pin{activePin})
	if err != nil {
		t.Fatalf("reconcile queued artifact pins: %v", err)
	}
	if pinned != 1 {
		t.Fatalf("reconciled pin count = %d, want 1", pinned)
	}
	if err := store.GC(ctx, 0); !errors.Is(err, artifact.ErrQuota) {
		t.Fatalf("GC with unfinished upload = %v, want ErrQuota", err)
	}
	if _, _, err := store.Get(ctx, staleKey); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("Get completed upload artifact = %v, want ErrNotFound", err)
	}
	if _, file, err := store.Get(ctx, activeKey); err != nil {
		t.Fatalf("Get unfinished upload artifact: %v", err)
	} else {
		_ = file.Close()
	}
}
