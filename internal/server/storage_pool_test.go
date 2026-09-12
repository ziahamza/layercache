//go:build linux || darwin

package server

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
)

func TestStoragePoolFailsClosedWithoutBoundedFilesystem(t *testing.T) {
	root := t.TempDir()
	p := StoragePoolConfig{Path: root, MaxBytes: 1024}
	if err := p.validate(); err == nil {
		t.Fatal("missing mount marker accepted")
	}
	if err := os.WriteFile(filepath.Join(root, ".layercache-pool"), []byte("layercache-pool-v1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := p.validate(); err == nil {
		t.Fatal("unbounded host filesystem accepted")
	}
	p.MinFreeBytes = 1 << 62
	if p.admits(1) {
		t.Fatal("exhausted reserve accepted")
	}
	if p.contains(filepath.Dir(root)) {
		t.Fatal("parent outside pool accepted")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if p.contains(filepath.Join(root, "escape")) {
		t.Fatal("symlink escape accepted")
	}
}

func TestRegistryMaintenanceDrainsReadersAndRejectsNewRequests(t *testing.T) {
	p := StoragePoolConfig{Path: t.TempDir()}
	if err := os.Mkdir(filepath.Join(p.Path, "control"), 0700); err != nil {
		t.Fatal(err)
	}
	release, err := p.registryLease()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(p.Path, "control/registry.lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
		t.Fatal("collector raced active request")
	}
	release()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if unlock, err := p.registryLease(); err == nil {
		unlock()
		t.Fatal("request raced collector")
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	unlock, err := p.registryLease()
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}
