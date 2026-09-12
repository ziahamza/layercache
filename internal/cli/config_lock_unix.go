//go:build linux || darwin

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

func lockConfiguration(ctx context.Context, configPath string) (func(), error) {
	directory := filepath.Dir(configPath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("prepare configuration lock directory: %w", err)
	}
	lockPath := configPath + ".lock"
	fd, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open configuration lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), lockPath)
	closeFile := func() { _ = file.Close() }
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		closeFile()
		return nil, fmt.Errorf("inspect configuration lock: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		closeFile()
		return nil, errors.New("configuration lock is not a regular single-link file")
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		closeFile()
		return nil, fmt.Errorf("protect configuration lock: %w", err)
	}
	for {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() {
				_ = unix.Flock(fd, unix.LOCK_UN)
				closeFile()
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			closeFile()
			return nil, fmt.Errorf("lock configuration: %w", err)
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			closeFile()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
