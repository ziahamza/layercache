package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/layercache/layercache/internal/config"
)

const ownershipMarkerName = ".layercache-owned.json"

var supportedAdapters = []string{"turbo", "actions", "buildkit"}

type diagnosticCheck struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type ownershipMarker struct {
	InstallationID string `json:"installationId"`
	ConfigPath     string `json:"configPath"`
}

type pidRepairResult struct {
	Changed bool
	Removed bool
	Rebuilt bool
	State   string
}

func redactedConfiguration(cfg config.Config) map[string]any {
	data, err := json.Marshal(cfg)
	if err != nil {
		return map[string]any{"error": "configuration could not be rendered"}
	}
	result := make(map[string]any)
	if err := json.Unmarshal(data, &result); err != nil {
		return map[string]any{"error": "configuration could not be rendered"}
	}
	for _, field := range []string{"localToken", "teamToken", "publisherToken", "publicPrivateKey"} {
		if value, present := result[field]; present && value != "" {
			result[field] = "[redacted]"
		}
	}
	return result
}

func runDoctor(args []string, stdout, stderr io.Writer) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "configuration file")
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load Layer Cache configuration: %w", err)
	}
	configCheck := inspectConfigPermissions(*configPath)
	dataCheck := inspectDataDirectory(cfg, *configPath)
	runtimeStatus, runtimeErr := probeRuntime(cfg)
	runtimeCheck := diagnosticCheck{OK: runtimeErr == nil && runtimeStatus.Running, Detail: "runtime is healthy"}
	if runtimeErr != nil {
		runtimeCheck.Detail = "runtime is not reachable: " + runtimeErr.Error()
	} else if !runtimeStatus.Running {
		runtimeCheck.Detail = "runtime responded but did not report healthy"
	}
	result := map[string]any{
		"healthy": configCheck.OK && dataCheck.OK && runtimeCheck.OK,
		"checks": map[string]diagnosticCheck{
			"config":  configCheck,
			"dataDir": dataCheck,
			"runtime": runtimeCheck,
		},
	}
	message := "Layer Cache diagnostics passed"
	if healthy, _ := result["healthy"].(bool); !healthy {
		message = "Layer Cache diagnostics found problems"
	}
	return printResult(stdout, *jsonOutput, result, message)
}

func inspectConfigPermissions(path string) diagnosticCheck {
	info, err := os.Stat(path)
	if err != nil {
		return diagnosticCheck{Detail: "cannot inspect configuration: " + err.Error()}
	}
	if !info.Mode().IsRegular() {
		return diagnosticCheck{Detail: "configuration is not a regular file"}
	}
	if info.Mode().Perm()&0o077 != 0 {
		return diagnosticCheck{Detail: fmt.Sprintf("configuration permissions are %04o; expected 0600", info.Mode().Perm())}
	}
	return diagnosticCheck{OK: true, Detail: "configuration is valid and protected"}
}

func inspectDataDirectory(cfg config.Config, configPath string) diagnosticCheck {
	info, err := os.Stat(cfg.DataDir)
	if err != nil {
		return diagnosticCheck{Detail: "cannot inspect Local Cache directory: " + err.Error()}
	}
	if !info.IsDir() {
		return diagnosticCheck{Detail: "Local Cache path is not a directory"}
	}
	if info.Mode().Perm()&0o077 != 0 {
		return diagnosticCheck{Detail: fmt.Sprintf("Local Cache directory permissions are %04o; expected 0700", info.Mode().Perm())}
	}
	if err := verifyOwnershipMarker(cfg, configPath); err != nil {
		return diagnosticCheck{Detail: err.Error()}
	}
	return diagnosticCheck{OK: true, Detail: "Local Cache directory is present and owned by this installation"}
}

