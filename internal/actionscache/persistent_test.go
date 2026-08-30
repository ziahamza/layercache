package actionscache_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/layercache/layercache/internal/actionscache"
	"github.com/layercache/layercache/internal/artifact"
)

func TestPersistentStorageStreamsUploadWithBoundedMemory(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storage, closeStorage := openPersistentStorage(t, ctx)
	defer closeStorage()
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-x64-node24",
	}
	const archiveSize = int64(256 * 1024)
	reservation, err := storage.Reserve(ctx, actionscache.ReserveRequest{
		Scope: scope, Key: "streamed", Version: "v1", CacheSize: int64Pointer(archiveSize),
	})
	if err != nil {
		t.Fatal(err)
	}
	body := &maxReadSizeReader{remaining: archiveSize, maxReadSize: 32 * 1024}
	if err := storage.Upload(ctx, actionscache.UploadRequest{
		ReservationID: reservation.ID, Start: 0, End: archiveSize - 1, Body: body,
	}); err != nil {
		t.Fatalf("stream upload: %v", err)
	}
	entry, err := storage.Commit(ctx, actionscache.CommitRequest{ReservationID: reservation.ID, Size: archiveSize})
	if err != nil {
		t.Fatal(err)
	}
	archive, err := storage.Open(ctx, actionscache.OpenRequest{Scope: scope, ID: entry.ID})
	if err != nil {
		t.Fatal(err)
	}
	written, readErr := io.Copy(io.Discard, archive.Body)
	closeErr := archive.Body.Close()
	if readErr != nil || closeErr != nil || written != archiveSize {
		t.Fatalf("opened archive bytes = %d, read error = %v, close error = %v", written, readErr, closeErr)
	}
}

