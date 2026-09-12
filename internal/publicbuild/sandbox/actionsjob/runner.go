//go:build linux

package actionsjob

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/layercache/layercache/internal/publicbuild"
	"go.yaml.in/yaml/v3"
	"golang.org/x/sys/unix"
)

const (
	cacheVersionSalt      = "1.0"
	compressionGzip       = "gzip"
	compressionZstd       = "zstd-without-long"
	maximumWorkflowBytes  = 1 << 20
	maximumArchiveEntries = 32_768
)

var (
	immutableActionRef = regexp.MustCompile(`^[0-9a-f]{40}(?:[0-9a-f]{24})?$`)
	environmentName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type Request struct {
	Source           string `json:"source"`
	Target           string `json:"target"`
	Platform         string `json:"platform"`
	Repository       string `json:"repository"`
	Commit           string `json:"commit"`
	RecipeDigest     string `json:"recipeDigest"`
	Compatibility    string `json:"compatibility"`
	Key              string `json:"key"`
	Ref              string `json:"ref"`
	Version          string `json:"version"`
	Toolchain        string `json:"toolchain"`
	Builder          string `json:"builder"`
	MaxArchiveBytes  int64  `json:"maxArchiveBytes"`
	MaxExpandedBytes int64  `json:"maxExpandedBytes"`
	MaxProcesses     int64  `json:"maxProcesses"`
}

type EnvironmentKV struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Result struct {
	ArchivePath string
	Paths       []string
	Compression string
	SizeBytes   int64
}

type workflowJob struct {
	steps       []workflowStep
	paths       []declaredPath
	compression string
	environment []EnvironmentKV
}

type declaredPath struct {
	version string
	clean   string
}

type workflowStep struct {
	name             string
	run              string
	shell            string
	workingDirectory string
	environment      []EnvironmentKV
}

// Run executes the maintained offline workflow subset and writes archive.bin
// below outputDirectory. The caller supplies process, memory, disk, and time
// limits at the microVM seam.
func Run(ctx context.Context, request Request, outputDirectory string) (Result, error) {
	if os.Geteuid() != 0 {
		return Result{}, errors.New("maintained Actions runner must start as root")
	}
	if err := validateRequest(request, outputDirectory); err != nil {
		return Result{}, err
	}
	if err := validateProductionPaths(request.Source, outputDirectory); err != nil {
		return Result{}, err
	}
	if err := applyProcessLimits(request.MaxProcesses); err != nil {
		return Result{}, err
	}
	return run(ctx, request, outputDirectory, uint32(65534), uint32(65534))
}

func validateProductionPaths(source, outputDirectory string) error {
	parent := filepath.Dir(source)
	if filepath.Dir(outputDirectory) != parent {
		return errors.New("Actions source and output must share one private work root")
	}
	for label, path := range map[string]string{"work root": parent, "output": outputDirectory} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("Actions %s must be a protected real directory", label)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != os.Geteuid() {
			return fmt.Errorf("Actions %s must be owned by the runner", label)
		}
	}
	entries, err := os.ReadDir(outputDirectory)
	if err != nil || len(entries) != 0 {
		return errors.New("Actions output directory must start empty")
	}
	return nil
}

func run(ctx context.Context, request Request, outputDirectory string, runUID, runGID uint32) (Result, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := validateRequest(request, outputDirectory); err != nil {
		return Result{}, err
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return Result{}, fmt.Errorf("enable Actions child-process cleanup: %w", err)
	}
	job, err := loadWorkflowJob(request)
	if err != nil {
		return Result{}, err
	}
	if err := prepareSourceOwnership(request.Source, runUID, runGID); err != nil {
		return Result{}, err
	}
	homeDirectory, temporaryDirectory, cleanupRuntime, err := prepareRuntimeDirectories(
		request.Source,
		runUID,
		runGID,
	)
	if err != nil {
		return Result{}, err
	}
	defer cleanupRuntime()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return Result{}, fmt.Errorf("disable Actions privilege gains: %w", err)
	}
	baseEnvironment, err := baseEnvironment(request, job.environment, homeDirectory, temporaryDirectory)
	if err != nil {
		return Result{}, err
	}
	for index, step := range job.steps {
		if err := executeStep(
			ctx,
			request.Source,
			baseEnvironment,
			step,
			temporaryDirectory,
			runUID,
			runGID,
		); err != nil {
			return Result{}, fmt.Errorf("Actions run step %d %q failed: %w", index+1, step.name, err)
		}
	}
	if err := killRunUIDProcesses(runUID); err != nil {
		return Result{}, err
	}
	paths, err := collectArchivePaths(request.Source, job.paths, request.MaxExpandedBytes)
	if err != nil {
		return Result{}, err
	}
	archivePath := filepath.Join(outputDirectory, "archive.bin")
	if err := writeArchive(
		archivePath,
		request.Source,
		paths,
		job.compression,
		request.MaxArchiveBytes,
		request.MaxExpandedBytes,
	); err != nil {
		return Result{}, err
	}
	info, err := os.Lstat(archivePath)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > request.MaxArchiveBytes {
		return Result{}, errors.New("Actions archive is not a bounded regular file")
	}
	return Result{
		ArchivePath: archivePath, Paths: versionPaths(job.paths),
		Compression: job.compression, SizeBytes: info.Size(),
	}, nil
}

