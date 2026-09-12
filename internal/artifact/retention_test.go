package artifact

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/layercache/layercache/internal/retention"
)

func TestImpactPutKeepsExpensiveArtifactAtTheSameQuotaAsLRU(t *testing.T) {
	for _, policy := range []retention.Policy{retention.LRU, retention.Impact} {
		t.Run(string(policy), func(t *testing.T) {
			ctx := context.Background()
			store, err := Open(ctx, t.TempDir(), 8, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := store.SetEvictionPolicy(policy); err != nil {
				t.Fatal(err)
			}
			putRetentionEntry(t, store, "expensive", "keep", 60000)
			putRetentionEntry(t, store, "cheap", "drop", 100)
			// A digest failure is rejected before it can affect retained data.
			if _, _, err := store.PutVerified(ctx, retentionKey("bad-upload"), Metadata{}, bytes.NewBufferString("fail"), strings.Repeat("0", 64)); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("corrupt publication error = %v", err)
			}
			if stats, err := store.Stats(ctx); err != nil || stats.Entries != 2 || stats.UsageBytes != 8 {
				t.Fatalf("failed upload changed retained data: %+v, %v", stats, err)
			}
			putRetentionEntry(t, store, "incoming", "next", 100)
			victim, survivor := "expensive", "cheap"
			if policy == retention.Impact {
				victim, survivor = survivor, victim
			}
			if _, err := store.Head(ctx, retentionKey(victim)); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s victim remains: %v", victim, err)
			}
			if _, err := store.Head(ctx, retentionKey(survivor)); err != nil {
				t.Fatalf("%s was unexpectedly evicted: %v", survivor, err)
			}
			if used, err := store.Usage(ctx); err != nil || used != 8 {
				t.Fatalf("quota accounting = %d, %v", used, err)
			}
		})
	}
}

func TestImpactDeduplicatesByteCostAndPreservesEveryPinnedAlias(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir(), 8, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SetEvictionPolicy(retention.Impact); err != nil {
		t.Fatal(err)
	}
	var shared Entry
	for _, name := range []string{"alias-a", "alias-b", "alias-c"} {
		duration := int64(0)
		if name == "alias-a" {
			duration = 10000
		}
		// One observed producer cost describes the shared bytes. Additional
		// aliases neither erase that evidence nor multiply its retention value.
		shared = putRetentionEntry(t, store, name, "same", duration)
	}
	putRetentionEntry(t, store, "other", "else", 6000)
	if err := store.GC(ctx, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Head(ctx, retentionKey("other")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("shared bytes were charged more than once: %v", err)
	}
	if stats, err := store.Stats(ctx); err != nil || stats.UsageBytes != 4 || stats.Artifacts != 1 || stats.Entries != 3 {
		t.Fatalf("deduplicated survivor = %+v, %v", stats, err)
	}
	pin := Pin{Owner: "required", Key: shared.Key, Digest: shared.Digest, Size: shared.Size}
	if err := store.Pin(ctx, "retention-test", pin); err != nil {
		t.Fatal(err)
	}
	if err := store.GC(ctx, 0); !errors.Is(err, ErrQuota) {
		t.Fatalf("GC below all-alias pin = %v", err)
	}
	for _, name := range []string{"alias-a", "alias-b", "alias-c"} {
		if _, err := store.Head(ctx, retentionKey(name)); err != nil {
			t.Fatalf("alias %s of pinned blob disappeared: %v", name, err)
		}
	}
	if err := store.Unpin(ctx, "retention-test", pin.Owner); err != nil {
		t.Fatal(err)
	}
	if err := store.GC(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if stats, err := store.Stats(ctx); err != nil || stats.UsageBytes != 0 || stats.Entries != 0 {
		t.Fatalf("whole group not reclaimed: %+v, %v", stats, err)
	}
}

func TestImpactUnknownBlobFallsBackToLRUWithoutTreatingItAsFree(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir(), 8, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SetEvictionPolicy(retention.Impact); err != nil {
		t.Fatal(err)
	}
	putRetentionEntry(t, store, "known-old", "old!", 60000)
	putRetentionEntry(t, store, "unknown-new", "new!", 0)
	if err := store.GC(ctx, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Head(ctx, retentionKey("known-old")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown cost should cause an LRU decision: %v", err)
	}
	if _, err := store.Head(ctx, retentionKey("unknown-new")); err != nil {
		t.Fatalf("unknown was treated as zero-cost recomputation: %v", err)
	}
}

func TestImpactVerifiedReadFrequencySurvivesReopenAndRejectsCorruptReads(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := Open(ctx, root, 8, 0)
	if err != nil {
		t.Fatal(err)
	}
	entry := putRetentionEntry(t, store, "popular", "keep", 1000)
	for range 3 {
		_, file, err := store.Get(ctx, entry.Key)
		if err != nil {
			t.Fatal(err)
		}
		file.Close()
	}
	if err := os.WriteFile(store.blobPath(entry.Digest), []byte("oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Get(ctx, entry.Key); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt Get = %v", err)
	}
	if err := os.WriteFile(store.blobPath(entry.Digest), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	var reads int64
	if err := store.db.QueryRowContext(ctx, `SELECT read_count FROM artifact_access WHERE digest = ?`, entry.Digest).Scan(&reads); err != nil || reads != 3 {
		t.Fatalf("verified read counter = %d, %v", reads, err)
	}
	putRetentionEntry(t, store, "recent", "next", 1000)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, root, 8, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SetEvictionPolicy(retention.Impact); err != nil {
		t.Fatal(err)
	}
	if err := store.GC(ctx, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Head(ctx, entry.Key); err != nil {
		t.Fatalf("reopen lost observed popularity: %v", err)
	}
}

func putRetentionEntry(t *testing.T, store *Store, name, contents string, durationMS int64) Entry {
	t.Helper()
	entry, _, err := store.Put(context.Background(), retentionKey(name), Metadata{DurationMS: durationMS}, bytes.NewBufferString(contents))
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func retentionKey(name string) Key {
	return Key{Integration: "turbo", Project: "github.com/acme/retention", Compatibility: "linux-amd64", Native: name}
}
