package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPutRemovesNewBlobWhenMetadataCommitFails(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir(), 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	commitFailure := errors.New("forced metadata commit failure")
	store.commitTx = func(*sql.Tx) error { return commitFailure }
	body := []byte("new artifact bytes")
	digest := fmt.Sprintf("%x", sha256.Sum256(body))

	_, _, err = store.Put(ctx, recoveryTestKey("new"), Metadata{}, bytes.NewReader(body))
	if !errors.Is(err, commitFailure) {
		t.Fatalf("Put error = %v, want forced commit failure", err)
	}
	if _, err := os.Stat(store.blobPath(digest)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new blob remains after failed metadata commit: %v", err)
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats != (Stats{}) {
		t.Fatalf("stats after failed metadata commit = %+v, want empty Store", stats)
	}
}

func TestPutRemovesNewBlobWhenMetadataCommitIsCanceled(t *testing.T) {
	checkCtx := context.Background()
	store, err := Open(checkCtx, t.TempDir(), 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	putCtx, cancel := context.WithCancel(checkCtx)
	store.commitTx = func(*sql.Tx) error {
		cancel()
		return putCtx.Err()
	}
	body := []byte("canceled artifact bytes")
	digest := fmt.Sprintf("%x", sha256.Sum256(body))

	_, _, err = store.Put(putCtx, recoveryTestKey("canceled"), Metadata{}, bytes.NewReader(body))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Put error = %v, want context cancellation", err)
	}
	if _, err := os.Stat(store.blobPath(digest)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new blob remains after canceled metadata commit: %v", err)
	}
	stats, err := store.Stats(checkCtx)
	if err != nil {
		t.Fatal(err)
	}
	if stats != (Stats{}) {
		t.Fatalf("stats after canceled metadata commit = %+v, want empty Store", stats)
	}
}

func TestPutPreservesNewBlobWhenCommitReportsFailureAfterMetadataCommitted(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir(), 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	commitFailure := errors.New("commit outcome unavailable")
	store.commitTx = func(tx *sql.Tx) error {
		if err := tx.Commit(); err != nil {
			return err
		}
		return commitFailure
	}
	body := []byte("committed artifact bytes")
	key := recoveryTestKey("ambiguous-commit")

	_, _, err = store.Put(ctx, key, Metadata{}, bytes.NewReader(body))
	if !errors.Is(err, commitFailure) {
		t.Fatalf("Put error = %v, want ambiguous commit failure", err)
	}
	_, file, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get committed entry after ambiguous commit: %v", err)
	}
	defer file.Close()
	got, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("restored bytes = %q, want %q", got, body)
	}
}