func killRunUIDProcesses(uid uint32) error {
	if os.Geteuid() != 0 || uid == 0 || int(uid) == os.Geteuid() {
		return nil
	}
	for attempt := 0; attempt < 32; attempt++ {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return fmt.Errorf("inspect Actions sandbox processes: %w", err)
		}
		found := false
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil || pid <= 1 || pid == os.Getpid() {
				continue
			}
			status, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
			if err != nil {
				continue
			}
			matches, zombie := processStatusUID(status, uid)
			if !matches || zombie {
				continue
			}
			found = true
			if err := unix.Kill(pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
				return fmt.Errorf("kill Actions sandbox process %d: %w", pid, err)
			}
		}
		reapChildren()
		if !found {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return errors.New("Actions run steps left sandbox processes alive before archive collection")
}

func processStatusUID(status []byte, uid uint32) (matches, zombie bool) {
	uidPrefix := "Uid:\t" + strconv.FormatUint(uint64(uid), 10) + "\t"
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, uidPrefix) {
			matches = true
		}
		if strings.HasPrefix(line, "State:\tZ") {
			zombie = true
		}
	}
	return matches, zombie
}

func validateRequest(request Request, outputDirectory string) error {
	for label, value := range map[string]string{
		"source": request.Source, "target": request.Target, "platform": request.Platform,
		"repository": request.Repository, "commit": request.Commit, "recipe digest": request.RecipeDigest,
		"compatibility": request.Compatibility, "key": request.Key, "ref": request.Ref,
		"version": request.Version, "toolchain": request.Toolchain, "builder": request.Builder,
		"output": outputDirectory,
	} {
		if strings.TrimSpace(value) != value || value == "" || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("Actions job %s is empty or invalid", label)
		}
	}
	if request.MaxArchiveBytes <= 0 || request.MaxArchiveBytes > 10<<30 ||
		request.MaxExpandedBytes <= 0 || request.MaxExpandedBytes > 100<<30 {
		return errors.New("Actions job byte limits must be positive")
	}
	if request.MaxProcesses < 2 || request.MaxProcesses > 4096 {
		return errors.New("Actions job process limit must be between 2 and 4096")
	}
	if request.Platform != "linux/amd64" && request.Platform != "linux/arm64" {
		return errors.New("Actions job platform is unsupported")
	}
	if _, _, err := publicbuild.ParseActionsWorkflowTarget(request.Target); err != nil {
		return err
	}
	repository := strings.TrimPrefix(request.Repository, "https://github.com/")
	if repository == request.Repository || strings.Count(repository, "/") != 1 ||
		strings.ContainsAny(repository, "?#@") ||
		!immutableActionRef.MatchString(request.Commit) ||
		!strings.HasPrefix(request.RecipeDigest, "sha256:") || len(request.RecipeDigest) != 71 ||
		len(request.Version) != 64 || request.Version != strings.ToLower(request.Version) {
		return errors.New("Actions job source or cache identity is invalid")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(request.RecipeDigest, "sha256:")); err != nil {
		return errors.New("Actions recipe digest must be a SHA-256 digest")
	}
	if _, err := hex.DecodeString(request.Version); err != nil {
		return errors.New("Actions cache version must be a SHA-256 hex digest")
	}
	if info, err := os.Lstat(request.Source); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Actions job source must be a real directory")
	}
	if info, err := os.Lstat(outputDirectory); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Actions job output must be a real directory")
	}
	return nil
}

