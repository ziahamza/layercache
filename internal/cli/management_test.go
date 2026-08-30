package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/config"
)

func TestSetupIsIdempotentAndPreservesGeneratedCredentials(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	dataDir := filepath.Join(root, "cache")

	runCLI(t,
		"setup", "--config", configPath, "--data-dir", dataDir,
		"--role", "public", "--project", "github.com/acme/project",
		"--public-build-repository", "https://github.com/acme/project",
		"--non-interactive", "--json",
	)
	want, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	wantBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if want.LocalToken == "" || want.PublisherToken == "" || want.PublicTrustKey == "" || want.PublicPrivateKey == "" {
		t.Fatalf("setup did not generate credentials: %+v", want)
	}
	wantInfo, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}

	runCLI(t, "setup", "--config", configPath, "--non-interactive", "--json")
	got, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	gotBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rerun changed configuration\ngot:  %+v\nwant: %+v", got, want)
	}
	if !bytes.Equal(gotBytes, wantBytes) {
		t.Fatal("rerun changed serialized configuration")
	}
	gotInfo, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !gotInfo.ModTime().Equal(wantInfo.ModTime()) {
		t.Fatal("rerun rewrote an unchanged configuration")
	}
}

func TestSetupAcceptsExplicitCompatibilityIdentity(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	dataDir := filepath.Join(root, "cache")
	runCLI(t,
		"setup", "--config", configPath, "--data-dir", dataDir,
		"--compatibility-id", "linux-amd64-glibc2.39-node@24-schema1",
		"--non-interactive", "--json",
	)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CompatibilityID != "linux-amd64-glibc2.39-node@24-schema1" {
		t.Fatalf("compatibilityId = %q", cfg.CompatibilityID)
	}
}

func TestSetupAcceptsMinFreeBytesUpwardOverride(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	dataDir := filepath.Join(root, "cache")
	runCLI(t,
		"setup", "--config", configPath, "--data-dir", dataDir,
		"--min-free-bytes", "8589934592",
		"--non-interactive", "--json",
	)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinFreeBytes != 8*1024*1024*1024 {
		t.Fatalf("minFreeBytes = %d, want 8 GiB", cfg.MinFreeBytes)
	}
}

func TestSetupPreviewReportsRedactedIntentWithoutWrites(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	dataDir := filepath.Join(root, "cache")
	secret := "do-not-print-this-token"

	stdout, _ := runCLI(t,
		"setup", "--config", configPath, "--data-dir", dataDir,
		"--role", "public", "--local-token", secret,
		"--preview", "--json",
	)
	if strings.Contains(stdout, secret) {
		t.Fatal("preview disclosed a bearer token")
	}
	var result struct {
		Preview       bool           `json:"preview"`
		Configuration map[string]any `json:"configuration"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode preview: %v\n%s", err, stdout)
	}
	if !result.Preview || result.Configuration["localToken"] != "[redacted]" || result.Configuration["publicPrivateKey"] != "[redacted]" {
		t.Fatalf("unexpected preview: %#v", result)
	}
	if _, err := os.Stat(configPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("preview wrote config: %v", err)
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("preview wrote data directory: %v", err)
	}
}

func TestSetupHelpDoesNotDisclosePreservedSecrets(t *testing.T) {
	configPath, _ := setupLocalConfig(t)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := callCLI("setup", "--config", configPath, "--help")
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help error = %v", err)
	}
	combined := stdout + stderr
	if strings.Contains(combined, cfg.LocalToken) {
		t.Fatal("setup help disclosed the existing Local Cache bearer token")
	}
}

func TestDoctorIsReadOnlyAndReportsChecks(t *testing.T) {
	configPath, dataDir := setupLocalConfig(t)
	beforeConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeEntries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}

	stdout, _ := runCLI(t, "doctor", "--config", configPath, "--json")
	var result struct {
		Healthy bool `json:"healthy"`
		Checks  struct {
			Config  diagnosticCheck `json:"config"`
			DataDir diagnosticCheck `json:"dataDir"`
			Runtime diagnosticCheck `json:"runtime"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode doctor output: %v\n%s", err, stdout)
	}
	if result.Healthy || !result.Checks.Config.OK || !result.Checks.DataDir.OK || result.Checks.Runtime.OK {
		t.Fatalf("unexpected checks: %+v", result)
	}
	afterConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	afterEntries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeConfig, afterConfig) || !reflect.DeepEqual(entryNames(beforeEntries), entryNames(afterEntries)) {
		t.Fatal("doctor mutated installation state")
	}
}