func runRepair(args []string, stdout, stderr io.Writer) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("repair", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "configuration file")
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load Layer Cache configuration: %w", err)
	}
	if cfg.InstallationID == "" {
		return errors.New("configuration predates ownership tracking; rerun layercache setup before repair")
	}
	if err := validateDeletionTarget(cfg.DataDir); err != nil {
		return fmt.Errorf("refusing to repair unsafe Local Cache directory: %w", err)
	}

	createdDataDir := false
	if info, statErr := os.Stat(cfg.DataDir); errors.Is(statErr, fs.ErrNotExist) {
		if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
			return fmt.Errorf("recreate Local Cache directory: %w", err)
		}
		createdDataDir = true
	} else if statErr != nil {
		return fmt.Errorf("inspect Local Cache directory: %w", statErr)
	} else if !info.IsDir() {
		return errors.New("configured Local Cache path is not a directory")
	}
	if err := ensureOwnershipMarker(cfg, *configPath); err != nil {
		return err
	}

	pidRepair, err := repairPIDFile(cfg)
	if err != nil {
		return err
	}
	result := map[string]any{
		"repaired":                createdDataDir || pidRepair.Changed,
		"createdDataDir":          createdDataDir,
		"removedStalePid":         pidRepair.Removed,
		"rebuiltRuntimeOwnership": pidRepair.Rebuilt,
		"pidState":                pidRepair.State,
		"dataDir":                 cfg.DataDir,
	}
	return printResult(stdout, *jsonOutput, result, "Layer Cache repair complete")
}

func repairPIDFile(cfg config.Config) (pidRepairResult, error) {
	if live, probeErr := probeRuntime(cfg); probeErr == nil {
		if live.RuntimePID <= 0 || live.RuntimeInstanceID == "" {
			return pidRepairResult{}, errors.New("authenticated Layer Cache runtime returned an invalid process identity")
		}
		ownership, readErr := readRuntimeOwnership(cfg)
		if readErr == nil && ownership.PID == live.RuntimePID && ownership.InstanceID == live.RuntimeInstanceID {
			return pidRepairResult{State: "runtime-ownership-healthy"}, nil
		}
		if err := writeRuntimeOwnership(cfg, runtimeOwnership{PID: live.RuntimePID, InstanceID: live.RuntimeInstanceID}); err != nil {
			return pidRepairResult{}, fmt.Errorf("rebuild Layer Cache runtime ownership: %w", err)
		}
		return pidRepairResult{Changed: true, Rebuilt: true, State: "runtime-ownership-rebuilt"}, nil
	}

	path := pidPath(cfg)
	_, statErr := os.Lstat(path)
	if errors.Is(statErr, fs.ErrNotExist) {
		return pidRepairResult{State: "absent"}, nil
	}
	if statErr != nil {
		return pidRepairResult{State: "unknown"}, fmt.Errorf("inspect Layer Cache runtime ownership file: %w", statErr)
	}
	pid, readErr := readPID(cfg)
	if readErr != nil {
		if err := os.Remove(path); err != nil {
			return pidRepairResult{State: "invalid"}, fmt.Errorf("remove invalid Layer Cache runtime ownership file: %w", err)
		}
		return pidRepairResult{Changed: true, Removed: true, State: "invalid-removed"}, nil
	}
	alive, err := processAlive(pid)
	if err != nil {
		return pidRepairResult{State: "unknown"}, fmt.Errorf("inspect Layer Cache PID %d: %w", pid, err)
	}
	if alive {
		return pidRepairResult{State: "process-alive-runtime-unverified"}, nil
	}
	if err := os.Remove(path); err != nil {
		return pidRepairResult{State: "stale"}, fmt.Errorf("remove stale Layer Cache runtime ownership file: %w", err)
	}
	return pidRepairResult{Changed: true, Removed: true, State: "stale-removed"}, nil
}

func processAlive(pid int) (bool, error) {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, err
	}
	err = process.Signal(syscall.Signal(0))
	if err == nil || errors.Is(err, syscall.EPERM) {
		return true, nil
	}
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return false, err
}