func loadWorkflowJob(request Request) (workflowJob, error) {
	workflowName, jobName, err := publicbuild.ParseActionsWorkflowTarget(request.Target)
	if err != nil {
		return workflowJob{}, err
	}
	workflowPath := filepath.Join(request.Source, filepath.FromSlash(workflowName))
	info, err := os.Lstat(workflowPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximumWorkflowBytes {
		return workflowJob{}, fmt.Errorf("workflow %q is not a bounded regular file", workflowName)
	}
	encoded, err := os.ReadFile(workflowPath)
	if err != nil {
		return workflowJob{}, err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(encoded, &document); err != nil {
		return workflowJob{}, fmt.Errorf("parse workflow %q: %w", workflowName, err)
	}
	root, err := documentRoot(&document)
	if err != nil {
		return workflowJob{}, fmt.Errorf("workflow %q: %w", workflowName, err)
	}
	rootValues, err := mapping(root)
	if err != nil {
		return workflowJob{}, fmt.Errorf("workflow %q root: %w", workflowName, err)
	}
	if rootValues["env"] != nil || rootValues["defaults"] != nil {
		return workflowJob{}, errors.New("workflow-level env and defaults are outside the maintained subset")
	}
	if err := parsePermissions(rootValues["permissions"]); err != nil {
		return workflowJob{}, fmt.Errorf("workflow permissions: %w", err)
	}
	selected, found, err := mappingValue(rootValues["jobs"], jobName)
	if err != nil {
		return workflowJob{}, fmt.Errorf("workflow %q target: %w", workflowName, err)
	}
	if !found {
		return workflowJob{}, fmt.Errorf("Actions target job %q was not found in %s", jobName, workflowName)
	}
	return parseSelectedJob(selected, request)
}

func parseSelectedJob(node *yaml.Node, request Request) (workflowJob, error) {
	values, err := mapping(node)
	if err != nil {
		return workflowJob{}, err
	}
	for key := range values {
		switch key {
		case "name", "runs-on", "steps", "env", "permissions":
		default:
			return workflowJob{}, fmt.Errorf("Actions target job field %q is outside the maintained subset", key)
		}
	}
	if err := parsePermissions(values["permissions"]); err != nil {
		return workflowJob{}, fmt.Errorf("Actions job permissions: %w", err)
	}
	runsOn, err := literalScalar(values["runs-on"], "runs-on")
	if err != nil || !supportedRunnerLabel(runsOn, request.Platform) {
		return workflowJob{}, errors.New("Actions target job must use one supported literal Linux runner label")
	}
	jobEnvironment, err := parseEnvironment(values["env"])
	if err != nil {
		return workflowJob{}, fmt.Errorf("Actions job env: %w", err)
	}
	stepsNode := values["steps"]
	if stepsNode == nil || stepsNode.Kind != yaml.SequenceNode || len(stepsNode.Content) == 0 {
		return workflowJob{}, errors.New("Actions target job must contain steps")
	}
	job := workflowJob{environment: jobEnvironment}
	checkoutSeen := false
	for index, stepNode := range stepsNode.Content {
		stepValues, err := mapping(stepNode)
		if err != nil {
			return workflowJob{}, fmt.Errorf("Actions step %d: %w", index+1, err)
		}
		_, hasRun := stepValues["run"]
		_, hasUses := stepValues["uses"]
		if hasRun == hasUses {
			return workflowJob{}, fmt.Errorf("Actions step %d must contain exactly one of run or uses", index+1)
		}
		if hasUses {
			uses, err := literalScalar(stepValues["uses"], "uses")
			if err != nil {
				return workflowJob{}, err
			}
			if strings.HasPrefix(uses, "actions/checkout@") {
				if checkoutSeen || len(job.steps) != 0 {
					return workflowJob{}, errors.New("immutable actions/checkout may appear once before all run steps")
				}
				if err := parseCheckoutStep(stepValues); err != nil {
					return workflowJob{}, err
				}
				checkoutSeen = true
				continue
			}
			if index != len(stepsNode.Content)-1 {
				return workflowJob{}, errors.New("Layer Cache action must be the final step in the maintained job subset")
			}
			paths, compression, err := parseCacheStep(stepValues, request)
			if err != nil {
				return workflowJob{}, err
			}
			job.paths = paths
			job.compression = compression
			continue
		}
		step, err := parseRunStep(stepValues)
		if err != nil {
			return workflowJob{}, fmt.Errorf("Actions run step %d: %w", index+1, err)
		}
		job.steps = append(job.steps, step)
	}
	if len(job.paths) == 0 || job.compression == "" {
		return workflowJob{}, errors.New("Actions target job has no final Layer Cache action")
	}
	return job, nil
}

func parsePermissions(node *yaml.Node) error {
	if node == nil {
		return nil
	}
	values, err := mapping(node)
	if err != nil {
		return errors.New("permissions must be a literal mapping")
	}
	for name, node := range values {
		value, err := literalScalar(node, "permissions."+name)
		if err != nil {
			return err
		}
		if name == "contents" && value == "read" || name == "id-token" && value == "write" {
			continue
		}
		return fmt.Errorf("permission %s=%s is outside the credential-free subset", name, value)
	}
	return nil
}

func parseCheckoutStep(values map[string]*yaml.Node) error {
	for key := range values {
		switch key {
		case "name", "uses", "with":
		default:
			return fmt.Errorf("actions/checkout step field %q is unsupported", key)
		}
	}
	uses, err := literalScalar(values["uses"], "uses")
	if err != nil {
		return err
	}
	repository, reference, found := strings.Cut(uses, "@")
	if !found || repository != "actions/checkout" || !immutableActionRef.MatchString(reference) {
		return errors.New("maintained job must use actions/checkout at an immutable commit")
	}
	if values["with"] == nil {
		return nil
	}
	with, err := mapping(values["with"])
	if err != nil {
		return fmt.Errorf("actions/checkout with: %w", err)
	}
	if len(with) != 1 || with["persist-credentials"] == nil {
		return errors.New("actions/checkout accepts only literal persist-credentials: false")
	}
	persist := with["persist-credentials"]
	if persist.Kind != yaml.ScalarNode ||
		persist.Value != "false" ||
		persist.Tag != "!!bool" && persist.Tag != "!!str" ||
		strings.Contains(persist.Value, "${{") {
		return errors.New("actions/checkout persist-credentials must be literal false")
	}
	return nil
}

func parseRunStep(values map[string]*yaml.Node) (workflowStep, error) {
	for key := range values {
		switch key {
		case "name", "run", "shell", "working-directory", "env":
		default:
			return workflowStep{}, fmt.Errorf("field %q is outside the maintained run-step subset", key)
		}
	}
	run, err := literalScalar(values["run"], "run")
	if err != nil || strings.TrimSpace(run) == "" {
		return workflowStep{}, errors.New("run must be a non-empty literal script")
	}
	name := "run"
	if values["name"] != nil {
		name, err = literalScalar(values["name"], "name")
		if err != nil {
			return workflowStep{}, err
		}
	}
	shell := ""
	if values["shell"] != nil {
		shell, err = literalScalar(values["shell"], "shell")
		if err != nil || shell != "sh" && shell != "bash" {
			return workflowStep{}, errors.New("shell must be literal sh or bash")
		}
	}
	workingDirectory := "."
	if values["working-directory"] != nil {
		workingDirectory, err = safeRelativePathScalar(values["working-directory"], "working-directory")
		if err != nil {
			return workflowStep{}, err
		}
	}
	environment, err := parseEnvironment(values["env"])
	if err != nil {
		return workflowStep{}, err
	}
	return workflowStep{
		name: name, run: run, shell: shell,
		workingDirectory: workingDirectory, environment: environment,
	}, nil
}

func parseCacheStep(values map[string]*yaml.Node, request Request) ([]declaredPath, string, error) {
	for key := range values {
		switch key {
		case "name", "uses", "with":
		default:
			return nil, "", fmt.Errorf("Layer Cache step field %q is unsupported", key)
		}
	}
	uses, err := literalScalar(values["uses"], "uses")
	if err != nil {
		return nil, "", err
	}
	repository, reference, found := strings.Cut(uses, "@")
	if !found || repository != "layercache/layercache/action/cache" || !immutableActionRef.MatchString(reference) {
		return nil, "", errors.New("maintained job must use Layer Cache action at an immutable commit")
	}
	with, err := mapping(values["with"])
	if err != nil {
		return nil, "", fmt.Errorf("Layer Cache with: %w", err)
	}
	allowed := map[string]struct{}{
		"path": {}, "key": {}, "endpoint": {}, "project": {}, "compatibility": {},
		"public-cache-mode": {}, "public-trust-key": {}, "public-recipe-digest": {},
		"public-platform": {}, "public-toolchain": {}, "public-builder": {},
		"lookup-timeout-seconds": {},
	}
	for key, value := range with {
		if _, ok := allowed[key]; !ok {
			return nil, "", fmt.Errorf("Layer Cache input %q is outside the maintained Public Build subset", key)
		}
		if _, err := literalScalar(value, "with."+key); err != nil {
			return nil, "", err
		}
	}
	key, err := literalScalar(with["key"], "with.key")
	if err != nil || key != request.Key {
		return nil, "", errors.New("Layer Cache action key does not match actions.key")
	}
	pathsValue, err := literalScalar(with["path"], "with.path")
	if err != nil {
		return nil, "", err
	}
	paths, err := parseDeclaredPaths(pathsValue)
	if err != nil {
		return nil, "", err
	}
	for input, want := range map[string]string{
		"compatibility":        request.Compatibility,
		"public-cache-mode":    "verified",
		"public-recipe-digest": request.RecipeDigest,
		"public-platform":      request.Platform,
		"public-toolchain":     request.Toolchain,
		"public-builder":       request.Builder,
	} {
		got, err := literalScalar(with[input], "with."+input)
		if err != nil || got != want {
			return nil, "", fmt.Errorf("Layer Cache action input %s does not match the Public Build", input)
		}
	}
	compression := ""
	for _, candidate := range []string{compressionZstd, compressionGzip} {
		if CacheVersion(versionPaths(paths), candidate) == request.Version {
			compression = candidate
			break
		}
	}
	if compression == "" {
		return nil, "", errors.New("actions.version does not match the declared paths and supported compression")
	}
	return paths, compression, nil
}

func parseDeclaredPaths(value string) ([]declaredPath, error) {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	paths := make([]declaredPath, 0, len(lines))
	seen := make(map[string]struct{})
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		cleaned, err := safeRelativePath(line)
		if err != nil || strings.HasPrefix(line, "!") || strings.ContainsAny(line, "*?[{") {
			return nil, fmt.Errorf("Layer Cache path %q must be a literal relative path", line)
		}
		if _, duplicate := seen[cleaned]; duplicate {
			return nil, fmt.Errorf("Layer Cache path %q is duplicated", cleaned)
		}
		for existing := range seen {
			if strings.HasPrefix(cleaned, existing+"/") || strings.HasPrefix(existing, cleaned+"/") {
				return nil, errors.New("Layer Cache paths cannot overlap")
			}
		}
		seen[cleaned] = struct{}{}
		paths = append(paths, declaredPath{version: line, clean: cleaned})
	}
	if len(paths) == 0 || len(paths) > 32 {
		return nil, errors.New("Layer Cache action must declare between one and 32 literal paths")
	}
	return paths, nil
}

func versionPaths(paths []declaredPath) []string {
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		result = append(result, path.version)
	}
	return result
}

