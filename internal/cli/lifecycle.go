package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"syscall"
	"time"

	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/publicbuild"
)

type runtimeStatus struct {
	Running            bool                       `json:"running"`
	Role               string                     `json:"role"`
	ProjectID          string                     `json:"projectId"`
	StartedAt          time.Time                  `json:"startedAt"`
	RuntimePID         int                        `json:"runtimePid"`
	RuntimeInstanceID  string                     `json:"runtimeInstanceId"`
	UsageBytes         int64                      `json:"usageBytes"`
	MaxBytes           int64                      `json:"maxBytes"`
	EvictionPolicy     string                     `json:"evictionPolicy"`
	Artifacts          int64                      `json:"artifacts"`
	Entries            int64                      `json:"entries"`
	PendingUploads     int64                      `json:"pendingUploads"`
	PendingUploadBytes int64                      `json:"pendingUploadBytes"`
	PublicBuild        *publicbuild.StatusSummary `json:"publicBuild,omitempty"`
}

type runtimeOwnership struct {
	PID        int    `json:"pid"`
	InstanceID string `json:"instanceId"`
}

func runStart(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("start", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "configuration file")
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	unlockConfiguration, err := lockConfiguration(ctx, *configPath)
	if err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		unlockConfiguration()
		return err
	}
	if live, err := probeRuntime(cfg); err == nil {
		unlockConfiguration()
		return printResult(stdout, *jsonOutput, map[string]any{
			"running": true, "pid": live.RuntimePID, "runtimeInstanceId": live.RuntimeInstanceID, "alreadyRunning": true,
		}, "Layer Cache is already running")
	}
	// Do not hold the configuration lock while the child starts: serve may
	// need it to persist a refreshed capability. A second locked comparison
	// below detects any mutation that won the startup race.
	unlockConfiguration()
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find Layer Cache executable: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(cfg.DataDir, "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open Layer Cache daemon log: %w", err)
	}
	command := exec.Command(executable, "serve", "--config", *configPath)
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start Layer Cache runtime: %w", err)
	}
	pid := command.Process.Pid
	_ = logFile.Close()
	deadline := time.Now().Add(5 * time.Second)
	var live runtimeStatus
	for time.Now().Before(deadline) {
		candidate, probeErr := probeRuntime(cfg)
		if probeErr == nil {
			if candidate.RuntimePID != pid || candidate.RuntimeInstanceID == "" {
				terminateStartedChild(command)
				return fmt.Errorf("authenticated runtime identity is pid %d instance %q, want newly started pid %d", candidate.RuntimePID, candidate.RuntimeInstanceID, pid)
			}
			live = candidate
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if live.RuntimePID == 0 {
		terminateStartedChild(command)
		return fmt.Errorf("Layer Cache runtime did not become healthy; inspect %s", filepath.Join(cfg.DataDir, "daemon.log"))
	}
	unlockConfiguration, err = lockConfiguration(ctx, *configPath)
	if err != nil {
		terminateStartedChild(command)
		return err
	}
	defer unlockConfiguration()
	current, err := config.Load(*configPath)
	if err != nil {
		terminateStartedChild(command)
		return fmt.Errorf("reload Layer Cache configuration after runtime startup: %w", err)
	}
	if !reflect.DeepEqual(current, cfg) {
		terminateStartedChild(command)
		return errors.New("Layer Cache configuration changed while the runtime was starting; the new runtime was stopped, rerun start")
	}
	if err := writeRuntimeOwnership(cfg, runtimeOwnership{PID: pid, InstanceID: live.RuntimeInstanceID}); err != nil {
		terminateStartedChild(command)
		return fmt.Errorf("persist Layer Cache runtime ownership: %w", err)
	}
	if err := command.Process.Release(); err != nil {
		terminateStartedChild(command)
		_ = os.Remove(pidPath(cfg))
		return fmt.Errorf("release Layer Cache runtime: %w", err)
	}
	return printResult(stdout, *jsonOutput, map[string]any{
		"running": true, "pid": pid, "runtimeInstanceId": live.RuntimeInstanceID,
	}, "Layer Cache started")
}

func terminateStartedChild(command *exec.Cmd) {
	if err := command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		_ = command.Process.Kill()
	}
	waited := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		_ = command.Process.Kill()
		<-waited
	}
}

