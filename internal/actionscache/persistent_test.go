package actionscache_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/actionscache"
	"github.com/layercache/layercache/internal/artifact"
)

func TestPersistentStorageRetriesTeamPublicationAfterRestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	team := &toggleTeamStorage{MemoryStorage: actionscache.NewMemoryStorage(), fail: true}
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-amd64-node24",
	}

	artifacts, err := artifact.Open(ctx, root, 64<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	local, err := actionscache.OpenPersistentStorage(ctx, root, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	chain, err := actionscache.NewCacheChain(local, team)
	if err != nil {
		t.Fatal(err)
	}
	producerDuration := 23 * time.Second
	commitArchiveWithProducerDuration(t, chain, scope, "durable-team", "v1", []byte("survives restart"), producerDuration)
	publicationStats, err := local.TeamPublicationStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if publicationStats.PendingJobs != 1 || publicationStats.PendingBytes != int64(len("survives restart")) {
		t.Fatalf("pending Actions Team publications = %#v", publicationStats)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if err := artifacts.Close(); err != nil {
		t.Fatal(err)
	}

	team.setFail(false)
	artifacts, err = artifact.Open(ctx, root, 64<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.Close()
	local, err = actionscache.OpenPersistentStorage(ctx, root, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	if _, err := actionscache.NewCacheChain(local, team); err != nil {
		t.Fatal(err)
	}
	// The publisher retries on a five-second idle poll. An equal test deadline
	// races that poll under CI load; allow two polls plus scheduling headroom.
	deadline := time.Now().Add(15 * time.Second)
	remotePublished := false
	var remote actionscache.LookupResult
	for {
		if !remotePublished {
			result, lookupErr := team.Lookup(ctx, actionscache.LookupRequest{
				Scope: scope, Keys: []string{"durable-team"}, Version: "v1",
			})
			remotePublished = lookupErr == nil && result.Match == actionscache.MatchExact
			remote = result
		}
		if remotePublished {
			publicationStats, statsErr := local.TeamPublicationStats(ctx)
			if statsErr != nil {
				t.Fatal(statsErr)
			}
			if publicationStats.PendingJobs == 0 && publicationStats.PendingBytes == 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("durable Team Cache publication did not finish after restart (remote published: %t)", remotePublished)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if remote.Entry.ProducerDuration == nil || *remote.Entry.ProducerDuration != producerDuration {
		t.Fatalf("durable Team publication producer duration = %v, want %s", remote.Entry.ProducerDuration, producerDuration)
	}
}

func TestPersistentStoragePinsQueuedTeamArtifactUntilPublicationCompletes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	artifacts, err := artifact.Open(ctx, filepath.Join(root, "cas"), 64<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.Close()
	local, err := actionscache.OpenPersistentStorage(ctx, filepath.Join(root, "actions"), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	team := &toggleTeamStorage{MemoryStorage: actionscache.NewMemoryStorage(), fail: true}
	chain, err := actionscache.NewCacheChain(local, team)
	if err != nil {
		t.Fatal(err)
	}
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-amd64-node24",
	}
	putArchive(t, chain, scope, "pinned-team-job", "v1", []byte("queued bytes"))
	key := artifact.Key{
		Integration: "actions", Project: scope.Repository, Compatibility: scope.Compatibility,
		Native: "pinned-team-job", Version: "v1", Ref: scope.Ref,
	}
	if err := artifacts.Delete(ctx, key); !errors.Is(err, artifact.ErrPinned) {
		t.Fatalf("delete queued Team Cache artifact = %v, want artifact.ErrPinned", err)
	}

	team.setFail(false)
	// The publisher retries on a five-second idle poll. An equal test deadline
	// races that poll under CI load; allow two polls plus scheduling headroom.
	deadline := time.Now().Add(15 * time.Second)
	for {
		stats, statsErr := local.TeamPublicationStats(ctx)
		if statsErr != nil {
			t.Fatal(statsErr)
		}
		if stats.PendingJobs == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queued Team Cache publication did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for {
		err := artifacts.Delete(ctx, key)
		if err == nil {
			break
		}
		if !errors.Is(err, artifact.ErrPinned) || time.Now().After(deadline) {
			t.Fatalf("delete artifact after Team Cache publication = %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPersistentStorageReconcilesQueuePinsAfterRestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	casRoot := filepath.Join(root, "cas")
	actionsRoot := filepath.Join(root, "actions")
	artifacts, err := artifact.Open(ctx, casRoot, 64<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	local, err := actionscache.OpenPersistentStorage(ctx, actionsRoot, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	team := &toggleTeamStorage{MemoryStorage: actionscache.NewMemoryStorage(), fail: true}
	chain, err := actionscache.NewCacheChain(local, team)
	if err != nil {
		t.Fatal(err)
	}
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-amd64-node24",
	}
	putArchive(t, chain, scope, "restart-pin", "v1", []byte("restart bytes"))
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := artifacts.ReconcilePins(ctx, "actions-team-publication", nil); err != nil {
		t.Fatal(err)
	}
	if err := artifacts.Close(); err != nil {
		t.Fatal(err)
	}

	artifacts, err = artifact.Open(ctx, casRoot, 64<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.Close()
	local, err = actionscache.OpenPersistentStorage(ctx, actionsRoot, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	key := artifact.Key{
		Integration: "actions", Project: scope.Repository, Compatibility: scope.Compatibility,
		Native: "restart-pin", Version: "v1", Ref: scope.Ref,
	}
	if err := artifacts.Delete(ctx, key); !errors.Is(err, artifact.ErrPinned) {
		t.Fatalf("delete recovered queued artifact = %v, want artifact.ErrPinned", err)
	}
}

func TestPersistentStorageKeepsMissingArtifactPublicationQueued(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	casRoot := filepath.Join(root, "cas")
	actionsRoot := filepath.Join(root, "actions")
	artifacts, err := artifact.Open(ctx, casRoot, 64<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.Close()
	local, err := actionscache.OpenPersistentStorage(ctx, actionsRoot, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	team := &toggleTeamStorage{MemoryStorage: actionscache.NewMemoryStorage(), fail: true}
	chain, err := actionscache.NewCacheChain(local, team)
	if err != nil {
		t.Fatal(err)
	}
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-amd64-node24",
	}
	putArchive(t, chain, scope, "missing-team-artifact", "v1", []byte("missing later"))
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := artifacts.ReconcilePins(ctx, "actions-team-publication", nil); err != nil {
		t.Fatal(err)
	}
	key := artifact.Key{
		Integration: "actions", Project: scope.Repository, Compatibility: scope.Compatibility,
		Native: "missing-team-artifact", Version: "v1", Ref: scope.Ref,
	}
	if err := artifacts.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	metadata, err := sql.Open("sqlite", filepath.Join(actionsRoot, "actions.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.ExecContext(ctx, `UPDATE actions_team_publications SET next_attempt = 0`); err != nil {
		metadata.Close()
		t.Fatal(err)
	}
	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}

	local, err = actionscache.OpenPersistentStorage(ctx, actionsRoot, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := actionscache.NewCacheChain(local, actionscache.NewMemoryStorage()); err != nil {
		t.Fatal(err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}

	local, err = actionscache.OpenPersistentStorage(ctx, actionsRoot, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	stats, err := local.TeamPublicationStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.PendingJobs != 1 || stats.PendingBytes != int64(len("missing later")) {
		t.Fatalf("missing-artifact Team Cache queue = %#v, want the job retained", stats)
	}
}

func TestPersistentStorageDrainsDueTeamPublicationOnClose(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	artifacts, err := artifact.Open(ctx, filepath.Join(root, "cas"), 64<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	local, err := actionscache.OpenPersistentStorage(ctx, filepath.Join(root, "actions"), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	team := &cancelFirstTeamStorage{
		MemoryStorage: actionscache.NewMemoryStorage(),
		firstStarted:  make(chan struct{}),
	}
	chain, err := actionscache.NewCacheChain(local, team)
	if err != nil {
		t.Fatal(err)
	}
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-amd64-node24",
	}
	putArchive(t, chain, scope, "graceful-shutdown", "v1", []byte("published before shutdown"))
	select {
	case <-team.firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("background Team Cache publication did not start")
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if err := artifacts.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := team.Lookup(ctx, actionscache.LookupRequest{
		Scope: scope, Keys: []string{"graceful-shutdown"}, Version: "v1",
	})
	if err != nil || result.Match != actionscache.MatchExact {
		t.Fatalf("Team Cache lookup after graceful shutdown = %#v, %v", result, err)
	}
}

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

func TestPersistentLookupInvalidatesCorruptBytesAndFallsBackToTeam(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	casRoot := filepath.Join(root, "cas")
	artifacts, err := artifact.Open(ctx, casRoot, 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.Close()
	local, err := actionscache.OpenPersistentStorage(ctx, filepath.Join(root, "actions"), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	team := actionscache.NewMemoryStorage()
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-amd64-node24",
	}
	putArchive(t, local, scope, "corruption-fallback", "v1", []byte("poisoned"))
	putArchive(t, team, scope, "corruption-fallback", "v1", []byte("healthy!"))
	blobs, err := filepath.Glob(filepath.Join(casRoot, "blobs", "sha256", "*", "*"))
	if err != nil || len(blobs) != 1 {
		t.Fatalf("find Local Cache blob = %v, %v", blobs, err)
	}
	if err := os.WriteFile(blobs[0], []byte("corrupt!"), 0o600); err != nil {
		t.Fatal(err)
	}
	chain, err := actionscache.NewCacheChain(local, team)
	if err != nil {
		t.Fatal(err)
	}
	result, err := chain.Lookup(ctx, actionscache.LookupRequest{
		Scope: scope, Keys: []string{"corruption-fallback"}, Version: "v1",
	})
	if err != nil || result.Source != actionscache.SourceTeamCache {
		t.Fatalf("lookup after local corruption = %#v, %v", result, err)
	}
	archive, err := chain.Open(ctx, actionscache.OpenRequest{Scope: scope, ID: result.Entry.ID})
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(archive.Body)
	closeErr := archive.Body.Close()
	if readErr != nil || closeErr != nil || string(got) != "healthy!" {
		t.Fatalf("fallback archive = %q, read %v, close %v", got, readErr, closeErr)
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

type toggleTeamStorage struct {
	*actionscache.MemoryStorage
	mu   sync.Mutex
	fail bool
}

type cancelFirstTeamStorage struct {
	*actionscache.MemoryStorage
	firstOnce    sync.Once
	firstStarted chan struct{}
}

func (storage *cancelFirstTeamStorage) Reserve(
	ctx context.Context,
	request actionscache.ReserveRequest,
) (actionscache.Reservation, error) {
	first := false
	storage.firstOnce.Do(func() {
		first = true
		close(storage.firstStarted)
	})
	if first {
		<-ctx.Done()
		return actionscache.Reservation{}, ctx.Err()
	}
	return storage.MemoryStorage.Reserve(ctx, request)
}

func (storage *toggleTeamStorage) Reserve(ctx context.Context, request actionscache.ReserveRequest) (actionscache.Reservation, error) {
	storage.mu.Lock()
	fail := storage.fail
	storage.mu.Unlock()
	if fail {
		return actionscache.Reservation{}, errors.New("simulated Team Cache outage")
	}
	return storage.MemoryStorage.Reserve(ctx, request)
}

func (storage *toggleTeamStorage) setFail(fail bool) {
	storage.mu.Lock()
	storage.fail = fail
	storage.mu.Unlock()
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