// CacheVersion matches @actions/cache v1 getCacheVersion on Linux.
func CacheVersion(paths []string, compression string) string {
	components := append([]string(nil), paths...)
	components = append(components, compression, cacheVersionSalt)
	digest := sha256.Sum256([]byte(strings.Join(components, "|")))
	return hex.EncodeToString(digest[:])
}

func executeStep(
	ctx context.Context,
	source string,
	base []EnvironmentKV,
	step workflowStep,
	temporaryDirectory string,
	runUID uint32,
	runGID uint32,
) error {
	directory := filepath.Join(source, filepath.FromSlash(step.workingDirectory))
	if err := verifyDirectoryInside(source, directory); err != nil {
		return err
	}
	script, err := os.CreateTemp(temporaryDirectory, "step-*.sh")
	if err != nil {
		return fmt.Errorf("create Actions run script: %w", err)
	}
	scriptPath := script.Name()
	defer os.Remove(scriptPath)
	if err := errors.Join(script.Chown(int(runUID), int(runGID)), script.Chmod(0o700)); err != nil {
		_ = script.Close()
		return fmt.Errorf("prepare Actions run script: %w", err)
	}
	if _, err := io.WriteString(script, step.run+"\n"); err != nil {
		_ = script.Close()
		return fmt.Errorf("write Actions run script: %w", err)
	}
	if err := script.Close(); err != nil {
		return fmt.Errorf("close Actions run script: %w", err)
	}
	var command *exec.Cmd
	switch step.shell {
	case "":
		command = exec.CommandContext(ctx, "/bin/bash", "-e", scriptPath)
	case "bash":
		command = exec.CommandContext(ctx, "/bin/bash", "--noprofile", "--norc", "-e", "-o", "pipefail", scriptPath)
	case "sh":
		command = exec.CommandContext(ctx, "/bin/dash", "-e", scriptPath)
	default:
		return fmt.Errorf("unsupported Actions shell %q", step.shell)
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if os.Geteuid() == 0 {
		command.SysProcAttr.Credential = &syscall.Credential{Uid: runUID, Gid: runGID}
	} else if int(runUID) != os.Geteuid() || int(runGID) != os.Getegid() {
		return errors.New("unprivileged Actions test runner cannot change process credentials")
	}
	command.WaitDelay = time.Second
	command.Dir = directory
	environment := append(append([]EnvironmentKV(nil), base...), step.environment...)
	if _, err := validatedEnvironment(environment); err != nil {
		return fmt.Errorf("step env: %w", err)
	}
	command.Env = encodeEnvironment(environment)
	log := &limitedOutput{remaining: 256 << 10}
	command.Stdout = log
	command.Stderr = log
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		return killProcessGroup(command.Process.Pid)
	}
	if err := command.Start(); err != nil {
		return err
	}
	waitErr := command.Wait()
	groupErr := killProcessGroup(command.Process.Pid)
	childrenErr := killDescendants(os.Getpid())
	if waitErr != nil || groupErr != nil || childrenErr != nil {
		return fmt.Errorf("%w: %s", errors.Join(waitErr, groupErr, childrenErr), log.String())
	}
	return nil
}

