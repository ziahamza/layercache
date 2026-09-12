package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/measurement"
)

const maximumTurboSummaryBytes = 64 << 20
const maximumTurboSummaryOutputLineBytes = 16 << 10
const measurementCapabilityEnvironment = "LAYER_CACHE_MEASUREMENT_TOKEN"

func runCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "configuration file")
	var transientBypasses stringList
	flags.Var(&transientBypasses, "bypass", "bypass turbo, actions, buildkit, or all for this command (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	commandArgs := flags.Args()
	if len(commandArgs) == 0 {
		return errors.New("a command is required after --")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	activeBypasses := make(map[string]bool, len(cfg.BypassAdapters)+len(transientBypasses))
	for _, adapter := range cfg.BypassAdapters {
		activeBypasses[adapter] = true
	}
	for _, adapter := range transientBypasses {
		if adapter == "all" {
			for _, supported := range supportedAdapters {
				activeBypasses[supported] = true
			}
			continue
		}
		if !isSupportedAdapter(adapter) {
			return fmt.Errorf("unknown transient bypass adapter %q", adapter)
		}
		activeBypasses[adapter] = true
	}
	cfg.BypassAdapters = canonicalAdapters(activeBypasses)
	discovered, scopeErr := discoverConfiguredProject(ctx, cfg, "", flagWasSet(flags, "config") || cfg.ProjectRoot == "")
	if scopeErr != nil {
		_, _ = fmt.Fprintf(stderr, "Layer Cache warning: %v; bypassing Layer Cache for this command\n", scopeErr)
		return runCommandWithScopeBypass(ctx, commandArgs, stdout, stderr)
	}
	if _, err := probeRuntime(cfg); err != nil {
		_, _ = fmt.Fprintf(stderr,
			"Layer Cache warning: the local daemon is unavailable; running the command without Layer Cache environment injection: %v\n", err,
		)
		return runCommandWithoutCache(ctx, commandArgs, stdout, stderr)
	}
	random, err := config.NewToken()
	if err != nil {
		return err
	}
	runID := "run-" + random[:20]
	now := time.Now().UTC()
	authority := access.Claims{
		Subject: cfg.InstallationID, RunID: runID, Project: cfg.ProjectID,
		Compatibility: cfg.CompatibilityID, Repository: cfg.ActionsRepository,
		Ref: cfg.ActionsRef, DefaultRef: cfg.ActionsDefaultRef,
		RecipeDigest: strings.TrimSpace(os.Getenv("LAYER_CACHE_RECIPE_DIGEST")),
		Platform:     runtime.GOOS + "/" + runtime.GOARCH, Toolchain: cfg.CompatibilityID,
		Builder:      strings.TrimSpace(os.Getenv("RUNNER_IMAGE")),
		Capabilities: []access.Capability{access.CapabilityWrite}, ExpiresAt: now.Add(12 * time.Hour),
	}
	if authority.Builder == "" {
		authority.Builder = "local-runtime"
	}
	discoveredRoot := discovered.Root
	if discoveredRoot != "" {
		if discovered.ActionsRepository != "" {
			authority.Repository = discovered.ActionsRepository
		}
		authority.Ref = discovered.Ref
		authority.DefaultRef = discovered.DefaultRef
		authority.SourceCommit = discovered.Commit
	}
	authority.WorkspaceID = workspaceMeasurementIdentity(cfg.ProjectID, discoveredRoot, cfg.ProjectRoot)
	turboAuthority := authority
	turboAuthority.Integration = "turbo"
	turboToken, err := access.MintCapabilityToken(cfg.LocalToken, turboAuthority, now)
	if err != nil {
		return err
	}
	actionsAuthority := authority
	actionsAuthority.Integration = "actions"
	actionsToken, err := access.MintCapabilityToken(cfg.LocalToken, actionsAuthority, now)
	if err != nil {
		return err
	}
	buildkitAuthority := authority
	buildkitAuthority.Integration = "buildkit"
	buildkitToken, err := access.MintCapabilityToken(cfg.LocalToken, buildkitAuthority, now)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, commandArgs[0], commandArgs[1:]...)
	command.Stdin = os.Stdin
	command.Stdout = stdout
	command.Stderr = stderr
	command.Env = os.Environ()
	summaryRoots := turboSummaryRoots(cfg.ProjectRoot, discoveredRoot)
	summaryEnabled := !isAdapterBypassed(cfg, "turbo") && len(summaryRoots) > 0
	var summaryCollector *turboSummaryPathCollector
	if summaryEnabled {
		summaryCollector = newTurboSummaryPathCollector()
		command.Stdout = summaryCollector.observe(stdout)
		command.Stderr = summaryCollector.observe(stderr)
	}
	injected := map[string]string{
		"LAYER_CACHE_RUN_ID":             runID,
		"LAYER_CACHE_CONFIG":             *configPath,
		"LAYER_CACHE_BYPASS":             strings.Join(cfg.BypassAdapters, ","),
		"TURBO_API":                      localRuntimeURL(cfg.Listen),
		"TURBO_TOKEN":                    turboToken,
		"TURBO_TEAM":                     cfg.ProjectID,
		"ACTIONS_CACHE_URL":              localRuntimeURL(cfg.Listen) + "/",
		"ACTIONS_RUNTIME_TOKEN":          actionsToken,
		"ACTIONS_CACHE_SERVICE_V2":       "",
		measurementCapabilityEnvironment: buildkitToken,
	}
	if summaryEnabled {
		// This is Turborepo's documented environment equivalent of
		// --summarize. It preserves the user's command line while ensuring the
		// completed task graph is available after the child exits.
		injected["TURBO_RUN_SUMMARY"] = "true"
	}
	if isAdapterBypassed(cfg, "turbo") {
		injected["TURBO_API"] = ""
		injected["TURBO_TOKEN"] = ""
		injected["TURBO_TEAM"] = ""
	}
	if isAdapterBypassed(cfg, "actions") {
		injected["ACTIONS_CACHE_URL"] = ""
		injected["ACTIONS_RUNTIME_TOKEN"] = ""
	}
	for key, value := range injected {
		command.Env = replaceEnvironment(command.Env, key, value)
	}
	commandErr := command.Run()
	finishedAt := time.Now().UTC()
	if summaryEnabled {
		documents, summaryErr := turboSummaryDocuments(summaryRoots, summaryCollector.paths())
		if summaryErr == nil && len(documents) > 0 {
			var reconciled int
			reconciled, summaryErr = reconcileTurboSummaries(
				ctx, cfg, runID, authority.WorkspaceID, now, finishedAt, documents,
			)
			if summaryErr == nil {
				_, _ = fmt.Fprintf(stderr, "Layer Cache Turbo summary: %d final cache-eligible task(s)\n", reconciled)
			}
		}
		if summaryErr != nil {
			_, _ = fmt.Fprintf(stderr, "Layer Cache warning: Turbo Run Summary reconciliation failed: %v; Turbo ROI was not recorded\n", summaryErr)
		}
	}
	_, _ = fmt.Fprintf(stderr, "Layer Cache run: %s\n", runID)
	if commandErr != nil {
		return fmt.Errorf("command failed in Layer Cache run %s: %w", runID, commandErr)
	}
	return nil
}