func TestPutFailureDoesNotRemoveExistingDeduplicatedBlob(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir(), 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	body := []byte("deduplicated artifact bytes")
	firstKey := recoveryTestKey("existing-digest")
	if _, _, err := store.Put(ctx, firstKey, Metadata{}, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	commitFailure := errors.New("forced metadata commit failure")
	store.commitTx = func(*sql.Tx) error { return commitFailure }

	_, _, err = store.Put(ctx, recoveryTestKey("failed-shared-digest"), Metadata{}, bytes.NewReader(body))
	if !errors.Is(err, commitFailure) {
		t.Fatalf("Put error = %v, want forced commit failure", err)
	}
	_, file, err := store.Get(ctx, firstKey)
	if err != nil {
		t.Fatalf("Get existing deduplicated entry: %v", err)
	}
	got, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read existing deduplicated entry: read %v, close %v", readErr, closeErr)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("existing deduplicated bytes = %q, want %q", got, body)
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantStats := Stats{UsageBytes: int64(len(body)), MetadataBytes: stats.MetadataBytes, Artifacts: 1, Entries: 1}
	if stats != wantStats {
		t.Fatalf("stats after failed shared-digest Put = %+v, want %+v", stats, wantStats)
	}
}

func TestPutVerifiedRepairsMissingCanonicalBlobForEverySharedEntry(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir(), 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	body := []byte("shared artifact to repair")
	digest := fmt.Sprintf("%x", sha256.Sum256(body))
	keys := []Key{recoveryTestKey("missing-one"), recoveryTestKey("missing-two")}
	for _, key := range keys {
		if _, _, err := store.Put(ctx, key, Metadata{}, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(store.blobPath(digest)); err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if _, _, err := store.Get(ctx, key); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Get %s before repair error = %v, want ErrCorrupt", key.Native, err)
		}
	}

	entry, created, err := store.PutVerified(ctx, keys[0], Metadata{}, bytes.NewReader(body), "sha256:"+digest)
	if err != nil {
		t.Fatal(err)
	}
	if created || entry.Digest != digest {
		t.Fatalf("PutVerified repair = created %v, entry %+v; want existing digest", created, entry)
	}
	for _, key := range keys {
		assertRecoveryContents(t, store, key, body)
	}
}

func TestPutRepairsCorruptCanonicalBlobForMatchingLogicalEntry(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir(), 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	body := []byte("canonical artifact")
	digest := fmt.Sprintf("%x", sha256.Sum256(body))
	key := recoveryTestKey("corrupt-canonical")
	if _, _, err := store.Put(ctx, key, Metadata{}, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.blobPath(digest), []byte("corrupt artifact!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Get(ctx, key); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Get before repair error = %v, want ErrCorrupt", err)
	}

	entry, created, err := store.Put(ctx, key, Metadata{}, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if created || entry.Digest != digest {
		t.Fatalf("Put repair = created %v, entry %+v; want existing digest", created, entry)
	}
	assertRecoveryContents(t, store, key, body)
}

func TestDeduplicatedPutRepairsCanonicalBlobBeforeAddingNewLogicalEntry(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir(), 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	body := []byte("shared canonical artifact")
	digest := fmt.Sprintf("%x", sha256.Sum256(body))
	first := recoveryTestKey("shared-corrupt-first")
	second := recoveryTestKey("shared-corrupt-second")
	if _, _, err := store.Put(ctx, first, Metadata{}, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.blobPath(digest), []byte("corrupted shared bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Get(ctx, first); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Get before shared repair error = %v, want ErrCorrupt", err)
	}

	entry, created, err := store.Put(ctx, second, Metadata{}, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if !created || entry.Digest != digest {
		t.Fatalf("deduplicated repair = created %v, entry %+v", created, entry)
	}
	assertRecoveryContents(t, store, first, body)
	assertRecoveryContents(t, store, second, body)
}

func TestPutCannotRepairMissingBlobWithConflictingBytes(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir(), 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	winner := []byte("immutable winner")
	winnerDigest := fmt.Sprintf("%x", sha256.Sum256(winner))
	key := recoveryTestKey("missing-conflict")
	if _, _, err := store.Put(ctx, key, Metadata{}, bytes.NewReader(winner)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.blobPath(winnerDigest)); err != nil {
		t.Fatal(err)
	}

	_, _, err = store.Put(ctx, key, Metadata{}, bytes.NewReader([]byte("different bytes")))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting Put error = %v, want ErrConflict", err)
	}
	entry, err := store.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Digest != winnerDigest {
		t.Fatalf("winner digest = %q, want %q", entry.Digest, winnerDigest)
	}
	if _, _, err := store.Get(ctx, key); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Get after conflicting Put error = %v, want missing winner to remain ErrCorrupt", err)
	}
}

func TestPutVerifiedRejectsKnownLogicalConflictBeforeReadingOrStaging(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir(), 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	winner := []byte("immutable winner")
	key := recoveryTestKey("preflight-conflict")
	if _, _, err := store.Put(ctx, key, Metadata{}, bytes.NewReader(winner)); err != nil {
		t.Fatal(err)
	}
	conflicting := []byte("different bytes")
	conflictingDigest := fmt.Sprintf("%x", sha256.Sum256(conflicting))
	body := &recoveryReadTracker{reader: bytes.NewReader(conflicting)}

	_, _, err = store.PutVerified(ctx, key, Metadata{}, body, conflictingDigest)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("PutVerified error = %v, want ErrConflict", err)
	}
	if body.reads != 0 {
		t.Fatalf("known conflicting PutVerified read body %d times, want 0", body.reads)
	}
	assertRecoveryContents(t, store, key, winner)
}

func TestPutVerifiedRejectsMalformedExpectedDigestBeforeReading(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir(), 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	body := &recoveryReadTracker{reader: bytes.NewReader([]byte("unused"))}

	_, _, err = store.PutVerified(ctx, recoveryTestKey("malformed-digest"), Metadata{}, body, "not-a-sha256")
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("PutVerified error = %v, want ErrCorrupt", err)
	}
	if body.reads != 0 {
		t.Fatalf("malformed PutVerified read body %d times, want 0", body.reads)
	}
}

