package cloud

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/artifact"
)

func TestProactivePruningBelowQuota(t *testing.T) {
	if os.Getenv("LAYER_CACHE_QA_POSTGRES_URL") == "" || os.Getenv("LAYER_CACHE_QA_S3_ENDPOINT") == "" {
		t.Skip("requires disposable PostgreSQL/S3")
	}
	ctx := context.Background()
	now := time.Now().UTC()
	cfg := Config{PostgresURL: os.Getenv("LAYER_CACHE_QA_POSTGRES_URL"), S3Endpoint: os.Getenv("LAYER_CACHE_QA_S3_ENDPOINT"),
		S3Bucket: os.Getenv("LAYER_CACHE_QA_S3_BUCKET"), S3Region: "us-east-1", S3PathStyle: true,
		Project: fmt.Sprintf("prune-%d", now.UnixNano()), MaxBytes: 1 << 20, IdleTTL: 7 * 24 * time.Hour, UnreusedTTL: 24 * time.Hour, Now: func() time.Time { return now }}
	store, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key := func(name string) artifact.Key {
		return artifact.Key{Project: cfg.Project, Integration: "turbo", Compatibility: "test", Native: name}
	}
	for _, name := range []string{"cold", "warm", "pinned", "reading"} {
		if _, _, err := store.Put(ctx, key(name), artifact.Metadata{}, bytes.NewBufferString(name)); err != nil {
			t.Fatal(err)
		}
	}
	pinned, err := store.Head(ctx, key("pinned"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Pin(ctx, "test", artifact.Pin{Owner: "pin", Key: key("pinned"), Digest: pinned.Digest, Size: pinned.Size}); err != nil {
		t.Fatal(err)
	}
	_, body, err := store.Get(ctx, key("warm"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(io.Discard, body); err != nil {
		t.Fatal(err)
	}
	body.Close()
	now = now.Add(25 * time.Hour)
	_, reader, err := store.Get(ctx, key("reading"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := store.pruneExpiredEntries(ctx, now)
	if err != nil || n != 1 {
		t.Fatalf("prune=%d,%v", n, err)
	}
	if _, err := store.Head(ctx, key("cold")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cold retained: %v", err)
	}
	for _, name := range []string{"warm", "pinned", "reading"} {
		if _, err := store.Head(ctx, key(name)); err != nil {
			t.Fatal(name, err)
		}
	}
	reader.Close()
	now = now.Add(8 * 24 * time.Hour)
	n, err = store.pruneExpiredEntries(ctx, now)
	if err != nil || n != 2 {
		t.Fatalf("idle prune=%d,%v", n, err)
	}
	if _, err := store.Head(ctx, key("pinned")); err != nil {
		t.Fatal(err)
	}
	if err := store.Unpin(ctx, "test", "pin"); err != nil {
		t.Fatal(err)
	}
	if err := store.GC(ctx, 0); err != nil {
		t.Fatal(err)
	}
}
