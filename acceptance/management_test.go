package acceptance_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestInstalledManagementLifecyclePreservesCredentialsAndArtifacts(t *testing.T) {
	root := t.TempDir()
	binary := buildLayerCache(t)
	configPath := filepath.Join(root, "config.json")
	dataDir := filepath.Join(root, "cache")

	preview := runBinary(t, binary,
		"setup", "--config", configPath, "--data-dir", dataDir,
		"--role", "public", "--preview", "--json",
	)
	var previewResult struct {
		Preview bool `json:"preview"`
	}
	if err := json.Unmarshal(preview, &previewResult); err != nil || !previewResult.Preview {
		t.Fatalf("setup preview = %s, %v", preview, err)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("preview wrote configuration: %v", err)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("preview wrote data: %v", err)
	}

	runBinary(t, binary,
		"setup", "--config", configPath, "--data-dir", dataDir,
		"--role", "public", "--non-interactive", "--json",
	)
	configured, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	runBinary(t, binary, "setup", "--config", configPath, "--non-interactive", "--json")
	rerun, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(configured, rerun) {
		t.Fatal("idempotent setup changed generated credentials or creation metadata")
	}

	doctor := runBinary(t, binary, "doctor", "--config", configPath, "--json")
	var diagnosis struct {
		Healthy bool `json:"healthy"`
		Checks  struct {
			Config struct {
				OK bool `json:"ok"`
			} `json:"config"`
			DataDir struct {
				OK bool `json:"ok"`
			} `json:"dataDir"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(doctor, &diagnosis); err != nil {
		t.Fatalf("doctor output: %v\n%s", err, doctor)
	}
	if diagnosis.Healthy || !diagnosis.Checks.Config.OK || !diagnosis.Checks.DataDir.OK {
		t.Fatalf("doctor result = %+v", diagnosis)
	}

	runBinary(t, binary, "bypass", "--config", configPath, "--adapter", "turbo", "--json")
	status := runBinary(t, binary, "status", "--config", configPath, "--json")
	var statusResult struct {
		BypassAdapters []string `json:"bypassAdapters"`
	}
	if err := json.Unmarshal(status, &statusResult); err != nil || len(statusResult.BypassAdapters) != 1 || statusResult.BypassAdapters[0] != "turbo" {
		t.Fatalf("status bypass = %+v, %v\n%s", statusResult, err, status)
	}

	artifact := filepath.Join(dataDir, "artifact")
	if err := os.WriteFile(artifact, []byte("preserve me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "runtime.pid"), []byte("invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repair := runBinary(t, binary, "repair", "--config", configPath, "--json")
	var repairResult struct {
		RemovedStalePID bool `json:"removedStalePid"`
	}
	if err := json.Unmarshal(repair, &repairResult); err != nil || !repairResult.RemovedStalePID {
		t.Fatalf("repair result = %+v, %v\n%s", repairResult, err, repair)
	}

	runBinary(t, binary, "uninstall", "--config", configPath, "--preserve-cache", "--json")
	if contents, err := os.ReadFile(artifact); err != nil || string(contents) != "preserve me" {
		t.Fatalf("uninstall did not preserve artifact: %q, %v", contents, err)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("uninstall left config: %v", err)
	}
}

func TestInstalledUninstallStopsTheOwnedRuntime(t *testing.T) {
	root := t.TempDir()
	binary := buildLayerCache(t)
	configPath := filepath.Join(root, "config.json")
	dataDir := filepath.Join(root, "cache")
	address := availableAddress(t)
	runBinary(t, binary,
		"setup", "--config", configPath, "--data-dir", dataDir,
		"--listen", address, "--non-interactive", "--json",
	)
	started := runBinary(t, binary, "start", "--config", configPath, "--json")
	var startResult struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(started, &startResult); err != nil || startResult.PID <= 0 {
		t.Fatalf("start result = %+v, %v\n%s", startResult, err, started)
	}
	process, err := os.FindProcess(startResult.PID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Kill() })
	gc := runBinary(t, binary, "gc", "--config", configPath, "--json")
	var gcResult struct {
		FreedBytes  int64 `json:"freedBytes"`
		BeforeBytes int64 `json:"beforeBytes"`
		AfterBytes  int64 `json:"afterBytes"`
		TargetBytes int64 `json:"targetBytes"`
	}
	if err := json.Unmarshal(gc, &gcResult); err != nil || gcResult.FreedBytes != 0 || gcResult.BeforeBytes != 0 || gcResult.AfterBytes != 0 || gcResult.TargetBytes <= 0 {
		t.Fatalf("garbage collection result = %+v, %v\n%s", gcResult, err, gc)
	}

	runBinary(t, binary, "uninstall", "--config", configPath, "--preserve-cache", "--json")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		response, requestErr := (&http.Client{Timeout: 50 * time.Millisecond}).Get("http://" + address + "/healthz")
		if requestErr != nil {
			break
		}
		response.Body.Close()
		time.Sleep(10 * time.Millisecond)
	}
	if response, requestErr := (&http.Client{Timeout: 50 * time.Millisecond}).Get("http://" + address + "/healthz"); requestErr == nil {
		response.Body.Close()
		t.Fatal("runtime still accepted requests after uninstall")
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("uninstall left config: %v", err)
	}
	if info, err := os.Stat(dataDir); err != nil || !info.IsDir() {
		t.Fatalf("uninstall did not preserve Local Cache: %v", err)
	}
}

func TestInstalledUninstallRefusesMismatchedRuntimeOwnership(t *testing.T) {
	root := t.TempDir()
	binary := buildLayerCache(t)
	configPath := filepath.Join(root, "config.json")
	dataDir := filepath.Join(root, "cache")
	address := availableAddress(t)
	runBinary(t, binary,
		"setup", "--config", configPath, "--data-dir", dataDir,
		"--listen", address, "--non-interactive", "--json",
	)
	runBinary(t, binary, "start", "--config", configPath, "--json")
	pidPath := filepath.Join(dataDir, "runtime.pid")
	originalOwnership, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(pidPath, originalOwnership, 0o600)
		command := exec.Command(binary, "stop", "--config", configPath, "--json")
		_, _ = command.CombinedOutput()
	})

	unrelated := startTermIgnoringProcess(t, root)
	var forged struct {
		PID        int    `json:"pid"`
		InstanceID string `json:"instanceId"`
	}
	if err := json.Unmarshal(originalOwnership, &forged); err != nil || forged.InstanceID == "" {
		t.Fatalf("decode runtime ownership: %+v, %v", forged, err)
	}
	forged.PID = unrelated.Process.Pid
	forgedOwnership, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidPath, append(forgedOwnership, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(binary, "uninstall", "--config", configPath, "--preserve-cache", "--json")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("uninstall accepted ownership for another PID: %s", output)
	}
	if !strings.Contains(string(output), "does not match authenticated runtime") {
		t.Fatalf("uninstall error did not identify the ownership mismatch: %v\n%s", err, output)
	}
	if err := unrelated.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("uninstall harmed the unrelated process: %v\n%s", err, output)
	}
	response, err := (&http.Client{Timeout: time.Second}).Get("http://" + address + "/healthz")
	if err != nil {
		t.Fatalf("uninstall harmed the authenticated runtime: %v\n%s", err, output)
	}
	response.Body.Close()
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("uninstall removed configuration despite refusing shutdown: %v", err)
	}
}
