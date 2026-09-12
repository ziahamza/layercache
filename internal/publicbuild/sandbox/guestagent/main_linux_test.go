//go:build linux

package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

type fakeInterfaceFlagController struct {
	flagsValue uint16
	flagsErr   error
	setErr     error
	setCalls   int
	setName    string
	setValue   uint16
}

func (controller *fakeInterfaceFlagController) flags(string) (uint16, error) {
	return controller.flagsValue, controller.flagsErr
}

func (controller *fakeInterfaceFlagController) setFlags(name string, flags uint16) error {
	controller.setCalls++
	controller.setName = name
	controller.setValue = flags
	return controller.setErr
}

func TestEnableLoopback(t *testing.T) {
	t.Run("brings loopback up and preserves flags", func(t *testing.T) {
		controller := &fakeInterfaceFlagController{
			flagsValue: uint16(unix.IFF_LOOPBACK | unix.IFF_MULTICAST),
		}
		if err := enableLoopback(controller); err != nil {
			t.Fatal(err)
		}
		if controller.setCalls != 1 || controller.setName != "lo" {
			t.Fatalf("set flags calls = %d for %q", controller.setCalls, controller.setName)
		}
		want := uint16(unix.IFF_LOOPBACK | unix.IFF_MULTICAST | unix.IFF_UP)
		if controller.setValue != want {
			t.Fatalf("set flags = %#x, want %#x", controller.setValue, want)
		}
	})

	t.Run("leaves enabled loopback alone", func(t *testing.T) {
		controller := &fakeInterfaceFlagController{
			flagsValue: uint16(unix.IFF_LOOPBACK | unix.IFF_UP),
		}
		if err := enableLoopback(controller); err != nil {
			t.Fatal(err)
		}
		if controller.setCalls != 0 {
			t.Fatalf("set flags calls = %d, want 0", controller.setCalls)
		}
	})

	t.Run("rejects a non-loopback interface", func(t *testing.T) {
		controller := &fakeInterfaceFlagController{}
		err := enableLoopback(controller)
		if err == nil || !strings.Contains(err.Error(), "not a loopback") {
			t.Fatalf("enable loopback error = %v", err)
		}
		if controller.setCalls != 0 {
			t.Fatalf("set flags calls = %d, want 0", controller.setCalls)
		}
	})

	t.Run("fails closed when flags cannot be changed", func(t *testing.T) {
		controller := &fakeInterfaceFlagController{
			flagsValue: uint16(unix.IFF_LOOPBACK),
			setErr:     errors.New("denied"),
		}
		err := enableLoopback(controller)
		if err == nil || !strings.Contains(err.Error(), "denied") {
			t.Fatalf("enable loopback error = %v", err)
		}
	})
}

func TestCopySourceTreeReconstructsGitCheckoutModes(t *testing.T) {
	source := t.TempDir()
	directory := filepath.Join(source, "pkg")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(directory, "data.txt")
	executable := filepath.Join(directory, "tool.sh")
	if err := os.WriteFile(regular, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("pkg/data.txt", filepath.Join(source, "data-link")); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "source")
	if err := copySourceTree(source, destination); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		destination:                                0o755,
		filepath.Join(destination, "pkg"):          0o755,
		filepath.Join(destination, "pkg/data.txt"): 0o644,
		filepath.Join(destination, "pkg/tool.sh"):  0o755,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("mode for %s = %04o, want %04o", path, got, want)
		}
	}
	if target, err := os.Readlink(filepath.Join(destination, "data-link")); err != nil || target != "pkg/data.txt" {
		t.Fatalf("copied source symlink target = %q, %v", target, err)
	}
}

func TestTurboCaptureRejectsArtifactKeyOutsideDryRunPlan(t *testing.T) {
	capture := &turboCapture{
		path: filepath.Join(t.TempDir(), "artifact.bin"), expectedKey: "planned-hash", maxBytes: 1 << 20,
	}
	wrong := httptest.NewRecorder()
	capture.ServeHTTP(wrong, httptest.NewRequest(
		http.MethodPut, "/v8/artifacts/another-task-hash", strings.NewReader("poison"),
	))
	if wrong.Code != http.StatusConflict {
		t.Fatalf("wrong-key capture status = %d, want 409", wrong.Code)
	}
	if capture.nativeKey != "" {
		t.Fatalf("wrong-key capture claimed native key %q", capture.nativeKey)
	}
	if _, err := os.Stat(capture.path); !os.IsNotExist(err) {
		t.Fatalf("wrong-key capture wrote an artifact: %v", err)
	}

	correct := httptest.NewRecorder()
	capture.ServeHTTP(correct, httptest.NewRequest(
		http.MethodPut, "/v8/artifacts/planned-hash", strings.NewReader("artifact"),
	))
	if correct.Code != http.StatusOK || capture.nativeKey != "planned-hash" {
		t.Fatalf("planned capture status = %d, native key = %q", correct.Code, capture.nativeKey)
	}
}

func TestRemoveEmptyBuildKitIngest(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		if err := removeEmptyBuildKitIngest(t.TempDir()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("empty directory", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "ingest")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := removeEmptyBuildKitIngest(root); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("empty ingest directory still exists: %v", err)
		}
	})
	for _, test := range []struct {
		name  string
		setup func(*testing.T, string)
		want  string
	}{
		{
			name: "regular file",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("unexpected"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "not a real directory",
		},
		{
			name: "non-empty directory",
			setup: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "pending"), []byte("unexpected"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "non-empty ingest",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "ingest")
			test.setup(t, path)
			err := removeEmptyBuildKitIngest(root)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("remove ingest error = %v, want %q", err, test.want)
			}
			if _, statErr := os.Lstat(path); statErr != nil {
				t.Fatalf("rejected ingest path was removed: %v", statErr)
			}
		})
	}
}