func runGC(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("gc", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "configuration file")
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load Layer Cache configuration: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+cfg.Listen+"/v1/gc", nil)
	if err != nil {
		return fmt.Errorf("create garbage collection request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+cfg.LocalToken)
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return fmt.Errorf("garbage collection requires a running runtime: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		detail := strings.TrimSpace(string(body))
		if detail == "" {
			detail = http.StatusText(response.StatusCode)
		}
		return fmt.Errorf("running runtime rejected garbage collection with HTTP %d: %s", response.StatusCode, detail)
	}
	result := make(map[string]any)
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil {
		return fmt.Errorf("decode garbage collection result: %w", err)
	}
	return printResult(stdout, *jsonOutput, result, "Layer Cache garbage collection complete")
}

func runBypass(args []string, stdout, stderr io.Writer) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("bypass", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "configuration file")
	adapter := flags.String("adapter", "", "adapter to bypass: turbo, actions, buildkit, or all")
	clearBypass := flags.Bool("clear", false, "clear bypasses (all when --adapter is omitted)")
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *adapter == "" && !*clearBypass {
		return errors.New("--adapter is required (turbo, actions, buildkit, or all)")
	}
	if *adapter != "" && *adapter != "all" && !isSupportedAdapter(*adapter) {
		return fmt.Errorf("unknown adapter %q", *adapter)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load Layer Cache configuration: %w", err)
	}

	active := make(map[string]bool, len(cfg.BypassAdapters))
	for _, name := range cfg.BypassAdapters {
		active[name] = true
	}
	if *clearBypass {
		if *adapter == "" || *adapter == "all" {
			clear(active)
		} else {
			delete(active, *adapter)
		}
	} else if *adapter == "all" {
		for _, name := range supportedAdapters {
			active[name] = true
		}
	} else {
		active[*adapter] = true
	}
	cfg.BypassAdapters = canonicalAdapters(active)
	if err := config.Save(*configPath, cfg); err != nil {
		return fmt.Errorf("save Layer Cache bypass settings: %w", err)
	}
	result := map[string]any{"bypassAdapters": cfg.BypassAdapters}
	return printResult(stdout, *jsonOutput, result, "Layer Cache bypass settings updated")
}

func canonicalAdapters(active map[string]bool) []string {
	result := make([]string, 0, len(active))
	for _, name := range supportedAdapters {
		if active[name] {
			result = append(result, name)
		}
	}
	return result
}

func isSupportedAdapter(adapter string) bool {
	for _, name := range supportedAdapters {
		if adapter == name {
			return true
		}
	}
	return false
}

func isAdapterBypassed(cfg config.Config, adapter string) bool {
	for _, name := range cfg.BypassAdapters {
		if name == adapter {
			return true
		}
	}
	return false
}

func runUninstall(args []string, stdout, stderr io.Writer) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "configuration file")
	preserveCache := flags.Bool("preserve-cache", false, "remove Layer Cache but retain cached artifacts")
	deleteCache := flags.Bool("delete-cache", false, "remove Layer Cache and its exact configured data directory")
	yes := flags.Bool("yes", false, "confirm irreversible Local Cache deletion")
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *preserveCache == *deleteCache {
		return errors.New("choose exactly one of --preserve-cache or --delete-cache")
	}
	if *deleteCache && !*yes {
		return errors.New("--delete-cache is irreversible and requires --yes")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load Layer Cache configuration: %w", err)
	}
	if err := validateDeletionTarget(cfg.DataDir); err != nil {
		return fmt.Errorf("refusing to uninstall from unsafe Local Cache directory: %w", err)
	}
	if err := verifyOwnershipMarker(cfg, *configPath); err != nil {
		return fmt.Errorf("refusing to remove unowned Local Cache files: %w", err)
	}
	if err := stopRuntimeForUninstall(cfg); err != nil {
		return err
	}

	if *deleteCache {
		if err := os.RemoveAll(cfg.DataDir); err != nil {
			return fmt.Errorf("delete configured Local Cache directory %s: %w", cfg.DataDir, err)
		}
	} else {
		for _, path := range []string{pidPath(cfg), filepath.Join(cfg.DataDir, "daemon.log"), filepath.Join(cfg.DataDir, ownershipMarkerName)} {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove owned runtime file %s: %w", path, err)
			}
		}
	}
	if err := os.Remove(*configPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove Layer Cache configuration: %w", err)
	}
	result := map[string]any{
		"uninstalled":    true,
		"cachePreserved": *preserveCache,
		"dataDir":        cfg.DataDir,
	}
	return printResult(stdout, *jsonOutput, result, "Layer Cache uninstalled")
}