func runCommandWithoutCache(ctx context.Context, commandArgs []string, stdout, stderr io.Writer) error {
	command := exec.CommandContext(ctx, commandArgs[0], commandArgs[1:]...)
	command.Stdin = os.Stdin
	command.Stdout = stdout
	command.Stderr = stderr
	command.Env = os.Environ()
	if err := command.Run(); err != nil {
		return fmt.Errorf("command failed while Layer Cache was unavailable: %w", err)
	}
	return nil
}

func runCommandWithScopeBypass(ctx context.Context, commandArgs []string, stdout, stderr io.Writer) error {
	command := exec.CommandContext(ctx, commandArgs[0], commandArgs[1:]...)
	command.Stdin = os.Stdin
	command.Stdout = stdout
	command.Stderr = stderr
	command.Env = os.Environ()
	// A nested invocation may inherit capabilities from its parent's checkout.
	// Remove those as well as suppressing new credentials. Nested Buildx commands
	// honor the all-adapter bypass even when passed an explicit configuration.
	for _, key := range []string{
		"LAYER_CACHE_RUN_ID", "LAYER_CACHE_CONFIG", measurementCapabilityEnvironment,
		"TURBO_API", "TURBO_TOKEN", "TURBO_TEAM", "TURBO_RUN_SUMMARY",
		"ACTIONS_CACHE_URL", "ACTIONS_RUNTIME_TOKEN", "ACTIONS_CACHE_SERVICE_V2",
		"ACTIONS_RESULTS_URL", "LOCAL_CI_ACTIONS_CACHE_URL", "LOCAL_CI_ACTIONS_RUNTIME_TOKEN",
	} {
		command.Env = replaceEnvironment(command.Env, key, "")
	}
	command.Env = replaceEnvironment(command.Env, "LAYER_CACHE_BYPASS", "all")
	if err := command.Run(); err != nil {
		return fmt.Errorf("command failed while Layer Cache was bypassed: %w", err)
	}
	return nil
}

