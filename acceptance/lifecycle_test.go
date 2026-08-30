package acceptance_test

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestInstalledRuntimeStartsReportsStatusAndStops(t *testing.T) {
	root := t.TempDir()
	binary := buildLayerCache(t)
	address := availableAddress(t)
	configPath := filepath.Join(root, "config.json")
	runBinary(t, binary,
		"setup", "--config", configPath,
		"--data-dir", filepath.Join(root, "cache"),
		"--listen", address,
		"--non-interactive", "--json",
	)
	output := runBinary(t, binary, "start", "--config", configPath, "--json")
	var started struct {
		Running bool `json:"running"`
		PID     int  `json:"pid"`
	}
	if err := json.Unmarshal(output, &started); err != nil {
		t.Fatalf("decode start result: %v\n%s", err, output)
	}
	if !started.Running || started.PID <= 0 {
		t.Fatalf("start result = %+v", started)
	}
	t.Cleanup(func() {
		cmd := exec.Command(binary, "stop", "--config", configPath, "--json")
		_, _ = cmd.CombinedOutput()
	})

	output = runBinary(t, binary, "status", "--config", configPath, "--json")
	var running struct {
		Running           bool   `json:"running"`
		UsageBytes        int64  `json:"usageBytes"`
		RuntimePID        int    `json:"runtimePid"`
		RuntimeInstanceID string `json:"runtimeInstanceId"`
	}
	if err := json.Unmarshal(output, &running); err != nil {
		t.Fatal(err)
	}
	if !running.Running || running.UsageBytes != 0 {
		t.Fatalf("running status = %+v", running)
	}
	if running.RuntimePID != started.PID || running.RuntimeInstanceID == "" {
		t.Fatalf("runtime identity = pid %d instance %q, want pid %d and a non-empty instance", running.RuntimePID, running.RuntimeInstanceID, started.PID)
	}

	runBinary(t, binary, "stop", "--config", configPath, "--json")
	output = runBinary(t, binary, "status", "--config", configPath, "--json")
	if err := json.Unmarshal(output, &running); err != nil {
		t.Fatal(err)
	}
	if running.Running {
		t.Fatal("runtime still reports running after stop")
	}
}

