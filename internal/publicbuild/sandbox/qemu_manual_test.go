//go:build linux

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/publicbuild"
)

// TestManualQEMUWorker is an installed-host seam for immutable image builders.
// It is skipped unless the operator supplies real, digest-pinned boot assets
// and an already delegated cgroup root. It never falls back to a process or
// container executor.
func TestManualQEMUWorker(t *testing.T) {
	if os.Getenv("LAYERCACHE_QEMU_MANUAL_QA") != "1" {
		t.Skip("set LAYERCACHE_QEMU_MANUAL_QA=1 with pinned QEMU guest assets")
	}
	sandboxUID := mustManualUint32(t, "LAYERCACHE_QEMU_SANDBOX_UID")
	sandboxGID := mustManualUint32(t, "LAYERCACHE_QEMU_SANDBOX_GID")
	worker, err := NewQEMUWorker(QEMUConfig{
		WorkerID:       "manual-qemu-worker",
		QEMUPath:       mustManualEnvironment(t, "LAYERCACHE_QEMU"),
		QEMUImagePath:  mustManualEnvironment(t, "LAYERCACHE_QEMU_IMG"),
		Mke2fsPath:     mustManualEnvironment(t, "LAYERCACHE_MKE2FS"),
		DebugFSPath:    mustManualEnvironment(t, "LAYERCACHE_DEBUGFS"),
		KernelPath:     mustManualEnvironment(t, "LAYERCACHE_QEMU_KERNEL"),
		KernelSHA256:   mustManualEnvironment(t, "LAYERCACHE_QEMU_KERNEL_SHA256"),
		RootFSPath:     mustManualEnvironment(t, "LAYERCACHE_QEMU_ROOTFS"),
		RootFSSHA256:   mustManualEnvironment(t, "LAYERCACHE_QEMU_ROOTFS_SHA256"),
		ContractPath:   mustManualEnvironment(t, "LAYERCACHE_QEMU_CONTRACT"),
		ContractSHA256: mustManualEnvironment(t, "LAYERCACHE_QEMU_CONTRACT_SHA256"),
		CgroupRoot:     mustManualEnvironment(t, "LAYERCACHE_QEMU_CGROUP_ROOT"),
		WorkRoot:       mustManualEnvironment(t, "LAYERCACHE_QEMU_WORK_ROOT"),
		SandboxUID:     sandboxUID, SandboxGID: sandboxGID,
		MaxScratchBytes: 4 << 30,
		SourceFetcher:   manualSourceFetcher{}, SourceArchiveBytes: 64 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	recipe := worker.Contract().Recipes[0]
	build := publicbuild.Build{
		ID: "manual-qemu-build",
		Request: publicbuild.BuildRequest{
			Repository: "https://github.com/layercache/manual-qa",
			Commit:     strings.Repeat("a", 40), Integration: recipe.Integration,
			Target: "manual", RecipeDigest: recipe.Digest, Platform: worker.Contract().Platform,
			Resources: publicbuild.Resources{
				CPUMillis: 1000, MemoryBytes: 256 << 20, DiskBytes: 128 << 20, Timeout: 90 * time.Second,
			},
		},
	}
	var logs []string
	publication, err := worker.Execute(context.Background(), build, func(_ context.Context, message string) error {
		logs = append(logs, message)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Discard(build.ID)
	if len(publication.Outputs) != 1 || publication.Outputs[0].Digest == "" || publication.Outputs[0].SizeBytes <= 0 {
		t.Fatalf("collected publication = %#v", publication)
	}
	for _, message := range logs {
		if strings.Contains(strings.ToLower(message), "secret-token") {
			t.Fatalf("guest log was not sanitized: %q", message)
		}
	}
}

type manualSourceFetcher struct{}

func (manualSourceFetcher) Fetch(_ context.Context, _, _, destination string, _ int64) error {
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(destination, "README.txt"), []byte("manual QEMU source\n"), 0o400)
}

func mustManualEnvironment(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is required", name)
	}
	return value
}

func mustManualUint32(t *testing.T, name string) uint32 {
	t.Helper()
	value, err := strconv.ParseUint(mustManualEnvironment(t, name), 10, 32)
	if err != nil || value == 0 {
		t.Fatalf("%s must be a non-zero uint32", name)
	}
	return uint32(value)
}
