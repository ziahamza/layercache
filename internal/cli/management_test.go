package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/access"
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
		"--public-build-approved-ref", "refs/heads/main",
		"--public-build-recipe", "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
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
	if want.LocalToken == "" || want.PublicBuildWorkerToken == "" || want.PublicCollectorToken == "" || want.PublicTrustKey == "" || want.PublicPrivateKey == "" {
		t.Fatalf("setup did not generate credentials: %+v", want)
	}
	if want.ActionsRepository != "acme/project" {
		t.Fatalf("Actions repository = %q, want project-derived acme/project", want.ActionsRepository)
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

func TestSetupRefusesConfigurationMutationWhileRuntimeIsRunning(t *testing.T) {
	runtime := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/status" || request.Header.Get("Authorization") != "Bearer local-secret" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"running":true,"runtimePid":4242,"runtimeInstanceId":"live-instance"}`)
	}))
	t.Cleanup(runtime.Close)

	address := strings.TrimPrefix(runtime.URL, "http://")
	configPath, _ := setupConfigWithAddress(t, address, "local-secret")
	_, _, err := callCLI(
		"setup", "--config", configPath, "--local-token", "rotated-secret",
		"--non-interactive", "--json",
	)
	if err == nil || !strings.Contains(err.Error(), "stop it before changing configuration") {
		t.Fatalf("live setup mutation error = %v", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LocalToken != "local-secret" {
		t.Fatal("rejected setup mutation changed the persisted runtime credential")
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
	postgresSecret := "postgres://layercache:do-not-print-db-password@127.0.0.1:5432/layercache"
	s3Access := "do-not-print-access-key"
	s3Secret := "do-not-print-s3-secret"

	stdout, _ := runCLI(t,
		"setup", "--config", configPath, "--data-dir", dataDir,
		"--role", "public", "--local-token", secret,
		"--cloud-postgres-url", postgresSecret,
		"--cloud-s3-endpoint", "http://127.0.0.1:9000",
		"--cloud-s3-bucket", "layercache-qa", "--cloud-s3-region", "us-east-1",
		"--cloud-s3-access-key", s3Access, "--cloud-s3-secret-key", s3Secret,
		"--preview", "--json",
	)
	for _, sensitive := range []string{secret, postgresSecret, "do-not-print-db-password", s3Access, s3Secret} {
		if strings.Contains(stdout, sensitive) {
			t.Fatalf("preview disclosed sensitive value %q", sensitive)
		}
	}
	var result struct {
		Preview       bool           `json:"preview"`
		Configuration map[string]any `json:"configuration"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode preview: %v\n%s", err, stdout)
	}
	if !result.Preview || result.Configuration["localToken"] != "[redacted]" || result.Configuration["publicPrivateKey"] != "[redacted]" ||
		result.Configuration["cloudPostgresUrl"] != "[redacted]" || result.Configuration["cloudS3AccessKey"] != "[redacted]" ||
		result.Configuration["cloudS3SecretKey"] != "[redacted]" {
		t.Fatalf("unexpected preview: %#v", result)
	}
	if _, err := os.Stat(configPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("preview wrote config: %v", err)
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("preview wrote data directory: %v", err)
	}
}

func TestSetupHumanPreviewExplainsTheIntendedInstallation(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	dataDir := filepath.Join(root, "cache")
	stdout, _ := runCLI(t,
		"setup", "--config", configPath, "--data-dir", dataDir,
		"--listen", "127.0.0.1:17437", "--project", "github.com/acme/widget",
		"--compatibility-id", "linux-amd64-test-v1", "--preview",
	)
	for _, want := range []string{
		"Layer Cache setup preview",
		"Configuration file: " + configPath,
		"Runtime: local role listening on 127.0.0.1:17437",
		"Project: github.com/acme/widget",
		"Compatibility: linux-amd64-test-v1",
		"Local Cache: " + dataDir,
		"Capacity: 20.0 GiB limit; 5.0 GiB minimum filesystem reserve",
		"BuildKit Local Cache: layercache builder; 5.0 GiB budget",
		"Team Cache: disabled",
		"Public Cache: disabled",
		"Detected integrations:",
		"Preview only: no files or integrations were changed.",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("setup preview omitted %q:\n%s", want, stdout)
		}
	}
	if _, err := os.Stat(configPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("human preview wrote config: %v", err)
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("human preview wrote data directory: %v", err)
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

func TestDoctorHumanOutputListsEveryCheck(t *testing.T) {
	configPath, _ := setupLocalConfig(t)
	stdout, _ := runCLI(t, "doctor", "--config", configPath)
	for _, want := range []string{
		"Layer Cache diagnostics found problems",
		"[ok] Configuration: configuration is valid and protected",
		"[ok] Local Cache directory:",
		"[problem] Runtime: runtime is not reachable",
		"Integrations:",
		"Remote caches:",
		"Credentials:",
		"Local Cache capacity:",
		"Recent degraded behavior:",
		"Public Build:",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("doctor output omitted %q:\n%s", want, stdout)
		}
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

func TestDisableDefaultsToAllAdaptersAndEnvironmentBypassIsTransient(t *testing.T) {
	configPath, _ := setupLocalConfig(t)
	runCLI(t, "disable", "--config", configPath, "--json")
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.BypassAdapters, supportedAdapters) {
		t.Fatalf("disabled adapters = %v", cfg.BypassAdapters)
	}

	cfg.BypassAdapters = nil
	t.Setenv("LAYER_CACHE_BYPASS", "actions,buildkit")
	if isAdapterBypassed(cfg, "turbo") || !isAdapterBypassed(cfg, "actions") || !isAdapterBypassed(cfg, "buildkit") {
		t.Fatal("transient bypass environment did not select only Actions and BuildKit")
	}
}

func TestBypassRejectsPositionalArguments(t *testing.T) {
	configPath, _ := setupLocalConfig(t)
	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{
		"bypass", "--config", configPath, "--adapter", "turbo", "unexpected",
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "does not accept positional arguments") {
		t.Fatalf("bypass positional argument error = %v", err)
	}
}

func TestVMRouteIssuesIntegrationScopedExpiringCredentials(t *testing.T) {
	configPath, _ := setupLocalConfig(t)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	sourceCommit := strings.Repeat("a", 40)
	stdout, _ := runCLI(t,
		"vm-route", "issue", "--config", configPath,
		"--endpoint", "http://127.0.0.1:7437", "--ttl", "5m", "--json",
		"--actions-repository", cfg.ActionsRepository,
		"--actions-ref", cfg.ActionsRef,
		"--actions-default-ref", cfg.ActionsDefaultRef,
		"--actions-source-commit", sourceCommit,
	)
	var result vmRouteResponse
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode VM route: %v\n%s", err, stdout)
	}
	if result.Endpoint != "http://127.0.0.1:7437" || result.Project != cfg.ProjectID ||
		result.Environment["TURBO_TOKEN"] == "" || result.Environment["ACTIONS_RUNTIME_TOKEN"] == "" {
		t.Fatalf("incomplete VM route response: %+v", result)
	}
	now := time.Now().UTC()
	turboClaims, err := access.ParseCapabilityToken(cfg.LocalToken, result.Environment["TURBO_TOKEN"], now)
	if err != nil {
		t.Fatal(err)
	}
	actionsClaims, err := access.ParseCapabilityToken(cfg.LocalToken, result.Environment["ACTIONS_RUNTIME_TOKEN"], now)
	if err != nil {
		t.Fatal(err)
	}
	if turboClaims.Integration != "turbo" || actionsClaims.Integration != "actions" ||
		turboClaims.Project != cfg.ProjectID || actionsClaims.Compatibility != cfg.CompatibilityID ||
		actionsClaims.SourceCommit != sourceCommit ||
		!turboClaims.Allows(access.CapabilityWrite) || turboClaims.ExpiresAt.After(now.Add(6*time.Minute)) {
		t.Fatalf("unexpected VM route claims: turbo=%+v actions=%+v", turboClaims, actionsClaims)
	}
}

func TestVMRouteWritesCredentialToExclusiveProtectedOutput(t *testing.T) {
	configPath, _ := setupLocalConfig(t)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	outputPath := filepath.Join(root, "vm-route.json")
	commit := strings.Repeat("b", 40)
	arguments := []string{
		"vm-route", "issue", "--config", configPath,
		"--endpoint", "http://127.0.0.1:7437", "--integration", "actions",
		"--actions-repository", cfg.ActionsRepository,
		"--actions-ref", cfg.ActionsRef,
		"--actions-default-ref", cfg.ActionsDefaultRef,
		"--actions-source-commit", commit,
		"--output", outputPath,
	}
	stdout, _ := runCLI(t, arguments...)
	if strings.Contains(stdout, "ACTIONS_RUNTIME_TOKEN") || strings.Contains(stdout, "lc2.") {
		t.Fatalf("output mode disclosed credential JSON on stdout: %q", stdout)
	}
	info, err := os.Lstat(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("VM route output mode = %s, want protected regular file", info.Mode())
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	var response vmRouteResponse
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatalf("decode protected VM route output: %v", err)
	}
	if response.Environment["ACTIONS_RUNTIME_TOKEN"] == "" {
		t.Fatal("protected VM route output omitted Actions credential")
	}

	_, _, err = callCLI(arguments...)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing VM route output error = %v", err)
	}
	assertFileContents(t, outputPath, data)

	targetPath := filepath.Join(root, "user-owned")
	if err := os.WriteFile(targetPath, []byte("keep\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(root, "route-link.json")
	if err := os.Symlink(targetPath, symlinkPath); err != nil {
		t.Fatal(err)
	}
	symlinkArguments := append([]string{}, arguments...)
	symlinkArguments[len(symlinkArguments)-1] = symlinkPath
	_, _, err = callCLI(symlinkArguments...)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("symlink VM route output error = %v", err)
	}
	assertFileContents(t, targetPath, []byte("keep\n"))

	noOutputArguments := append([]string{}, arguments[:len(arguments)-2]...)
	_, _, err = callCLI(noOutputArguments...)
	if err == nil || !strings.Contains(err.Error(), "--output") || !strings.Contains(err.Error(), "--json") {
		t.Fatalf("missing VM route output mode error = %v", err)
	}
	conflictingArguments := append(append([]string{}, arguments...), "--json")
	_, _, err = callCLI(conflictingArguments...)
	if err == nil || !strings.Contains(err.Error(), "cannot be used together") {
		t.Fatalf("conflicting VM route output modes error = %v", err)
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
	markerData, err := os.ReadFile(filepath.Join(dataDir, ownershipMarkerName))
	if err != nil || !strings.Contains(string(markerData), `"preserved": true`) {
		t.Fatalf("uninstall did not leave a safe preserved-cache marker: %q, %v", markerData, err)
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

func TestSetupRefusesToClaimNonEmptyUnownedDataDirectory(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config", "config.json")
	dataDir := filepath.Join(root, "shared", "cache")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(dataDir, "keep-me.txt")
	if err := os.WriteFile(unrelated, []byte("unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := callCLI(
		"setup", "--config", configPath, "--data-dir", dataDir,
		"--non-interactive", "--json",
	)
	if err == nil || !strings.Contains(err.Error(), "refusing to claim a non-empty") {
		t.Fatalf("setup error = %v, want unowned non-empty directory rejection", err)
	}
	if contents, readErr := os.ReadFile(unrelated); readErr != nil || string(contents) != "unrelated" {
		t.Fatalf("unrelated file changed: %q, %v", contents, readErr)
	}
	if _, statErr := os.Stat(configPath); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("rejected setup wrote a configuration: %v", statErr)
	}
}

func TestRepairRecoversIntegrationOwnershipStateFromRecoveryCopy(t *testing.T) {
	configPath, _ := setupLocalConfig(t)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	want := integrationState{
		Version: integrationStateVersion, InstallationID: cfg.InstallationID,
		Records: map[string]integrationRecord{"turbo": {Name: "turbo", State: "active", Path: "/tmp/project/.turbo/config.json"}},
	}
	if err := saveIntegrationState(cfg, want); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(integrationStatePath(cfg), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCLI(t, "repair", "--config", configPath, "--json")
	got, err := loadIntegrationState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Records["turbo"].Path != want.Records["turbo"].Path {
		t.Fatalf("recovered record = %#v", got.Records["turbo"])
	}
}

func TestRepairFailsWhenIntegrationOwnershipStateHasNoRecoveryCopy(t *testing.T) {
	configPath, dataDir := setupLocalConfig(t)
	integrationsDir := filepath.Join(dataDir, "integrations")
	if err := os.MkdirAll(integrationsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(integrationsDir, "state.json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := callCLI("repair", "--config", configPath, "--json")
	if err == nil || !strings.Contains(err.Error(), "no recovery copy") {
		t.Fatalf("repair error = %v, want unrecoverable integration state", err)
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