func turboSummaryRoots(configuredRoot, discoveredRoot string) []string {
	candidates := []string{configuredRoot, discoveredRoot}
	if workingDirectory, err := os.Getwd(); err == nil {
		candidates = append(candidates, workingDirectory)
	}
	seen := make(map[string]struct{}, len(candidates))
	result := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		absolute, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		absolute = filepath.Clean(absolute)
		if _, exists := seen[absolute]; exists {
			continue
		}
		seen[absolute] = struct{}{}
		result = append(result, absolute)
	}
	return result
}

func turboSummaryDocuments(roots, paths []string) ([][]byte, error) {
	sort.Strings(paths)
	documents := make([][]byte, 0)
	for _, path := range paths {
		if !turboSummaryPathAllowed(roots, path) {
			return nil, fmt.Errorf("Turbo reported a Run Summary outside an expected .turbo/runs directory: %q", path)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect Turbo Run Summary %q: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("Turbo Run Summary %q is not a regular file", path)
		}
		if info.Size() > maximumTurboSummaryBytes {
			return nil, fmt.Errorf("Turbo Run Summary %q exceeds %d bytes", path, maximumTurboSummaryBytes)
		}
		document, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read Turbo Run Summary %q: %w", path, err)
		}
		documents = append(documents, document)
	}
	return documents, nil
}

func turboSummaryPathAllowed(roots []string, path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	path = filepath.Clean(path)
	for _, root := range roots {
		directory := filepath.Join(root, ".turbo", "runs")
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			continue
		}
		if relative != "." && filepath.Dir(relative) == "." && strings.EqualFold(filepath.Ext(relative), ".json") {
			return true
		}
	}
	return false
}

type turboSummaryPathCollector struct {
	mu          sync.Mutex
	pathsByName map[string]struct{}
	observers   []*turboSummaryObservingWriter
}

func newTurboSummaryPathCollector() *turboSummaryPathCollector {
	return &turboSummaryPathCollector{pathsByName: make(map[string]struct{})}
}

func (collector *turboSummaryPathCollector) observe(output io.Writer) io.Writer {
	observer := &turboSummaryObservingWriter{output: output, collector: collector}
	collector.mu.Lock()
	collector.observers = append(collector.observers, observer)
	collector.mu.Unlock()
	return observer
}

func (collector *turboSummaryPathCollector) addLine(line []byte) {
	plain := strings.TrimSpace(stripTerminalControlSequences(string(line)))
	if !strings.HasPrefix(plain, "Summary:") {
		return
	}
	path := strings.TrimSpace(strings.TrimPrefix(plain, "Summary:"))
	if path == "" {
		return
	}
	collector.mu.Lock()
	collector.pathsByName[path] = struct{}{}
	collector.mu.Unlock()
}