func stopRuntimeForUninstall(cfg config.Config) error {
	live, probeErr := probeRuntime(cfg)
	ownership, ownershipErr := readRuntimeOwnership(cfg)
	if probeErr != nil {
		if ownershipErr != nil {
			return nil
		}
		alive, err := processAlive(ownership.PID)
		if err != nil {
			return fmt.Errorf("inspect Layer Cache runtime before uninstall: %w", err)
		}
		if alive {
			return fmt.Errorf("Layer Cache PID %d is alive but the authenticated runtime is unavailable; refusing to signal it", ownership.PID)
		}
		return nil
	}
	if ownershipErr != nil {
		return errors.New("Layer Cache is running but its runtime ownership file is missing or invalid; use repair to rebuild it before uninstalling")
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
		if _, err := probeRuntime(cfg); err != nil {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return errors.New("Layer Cache runtime did not stop within five seconds")
}

func validateDeletionTarget(path string) error {
	if path == "" {
		return errors.New("path is empty")
	}
	if !filepath.IsAbs(path) {
		return errors.New("path must be absolute")
	}
	clean := filepath.Clean(path)
	if clean != path {
		return errors.New("path must be normalized")
	}
	if err := validateResolvedDeletionTarget(clean); err != nil {
		return err
	}
	resolved, err := resolveThroughExistingAncestor(clean)
	if err != nil {
		return fmt.Errorf("resolve path symlinks: %w", err)
	}
	if err := validateResolvedDeletionTarget(resolved); err != nil {
		return fmt.Errorf("resolved path %s is unsafe: %w", resolved, err)
	}
	return nil
}

func resolveThroughExistingAncestor(path string) (string, error) {
	candidate := path
	suffix := make([]string, 0)
	for {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err == nil {
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return "", err
		}
		suffix = append(suffix, filepath.Base(candidate))
		candidate = parent
	}
}

func validateResolvedDeletionTarget(path string) error {
	volume := filepath.VolumeName(path)
	root := volume + string(os.PathSeparator)
	if filepath.Clean(path) == filepath.Clean(root) {
		return errors.New("filesystem root is never a valid Local Cache directory")
	}
	relativeToRoot, err := filepath.Rel(root, path)
	if err != nil || relativeToRoot == "." || relativeToRoot == ".." || strings.HasPrefix(relativeToRoot, ".."+string(os.PathSeparator)) {
		return errors.New("path is not safely below a filesystem root")
	}
	if len(strings.FieldsFunc(relativeToRoot, func(r rune) bool { return r == '/' || r == '\\' })) < 2 {
		return errors.New("top-level system directories are never valid Local Cache deletion targets")
	}
	for _, protected := range protectedDeletionTargets() {
		if samePath(path, protected) {
			return fmt.Errorf("protected directory %s is never a valid Local Cache deletion target", protected)
		}
	}
	return nil
}

func protectedDeletionTargets() []string {
	result := make([]string, 0, 2)
	if home, err := os.UserHomeDir(); err == nil {
		result = append(result, filepath.Clean(home))
	}
	if workingDirectory, err := os.Getwd(); err == nil {
		result = append(result, filepath.Clean(workingDirectory))
	}
	return result
}

func samePath(left, right string) bool {
	return filepath.Clean(left) == filepath.Clean(right)
}

func ensureOwnershipMarker(cfg config.Config, configPath string) error {
	if cfg.InstallationID == "" {
		return errors.New("installationId is required to own Local Cache files")
	}
	absoluteConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return fmt.Errorf("resolve configuration path: %w", err)
	}
	marker := ownershipMarker{InstallationID: cfg.InstallationID, ConfigPath: absoluteConfigPath}
	path := filepath.Join(cfg.DataDir, ownershipMarkerName)
	if data, readErr := os.ReadFile(path); readErr == nil {
		var existing ownershipMarker
		if jsonErr := json.Unmarshal(data, &existing); jsonErr != nil {
			return fmt.Errorf("decode existing Local Cache ownership marker: %w", jsonErr)
		}
		if existing != marker {
			return errors.New("Local Cache directory is owned by a different Layer Cache installation")
		}
		return nil
	} else if !errors.Is(readErr, fs.ErrNotExist) {
		return fmt.Errorf("read Local Cache ownership marker: %w", readErr)
	}
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Local Cache ownership marker: %w", err)
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(cfg.DataDir, ".ownership-*")
	if err != nil {
		return fmt.Errorf("stage Local Cache ownership marker: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("protect Local Cache ownership marker: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write Local Cache ownership marker: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync Local Cache ownership marker: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close Local Cache ownership marker: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("commit Local Cache ownership marker: %w", err)
	}
	return nil
}

func rejectOwnershipConflict(cfg config.Config, configPath string) error {
	path := filepath.Join(cfg.DataDir, ownershipMarkerName)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read existing Local Cache ownership marker: %w", err)
	}
	var marker ownershipMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return fmt.Errorf("decode existing Local Cache ownership marker: %w", err)
	}
	absoluteConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return fmt.Errorf("resolve configuration path: %w", err)
	}
	want := ownershipMarker{InstallationID: cfg.InstallationID, ConfigPath: absoluteConfigPath}
	if marker != want {
		return errors.New("Local Cache directory is owned by a different Layer Cache installation")
	}
	return nil
}

