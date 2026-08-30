//go:build !linux && !darwin

package artifact

import (
	"errors"
	"runtime"
)

func defaultProbeSpace(string) (filesystemSpace, error) {
	return filesystemSpace{}, errors.New("free-space admission is not supported on " + runtime.GOOS)
}
