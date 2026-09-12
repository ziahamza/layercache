package buildkit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const maxBuildkitReservedBytes = int64(2 << 30)

func validateLocalCacheBudget(budget LocalCacheBudget) error {
	if budget.StateDir == "" {
		return errors.New("BuildKit state directory is required with a Local Cache budget")
	}
	if budget.MaxBytes <= 0 {
		return errors.New("Local Cache budget must be positive")
	}
	if budget.BuildkitMaxBytes <= 0 {
		return errors.New("BuildKit Local Cache budget must be positive")
	}
	if budget.UsedBytes < 0 {
		return errors.New("Local Cache usage cannot be negative")
	}
	if budget.MinFreeBytes < 0 {
		return errors.New("minimum free disk space cannot be negative")
	}
	return nil
}

type nativeGCBudget struct {
	reservedBytes int64
	maxUsedBytes  int64
	minFreeBytes  int64
}

func (budget LocalCacheBudget) nativeGC() (nativeGCBudget, error) {
	minFreeBytes, err := EffectiveMinFreeBytes(budget.StateDir, budget.MinFreeBytes)
	if err != nil {
		return nativeGCBudget{}, err
	}
	remaining := budget.MaxBytes - budget.UsedBytes
	if remaining > budget.BuildkitMaxBytes {
		remaining = budget.BuildkitMaxBytes
	}
	if remaining < 1 {
		// Buildx treats zero as an omitted limit. One byte asks native GC to
		// remove every disposable cache record when the shared budget is full.
		remaining = 1
	}
	reserved := remaining / 10
	if reserved > maxBuildkitReservedBytes {
		reserved = maxBuildkitReservedBytes
	}
	return nativeGCBudget{
		reservedBytes: reserved,
		maxUsedBytes:  remaining,
		minFreeBytes:  minFreeBytes,
	}, nil
}

// daemonGC is stable across ordinary Local Cache usage changes because a
// running BuildKit daemon does not reload this file. Dynamic sharing is
// enforced by explicit pre- and post-build prune operations.
func (budget LocalCacheBudget) daemonGC() (nativeGCBudget, error) {
	budget.UsedBytes = 0
	return budget.nativeGC()
}

// EffectiveMinFreeBytes applies the same percentage floor as the artifact
// store to BuildKit's native GC policy. The configured value remains an
// upward override; large filesystems reserve at least five percent.
func EffectiveMinFreeBytes(path string, configured int64) (int64, error) {
	if configured < 0 {
		return 0, errors.New("minimum free disk space cannot be negative")
	}
	probePath, err := existingFilesystemPath(path)
	if err != nil {
		return 0, fmt.Errorf("locate BuildKit cache filesystem: %w", err)
	}
	totalBytes, err := filesystemCapacityBytes(probePath)
	if err != nil {
		return 0, fmt.Errorf("inspect BuildKit cache filesystem capacity: %w", err)
	}
	return effectiveMinFreeBytes(configured, totalBytes), nil
}

func effectiveMinFreeBytes(configured, totalBytes int64) int64 {
	if fivePercent := totalBytes / 20; fivePercent > configured {
		return fivePercent
	}
	return configured
}

func existingFilesystemPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("BuildKit state directory is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	for candidate := filepath.Clean(absolute); ; candidate = filepath.Dir(candidate) {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return "", fmt.Errorf("no existing ancestor for %s", path)
		}
	}
}

func (a *Adapter) writeBuildkitdConfig() (string, error) {
	if a.config.LocalBudget == nil {
		return "", nil
	}
	if err := os.MkdirAll(a.config.LocalBudget.StateDir, 0o700); err != nil {
		return "", fmt.Errorf("create BuildKit state directory: %w", err)
	}
	budget, err := a.config.LocalBudget.daemonGC()
	if err != nil {
		return "", err
	}
	path := filepath.Join(a.config.LocalBudget.StateDir, "buildkitd.toml")
	content := []byte(fmt.Sprintf(
		"[worker.oci]\n  gc = true\n  reservedSpace = %q\n  maxUsedSpace = %q\n  minFreeSpace = %q\n",
		formatBuildkitBytes(budget.reservedBytes),
		formatBuildkitBytes(budget.maxUsedBytes),
		formatBuildkitBytes(budget.minFreeBytes),
	))
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, content) {
		return path, nil
	}
	temporary, err := os.CreateTemp(a.config.LocalBudget.StateDir, ".buildkitd-*.toml")
	if err != nil {
		return "", fmt.Errorf("stage BuildKit GC configuration: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return "", fmt.Errorf("protect BuildKit GC configuration: %w", err)
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return "", fmt.Errorf("write BuildKit GC configuration: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return "", fmt.Errorf("sync BuildKit GC configuration: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close BuildKit GC configuration: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", fmt.Errorf("commit BuildKit GC configuration: %w", err)
	}
	return path, nil
}

func (a *Adapter) pruneToLocalBudget(ctx context.Context, stderr io.Writer) error {
	budget, err := a.config.LocalBudget.nativeGC()
	if err != nil {
		return err
	}
	command := Command{Path: a.config.DockerCommand, Args: []string{
		"buildx", "prune", "--builder", a.config.BuilderName, "--force",
		"--reserved-space", formatBuildkitBytes(budget.reservedBytes),
		"--max-used-space", formatBuildkitBytes(budget.maxUsedBytes),
		"--min-free-space", formatBuildkitBytes(budget.minFreeBytes),
	}}
	if err := a.runner.Run(ctx, command, stderr, stderr); err != nil {
		return err
	}
	return nil
}

func formatBuildkitBytes(value int64) string {
	return fmt.Sprintf("%dB", value)
}
