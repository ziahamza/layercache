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
	"github.com/layercache/layercache/internal/retention"
)

func TestCloudImpactRetentionAgainstPostgresAndS3(t *testing.T) {
	if os.Getenv("LAYER_CACHE_QA_POSTGRES_URL") == "" || os.Getenv("LAYER_CACHE_QA_S3_ENDPOINT") == "" {
		t.Skip("set PostgreSQL and S3 QA endpoints")
	}
	for _, policy := range []retention.Policy{retention.LRU, retention.Impact} {
		t.Run(string(policy), func(t *testing.T) {
			ctx := context.Background()
			cfg := Config{
				PostgresURL: os.Getenv("LAYER_CACHE_QA_POSTGRES_URL"), S3Endpoint: os.Getenv("LAYER_CACHE_QA_S3_ENDPOINT"),
				S3Bucket: os.Getenv("LAYER_CACHE_QA_S3_BUCKET"), S3Region: "us-east-1", S3PathStyle: true,
				Project: fmt.Sprintf("retention-%s-%d", policy, time.Now().UnixNano()), MaxBytes: 8, EvictionPolicy: policy,
			}
			store, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			key := func(name string) artifact.Key {
				return artifact.Key{Project: cfg.Project, Integration: "turbo", Compatibility: "linux-amd64", Native: name}
			}
			put := func(name, body string, duration int64) artifact.Entry {
				t.Helper()
				entry, _, err := store.Put(ctx, key(name), artifact.Metadata{DurationMS: duration}, bytes.NewBufferString(body))
				if err != nil {
					t.Fatal(err)
				}
				return entry
			}
			put("expensive", "keep", 60000)
			put("cheap", "drop", 100)
			put("incoming", "next", 100)
			victim, survivor := "expensive", "cheap"
			if policy == retention.Impact {
				victim, survivor = survivor, victim
			}
			if _, err := store.Head(ctx, key(victim)); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s remains: %v", victim, err)
			}
			entry, body, err := store.Get(ctx, key(survivor))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, body); err != nil {
				t.Fatal(err)
			}
			body.Close()
			alias := key("survivor-alias")
			contents := "drop"
			if policy == retention.Impact {
				contents = "keep"
			}
			// A new alias without its own timing keeps the known cost for the
			// same blob, and never counts that blob's bytes twice.
			if _, _, err := store.Put(ctx, alias, artifact.Metadata{}, bytes.NewBufferString(contents)); err != nil {
				t.Fatal(err)
			}
			pin := artifact.Pin{Owner: "required", Key: alias, Digest: entry.Digest, Size: entry.Size}
			if err := store.Pin(ctx, "retention-test", pin); err != nil {
				t.Fatal(err)
			}
			replica, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer replica.Close()
			var reads int64
			if err := replica.database.QueryRowContext(ctx, `SELECT read_count FROM layercache_artifact_access_v1 WHERE project_id = $1 AND digest = $2`, cfg.Project, entry.Digest).Scan(&reads); err != nil || reads != 1 {
				t.Fatalf("verified frequency did not survive reopen: %d, %v", reads, err)
			}
			if err := replica.GC(ctx, 4); err != nil {
				t.Fatal(err)
			}
			if policy == retention.Impact {
				if _, err := replica.Head(ctx, entry.Key); err != nil {
					t.Fatalf("unpinned alias of pinned blob was deleted: %v", err)
				}
			}
			if used, err := replica.Usage(ctx); err != nil || used != 4 {
				t.Fatalf("deduplicated usage = %d, %v", used, err)
			}
			if err := replica.GC(ctx, 0); !errors.Is(err, ErrQuota) {
				t.Fatalf("GC below pin = %v", err)
			}
			if err := replica.Unpin(ctx, "retention-test", pin.Owner); err != nil {
				t.Fatal(err)
			}
			if err := replica.GC(ctx, 0); err != nil {
				t.Fatal(err)
			}
			put("old-known", "old!", 60000)
			put("new-unknown", "new!", 0)
			if err := replica.GC(ctx, 4); err != nil {
				t.Fatal(err)
			}
			if _, err := replica.Head(ctx, key("old-known")); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing blob timing did not fall back to LRU: %v", err)
			}
			if _, err := replica.Head(ctx, key("new-unknown")); err != nil {
				t.Fatalf("unknown producer cost was treated as free: %v", err)
			}
			if err := replica.GC(ctx, 0); err != nil {
				t.Fatal(err)
			}
		})
	}
}