func baseEnvironment(
	request Request,
	job []EnvironmentKV,
	homeDirectory string,
	temporaryDirectory string,
) ([]EnvironmentKV, error) {
	workflowPath, jobName, err := publicbuild.ParseActionsWorkflowTarget(request.Target)
	if err != nil {
		return nil, err
	}
	repository := strings.TrimPrefix(request.Repository, "https://github.com/")
	environment := []EnvironmentKV{
		{Name: "CI", Value: "true"},
		{Name: "GITHUB_ACTIONS", Value: "true"},
		{Name: "GITHUB_JOB", Value: jobName},
		{Name: "GITHUB_REF", Value: request.Ref},
		{Name: "GITHUB_REPOSITORY", Value: repository},
		{Name: "GITHUB_SHA", Value: request.Commit},
		{Name: "GITHUB_WORKFLOW_REF", Value: repository + "/" + workflowPath + "@" + request.Ref},
		{Name: "GITHUB_WORKSPACE", Value: request.Source},
		{Name: "HOME", Value: homeDirectory},
		{Name: "LANG", Value: "C"},
		{Name: "LC_ALL", Value: "C"},
		{Name: "PATH", Value: "/opt/layercache/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		{Name: "RUNNER_OS", Value: "Linux"},
		{Name: "TMPDIR", Value: temporaryDirectory},
	}
	if request.Platform == "linux/amd64" {
		environment = append(environment, EnvironmentKV{Name: "RUNNER_ARCH", Value: "X64"})
	} else {
		environment = append(environment, EnvironmentKV{Name: "RUNNER_ARCH", Value: "ARM64"})
	}
	environment = append(environment, job...)
	if _, err := validatedEnvironment(environment); err != nil {
		return nil, err
	}
	return environment, nil
}

func applyProcessLimits(maximum int64) error {
	processes := unix.Rlimit{Cur: uint64(maximum), Max: uint64(maximum)}
	if err := unix.Setrlimit(unix.RLIMIT_NPROC, &processes); err != nil {
		return fmt.Errorf("apply Actions process limit: %w", err)
	}
	cores := unix.Rlimit{}
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &cores); err != nil {
		return fmt.Errorf("disable Actions core dumps: %w", err)
	}
	return nil
}

func prepareRuntimeDirectories(source string, runUID, runGID uint32) (string, string, func(), error) {
	root := filepath.Dir(source)
	home := filepath.Join(root, "actions-home")
	temporary := filepath.Join(root, "actions-tmp")
	cleanup := func() {
		_ = os.RemoveAll(home)
		_ = os.RemoveAll(temporary)
	}
	for _, directory := range []string{home, temporary} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			cleanup()
			return "", "", func() {}, fmt.Errorf("create Actions runtime directory: %w", err)
		}
		if err := os.Chown(directory, int(runUID), int(runGID)); err != nil {
			cleanup()
			return "", "", func() {}, fmt.Errorf("assign Actions runtime directory: %w", err)
		}
	}
	return home, temporary, cleanup, nil
}