func TestDoctorDetectsUnsafeConfigPermissions(t *testing.T) {
	configPath, _ := setupLocalConfig(t)
	if err := os.Chmod(configPath, 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _ := runCLI(t, "doctor", "--config", configPath, "--json")
	var result struct {
		Checks struct {
			Config diagnosticCheck `json:"config"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if result.Checks.Config.OK || !strings.Contains(result.Checks.Config.Detail, "0600") {
		t.Fatalf("config permission check = %+v", result.Checks.Config)
	}
}

func TestRepairRecreatesOwnedDataDirectoryAndRemovesOnlyStalePID(t *testing.T) {
	configPath, dataDir := setupLocalConfig(t)
	if err := os.RemoveAll(dataDir); err != nil {
		t.Fatal(err)
	}
	stdout, _ := runCLI(t, "repair", "--config", configPath, "--json")
	var first struct {
		CreatedDataDir bool `json:"createdDataDir"`
	}
	if err := json.Unmarshal([]byte(stdout), &first); err != nil {
		t.Fatal(err)
	}
	if !first.CreatedDataDir {
		t.Fatalf("repair result = %s", stdout)
	}
	if _, err := os.Stat(filepath.Join(dataDir, ownershipMarkerName)); err != nil {
		t.Fatalf("repair did not restore ownership marker: %v", err)
	}

	artifact := filepath.Join(dataDir, "keep-me")
	if err := os.WriteFile(artifact, []byte("artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "runtime.pid"), []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _ = runCLI(t, "repair", "--config", configPath, "--json")
	var second struct {
		RemovedStalePID bool `json:"removedStalePid"`
	}
	if err := json.Unmarshal([]byte(stdout), &second); err != nil {
		t.Fatal(err)
	}
	if !second.RemovedStalePID {
		t.Fatalf("repair result = %s", stdout)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "runtime.pid")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale PID remains: %v", err)
	}
	contents, err := os.ReadFile(artifact)
	if err != nil || string(contents) != "artifact" {
		t.Fatalf("repair changed cached data: %q, %v", contents, err)
	}
}

func TestGarbageCollectionUsesAuthenticatedRuntimeEndpoint(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requestSeen := make(chan struct{}, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/gc" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer local-secret" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		requestSeen <- struct{}{}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"reclaimedBytes":123,"usageBytes":456}`))
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	configPath, _ := setupConfigWithAddress(t, listener.Addr().String(), "local-secret")
	stdout, _ := runCLI(t, "gc", "--config", configPath, "--json")
	select {
	case <-requestSeen:
	case <-time.After(time.Second):
		t.Fatal("runtime did not receive GC request")
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if result["reclaimedBytes"] != float64(123) || result["usageBytes"] != float64(456) {
		t.Fatalf("GC result = %v", result)
	}
}

func TestGarbageCollectionReportsUnavailableRuntime(t *testing.T) {
	configPath, _ := setupConfigWithAddress(t, "127.0.0.1:1", "local-secret")
	_, _, err := callCLI("gc", "--config", configPath, "--json")
	if err == nil || !strings.Contains(err.Error(), "running runtime") {
		t.Fatalf("GC error = %v", err)
	}
}

func TestBypassPersistsAdaptersAndStatusReportsThem(t *testing.T) {
	configPath, _ := setupLocalConfig(t)
	runCLI(t, "bypass", "--config", configPath, "--adapter", "turbo", "--json")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.BypassAdapters, []string{"turbo"}) {
		t.Fatalf("bypass adapters = %v", cfg.BypassAdapters)
	}
	stdout, _ := runCLI(t, "status", "--config", configPath, "--json")
	var status struct {
		BypassAdapters []string `json:"bypassAdapters"`
	}
	if err := json.Unmarshal([]byte(stdout), &status); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(status.BypassAdapters, []string{"turbo"}) {
		t.Fatalf("status bypass adapters = %v", status.BypassAdapters)
	}

	runCLI(t, "bypass", "--config", configPath, "--adapter", "all", "--json")
	cfg, err = config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.BypassAdapters, supportedAdapters) {
		t.Fatalf("all bypass adapters = %v", cfg.BypassAdapters)
	}
	runCLI(t, "bypass", "--config", configPath, "--clear", "--json")
	cfg, err = config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.BypassAdapters) != 0 {
		t.Fatalf("clear left bypass adapters = %v", cfg.BypassAdapters)
	}
}

