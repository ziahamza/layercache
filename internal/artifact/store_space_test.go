package artifact

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPutKeepsFiveGiBFloorWhenConfiguredReserveIsLower(t *testing.T) {
	const fiveGiB = int64(5 * 1024 * 1024 * 1024)

	store, err := Open(context.Background(), t.TempDir(), 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.probeSpace = func(string) (filesystemSpace, error) {
		return filesystemSpace{
			availableBytes: fiveGiB + 2,
			totalBytes:     20 * fiveGiB,
			allocationUnit: 1,
		}, nil
	}

	_, _, err = store.Put(context.Background(), recoveryTestKey("reserve-rejected"), Metadata{}, bytes.NewReader([]byte("new")))
	if !errors.Is(err, ErrMinFreeSpace) {
		t.Fatalf("Put error = %v, want ErrMinFreeSpace", err)
	}
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("Put error = %v, want it classified as ErrQuota", err)
	}
	if !strings.Contains(err.Error(), "Local Cache") {
		t.Fatalf("Put error = %q, want clear Local Cache wording", err)
	}
	stats, statsErr := store.Stats(context.Background())
	if statsErr != nil {
		t.Fatal(statsErr)
	}
	if stats != (Stats{}) {
		t.Fatalf("stats after rejected Put = %+v, want empty Local Cache", stats)
	}
}