func TestInstalledStopWaitsForRuntimeCleanupAndExit(t *testing.T) {
	root := t.TempDir()
	binary := buildLayerCache(t)
	address := availableAddress(t)
	configPath := filepath.Join(root, "config.json")
	dataDir := filepath.Join(root, "cache")
	runBinary(t, binary,
		"setup", "--config", configPath,
		"--data-dir", dataDir,
		"--listen", address,
		"--non-interactive", "--json",
	)
	output := runBinary(t, binary, "start", "--config", configPath, "--json")
	var started struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(output, &started); err != nil || started.PID <= 0 {
		t.Fatalf("decode start result: pid %d, %v\n%s", started.PID, err, output)
	}
	t.Cleanup(func() { terminateExactDetachedRuntime(t, binary, configPath) })

	// Persistent Actions uploads are cleaned after the HTTP listener closes.
	// Enough disposable files make that post-listener shutdown phase observable.
	staging := filepath.Join(dataDir, "actions-staging")
	for index := range 4096 {
		path := filepath.Join(staging, "pending-"+strconv.Itoa(index))
		if err := os.WriteFile(path, []byte("pending"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	runBinary(t, binary, "stop", "--config", configPath, "--json")
	if err := syscall.Kill(started.PID, 0); err == nil {
		t.Fatalf("stop returned while runtime PID %d was still alive", started.PID)
	} else if err != syscall.ESRCH {
		t.Fatalf("inspect stopped runtime PID %d: %v", started.PID, err)
	}
}

func TestInstalledStatusReportsPersistedLocalCacheStatsAfterStop(t *testing.T) {
	root := t.TempDir()
	binary := buildLayerCache(t)
	address := availableAddress(t)
	configPath := filepath.Join(root, "config.json")
	runBinary(t, binary,
		"setup", "--config", configPath,
		"--data-dir", filepath.Join(root, "cache"),
		"--listen", address,
		"--non-interactive", "--json",
	)
	var connection turboConnection
	if err := json.Unmarshal(runBinary(t, binary,
		"integration", "turbo", "--config", configPath, "--json",
	), &connection); err != nil {
		t.Fatal(err)
	}
	runBinary(t, binary, "start", "--config", configPath, "--json")
	t.Cleanup(func() {
		cmd := exec.Command(binary, "stop", "--config", configPath, "--json")
		_, _ = cmd.CombinedOutput()
	})

	putTurboArtifact(t, connection, "offline-status-fixture", []byte("cached bytes"))
	runBinary(t, binary, "stop", "--config", configPath, "--json")

	output := runBinary(t, binary, "status", "--config", configPath, "--json")
	var status struct {
		Running    bool  `json:"running"`
		UsageBytes int64 `json:"usageBytes"`
		Artifacts  int64 `json:"artifacts"`
		Entries    int64 `json:"entries"`
	}
	if err := json.Unmarshal(output, &status); err != nil {
		t.Fatalf("decode stopped status: %v\n%s", err, output)
	}
	if status.Running || status.UsageBytes != 12 || status.Artifacts != 1 || status.Entries != 1 {
		t.Fatalf("stopped status = %+v, want persisted 12-byte Local Cache entry", status)
	}
}

func TestInstalledStartStopsItsChildWhenPIDOwnershipCannotBePersisted(t *testing.T) {
	root := t.TempDir()
	binary := buildLayerCache(t)
	address := availableAddress(t)
	configPath := filepath.Join(root, "config.json")
	dataDir := filepath.Join(root, "cache")
	runBinary(t, binary,
		"setup", "--config", configPath,
		"--data-dir", dataDir,
		"--listen", address,
		"--non-interactive", "--json",
	)

	// A directory at the ownership-file path lets the daemon initialize fully,
	// then makes only the final ownership persistence fail.
	if err := os.Mkdir(filepath.Join(dataDir, "runtime.pid"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { terminateExactDetachedRuntime(t, binary, configPath) })

	command := exec.Command(binary, "start", "--config", configPath, "--json")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("start succeeded despite unusable PID ownership path: %s", output)
	}

	client := &http.Client{Timeout: 50 * time.Millisecond}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		response, requestErr := client.Get("http://" + address + "/healthz")
		if requestErr == nil {
			response.Body.Close()
			t.Fatalf("daemon remained reachable after failed start: %s", output)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(string(output), "persist Layer Cache runtime ownership") {
		t.Fatalf("start error did not identify ownership persistence: %v\n%s", err, output)
	}
}

func TestInstalledStopRefusesToSignalAnUnverifiedReusedPID(t *testing.T) {
	root := t.TempDir()
	binary := buildLayerCache(t)
	configPath := filepath.Join(root, "config.json")
	dataDir := filepath.Join(root, "cache")
	runBinary(t, binary,
		"setup", "--config", configPath,
		"--data-dir", dataDir,
		"--listen", availableAddress(t),
		"--non-interactive", "--json",
	)

	unrelated := startTermIgnoringProcess(t, root)

	pidPath := filepath.Join(dataDir, "runtime.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(unrelated.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "stop", "--config", configPath, "--json")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("stop accepted an unauthenticated reused PID: %s", output)
	}
	if !strings.Contains(string(output), "refusing to signal") {
		t.Fatalf("stop error did not explain the safe refusal: %v\n%s", err, output)
	}
	if err := unrelated.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("stop harmed the unrelated process: %v\n%s", err, output)
	}
}

func TestInstalledStopRequiresPIDToMatchTheAuthenticatedRuntime(t *testing.T) {
	root := t.TempDir()
	binary := buildLayerCache(t)
	address := availableAddress(t)
	configPath := filepath.Join(root, "config.json")
	dataDir := filepath.Join(root, "cache")
	runBinary(t, binary,
		"setup", "--config", configPath,
		"--data-dir", dataDir,
		"--listen", address,
		"--non-interactive", "--json",
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

	command := exec.Command(binary, "stop", "--config", configPath, "--json")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("stop accepted ownership for another PID: %s", output)
	}
	if !strings.Contains(string(output), "does not match authenticated runtime") {
		t.Fatalf("stop error did not identify the ownership mismatch: %v\n%s", err, output)
	}
	if err := unrelated.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("stop harmed the unrelated process: %v\n%s", err, output)
	}
	response, err := (&http.Client{Timeout: time.Second}).Get("http://" + address + "/healthz")
	if err != nil {
		t.Fatalf("stop harmed the authenticated runtime: %v\n%s", err, output)
	}
	response.Body.Close()
}

func TestInstalledRepairRebuildsMissingOwnershipFromAuthenticatedRuntime(t *testing.T) {
	root := t.TempDir()
	binary := buildLayerCache(t)
	configPath := filepath.Join(root, "config.json")
	dataDir := filepath.Join(root, "cache")
	runBinary(t, binary,
		"setup", "--config", configPath,
		"--data-dir", dataDir,
		"--listen", availableAddress(t),
		"--non-interactive", "--json",
	)
	runBinary(t, binary, "start", "--config", configPath, "--json")
	pidPath := filepath.Join(dataDir, "runtime.pid")
	originalOwnership, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(pidPath); os.IsNotExist(err) {
			_ = os.WriteFile(pidPath, originalOwnership, 0o600)
		}
		command := exec.Command(binary, "stop", "--config", configPath, "--json")
		_, _ = command.CombinedOutput()
	})
	if err := os.Remove(pidPath); err != nil {
		t.Fatal(err)
	}

	stop := exec.Command(binary, "stop", "--config", configPath, "--json")
	stopOutput, err := stop.CombinedOutput()
	if err == nil || !strings.Contains(string(stopOutput), "use repair to rebuild it") {
		t.Fatalf("stop did not direct the recoverable ownership failure to repair: %v\n%s", err, stopOutput)
	}

	repairOutput := runBinary(t, binary, "repair", "--config", configPath, "--json")
	var repair struct {
		Repaired bool   `json:"repaired"`
		PIDState string `json:"pidState"`
	}
	if err := json.Unmarshal(repairOutput, &repair); err != nil {
		t.Fatal(err)
	}
	if !repair.Repaired || repair.PIDState != "runtime-ownership-rebuilt" {
		t.Fatalf("repair result = %+v\n%s", repair, repairOutput)
	}
	runBinary(t, binary, "stop", "--config", configPath, "--json")
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func startTermIgnoringProcess(t *testing.T, root string) *exec.Cmd {
	t.Helper()
	readyPath := filepath.Join(root, "unrelated-ready")
	command := exec.Command("sh", "-c", `trap '' TERM; printf ready > "$1"; while :; do sleep 1; done`, "sh", readyPath)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	})
	waitForFile(t, readyPath)
	return command
}

func terminateExactDetachedRuntime(t *testing.T, binary, configPath string) {
	t.Helper()
	output, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		t.Logf("inspect detached runtime for cleanup: %v", err)
		return
	}
	want := binary + " serve --config " + configPath
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		separator := strings.IndexByte(line, ' ')
		if separator < 1 || strings.TrimSpace(line[separator+1:]) != want {
			continue
		}
		pid, parseErr := strconv.Atoi(line[:separator])
		if parseErr != nil || pid <= 0 {
			continue
		}
		process, findErr := os.FindProcess(pid)
		if findErr == nil {
			_ = process.Kill()
		}
	}
}

func runBinary(t *testing.T, binary string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(binary, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", binary, args, err, output)
	}
	return output
}
