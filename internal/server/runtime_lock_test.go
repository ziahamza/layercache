//go:build linux || darwin

package server

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/layercache/layercache/internal/config"
)

func TestServerLocksDataDirectoryBeforeStartupCleanup(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	cfg := config.Config{
		Version: 1, Role: "local", DataDir: dataDir, Listen: "127.0.0.1:7437",
		MaxBytes: 1 << 20, ProjectID: "github.com/acme/widget", LocalToken: "local-token",
		CompatibilityID: "linux-amd64-schema1", ActionsRepository: "acme/widget",
		ActionsRef: "refs/heads/main", ActionsDefaultRef: "refs/heads/main",
		BuildkitBuilder: "layercache-test",
	}
	first, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = first.Close()
		}
	})

	staged := filepath.Join(dataDir, "staging", "live-upload")
	if err := os.WriteFile(staged, []byte("in flight"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := New(context.Background(), cfg)
	if second != nil {
		_ = second.Close()
		t.Fatal("second server opened a live data directory")
	}
	if err == nil || !strings.Contains(err.Error(), "another Layer Cache runtime") {
		t.Fatalf("second server error = %v", err)
	}
	if contents, readErr := os.ReadFile(staged); readErr != nil || string(contents) != "in flight" {
		t.Fatalf("rejected startup disturbed live staging: %q, %v", contents, readErr)
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	third, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open data directory after owner stopped: %v", err)
	}
	t.Cleanup(func() { _ = third.Close() })
	if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new owner did not clean abandoned staging: %v", err)
	}
}

func TestRunHTTPDoesNotReportReadyWhenListenerCannotBind(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	cfg := config.Config{
		Version: 1, Role: "local", DataDir: t.TempDir(), Listen: listener.Addr().String(),
		MaxBytes: 1 << 20, ProjectID: "github.com/acme/widget", LocalToken: "local-token",
		CompatibilityID: "linux-amd64-schema1", ActionsRepository: "acme/widget",
		ActionsRef: "refs/heads/main", ActionsDefaultRef: "refs/heads/main",
		BuildkitBuilder: "layercache-test",
	}
	ready := false
	err = RunHTTPWithReady(context.Background(), cfg, func(*Server) { ready = true })
	if err == nil || !strings.Contains(err.Error(), "serve Layer Cache") {
		t.Fatalf("RunHTTPWithReady error = %v", err)
	}
	if ready {
		t.Fatal("RunHTTPWithReady reported readiness before binding its listener")
	}

	reopened, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("failed listener startup retained the runtime directory lock: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}