func TestPutUsesFivePercentOfVolumeWhenItExceedsFixedReserve(t *testing.T) {
	const gib = int64(1024 * 1024 * 1024)

	store, err := Open(context.Background(), t.TempDir(), 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.probeSpace = func(string) (filesystemSpace, error) {
		return filesystemSpace{
			availableBytes: 10*gib + 2,
			totalBytes:     200 * gib,
			allocationUnit: 1,
		}, nil
	}

	_, _, err = store.Put(context.Background(), recoveryTestKey("percentage-reserve"), Metadata{}, bytes.NewReader([]byte("new")))
	if !errors.Is(err, ErrMinFreeSpace) {
		t.Fatalf("Put error = %v, want 5%% volume reserve rejection", err)
	}
}

func TestPutTreatsConfiguredMinFreeBytesAsAnExplicitOverride(t *testing.T) {
	const gib = int64(1024 * 1024 * 1024)

	store, err := Open(context.Background(), t.TempDir(), 1<<20, 12*gib)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.probeSpace = func(string) (filesystemSpace, error) {
		return filesystemSpace{
			availableBytes: 12*gib + 2,
			totalBytes:     100 * gib,
			allocationUnit: 1,
		}, nil
	}

	_, _, err = store.Put(context.Background(), recoveryTestKey("configured-reserve"), Metadata{}, bytes.NewReader([]byte("new")))
	if !errors.Is(err, ErrMinFreeSpace) {
		t.Fatalf("Put error = %v, want configured reserve rejection", err)
	}
}

func TestExplicitReserveReplacesPercentageFloor(t *testing.T) {
	const gib = int64(1024 * 1024 * 1024)
	for _, test := range []struct{ configured, want int64 }{
		{0, 50 * gib}, {10 * gib, 10 * gib}, {1, 5 * gib}, {60 * gib, 60 * gib},
	} {
		store := &Store{minFreeBytes: test.configured}
		if got := store.effectiveReserve(1000 * gib); got != test.want {
			t.Fatalf("configured %d: reserve = %d, want %d", test.configured, got, test.want)
		}
	}
}

func TestPutAdmitsBytesWhenAllocationLeavesTheReserveIntact(t *testing.T) {
	const fiveGiB = int64(5 * 1024 * 1024 * 1024)

	store, err := Open(context.Background(), t.TempDir(), 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.probeSpace = func(string) (filesystemSpace, error) {
		return filesystemSpace{
			availableBytes: fiveGiB + 4,
			totalBytes:     20 * fiveGiB,
			allocationUnit: 4,
		}, nil
	}

	entry, created, err := store.Put(context.Background(), recoveryTestKey("reserve-admitted"), Metadata{}, bytes.NewReader([]byte("safe")))
	if err != nil {
		t.Fatal(err)
	}
	if !created || entry.Size != 4 {
		t.Fatalf("Put = created %v, entry %+v; want a new four-byte entry", created, entry)
	}
}

func TestPutDoesNotEvictCommittedEntriesForRejectedStaging(t *testing.T) {
	const fiveGiB = int64(5 * 1024 * 1024 * 1024)
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir(), 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	oldKey := recoveryTestKey("free-space-victim")
	if _, _, err := store.Put(ctx, oldKey, Metadata{}, bytes.NewReader([]byte("old!"))); err != nil {
		t.Fatal(err)
	}
	recentKey := recoveryTestKey("free-space-recent")
	if _, _, err := store.Put(ctx, recentKey, Metadata{}, bytes.NewReader([]byte("keep"))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Head(ctx, recentKey); err != nil {
		t.Fatal(err)
	}
	store.probeSpace = func(string) (filesystemSpace, error) {
		usage, usageErr := store.Usage(ctx)
		if usageErr != nil {
			return filesystemSpace{}, usageErr
		}
		return filesystemSpace{
			availableBytes: fiveGiB + 8 - usage,
			totalBytes:     20 * fiveGiB,
			allocationUnit: 1,
		}, nil
	}

	newKey := recoveryTestKey("free-space-rejected")
	_, _, err = store.Put(ctx, newKey, Metadata{}, bytes.NewReader([]byte("new!")))
	if !errors.Is(err, ErrMinFreeSpace) {
		t.Fatalf("Put error = %v, want ErrMinFreeSpace", err)
	}
	if _, err := store.Head(ctx, oldKey); err != nil {
		t.Fatalf("rejected staging evicted the least-recent entry: %v", err)
	}
	if _, err := store.Head(ctx, recentKey); err != nil {
		t.Fatalf("rejected staging evicted the recent entry: %v", err)
	}
	if _, err := store.Head(ctx, newKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected identity Head error = %v, want ErrNotFound", err)
	}
}

func TestKnownConflictIsRejectedBeforeFreeSpaceAdmission(t *testing.T) {
	const fiveGiB = int64(5 * 1024 * 1024 * 1024)
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir(), 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	protectedKey := recoveryTestKey("protected-identity")
	if _, _, err := store.Put(ctx, protectedKey, Metadata{}, bytes.NewReader([]byte("old!"))); err != nil {
		t.Fatal(err)
	}
	recentKey := recoveryTestKey("evictable-identity")
	if _, _, err := store.Put(ctx, recentKey, Metadata{}, bytes.NewReader([]byte("drop"))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Head(ctx, recentKey); err != nil {
		t.Fatal(err)
	}
	store.probeSpace = func(string) (filesystemSpace, error) {
		usage, usageErr := store.Usage(ctx)
		if usageErr != nil {
			return filesystemSpace{}, usageErr
		}
		return filesystemSpace{
			availableBytes: fiveGiB + 8 - usage,
			totalBytes:     20 * fiveGiB,
			allocationUnit: 1,
		}, nil
	}

	body := &recoveryReadTracker{reader: bytes.NewReader([]byte("new!"))}
	_, _, err = store.PutVerified(ctx, protectedKey, Metadata{}, body, strings.Repeat("0", 64))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting PutVerified error = %v, want ErrConflict", err)
	}
	if body.reads != 0 {
		t.Fatalf("known conflict read body %d times, want 0", body.reads)
	}
	_, file, err := store.Get(ctx, protectedKey)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	contents := make([]byte, 4)
	if _, err := file.Read(contents); err != nil {
		t.Fatal(err)
	}
	if string(contents) != "old!" {
		t.Fatalf("protected identity bytes = %q, want original bytes", contents)
	}
	if _, err := store.Head(ctx, recentKey); err != nil {
		t.Fatalf("known conflict evicted a committed entry: %v", err)
	}
}

func TestBadExpectedDigestDoesNotEvictCommittedEntries(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir(), 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	committedKey := recoveryTestKey("digest-preserved")
	if _, _, err := store.Put(ctx, committedKey, Metadata{}, bytes.NewReader([]byte("old!"))); err != nil {
		t.Fatal(err)
	}
	_, _, err = store.PutVerified(
		ctx, recoveryTestKey("bad-digest"), Metadata{}, bytes.NewReader([]byte("new!")), strings.Repeat("0", 64),
	)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("bad-digest PutVerified error = %v, want ErrCorrupt", err)
	}
	if _, err := store.Head(ctx, committedKey); err != nil {
		t.Fatalf("bad expected digest evicted a committed entry: %v", err)
	}
}

func TestSpaceCheckedWritersDoNotOverAdmitConcurrentStagingBytes(t *testing.T) {
	const fiveGiB = int64(5 * 1024 * 1024 * 1024)

	store, err := Open(context.Background(), t.TempDir(), 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var available atomic.Int64
	available.Store(fiveGiB + 4)
	store.probeSpace = func(string) (filesystemSpace, error) {
		return filesystemSpace{
			availableBytes: available.Load(),
			totalBytes:     20 * fiveGiB,
			allocationUnit: 1,
		}, nil
	}

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	t.Cleanup(release)
	firstResult := make(chan error, 1)
	go func() {
		_, writeErr := store.SpaceCheckedWriter(context.Background(), spaceConsumingWriter{
			available: &available,
			entered:   firstEntered,
			release:   releaseFirst,
		}).Write([]byte("four"))
		firstResult <- writeErr
	}()
	<-firstEntered

	secondEntered := make(chan struct{})
	secondStarted := make(chan struct{})
	secondResult := make(chan error, 1)
	go func() {
		close(secondStarted)
		_, writeErr := store.SpaceCheckedWriter(context.Background(), spaceConsumingWriter{
			available: &available,
			entered:   secondEntered,
		}).Write([]byte("four"))
		secondResult <- writeErr
	}()
	<-secondStarted
	select {
	case <-secondEntered:
		t.Fatal("second staging write started before the first admission completed")
	case <-time.After(250 * time.Millisecond):
	}

	release()
	if err := <-firstResult; err != nil {
		t.Fatalf("first staging write: %v", err)
	}
	if err := <-secondResult; !errors.Is(err, ErrMinFreeSpace) {
		t.Fatalf("second staging write error = %v, want ErrMinFreeSpace", err)
	}
	select {
	case <-secondEntered:
		t.Fatal("rejected second staging write reached its destination")
	default:
	}
}

type spaceConsumingWriter struct {
	available *atomic.Int64
	entered   chan<- struct{}
	release   <-chan struct{}
}

func (writer spaceConsumingWriter) Write(bytes []byte) (int, error) {
	close(writer.entered)
	if writer.release != nil {
		<-writer.release
	}
	writer.available.Add(-int64(len(bytes)))
	return len(bytes), nil
}
