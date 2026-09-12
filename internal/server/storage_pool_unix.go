//go:build linux || darwin

package server

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The filesystem is the physical hard backstop, including databases, unknown
// length uploads and abandoned staging. Admission preserves working headroom;
// it is not a replacement for that filesystem limit.
type StoragePoolConfig struct {
	Path             string `json:"path"`
	MaxBytes         int64  `json:"maxBytes"`
	MinFreeBytes     int64  `json:"minFreeBytes"`
	HostPath         string `json:"hostPath"`
	HostMinFreeBytes int64  `json:"hostMinFreeBytes"`
}

func (p StoragePoolConfig) registryLease() (func(), error) {
	if p.Path == "" {
		return func() {}, nil
	}
	f, err := os.OpenFile(filepath.Join(p.Path, "control", "registry.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
}

func (p StoragePoolConfig) touchRegistry(repo, digest string) {
	if p.Path == "" || !canonicalGatewayPath("/"+repo) || !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 {
		return
	}
	for _, c := range digest[7:] {
		if !(c >= 'a' && c <= 'f' || c >= '0' && c <= '9') {
			return
		}
	}
	path := filepath.Join(p.Path, "registry/docker/registry/v2/repositories", repo, "_manifests/revisions/sha256", digest[7:], "link")
	now := time.Now()
	_ = os.Chtimes(path, now, now)
}

func poolSpace(path string) (int64, int64, error) {
	var s unix.Statfs_t
	if err := unix.Statfs(path, &s); err != nil {
		return 0, 0, err
	}
	return int64(s.Blocks) * int64(s.Bsize), int64(s.Bavail) * int64(s.Bsize), nil
}

func (p StoragePoolConfig) status() map[string]any {
	total, free, err := poolSpace(p.Path)
	result := map[string]any{"hardBytes": p.MaxBytes, "filesystemBytes": total, "availableBytes": free, "usedBytes": total - free, "reserveBytes": p.MinFreeBytes, "available": err == nil}
	if p.HostPath != "" {
		_, hostFree, hostErr := poolSpace(p.HostPath)
		result["hostAvailableBytes"] = hostFree
		result["hostReserveBytes"] = p.HostMinFreeBytes
		result["hostProbeHealthy"] = hostErr == nil
	}
	return result
}

func (p StoragePoolConfig) validate() error {
	if p.Path == "" {
		if p.MaxBytes != 0 {
			return errors.New("storage pool path required")
		}
		return nil
	}
	if !filepath.IsAbs(p.Path) || p.MaxBytes <= 0 || p.MinFreeBytes < 0 || p.MinFreeBytes >= p.MaxBytes || p.HostMinFreeBytes < 0 {
		return errors.New("invalid storage pool limits")
	}
	marker, err := os.ReadFile(filepath.Join(p.Path, ".layercache-pool"))
	if err != nil || strings.TrimSpace(string(marker)) != "layercache-pool-v1" {
		return errors.New("bounded storage pool is not mounted")
	}
	size, _, err := poolSpace(p.Path)
	if err != nil || size <= 0 || size > p.MaxBytes {
		return errors.New("storage pool filesystem exceeds hard limit or is unavailable")
	}
	if p.HostPath != "" {
		if _, _, err := poolSpace(p.HostPath); err != nil {
			return errors.New("host free-space probe unavailable")
		}
	}
	return nil
}

func (p StoragePoolConfig) admits(bytes int64) bool {
	if p.Path == "" {
		return true
	}
	_, free, err := poolSpace(p.Path)
	if err != nil || bytes < 0 || free < p.MinFreeBytes || bytes > free-p.MinFreeBytes {
		return false
	}
	if p.HostPath != "" {
		_, free, err = poolSpace(p.HostPath)
		if err != nil || free < p.HostMinFreeBytes || bytes > free-p.HostMinFreeBytes {
			return false
		}
	}
	return true
}

func (p StoragePoolConfig) contains(path string) bool {
	if p.Path == "" {
		return true
	}
	root, err := filepath.EvalSymlinks(p.Path)
	if err != nil {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, resolved)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
