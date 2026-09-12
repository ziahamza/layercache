//go:build !linux && !darwin

package buildkit

import (
	"errors"
	"runtime"
)

func filesystemCapacityBytes(string) (int64, error) {
	return 0, errors.New("filesystem capacity inspection is not supported on " + runtime.GOOS)
}