func prepareSourceOwnership(source string, runUID, runGID uint32) error {
	return filepath.Walk(source, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if err := os.Lchown(path, int(runUID), int(runGID)); err != nil {
				return fmt.Errorf("assign Actions source symlink %q to the sandbox user: %w", path, err)
			}
			return nil
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("Actions source path %q is not a regular file or directory", path)
		}
		if err := os.Chown(path, int(runUID), int(runGID)); err != nil {
			return fmt.Errorf("assign Actions source path %q to the sandbox user: %w", path, err)
		}
		return nil
	})
}

func parseEnvironment(node *yaml.Node) ([]EnvironmentKV, error) {
	if node == nil {
		return nil, nil
	}
	values, err := mapping(node)
	if err != nil {
		return nil, err
	}
	result := make([]EnvironmentKV, 0, len(values))
	for name, node := range values {
		value, err := literalScalar(node, "env."+name)
		if err != nil {
			return nil, err
		}
		result = append(result, EnvironmentKV{Name: name, Value: value})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Name < result[right].Name })
	_, err = validatedEnvironment(result)
	return result, err
}

func validatedEnvironment(values []EnvironmentKV) (map[string]string, error) {
	result := make(map[string]string, len(values))
	for _, value := range values {
		upper := strings.ToUpper(value.Name)
		if !environmentName.MatchString(value.Name) || strings.ContainsAny(value.Value, "\x00\r\n") ||
			strings.Contains(value.Value, "${{") || strings.Contains(upper, "TOKEN") ||
			strings.Contains(upper, "SECRET") || strings.Contains(upper, "PASSWORD") ||
			strings.Contains(upper, "CREDENTIAL") || strings.Contains(upper, "AUTH") {
			return nil, fmt.Errorf("environment variable %q is outside the credential-free subset", value.Name)
		}
		if _, duplicate := result[value.Name]; duplicate {
			return nil, fmt.Errorf("environment variable %q is duplicated", value.Name)
		}
		result[value.Name] = value.Value
	}
	return result, nil
}

func encodeEnvironment(values []EnvironmentKV) []string {
	encoded := make([]string, 0, len(values))
	for _, value := range values {
		encoded = append(encoded, value.Name+"="+value.Value)
	}
	return encoded
}

type archivePath struct {
	absolute            string
	relative            string
	info                os.FileInfo
	linkTarget          string
	canonicalLinkTarget string
}

