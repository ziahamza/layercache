//go:build linux || darwin

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

const maximumSecretFileBytes = int64(1 << 20)

func readSecretFile(path string) (string, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", fmt.Errorf("open protected secret file: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return "", fmt.Errorf("inspect protected secret file: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		return "", errors.New("secret file must be a regular single-link file")
	}
	if stat.Mode&0o077 != 0 {
		return "", errors.New("secret file must not be accessible by group or other users")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return "", errors.New("secret file must be owned by the current user")
	}
	if stat.Size > maximumSecretFileBytes {
		return "", fmt.Errorf("secret file exceeds %d bytes", maximumSecretFileBytes)
	}
	contents, err := io.ReadAll(io.LimitReader(file, maximumSecretFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("read protected secret file: %w", err)
	}
	if int64(len(contents)) > maximumSecretFileBytes {
		return "", fmt.Errorf("secret file exceeds %d bytes", maximumSecretFileBytes)
	}
	value := strings.TrimSuffix(string(contents), "\n")
	value = strings.TrimSuffix(value, "\r")
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("secret file must contain one value without embedded NUL or newline characters")
	}
	return value, nil
}