func runStop(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("stop", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "configuration file")
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	unlockConfiguration, err := lockConfiguration(ctx, *configPath)
	if err != nil {
		return err
	}
	defer unlockConfiguration()
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	ownership, err := readRuntimeOwnership(cfg)
	if err != nil {
		if _, probeErr := probeRuntime(cfg); probeErr != nil {
			return printResult(stdout, *jsonOutput, map[string]any{"running": false}, "Layer Cache is stopped")
		}
		return errors.New("Layer Cache is running but its PID ownership file is missing or invalid; use repair to rebuild it")
	}
	live, probeErr := probeRuntime(cfg)
	if probeErr != nil {
		alive, aliveErr := processAlive(ownership.PID)
		if aliveErr != nil {
			return fmt.Errorf("inspect Layer Cache PID %d before stop: %w", ownership.PID, aliveErr)
		}
		if alive {
			return fmt.Errorf("authenticated Layer Cache runtime is unavailable; refusing to signal unverified PID %d", ownership.PID)
		}
		_ = os.Remove(pidPath(cfg))
		return printResult(stdout, *jsonOutput, map[string]any{"running": false}, "Layer Cache is stopped")
	}
	if err := verifyRuntimeOwnership(ownership, live); err != nil {
		return err
	}
	process, err := os.FindProcess(ownership.PID)
	if err != nil {
		return fmt.Errorf("find Layer Cache runtime: %w", err)
	}
	if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("stop Layer Cache runtime: %w", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		alive, aliveErr := processAlive(ownership.PID)
		if aliveErr != nil {
			return fmt.Errorf("confirm Layer Cache runtime PID %d stopped: %w", ownership.PID, aliveErr)
		}
		if !alive {
			_ = os.Remove(pidPath(cfg))
			return printResult(stdout, *jsonOutput, map[string]any{"running": false, "pid": ownership.PID}, "Layer Cache stopped")
		}
		time.Sleep(25 * time.Millisecond)
	}
	return errors.New("Layer Cache runtime did not exit within five seconds")
}

func verifyRuntimeOwnership(ownership runtimeOwnership, live runtimeStatus) error {
	if ownership.InstanceID != "" && ownership.PID == live.RuntimePID && ownership.InstanceID == live.RuntimeInstanceID {
		return nil
	}
	return fmt.Errorf(
		"runtime ownership pid %d instance %q does not match authenticated runtime pid %d instance %q; use repair to rebuild it",
		ownership.PID, ownership.InstanceID, live.RuntimePID, live.RuntimeInstanceID,
	)
}

func probeRuntime(cfg config.Config) (runtimeStatus, error) {
	request, err := http.NewRequest(http.MethodGet, localRuntimeURL(cfg.Listen)+"/v1/status", nil)
	if err != nil {
		return runtimeStatus{}, err
	}
	request.Header.Set("Authorization", "Bearer "+cfg.LocalToken)
	client := newLocalCLIHTTPClient(300 * time.Millisecond)
	response, err := client.Do(request)
	if err != nil {
		return runtimeStatus{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return runtimeStatus{}, fmt.Errorf("runtime status returned HTTP %d", response.StatusCode)
	}
	var status runtimeStatus
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&status); err != nil {
		return runtimeStatus{}, err
	}
	return status, nil
}

func rejectConfigurationMutationWhileRuntimeActive(cfg config.Config) error {
	ownership, ownershipErr := readRuntimeOwnership(cfg)
	if ownershipErr == nil {
		alive, err := processAlive(ownership.PID)
		if err != nil {
			return fmt.Errorf("inspect owned Layer Cache runtime before changing configuration: %w", err)
		}
		if alive {
			return fmt.Errorf(
				"refusing to change Layer Cache setup while owned runtime pid %d may still be running; stop it before changing configuration",
				ownership.PID,
			)
		}
	}
	if live, err := probeRuntime(cfg); err == nil && live.Running {
		return fmt.Errorf(
			"refusing to change Layer Cache setup while runtime pid %d is running; stop it before changing configuration",
			live.RuntimePID,
		)
	}
	return nil
}

func readPID(cfg config.Config) (int, error) {
	ownership, err := readRuntimeOwnership(cfg)
	if err != nil {
		return 0, err
	}
	return ownership.PID, nil
}

func readRuntimeOwnership(cfg config.Config) (runtimeOwnership, error) {
	data, err := os.ReadFile(pidPath(cfg))
	if err != nil {
		return runtimeOwnership{}, err
	}
	trimmed := bytesTrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var ownership runtimeOwnership
		if err := json.Unmarshal(trimmed, &ownership); err != nil || ownership.PID <= 0 || ownership.InstanceID == "" {
			return runtimeOwnership{}, errors.New("invalid Layer Cache runtime ownership file")
		}
		return ownership, nil
	}
	pid, err := strconv.Atoi(string(trimmed))
	if err != nil || pid <= 0 {
		return runtimeOwnership{}, errors.New("invalid Layer Cache runtime ownership file")
	}
	return runtimeOwnership{PID: pid}, nil
}

func writeRuntimeOwnership(cfg config.Config, ownership runtimeOwnership) error {
	if ownership.PID <= 0 || ownership.InstanceID == "" {
		return errors.New("runtime ownership requires a PID and instance identity")
	}
	encoded, err := json.Marshal(ownership)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	temporary, err := os.CreateTemp(cfg.DataDir, ".runtime-ownership-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, pidPath(cfg))
}

func pidPath(cfg config.Config) string {
	return filepath.Join(cfg.DataDir, "runtime.pid")
}

func bytesTrimSpace(value []byte) []byte {
	start, end := 0, len(value)
	for start < end && (value[start] == ' ' || value[start] == '\n' || value[start] == '\r' || value[start] == '\t') {
		start++
	}
	for end > start && (value[end-1] == ' ' || value[end-1] == '\n' || value[end-1] == '\r' || value[end-1] == '\t') {
		end--
	}
	return value[start:end]
}
