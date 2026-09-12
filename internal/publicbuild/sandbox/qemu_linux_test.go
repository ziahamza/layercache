//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/publicbuild"
)

func TestReclaimStaleScratchOnlyRemovesThisWorkersOwnedDirectories(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	prefix := "build-0123456789abcdef-"
	stale := filepath.Join(root, prefix+"stale123")
	other := filepath.Join(root, "build-fedcba9876543210-live123")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "disk"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := reclaimStaleScratchOwned(root, prefix, uint32(os.Geteuid()), uint32(os.Getgid())); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale worker scratch still exists: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("another worker's scratch was removed: %v", err)
	}
}

func TestReclaimStaleScratchRejectsUnsafeMatchingEntry(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	prefix := "build-0123456789abcdef-"
	unsafe := filepath.Join(root, prefix+"unsafe123")
	if err := os.Mkdir(unsafe, 0o722); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafe, 0o722); err != nil {
		t.Fatal(err)
	}
	err := reclaimStaleScratchOwned(root, prefix, uint32(os.Geteuid()), uint32(os.Getgid()))
	if err == nil || !strings.Contains(err.Error(), "unsafe ownership or mode") {
		t.Fatalf("unsafe stale scratch error = %v", err)
	}
}

func TestQEMUPlanHasKVMOnlyNoNetworkAndBoundedDevices(t *testing.T) {
	t.Parallel()
	worker := &QEMUWorker{
		config:   QEMUConfig{KernelPath: "/images/kernel"},
		contract: testGuestContract("sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
	}
	build := publicbuild.Build{Request: publicbuild.BuildRequest{
		Platform: publicbuild.PlatformLinuxAMD64,
		Resources: publicbuild.Resources{
			CPUMillis: 1500, MemoryBytes: 512 << 20, DiskBytes: 2 << 30, Timeout: time.Minute,
		},
	}}
	arguments, err := worker.qemuArguments(build, qemuPaths{
		rootOverlay: "/worker/root.qcow2", sourceImage: "/worker/source.raw",
		workImage: "/worker/work.qcow2", controlSocket: "/worker/run/control.sock",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(arguments, " ")
	for _, required := range []string{
		"-accel kvm", "-nic none", "obsolete=deny", "elevateprivileges=deny",
		"spawn=deny", "resourcecontrol=deny", "read-only", "536870912b",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("QEMU arguments do not contain %q: %s", required, joined)
		}
	}
	for _, forbidden := range []string{"-netdev", "hostfwd", "docker.sock", "collector-token"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("QEMU arguments contain forbidden value %q: %s", forbidden, joined)
		}
	}
	if !slices.Contains(arguments, "virtio-serial-device") {
		t.Fatalf("QEMU arguments lack bounded host collection transport: %v", arguments)
	}
}

func TestARM64QEMUPlanUsesNativeVirtMachineAndPL011Console(t *testing.T) {
	t.Parallel()
	contract := testGuestContract("sha256:" + strings.Repeat("c", 64))
	contract.Platform = publicbuild.PlatformLinuxARM64
	worker := &QEMUWorker{config: QEMUConfig{KernelPath: "/images/Image"}, contract: contract}
	arguments, err := worker.qemuArguments(publicbuild.Build{Request: publicbuild.BuildRequest{
		Platform:  publicbuild.PlatformLinuxARM64,
		Resources: publicbuild.Resources{CPUMillis: 1000, MemoryBytes: 512 << 20},
	}}, qemuPaths{controlSocket: "/worker/run/control.sock"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(arguments, " ")
	for _, want := range []string{"-machine virt,gic-version=host", "console=ttyAMA0", "-accel kvm", "-cpu host", "-nic none", "virtio-blk-device", "virtio-serial-device"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("arm64 QEMU arguments missing %q: %s", want, joined)
		}
	}
	for _, forbidden := range []string{"microvm", "ttyS0", "tcg", "-netdev"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("arm64 QEMU arguments contain %q: %s", forbidden, joined)
		}
	}
}