func (collector *turboSummaryPathCollector) paths() []string {
	collector.mu.Lock()
	observers := append([]*turboSummaryObservingWriter(nil), collector.observers...)
	collector.mu.Unlock()
	for _, observer := range observers {
		observer.flush()
	}
	collector.mu.Lock()
	paths := make([]string, 0, len(collector.pathsByName))
	for path := range collector.pathsByName {
		paths = append(paths, path)
	}
	collector.mu.Unlock()
	return paths
}

type turboSummaryObservingWriter struct {
	mu        sync.Mutex
	output    io.Writer
	collector *turboSummaryPathCollector
	pending   []byte
}

func (writer *turboSummaryObservingWriter) Write(data []byte) (int, error) {
	written, err := writer.output.Write(data)
	if written == 0 {
		return written, err
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	writer.pending = append(writer.pending, data[:written]...)
	for {
		newline := bytes.IndexByte(writer.pending, '\n')
		if newline < 0 {
			break
		}
		writer.collector.addLine(writer.pending[:newline])
		writer.pending = writer.pending[newline+1:]
	}
	if len(writer.pending) > maximumTurboSummaryOutputLineBytes {
		writer.pending = append([]byte(nil), writer.pending[len(writer.pending)-maximumTurboSummaryOutputLineBytes:]...)
	}
	return written, err
}

func (writer *turboSummaryObservingWriter) flush() {
	writer.mu.Lock()
	pending := append([]byte(nil), writer.pending...)
	writer.pending = nil
	writer.mu.Unlock()
	if len(pending) > 0 {
		writer.collector.addLine(pending)
	}
}

func stripTerminalControlSequences(value string) string {
	result := make([]byte, 0, len(value))
	for index := 0; index < len(value); {
		if value[index] != 0x1b || index+1 >= len(value) || value[index+1] != '[' {
			result = append(result, value[index])
			index++
			continue
		}
		index += 2
		for index < len(value) {
			character := value[index]
			index++
			if character >= 0x40 && character <= 0x7e {
				break
			}
		}
	}
	return string(result)
}

func reconcileTurboSummaries(
	ctx context.Context,
	cfg config.Config,
	runID string,
	workspaceID string,
	startedAt time.Time,
	finishedAt time.Time,
	documents [][]byte,
) (int, error) {
	type summaryRepository interface {
		ReconcileTurboSummaries(string, measurement.TurboReconcileOptions, [][]byte) (int, error)
		Close() error
	}
	var repository summaryRepository
	var err error
	if cfg.CloudPostgresURL != "" {
		repository, err = measurement.OpenPostgresRepository(ctx, cfg.CloudPostgresURL, cfg.ProjectID)
	} else {
		repository, err = measurement.OpenSQLiteRepository(filepath.Join(cfg.DataDir, "measurements.db"))
	}
	if err != nil {
		return 0, err
	}
	count, reconcileErr := repository.ReconcileTurboSummaries(runID, measurement.TurboReconcileOptions{
		Project:         cfg.ProjectID,
		WorkspaceID:     workspaceID,
		CompatibilityID: cfg.CompatibilityID,
		RunStartedAt:    startedAt,
		RunFinishedAt:   finishedAt,
	}, documents)
	return count, errors.Join(reconcileErr, repository.Close())
}

func workspaceMeasurementIdentity(projectID string, roots ...string) string {
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		absolute, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		if evaluated, evaluateErr := filepath.EvalSymlinks(absolute); evaluateErr == nil {
			absolute = evaluated
		}
		return measurement.WorkspaceIdentity(projectID, filepath.Clean(absolute))
	}
	if workingDirectory, err := os.Getwd(); err == nil {
		return measurement.WorkspaceIdentity(projectID, filepath.Clean(workingDirectory))
	}
	return ""
}

func replaceEnvironment(environment []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if !strings.HasPrefix(item, prefix) {
			result = append(result, item)
		}
	}
	if value != "" {
		result = append(result, prefix+value)
	}
	return result
}