func TestUninstallPreservesCacheAndRemovesOnlyOwnedRuntimeFiles(t *testing.T) {
	configPath, dataDir := setupLocalConfig(t)
	artifact := filepath.Join(dataDir, "artifact")
	unrelated := filepath.Join(filepath.Dir(configPath), "unrelated")
	for path, contents := range map[string]string{
		artifact:                              "cached",
		filepath.Join(dataDir, "daemon.log"):  "log",
		filepath.Join(dataDir, "runtime.pid"): "invalid",
		unrelated:                             "keep",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runCLI(t, "uninstall", "--config", configPath, "--preserve-cache", "--json")
	if _, err := os.Stat(configPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("config still exists: %v", err)
	}
	if contents, err := os.ReadFile(artifact); err != nil || string(contents) != "cached" {
		t.Fatalf("cache was not preserved: %q, %v", contents, err)
	}
	for _, path := range []string{filepath.Join(dataDir, "daemon.log"), filepath.Join(dataDir, "runtime.pid")} {
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("owned runtime file remains at %s: %v", path, err)
		}
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated config-directory file removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, ownershipMarkerName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("uninstall left ownership metadata that prevents reinstall: %v", err)
	}
	runCLI(t,
		"setup", "--config", configPath, "--data-dir", dataDir,
		"--non-interactive", "--json",
	)
	if contents, err := os.ReadFile(artifact); err != nil || string(contents) != "cached" {
		t.Fatalf("reinstall did not reuse preserved cache: %q, %v", contents, err)
	}
}

func TestUninstallDeleteCacheRequiresConfirmationAndOwnership(t *testing.T) {
	configPath, dataDir := setupLocalConfig(t)
	if _, _, err := callCLI("uninstall", "--config", configPath, "--delete-cache", "--json"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("unconfirmed uninstall error = %v", err)
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("unconfirmed uninstall removed config: %v", err)
	}
	if _, err := os.Stat(dataDir); err != nil {
		t.Fatalf("unconfirmed uninstall removed cache: %v", err)
	}

	if err := os.Remove(filepath.Join(dataDir, ownershipMarkerName)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := callCLI("uninstall", "--config", configPath, "--delete-cache", "--yes", "--json"); err == nil || !strings.Contains(err.Error(), "ownership marker") {
		t.Fatalf("unowned uninstall error = %v", err)
	}
	if _, err := os.Stat(dataDir); err != nil {
		t.Fatalf("unowned uninstall removed cache: %v", err)
	}

	runCLI(t, "repair", "--config", configPath, "--json")
	runCLI(t, "uninstall", "--config", configPath, "--delete-cache", "--yes", "--json")
	if _, err := os.Stat(dataDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("cache still exists: %v", err)
	}
	if _, err := os.Stat(configPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("config still exists: %v", err)
	}
}

func TestDeletionTargetValidationRejectsBroadOrIndirectTargets(t *testing.T) {
	root := t.TempDir()
	owned := filepath.Join(root, "cache")
	if err := os.MkdirAll(owned, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", ".", t.TempDir()} {
		if path == owned {
			continue
		}
		if err := validateDeletionTarget(path); err == nil && (path == "/" || path == ".") {
			t.Errorf("accepted unsafe target %q", path)
		}
	}
	if err := validateDeletionTarget(owned); err != nil {
		t.Fatalf("rejected scoped target: %v", err)
	}
	if home, err := os.UserHomeDir(); err == nil {
		if err := validateDeletionTarget(home); err == nil {
			t.Fatalf("accepted home directory %q", home)
		}
	}
}

func setupLocalConfig(t *testing.T) (string, string) {
	t.Helper()
	return setupConfigWithAddress(t, "127.0.0.1:1", "local-secret")
}

func setupConfigWithAddress(t *testing.T, address, token string) (string, string) {
	t.Helper()
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	dataDir := filepath.Join(root, "cache")
	runCLI(t,
		"setup", "--config", configPath, "--data-dir", dataDir,
		"--listen", address, "--local-token", token,
		"--non-interactive", "--json",
	)
	return configPath, dataDir
}

func runCLI(t *testing.T, args ...string) (string, string) {
	t.Helper()
	stdout, stderr, err := callCLI(args...)
	if err != nil {
		t.Fatalf("layercache %v: %v\nstdout: %s\nstderr: %s", args, err, stdout, stderr)
	}
	return stdout, stderr
}

func callCLI(args ...string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), args, &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

func entryNames(entries []os.DirEntry) []string {
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry.Name())
	}
	return result
}