func collectArchivePaths(source string, declared []declaredPath, maximumBytes int64) ([]archivePath, error) {
	result := make([]archivePath, 0)
	seen := make(map[string]archivePath)
	var bytesUsed int64
	for _, declaredPath := range declared {
		absolute := filepath.Join(source, filepath.FromSlash(declaredPath.clean))
		if err := verifyPathInside(source, absolute); err != nil {
			return nil, err
		}
		err := filepath.Walk(absolute, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if len(result) >= maximumArchiveEntries {
				return fmt.Errorf("Actions output exceeds %d archive entries", maximumArchiveEntries)
			}
			if info.Mode()&os.ModeSymlink == 0 && !info.IsDir() && !info.Mode().IsRegular() {
				return fmt.Errorf("Actions output path %q is not a regular file or directory", path)
			}
			relative, err := filepath.Rel(source, path)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			if _, err := safeRelativePath(relative); err != nil || len(relative) > 4096 {
				return fmt.Errorf("Actions output path %q is unsafe for verified restore", relative)
			}
			if _, duplicate := seen[relative]; duplicate {
				return fmt.Errorf("Actions output repeats archive path %q", relative)
			}
			entry := archivePath{absolute: path, relative: relative, info: info}
			if info.Mode()&os.ModeSymlink != 0 {
				linkTarget, err := os.Readlink(path)
				if err != nil {
					return err
				}
				canonicalTarget, err := canonicalArchiveSymlinkTarget(relative, linkTarget)
				if err != nil {
					return fmt.Errorf("Actions output symlink %q: %w", relative, err)
				}
				entry.linkTarget = linkTarget
				entry.canonicalLinkTarget = canonicalTarget
			}
			seen[relative] = entry
			if info.Mode().IsRegular() {
				if info.Size() < 0 || bytesUsed > maximumBytes-info.Size() {
					return fmt.Errorf("Actions output exceeds %d expanded bytes", maximumBytes)
				}
				bytesUsed += info.Size()
			}
			result = append(result, entry)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	for _, entry := range result {
		if entry.linkTarget == "" {
			continue
		}
		target, ok := seen[entry.canonicalLinkTarget]
		if !ok || target.info.Mode()&os.ModeSymlink != 0 ||
			!target.info.IsDir() && !target.info.Mode().IsRegular() {
			return nil, fmt.Errorf(
				"Actions output symlink %q must target another archived regular file or directory",
				entry.relative,
			)
		}
	}
	sort.Slice(result, func(left, right int) bool { return result[left].relative < result[right].relative })
	return result, nil
}

func canonicalArchiveSymlinkTarget(relative, linkTarget string) (string, error) {
	if linkTarget == "" || len(linkTarget) > 4096 || strings.ContainsAny(linkTarget, "\\\x00") ||
		path.IsAbs(linkTarget) || strings.IndexFunc(linkTarget, func(character rune) bool {
		return character < 0x20 || character == 0x7f
	}) >= 0 {
		return "", errors.New("link target must be one safe relative path")
	}
	canonical := path.Clean(path.Join(path.Dir(relative), filepath.ToSlash(linkTarget)))
	if canonical == "." || canonical == ".." || strings.HasPrefix(canonical, "../") || len(canonical) > 4096 {
		return "", errors.New("link target must stay inside the workspace")
	}
	return canonical, nil
}

func writeArchive(
	destination string,
	source string,
	paths []archivePath,
	compression string,
	maximumBytes int64,
	maximumExpandedBytes int64,
) error {
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	limited := &maximumWriter{writer: file, remaining: maximumBytes}
	var compressed io.WriteCloser
	switch compression {
	case compressionGzip:
		writer := gzip.NewWriter(limited)
		writer.Header.ModTime = time.Unix(0, 0)
		compressed = writer
	case compressionZstd:
		compressed, err = zstd.NewWriter(limited, zstd.WithEncoderConcurrency(1))
	default:
		err = errors.New("unsupported Actions archive compression")
	}
	if err != nil {
		file.Close()
		return err
	}
	uncompressed := &countingWriter{writer: compressed}
	archive := tar.NewWriter(uncompressed)
	rootDescriptor, err := unix.Open(source, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = archive.Close()
		_ = compressed.Close()
		_ = file.Close()
		return fmt.Errorf("open Actions source root: %w", err)
	}
	defer unix.Close(rootDescriptor)
	var expandedBytes int64
	for _, entry := range paths {
		current, err := os.Lstat(entry.absolute)
		if err != nil || !os.SameFile(entry.info, current) {
			_ = file.Close()
			return fmt.Errorf("Actions output %q changed during collection", entry.relative)
		}
		if current.Mode().IsRegular() {
			if current.Size() < 0 || expandedBytes > maximumExpandedBytes-current.Size() {
				_ = file.Close()
				return fmt.Errorf("Actions output exceeds %d expanded bytes", maximumExpandedBytes)
			}
			expandedBytes += current.Size()
		}
		header := &tar.Header{
			Name: entry.relative, Mode: int64(current.Mode().Perm()),
			Uid: 0, Gid: 0, ModTime: time.Unix(0, 0),
			AccessTime: time.Unix(0, 0), ChangeTime: time.Unix(0, 0),
		}
		if current.IsDir() {
			header.Typeflag = tar.TypeDir
		} else if current.Mode()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(entry.absolute)
			if err != nil || linkTarget != entry.linkTarget {
				_ = file.Close()
				return fmt.Errorf("Actions output symlink %q changed during collection", entry.relative)
			}
			canonicalTarget, err := canonicalArchiveSymlinkTarget(entry.relative, linkTarget)
			if err != nil || canonicalTarget != entry.canonicalLinkTarget {
				_ = file.Close()
				return fmt.Errorf("Actions output symlink %q changed its target", entry.relative)
			}
			header.Typeflag = tar.TypeSymlink
			header.Linkname = linkTarget
		} else {
			header.Typeflag = tar.TypeReg
			header.Size = current.Size()
		}
		if err := archive.WriteHeader(header); err != nil {
			file.Close()
			return fmt.Errorf("write Actions archive header: %w", err)
		}
		if current.Mode().IsRegular() {
			input, err := openArchiveFile(rootDescriptor, entry.relative)
			if err != nil {
				_ = file.Close()
				return err
			}
			openedInfo, statErr := input.Stat()
			if statErr != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(current, openedInfo) ||
				openedInfo.Size() != current.Size() {
				_ = input.Close()
				_ = file.Close()
				return fmt.Errorf("Actions output %q changed before it was opened", entry.relative)
			}
			_, copyErr := io.CopyN(archive, input, current.Size())
			closeErr := input.Close()
			if copyErr != nil || closeErr != nil {
				file.Close()
				return errors.Join(copyErr, closeErr)
			}
		}
	}
	if err := errors.Join(archive.Close(), compressed.Close(), file.Sync()); err != nil {
		_ = file.Close()
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	allowedExpanded := max(int64(1<<30), info.Size()*100)
	allowedExpanded = min(int64(100<<30), allowedExpanded)
	if uncompressed.written > allowedExpanded {
		_ = file.Close()
		return fmt.Errorf(
			"Actions archive expands to %d bytes, beyond the verified restore limit %d",
			uncompressed.written,
			allowedExpanded,
		)
	}
	return file.Close()
}

func openArchiveFile(rootDescriptor int, relative string) (*os.File, error) {
	how := &unix.OpenHow{
		Flags:   uint64(unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	descriptor, err := unix.Openat2(rootDescriptor, relative, how)
	if err != nil {
		return nil, fmt.Errorf("open Actions output %q without links: %w", relative, err)
	}
	return os.NewFile(uintptr(descriptor), relative), nil
}

func killProcessGroup(pid int) error {
	if pid <= 0 {
		return nil
	}
	err := unix.Kill(-pid, unix.SIGKILL)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

func killDescendants(parentPID int) error {
	for attempt := 0; attempt < 16; attempt++ {
		parents, err := processParents()
		if err != nil {
			return err
		}
		descendants := descendantPIDs(parentPID, parents)
		if len(descendants) == 0 {
			reapChildren()
			return nil
		}
		for _, pid := range descendants {
			if err := unix.Kill(pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
				return fmt.Errorf("kill Actions descendant %d: %w", pid, err)
			}
		}
		reapChildren()
	}
	return errors.New("Actions run step left processes running")
}

func reapChildren() {
	for {
		pid, err := unix.Wait4(-1, nil, unix.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
	}
}

func processParents() (map[int]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("inspect Actions processes: %w", err)
	}
	parents := make(map[int]int)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		encoded, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(encoded), "\n") {
			value, found := strings.CutPrefix(line, "PPid:\t")
			if !found {
				continue
			}
			parent, err := strconv.Atoi(strings.TrimSpace(value))
			if err == nil {
				parents[pid] = parent
			}
			break
		}
	}
	return parents, nil
}

func descendantPIDs(parentPID int, parents map[int]int) []int {
	marked := map[int]struct{}{parentPID: {}}
	for changed := true; changed; {
		changed = false
		for pid, parent := range parents {
			if _, already := marked[pid]; already {
				continue
			}
			if _, descends := marked[parent]; descends {
				marked[pid] = struct{}{}
				changed = true
			}
		}
	}
	delete(marked, parentPID)
	result := make([]int, 0, len(marked))
	for pid := range marked {
		result = append(result, pid)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(result)))
	return result
}

func verifyDirectoryInside(root, path string) error {
	if err := verifyPathInside(root, path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Actions working-directory is not a real directory")
	}
	return nil
}

func verifyPathInside(root, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("Actions path escapes the copied source")
	}
	current := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Actions path %q traverses a symlink", relative)
		}
	}
	return nil
}