func TestOpenReconcilesOnlyUnreferencedCanonicalBlobFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := Open(ctx, root, 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	liveBody := []byte("shared live artifact")
	for _, native := range []string{"shared-one", "shared-two"} {
		if _, _, err := store.Put(ctx, recoveryTestKey(native), Metadata{}, bytes.NewReader(liveBody)); err != nil {
			t.Fatal(err)
		}
	}
	liveDigest := fmt.Sprintf("%x", sha256.Sum256(liveBody))
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	orphanDigest := fmt.Sprintf("%x", sha256.Sum256([]byte("orphan artifact")))
	orphanPath := recoveryBlobPath(root, orphanDigest)
	writeRecoveryFile(t, orphanPath, []byte("unreferenced bytes"))

	filesToPreserve := map[string][]byte{
		filepath.Join(root, "blobs", "sha256", "ab", "short-name"):                     []byte("malformed digest name"),
		filepath.Join(root, "blobs", "sha256", "AB", strings.Repeat("a", 62)):          []byte("uppercase shard"),
		filepath.Join(root, "blobs", "sha256", "cd", strings.Repeat("E", 62)):          []byte("uppercase digest suffix"),
		filepath.Join(root, "blobs", "sha256", strings.Repeat("f", 64)):                []byte("missing shard directory"),
		filepath.Join(root, "blobs", "other-layout", "ab", strings.Repeat("1", 62)):    []byte("outside sha256 layout"),
		filepath.Join(root, "staging", "active-upload", "partial-artifact"):            []byte("staged bytes"),
		filepath.Join(root, "blobs", "sha256", "not-a-shard", strings.Repeat("2", 62)): []byte("malformed shard"),
	}
	for path, contents := range filesToPreserve {
		writeRecoveryFile(t, path, contents)
	}

	unexpectedDirectory := recoveryBlobPath(root, strings.Repeat("d", 64))
	writeRecoveryFile(t, filepath.Join(unexpectedDirectory, "nested"), []byte("directory at blob path"))
	outsideTarget := filepath.Join(root, "outside-owned-layout")
	writeRecoveryFile(t, outsideTarget, []byte("outside target"))
	symlinkPath := recoveryBlobPath(root, strings.Repeat("e", 64))
	if err := os.MkdirAll(filepath.Dir(symlinkPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideTarget, symlinkPath); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, root, 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	if _, err := os.Stat(orphanPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan blob remains after restart: %v", err)
	}
	if _, err := os.Stat(recoveryBlobPath(root, liveDigest)); err != nil {
		t.Fatalf("live shared blob removed during reconciliation: %v", err)
	}
	for _, native := range []string{"shared-one", "shared-two"} {
		_, file, err := reopened.Get(ctx, recoveryTestKey(native))
		if err != nil {
			t.Fatalf("Get %s after reconciliation: %v", native, err)
		}
		got, readErr := io.ReadAll(file)
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read %s after reconciliation: read %v, close %v", native, readErr, closeErr)
		}
		if !bytes.Equal(got, liveBody) {
			t.Fatalf("Get %s = %q, want %q", native, got, liveBody)
		}
	}
	for path, want := range filesToPreserve {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("unexpected file %s changed: got %q, error %v", path, got, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(unexpectedDirectory, "nested")); err != nil || string(got) != "directory at blob path" {
		t.Errorf("directory at canonical blob path changed: got %q, error %v", got, err)
	}
	if target, err := os.Readlink(symlinkPath); err != nil || target != outsideTarget {
		t.Errorf("symlink at canonical blob path changed: target %q, error %v", target, err)
	}
	if got, err := os.ReadFile(outsideTarget); err != nil || string(got) != "outside target" {
		t.Errorf("file outside owned blob layout changed: got %q, error %v", got, err)
	}
}

func recoveryTestKey(native string) Key {
	return Key{
		Integration:   "turbo",
		Project:       "github.com/acme/widget",
		Compatibility: "linux-amd64-schema1",
		Native:        native,
	}
}

func recoveryBlobPath(root, digest string) string {
	return filepath.Join(root, "blobs", "sha256", digest[:2], digest[2:])
}

func writeRecoveryFile(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertRecoveryContents(t *testing.T, store *Store, key Key, want []byte) {
	t.Helper()
	_, file, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read repaired %s: read %v, close %v", key.Native, readErr, closeErr)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("repaired %s bytes = %q, want %q", key.Native, got, want)
	}
}

type recoveryReadTracker struct {
	reader io.Reader
	reads  int
}

func (reader *recoveryReadTracker) Read(bytes []byte) (int, error) {
	reader.reads++
	return reader.reader.Read(bytes)
}
