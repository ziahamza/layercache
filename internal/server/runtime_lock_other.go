//go:build !linux && !darwin

package server

import "errors"

type runtimeDirectoryLock struct{}

func acquireRuntimeDirectoryLock(string) (*runtimeDirectoryLock, error) {
	return nil, errors.New("runtime data-directory locking is unsupported on this platform")
}

func (*runtimeDirectoryLock) Close() error { return nil }
