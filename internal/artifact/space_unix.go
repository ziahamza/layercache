//go:build linux || darwin

package artifact

import (
	"math"

	"golang.org/x/sys/unix"
)

func defaultProbeSpace(path string) (filesystemSpace, error) {
	var stats unix.Statfs_t
	if err := unix.Statfs(path, &stats); err != nil {
		return filesystemSpace{}, err
	}
	blockSize := uint64(stats.Bsize)
	return filesystemSpace{
		availableBytes: filesystemBytes(stats.Bavail, blockSize),
		totalBytes:     filesystemBytes(stats.Blocks, blockSize),
		allocationUnit: filesystemBytes(1, blockSize),
	}, nil
}

func filesystemBytes(blocks, blockSize uint64) int64 {
	if blockSize != 0 && blocks > uint64(math.MaxInt64)/blockSize {
		return math.MaxInt64
	}
	return int64(blocks * blockSize)
}
