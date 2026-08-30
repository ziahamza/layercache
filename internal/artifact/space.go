package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
)

const minimumFreeBytesFloor = int64(5 * 1024 * 1024 * 1024)

type filesystemSpace struct {
	availableBytes int64
	totalBytes     int64
	allocationUnit int64
}

type spaceCheckedWriter struct {
	ctx         context.Context
	store       *Store
	destination io.Writer
	lease       *StagingLease
}

// SpaceCheckedWriter protects writes to Local Cache staging files with the
// same minimum free-space admission policy used for artifact ingestion. The
// caller must use SyncStaged before committing the staged file.
func (store *Store) SpaceCheckedWriter(ctx context.Context, destination io.Writer) io.Writer {
	return store.spaceCheckedWriter(ctx, destination, nil)
}

func (store *Store) spaceCheckedWriter(ctx context.Context, destination io.Writer, lease *StagingLease) io.Writer {
	if ctx == nil {
		ctx = context.Background()
	}
	return &spaceCheckedWriter{ctx: ctx, store: store, destination: destination, lease: lease}
}

func (writer *spaceCheckedWriter) Write(bytes []byte) (int, error) {
	if writer.lease != nil {
		if err := writer.lease.grow(int64(len(bytes))); err != nil {
			return 0, err
		}
	}
	releaseUnwritten := func(written int) {
		if writer.lease == nil {
			return
		}
		if written < 0 {
			written = 0
		}
		if written > len(bytes) {
			written = len(bytes)
		}
		writer.lease.shrink(int64(len(bytes) - written))
	}

	writer.store.spaceMu.Lock()
	defer writer.store.spaceMu.Unlock()

	if err := writer.store.ensureFreeSpace(writer.ctx, int64(len(bytes))); err != nil {
		releaseUnwritten(0)
		return 0, err
	}
	written, err := writer.destination.Write(bytes)
	if written != len(bytes) && err == nil {
		releaseUnwritten(written)
		return written, io.ErrShortWrite
	}
	releaseUnwritten(written)
	return written, err
}

// SyncStaged flushes staged Local Cache bytes and confirms the filesystem
// still satisfies the minimum free-space policy before those bytes commit.
func (store *Store) SyncStaged(ctx context.Context, file interface{ Sync() error }) error {
	return store.syncStaged(ctx, file)
}

func (store *Store) syncStaged(ctx context.Context, file interface{ Sync() error }) error {
	store.spaceMu.Lock()
	defer store.spaceMu.Unlock()
	if err := file.Sync(); err != nil {
		return err
	}
	return store.ensureFreeSpace(ctx, 0)
}

// ensureFreeSpace runs while spaceMu is held. Staging never evicts committed
// entries: logical validation and quota admission happen only after the staged
// payload is complete, so an upload that will be rejected cannot disturb LRU.
func (store *Store) ensureFreeSpace(_ context.Context, writeBytes int64) error {
	space, err := store.readFilesystemSpace()
	if err != nil {
		return err
	}
	required := roundedAllocationBytes(writeBytes, space.allocationUnit)
	if !wouldViolateReserve(space.availableBytes, store.effectiveReserve(space.totalBytes), required) {
		return nil
	}
	return ErrMinFreeSpace
}

func (store *Store) readFilesystemSpace() (filesystemSpace, error) {
	space, err := store.probeSpace(store.root)
	if err != nil {
		return filesystemSpace{}, fmt.Errorf("inspect Local Cache free space: %w", err)
	}
	if space.availableBytes < 0 || space.totalBytes < 0 {
		return filesystemSpace{}, errors.New("inspect Local Cache free space: filesystem returned a negative byte count")
	}
	if space.allocationUnit <= 0 {
		space.allocationUnit = 1
	}
	return space, nil
}

func (store *Store) effectiveReserve(totalBytes int64) int64 {
	reserve := minimumFreeBytesFloor
	if store.minFreeBytes > reserve {
		reserve = store.minFreeBytes
	}
	if fivePercent := totalBytes / 20; fivePercent > reserve {
		reserve = fivePercent
	}
	return reserve
}

func wouldViolateReserve(availableBytes, reserveBytes, requiredBytes int64) bool {
	return availableBytes < reserveBytes || requiredBytes > availableBytes-reserveBytes
}

func roundedAllocationBytes(bytes, allocationUnit int64) int64 {
	if bytes <= 0 {
		return 0
	}
	if allocationUnit <= 1 {
		return bytes
	}
	remainder := bytes % allocationUnit
	if remainder == 0 {
		return bytes
	}
	increase := allocationUnit - remainder
	if bytes > math.MaxInt64-increase {
		return math.MaxInt64
	}
	return bytes + increase
}
