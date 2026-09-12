//go:build linux || darwin

package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

const runtimeDirectoryLockName = ".layercache-runtime.lock"

type runtimeDirectoryLock struct {
	file *os.File
	once sync.Once
	err  error
}

func acquireRuntimeDirectoryLock(dataDir string) (*runtimeDirectoryLock, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create runtime data directory: %w", err)
	}
	path := filepath.Join(dataDir, runtimeDirectoryLockName)
	fd, err := unix.Open(path, unix.O_CLOEXEC|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open runtime data-directory lock: %w", err)
	}
	closeFD := true
	defer func() {
		if closeFD {
			_ = unix.Close(fd)
		}
	}()
	var status unix.Stat_t
	if err := unix.Fstat(fd, &status); err != nil {
		return nil, fmt.Errorf("inspect runtime data-directory lock: %w", err)
	}
	if status.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.New("runtime data-directory lock is not a regular file")
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return nil, fmt.Errorf("protect runtime data-directory lock: %w", err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf("another Layer Cache runtime is already using data directory %s", dataDir)
		}
		return nil, fmt.Errorf("lock runtime data directory: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, errors.New("adopt runtime data-directory lock file")
	}
	closeFD = false
	return &runtimeDirectoryLock{file: file}, nil
}

func (lock *runtimeDirectoryLock) Close() error {
	if lock == nil {
		return nil
	}
	lock.once.Do(func() {
		unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
		closeErr := lock.file.Close()
		lock.err = errors.Join(unlockErr, closeErr)
	})
	return lock.err
}
