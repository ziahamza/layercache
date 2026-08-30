package artifact_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/layercache/layercache/internal/artifact"
)

func TestReadStatsReadsClosedLocalCacheWithoutMutation(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "Local Cache with spaces & symbols")
	store, err := artifact.Open(ctx, root, 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, nativeKey := range []string{"first", "second"} {
		_, _, err := store.Put(ctx, artifact.Key{
			Integration: "turbo", Compatibility: "linux-amd64-schema1",
			Project: "github.com/acme/widget", Native: nativeKey,
		}, artifact.Metadata{}, bytes.NewReader([]byte("cached bytes")))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	staged := filepath.Join(root, "staging", "unfinished-upload")
	if err := os.WriteFile(staged, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	stats, err := artifact.ReadStats(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if stats.UsageBytes != 12 || stats.Artifacts != 1 || stats.Entries != 2 {
		t.Fatalf("offline stats = %+v, want 12 bytes, 1 artifact, 2 entries", stats)
	}
	if contents, err := os.ReadFile(staged); err != nil || string(contents) != "keep" {
		t.Fatalf("offline status changed staged data: %q, %v", contents, err)
	}
}