func safeRelativePathScalar(node *yaml.Node, label string) (string, error) {
	value, err := literalScalar(node, label)
	if err != nil {
		return "", err
	}
	return safeRelativePath(value)
}

func safeRelativePath(value string) (string, error) {
	if value == "" || len(value) > 4096 || filepath.IsAbs(value) || strings.HasPrefix(value, "~") ||
		len(value) >= 2 && value[1] == ':' || strings.Contains(value, "\\") ||
		strings.Contains(value, "..") ||
		strings.IndexFunc(value, func(character rune) bool { return character < 0x20 || character == 0x7f }) >= 0 {
		return "", errors.New("path must be one literal relative path")
	}
	for _, component := range strings.Split(filepath.ToSlash(value), "/") {
		if component == ".." {
			return "", errors.New("path cannot contain parent traversal")
		}
	}
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("path must stay below the copied source")
	}
	return cleaned, nil
}

func supportedRunnerLabel(value, platform string) bool {
	if platform == "linux/arm64" {
		return value == "ubuntu-24.04-arm"
	}
	return value == "ubuntu-latest" || value == "ubuntu-24.04" || value == "ubuntu-22.04"
}

func literalScalar(node *yaml.Node, label string) (string, error) {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!str" && node.Tag != "" ||
		strings.Contains(node.Value, "${{") || strings.ContainsAny(node.Value, "\x00") {
		return "", fmt.Errorf("%s must be one literal string", label)
	}
	return node.Value, nil
}

func documentRoot(document *yaml.Node) (*yaml.Node, error) {
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("workflow root must be a mapping")
	}
	if hasAlias(document.Content[0]) {
		return nil, errors.New("workflow aliases are outside the maintained subset")
	}
	return document.Content[0], nil
}

func hasAlias(node *yaml.Node) bool {
	if node.Kind == yaml.AliasNode || node.Alias != nil {
		return true
	}
	for _, child := range node.Content {
		if hasAlias(child) {
			return true
		}
	}
	return false
}

func mappingValue(node *yaml.Node, key string) (*yaml.Node, bool, error) {
	values, err := mapping(node)
	if err != nil {
		return nil, false, err
	}
	value, found := values[key]
	return value, found, nil
}

func mapping(node *yaml.Node) (map[string]*yaml.Node, error) {
	if node == nil || node.Kind != yaml.MappingNode || len(node.Content)%2 != 0 {
		return nil, errors.New("value must be a mapping")
	}
	values := make(map[string]*yaml.Node, len(node.Content)/2)
	for index := 0; index < len(node.Content); index += 2 {
		key := node.Content[index]
		if key.Kind != yaml.ScalarNode || key.Value == "" {
			return nil, errors.New("mapping key must be a non-empty scalar")
		}
		if _, duplicate := values[key.Value]; duplicate {
			return nil, fmt.Errorf("mapping repeats key %q", key.Value)
		}
		values[key.Value] = node.Content[index+1]
	}
	return values, nil
}

type limitedOutput struct {
	builder   strings.Builder
	remaining int64
}

func (output *limitedOutput) Write(data []byte) (int, error) {
	length := len(data)
	if output.remaining > 0 {
		keep := min(int64(length), output.remaining)
		_, _ = output.builder.Write(data[:keep])
		output.remaining -= keep
	}
	return length, nil
}

func (output *limitedOutput) String() string {
	value := strings.TrimSpace(output.builder.String())
	if len(value) > 4096 {
		value = value[len(value)-4096:]
	}
	return value
}

type maximumWriter struct {
	writer    io.Writer
	remaining int64
}

type countingWriter struct {
	writer  io.Writer
	written int64
}

func (writer *countingWriter) Write(data []byte) (int, error) {
	written, err := writer.writer.Write(data)
	writer.written += int64(written)
	return written, err
}

func (writer *maximumWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > writer.remaining {
		return 0, errors.New("Actions archive exceeds its compressed byte limit")
	}
	written, err := writer.writer.Write(data)
	writer.remaining -= int64(written)
	return written, err
}
