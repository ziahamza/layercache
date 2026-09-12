package cloud

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/artifact"
)

func TestCloudQuotaAdministrationAgainstPostgresAndS3(t *testing.T) {
	if os.Getenv("LAYER_CACHE_QA_POSTGRES_URL") == "" || os.Getenv("LAYER_CACHE_QA_S3_ENDPOINT") == "" {
		t.Skip("set PostgreSQL and S3 QA endpoints")
	}
	ctx := context.Background()
	cfg := Config{
		PostgresURL: os.Getenv("LAYER_CACHE_QA_POSTGRES_URL"), S3Endpoint: os.Getenv("LAYER_CACHE_QA_S3_ENDPOINT"),
		S3Bucket: os.Getenv("LAYER_CACHE_QA_S3_BUCKET"), S3Region: "us-east-1", S3PathStyle: true,
		Project: "quota-qa-" + time.Now().UTC().Format("20060102t150405.000000000"), MaxBytes: 8,
	}
	store, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key := artifact.Key{Integration: "turbo", Project: cfg.Project, Compatibility: "linux-amd64", Native: "pinned"}
	entry, _, err := store.Put(ctx, key, artifact.Metadata{}, bytes.NewBufferString("12345678"))
	if err != nil {
		t.Fatal(err)
	}
	pin := artifact.Pin{Owner: "important", Key: key, Digest: entry.Digest, Size: entry.Size}
	if err := store.SetAdministratorPin(ctx, "qa-admin", pin); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetQuota(ctx, "qa-admin", 4); !errors.Is(err, ErrQuota) {
		t.Fatalf("shrink below pin: %v", err)
	}
	quota, err := store.Quota(ctx)
	if err != nil || quota.MaxBytes != 8 || quota.UsedBytes != 8 {
		t.Fatalf("failed shrink changed quota: %+v, %v", quota, err)
	}
	if _, err := store.SetQuota(ctx, "qa-admin", 24); err != nil {
		t.Fatal(err)
	}
	replica, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	quota, err = replica.Quota(ctx)
	if err != nil || quota.MaxBytes != 24 {
		t.Fatalf("restart reverted administrator quota: %+v, %v", quota, err)
	}
	second := key
	second.Native = "larger-than-startup-quota"
	if _, _, err := replica.Put(ctx, second, artifact.Metadata{}, bytes.NewBufferString("abcdefghijklmnop")); err != nil {
		t.Fatalf("replica did not apply raised quota: %v", err)
	}
	pins, err := replica.AdministratorPins(ctx, "", 100)
	if err != nil || len(pins) != 1 || pins[0] != pin {
		t.Fatalf("persisted pins: %+v, %v", pins, err)
	}
	if err := replica.RemoveAdministratorPin(ctx, "qa-admin", pin.Owner); err != nil {
		t.Fatal(err)
	}
	quota, err = store.SetQuota(ctx, "qa-admin", 4)
	if err != nil || quota.UsedBytes > 4 || quota.MaxBytes != 4 {
		t.Fatalf("shrink: %+v, %v", quota, err)
	}
	if _, err := store.Head(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unpinned victim remained: %v", err)
	}
	events, err := store.ReadAudit(ctx, AuditQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, event := range events {
		if event.Actor == "qa-admin" {
			found[event.Action] = true
		}
	}
	for _, action := range []string{"quota.set", "cache.pin", "cache.unpin"} {
		if !found[action] {
			t.Errorf("missing audit action %s", action)
		}
	}
}