func verifyOwnershipMarker(cfg config.Config, configPath string) error {
	if cfg.InstallationID == "" {
		return errors.New("configuration has no installation ownership identity")
	}
	absoluteConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return fmt.Errorf("resolve configuration path: %w", err)
	}
	markerPath := filepath.Join(cfg.DataDir, ownershipMarkerName)
	info, err := os.Lstat(markerPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errors.New("Local Cache ownership marker is missing; run layercache repair")
		}
		return fmt.Errorf("inspect Local Cache ownership marker: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("Local Cache ownership marker must be a protected regular file with 0600 permissions")
	}
	data, err := os.ReadFile(markerPath)
	if err != nil {
		return fmt.Errorf("read Local Cache ownership marker: %w", err)
	}
	var marker ownershipMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return fmt.Errorf("decode Local Cache ownership marker: %w", err)
	}
	want := ownershipMarker{InstallationID: cfg.InstallationID, ConfigPath: absoluteConfigPath}
	if marker != want {
		return errors.New("Local Cache ownership marker belongs to another installation")
	}
	return nil
}

type repeatableStringFlag struct {
	values []string
	set    bool
}

func newRepeatableStringFlag(defaults []string) *repeatableStringFlag {
	return &repeatableStringFlag{values: append([]string(nil), defaults...)}
}

func (value *repeatableStringFlag) Set(entry string) error {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return errors.New("value cannot be empty")
	}
	if !value.set {
		value.values = nil
		value.set = true
	}
	value.values = append(value.values, entry)
	return nil
}

func (value *repeatableStringFlag) String() string {
	return strings.Join(value.values, ",")
}

func (value *repeatableStringFlag) Values() []string {
	return append([]string(nil), value.values...)
}