func TestPersistentStorageRejectsBodyLengthMismatchWithoutPublishing(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		body string
	}{
		{name: "truncated", body: "ab"},
		{name: "extra", body: "abcd"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			storage, closeStorage := openPersistentStorage(t, ctx)
			defer closeStorage()
			scope := actionscache.Scope{
				Repository: "acme/widgets", Ref: "refs/heads/main",
				DefaultRef: "refs/heads/main", Compatibility: "linux-x64-node24",
			}
			reservation, err := storage.Reserve(ctx, actionscache.ReserveRequest{
				Scope: scope, Key: "invalid-" + test.name, Version: "v1",
			})
			if err != nil {
				t.Fatal(err)
			}
			err = storage.Upload(ctx, actionscache.UploadRequest{
				ReservationID: reservation.ID, Start: 0, End: 2, Body: strings.NewReader(test.body),
			})
			if !errors.Is(err, actionscache.ErrInvalidUpload) {
				t.Fatalf("upload error = %v, want ErrInvalidUpload", err)
			}
			if _, err := storage.Commit(ctx, actionscache.CommitRequest{
				ReservationID: reservation.ID, Size: 3,
			}); !errors.Is(err, actionscache.ErrIncompleteUpload) {
				t.Fatalf("commit after rejected chunk error = %v, want ErrIncompleteUpload", err)
			}
			if _, err := storage.Lookup(ctx, actionscache.LookupRequest{
				Scope: scope, Keys: []string{"invalid-" + test.name}, Version: "v1",
			}); !errors.Is(err, actionscache.ErrNotFound) {
				t.Fatalf("lookup after rejected chunk error = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestPersistentStorageRepeatedChunkIsIdempotentOnlyForTheSameBytes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storage, closeStorage := openPersistentStorage(t, ctx)
	defer closeStorage()
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-x64-node24",
	}
	reservation, err := storage.Reserve(ctx, actionscache.ReserveRequest{
		Scope: scope, Key: "repeated", Version: "v1", CacheSize: int64Pointer(3),
	})
	if err != nil {
		t.Fatal(err)
	}
	upload := func(body string) error {
		return storage.Upload(ctx, actionscache.UploadRequest{
			ReservationID: reservation.ID, Start: 0, End: 2, Body: strings.NewReader(body),
		})
	}
	if err := upload("abc"); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	if err := upload("abc"); err != nil {
		t.Fatalf("idempotent upload: %v", err)
	}
	if err := upload("abd"); !errors.Is(err, actionscache.ErrInvalidUpload) {
		t.Fatalf("conflicting repeated upload error = %v, want ErrInvalidUpload", err)
	}
	entry, err := storage.Commit(ctx, actionscache.CommitRequest{ReservationID: reservation.ID, Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	archive, err := storage.Open(ctx, actionscache.OpenRequest{Scope: scope, ID: entry.ID})
	if err != nil {
		t.Fatal(err)
	}
	contents, readErr := io.ReadAll(archive.Body)
	closeErr := archive.Body.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(contents, []byte("abc")) {
		t.Fatalf("archive = %q, read error = %v, close error = %v", contents, readErr, closeErr)
	}
}

func TestPersistentStorageRejectsOverlappingRangesAndPreservesOutOfOrderChunks(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storage, closeStorage := openPersistentStorage(t, ctx)
	defer closeStorage()
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-x64-node24",
	}
	reservation, err := storage.Reserve(ctx, actionscache.ReserveRequest{
		Scope: scope, Key: "out-of-order", Version: "v1", CacheSize: int64Pointer(6),
	})
	if err != nil {
		t.Fatal(err)
	}
	upload := func(start int64, body string) error {
		return storage.Upload(ctx, actionscache.UploadRequest{
			ReservationID: reservation.ID, Start: start,
			End: start + int64(len(body)) - 1, Body: strings.NewReader(body),
		})
	}
	if err := upload(3, "def"); err != nil {
		t.Fatalf("out-of-order trailing chunk: %v", err)
	}
	if err := upload(2, "cde"); !errors.Is(err, actionscache.ErrInvalidUpload) {
		t.Fatalf("overlapping upload error = %v, want ErrInvalidUpload", err)
	}
	if err := upload(0, "abc"); err != nil {
		t.Fatalf("out-of-order leading chunk: %v", err)
	}
	entry, err := storage.Commit(ctx, actionscache.CommitRequest{ReservationID: reservation.ID, Size: 6})
	if err != nil {
		t.Fatal(err)
	}
	archive, err := storage.Open(ctx, actionscache.OpenRequest{Scope: scope, ID: entry.ID})
	if err != nil {
		t.Fatal(err)
	}
	contents, readErr := io.ReadAll(archive.Body)
	closeErr := archive.Body.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(contents, []byte("abcdef")) {
		t.Fatalf("archive = %q, read error = %v, close error = %v", contents, readErr, closeErr)
	}
}

func TestPersistentStorageAccountsForConcurrentOverlappingUploadsBeforeReading(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storage, closeStorage := openPersistentStorage(t, ctx)
	defer closeStorage()
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-x64-node24",
	}
	reservation, err := storage.Reserve(ctx, actionscache.ReserveRequest{
		Scope: scope, Key: "concurrent-overlap", Version: "v1", CacheSize: int64Pointer(6),
	})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	firstResult := make(chan error, 1)
	go func() {
		firstResult <- storage.Upload(ctx, actionscache.UploadRequest{
			ReservationID: reservation.ID, Start: 0, End: 3,
			Body: &gatedReader{reader: strings.NewReader("abcd"), started: started, release: release},
		})
	}()
	<-started
	secondBody := &readTrackingReader{reader: strings.NewReader("cdef")}
	secondErr := storage.Upload(ctx, actionscache.UploadRequest{
		ReservationID: reservation.ID, Start: 2, End: 5, Body: secondBody,
	})
	close(release)
	firstErr := <-firstResult
	if !errors.Is(secondErr, actionscache.ErrInvalidUpload) {
		t.Fatalf("concurrent overlapping upload error = %v, want ErrInvalidUpload", secondErr)
	}
	if secondBody.reads != 0 {
		t.Fatalf("rejected overlapping upload read body %d times, want 0", secondBody.reads)
	}
	if firstErr != nil {
		t.Fatalf("admitted upload error = %v", firstErr)
	}
}

func TestPersistentStorageBoundsUnknownSizeReservationByConfiguredMaximum(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	storage, closeStorage := openPersistentStorage(t, ctx)
	defer closeStorage()
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-x64-node24",
	}
	reservation, err := storage.Reserve(ctx, actionscache.ReserveRequest{
		Scope: scope, Key: "bounded-unknown-size", Version: "v1", MaxArtifactBytes: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	rejectedBody := &readTrackingReader{reader: strings.NewReader("x")}
	err = storage.Upload(ctx, actionscache.UploadRequest{
		ReservationID: reservation.ID, Start: 6, End: 6, Body: rejectedBody,
	})
	if !errors.Is(err, actionscache.ErrInvalidUpload) {
		t.Fatalf("upload beyond configured maximum error = %v, want ErrInvalidUpload", err)
	}
	if rejectedBody.reads != 0 {
		t.Fatalf("upload beyond configured maximum read body %d times, want 0", rejectedBody.reads)
	}
	for _, chunk := range []struct {
		start int64
		body  string
	}{{start: 3, body: "def"}, {start: 0, body: "abc"}} {
		if err := storage.Upload(ctx, actionscache.UploadRequest{
			ReservationID: reservation.ID, Start: chunk.start,
			End: chunk.start + int64(len(chunk.body)) - 1, Body: strings.NewReader(chunk.body),
		}); err != nil {
			t.Fatalf("upload chunk at %d: %v", chunk.start, err)
		}
	}
	if _, err := storage.Commit(ctx, actionscache.CommitRequest{
		ReservationID: reservation.ID, Size: 6,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPersistentStorageBoundsAggregateRetainedUploadsBeforeReading(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	artifacts, err := artifact.Open(ctx, filepath.Join(root, "cas"), 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.Close()
	storage, err := actionscache.OpenPersistentStorage(ctx, filepath.Join(root, "actions"), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-x64-node24",
	}
	reserve := func(key string) actionscache.Reservation {
		reservation, reserveErr := storage.Reserve(ctx, actionscache.ReserveRequest{
			Scope: scope, Key: key, Version: "v1", CacheSize: int64Pointer(4),
		})
		if reserveErr != nil {
			t.Fatal(reserveErr)
		}
		return reservation
	}
	first := reserve("aggregate-first")
	second := reserve("aggregate-second")
	if err := storage.Upload(ctx, actionscache.UploadRequest{
		Scope: &scope, ReservationID: first.ID, Start: 0, End: 3, Body: strings.NewReader("abcd"),
	}); err != nil {
		t.Fatal(err)
	}
	rejected := &readTrackingReader{reader: strings.NewReader("wxyz")}
	err = storage.Upload(ctx, actionscache.UploadRequest{
		Scope: &scope, ReservationID: second.ID, Start: 0, End: 3, Body: rejected,
	})
	if !errors.Is(err, artifact.ErrStagingQuota) {
		t.Fatalf("aggregate upload error = %v, want artifact.ErrStagingQuota", err)
	}
	if rejected.reads != 0 {
		t.Fatalf("aggregate-rejected upload read body %d times, want 0", rejected.reads)
	}
	if _, err := storage.Commit(ctx, actionscache.CommitRequest{
		Scope: &scope, ReservationID: first.ID, Size: 4,
	}); err != nil {
		t.Fatal(err)
	}
	if err := storage.Upload(ctx, actionscache.UploadRequest{
		Scope: &scope, ReservationID: second.ID, Start: 0, End: 3, Body: strings.NewReader("wxyz"),
	}); err != nil {
		t.Fatalf("upload after committed staging release: %v", err)
	}
	if err := storage.Abort(ctx, second.ID); err != nil {
		t.Fatalf("abort retained upload: %v", err)
	}
	third := reserve("aggregate-third")
	if err := storage.Upload(ctx, actionscache.UploadRequest{
		Scope: &scope, ReservationID: third.ID, Start: 0, End: 3, Body: strings.NewReader("last"),
	}); err != nil {
		t.Fatalf("upload after aborted staging release: %v", err)
	}
}

func TestPersistentStorageRestartDiscardsIncompleteUploadsAndPreservesCommittedEntries(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	casRoot := filepath.Join(root, "cas")
	actionsRoot := filepath.Join(root, "actions")
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-x64-node24",
	}

	artifacts, err := artifact.Open(ctx, casRoot, 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := actionscache.OpenPersistentStorage(ctx, actionsRoot, artifacts)
	if err != nil {
		artifacts.Close()
		t.Fatal(err)
	}
	committed, err := storage.Reserve(ctx, actionscache.ReserveRequest{
		Scope: scope, Key: "committed", Version: "v1", CacheSize: int64Pointer(3),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Upload(ctx, actionscache.UploadRequest{
		ReservationID: committed.ID, Start: 0, End: 2, Body: strings.NewReader("old"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Commit(ctx, actionscache.CommitRequest{ReservationID: committed.ID, Size: 3}); err != nil {
		t.Fatal(err)
	}
	abandoned, err := storage.Reserve(ctx, actionscache.ReserveRequest{
		Scope: scope, Key: "abandoned", Version: "v1", CacheSize: int64Pointer(6),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Upload(ctx, actionscache.UploadRequest{
		ReservationID: abandoned.ID, Start: 0, End: 2, Body: strings.NewReader("old"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	if err := artifacts.Close(); err != nil {
		t.Fatal(err)
	}

	artifacts, err = artifact.Open(ctx, casRoot, 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.Close()
	storage, err = actionscache.OpenPersistentStorage(ctx, actionsRoot, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()

	result, err := storage.Lookup(ctx, actionscache.LookupRequest{
		Scope: scope, Keys: []string{"committed"}, Version: "v1",
	})
	if err != nil {
		t.Fatalf("lookup committed entry after restart: %v", err)
	}
	archive, err := storage.Open(ctx, actionscache.OpenRequest{Scope: scope, ID: result.Entry.ID})
	if err != nil {
		t.Fatal(err)
	}
	contents, readErr := io.ReadAll(archive.Body)
	closeErr := archive.Body.Close()
	if readErr != nil || closeErr != nil || string(contents) != "old" {
		t.Fatalf("committed archive after restart = %q, read error = %v, close error = %v", contents, readErr, closeErr)
	}
	if _, err := storage.Reserve(ctx, actionscache.ReserveRequest{
		Scope: scope, Key: "abandoned", Version: "v1", CacheSize: int64Pointer(3),
	}); err != nil {
		t.Fatalf("reserve identity abandoned before restart: %v", err)
	}
	staged, err := filepath.Glob(filepath.Join(actionsRoot, "actions-staging", "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(staged) != 0 {
		t.Fatalf("staged files after restart = %v, want none", staged)
	}
}

func openPersistentStorage(
	t *testing.T,
	ctx context.Context,
) (*actionscache.PersistentStorage, func()) {
	t.Helper()
	root := t.TempDir()
	artifacts, err := artifact.Open(ctx, filepath.Join(root, "cas"), 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := actionscache.OpenPersistentStorage(ctx, filepath.Join(root, "actions"), artifacts)
	if err != nil {
		artifacts.Close()
		t.Fatal(err)
	}
	return storage, func() {
		if err := storage.Close(); err != nil {
			t.Error(err)
		}
		if err := artifacts.Close(); err != nil {
			t.Error(err)
		}
	}
}

func int64Pointer(value int64) *int64 {
	return &value
}

type maxReadSizeReader struct {
	remaining   int64
	maxReadSize int
}

type gatedReader struct {
	reader  io.Reader
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (reader *gatedReader) Read(buffer []byte) (int, error) {
	reader.once.Do(func() { close(reader.started) })
	<-reader.release
	return reader.reader.Read(buffer)
}

type readTrackingReader struct {
	reader io.Reader
	reads  int
}

func (reader *readTrackingReader) Read(buffer []byte) (int, error) {
	reader.reads++
	return reader.reader.Read(buffer)
}

func (reader *maxReadSizeReader) Read(buffer []byte) (int, error) {
	if len(buffer) > reader.maxReadSize {
		return 0, fmt.Errorf("reader received a %d-byte buffer, limit is %d", len(buffer), reader.maxReadSize)
	}
	if reader.remaining == 0 {
		return 0, io.EOF
	}
	count := len(buffer)
	if int64(count) > reader.remaining {
		count = int(reader.remaining)
	}
	for index := range buffer[:count] {
		buffer[index] = 'x'
	}
	reader.remaining -= int64(count)
	return count, nil
}
