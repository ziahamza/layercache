//go:build linux || darwin

package buildkit

import (
	"errors"
	"math"

	"golang.org/x/sys/unix"
)

func filesystemCapacityBytes(path string) (int64, error) {
	var stats unix.Statfs_t
	if err := unix.Statfs(path, &stats); err != nil {
		return 0, err
	}
	if stats.Bsize <= 0 {
		return 0, errors.New("filesystem returned an invalid block size")
	}
	blockSize := uint64(stats.Bsize)
	blocks := uint64(stats.Blocks)
	if blocks > uint64(math.MaxInt64)/blockSize {
		return math.MaxInt64, nil
	}
	return int64(blocks * blockSize), nil
}
