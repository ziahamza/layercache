package buildkit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
)

const defaultMaxTeamImports = 4

var ociTagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
var platformPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)?$`)

type Output string

const (
	OutputLoad Output = "load"
	OutputPush Output = "push"
)

type Config struct {
	DockerCommand  string
	BuilderName    string
	TeamRepository string
	MaxTeamImports int
}

type PublicCache struct {
	Repository string
	Digest     string
}

type BuildRequest struct {
	ContextPath    string
	TargetPlatform string
	TeamImportTags []string
	TeamExportID   string
	PublicImports  []PublicCache
	Output         Output
	ExtraArgs      []string
}

type Command struct {
	Path string
	Args []string
}

type Plan struct {
	Command Command
}

type ExecutionResult struct {
	Plan            Plan            `json:"plan"`
	Metrics         ProgressMetrics `json:"metrics"`
	ProgressWarning string          `json:"progressWarning,omitempty"`
}

type Adapter struct {
	config Config
	runner CommandRunner
}

func New(config Config) (*Adapter, error) {
	return NewWithRunner(config, execCommandRunner{})
}

type CommandRunner interface {
	Run(context.Context, Command, io.Writer, io.Writer) error
}

func NewWithRunner(config Config, runner CommandRunner) (*Adapter, error) {
	if runner == nil {
		return nil, errors.New("command runner is required")
	}
	if config.DockerCommand == "" {
		config.DockerCommand = "docker"
	}
	if config.BuilderName == "" {
		return nil, errors.New("builder name is required")
	}
	if config.MaxTeamImports == 0 {
		config.MaxTeamImports = defaultMaxTeamImports
	}
	if config.MaxTeamImports < 0 {
		return nil, errors.New("maximum Team Cache imports cannot be negative")
	}
	config.TeamRepository = strings.TrimSuffix(config.TeamRepository, "/")
	if config.TeamRepository != "" {
		if err := validateCacheRepository("Team Cache", config.TeamRepository); err != nil {
			return nil, err
		}
	}
	return &Adapter{config: config, runner: runner}, nil
}

func (a *Adapter) EnsureBuilder(ctx context.Context, stdout, stderr io.Writer) error {
	stdout = writerOrDiscard(stdout)
	stderr = writerOrDiscard(stderr)
	list := Command{
		Path: a.config.DockerCommand,
		Args: []string{"buildx", "ls", "--format", "{{json .}}"},
	}
	var builders bytes.Buffer
	if err := a.runner.Run(ctx, list, &builders, io.Discard); err != nil {
		return fmt.Errorf("list BuildKit builders: %w", err)
	}
	driver, found, err := findBuilderDriver(&builders, a.config.BuilderName)
	if err != nil {
		return err
	}
	if !found {
		create := Command{
			Path: a.config.DockerCommand,
			Args: []string{"buildx", "create", "--name", a.config.BuilderName, "--driver", "docker-container", "--use"},
		}
		if err := a.runner.Run(ctx, create, stdout, stderr); err != nil {
			return fmt.Errorf("create BuildKit builder %q: %w", a.config.BuilderName, err)
		}
	} else if driver != "docker-container" {
		return fmt.Errorf("BuildKit builder %q uses driver %q, want %q", a.config.BuilderName, driver, "docker-container")
	}

	bootstrap := Command{
		Path: a.config.DockerCommand,
		Args: []string{"buildx", "inspect", a.config.BuilderName, "--bootstrap"},
	}
	if err := a.runner.Run(ctx, bootstrap, stdout, stderr); err != nil {
		return fmt.Errorf("bootstrap BuildKit builder %q: %w", a.config.BuilderName, err)
	}
	return nil
}

func findBuilderDriver(reader io.Reader, name string) (string, bool, error) {
	type builderRecord struct {
		Name   string `json:"Name"`
		Driver string `json:"Driver"`
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 16*1024), 1024*1024)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var record builderRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return "", false, fmt.Errorf("decode BuildKit builder list: %w", err)
		}
		if record.Name == name {
			return record.Driver, true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", false, fmt.Errorf("read BuildKit builder list: %w", err)
	}
	return "", false, nil
}

func (a *Adapter) Execute(ctx context.Context, request BuildRequest, stdout, stderr io.Writer) (ExecutionResult, error) {
	plan, err := a.Plan(request)
	if err != nil {
		return ExecutionResult{}, err
	}
	stdout = writerOrDiscard(stdout)
	stderr = writerOrDiscard(stderr)
	if err := a.EnsureBuilder(ctx, stdout, stderr); err != nil {
		return ExecutionResult{Plan: plan}, err
	}

	var progress bytes.Buffer
	runErr := a.runner.Run(ctx, plan.Command, stdout, io.MultiWriter(stderr, &progress))
	result := ExecutionResult{Plan: plan}
	metrics, progressErr := ParseProgress(&progress)
	if progressErr != nil {
		result.ProgressWarning = progressErr.Error()
	} else {
		result.Metrics = metrics
	}
	if runErr != nil {
		return result, fmt.Errorf("run BuildKit build: %w", runErr)
	}
	return result, nil
}

func (a *Adapter) Plan(request BuildRequest) (Plan, error) {
	if request.ContextPath == "" {
		return Plan{}, errors.New("build context path is required")
	}
	if request.TargetPlatform == "" {
		return Plan{}, errors.New("target platform is required")
	}
	if !platformPattern.MatchString(request.TargetPlatform) {
		return Plan{}, fmt.Errorf("target platform %q must be an os/architecture[/variant] value", request.TargetPlatform)
	}
	if request.Output != OutputLoad && request.Output != OutputPush {
		return Plan{}, fmt.Errorf("unsupported output %q", request.Output)
	}
	if len(request.TeamImportTags) > a.config.MaxTeamImports {
		return Plan{}, fmt.Errorf("use at most %d Team Cache imports", a.config.MaxTeamImports)
	}
	if (len(request.TeamImportTags) > 0 || request.TeamExportID != "") && a.config.TeamRepository == "" {
		return Plan{}, errors.New("Team Cache repository is required for Team Cache imports or publication")
	}
	for _, tag := range request.TeamImportTags {
		if !ociTagPattern.MatchString(tag) {
			return Plan{}, fmt.Errorf("Team Cache import %q must be a safe OCI tag", tag)
		}
	}
	if request.TeamExportID != "" && !ociTagPattern.MatchString(request.TeamExportID) {
		return Plan{}, fmt.Errorf("Team Cache export ID %q must be a safe OCI tag", request.TeamExportID)
	}
	if err := validateExtraArgs(request.ExtraArgs); err != nil {
		return Plan{}, err
	}

	args := []string{
		"buildx", "build",
		"--builder", a.config.BuilderName,
		"--platform", request.TargetPlatform,
		"--progress=rawjson",
	}
	platform := strings.ReplaceAll(request.TargetPlatform, "/", "-")
	teamRepository := a.config.TeamRepository + "/" + platform
	for _, tag := range request.TeamImportTags {
		args = append(args, "--cache-from", "type=registry,ref="+teamRepository+":"+tag)
	}
	for _, public := range request.PublicImports {
		if err := validatePublicCache(public); err != nil {
			return Plan{}, err
		}
		repository := strings.TrimSuffix(public.Repository, "/") + "/" + platform
		args = append(args, "--cache-from", "type=registry,ref="+repository+"@"+public.Digest)
	}
	if request.TeamExportID != "" {
		cacheTo := "type=registry,ref=" + teamRepository + ":build-" + request.TeamExportID + ",mode=max,oci-mediatypes=true,image-manifest=true,ignore-error=true"
		args = append(args, "--cache-to", cacheTo)
	}
	if request.Output == OutputPush {
		args = append(args, "--push")
	} else {
		args = append(args, "--load")
	}
	args = append(args, request.ExtraArgs...)
	args = append(args, request.ContextPath)

	return Plan{Command: Command{Path: a.config.DockerCommand, Args: args}}, nil
}

func validatePublicCache(public PublicCache) error {
	if public.Repository == "" {
		return errors.New("Public Cache repository is required")
	}
	if err := validateCacheRepository("Public Cache", public.Repository); err != nil {
		return err
	}
	encoded, ok := strings.CutPrefix(public.Digest, "sha256:")
	if !ok || len(encoded) != 64 {
		return fmt.Errorf("Public Cache %q must use a complete SHA-256 digest", public.Repository)
	}
	if _, err := hex.DecodeString(encoded); err != nil {
		return fmt.Errorf("Public Cache %q must use a complete SHA-256 digest: %w", public.Repository, err)
	}
	return nil
}

func validateCacheRepository(scope, repository string) error {
	if strings.ContainsAny(repository, ",@\r\n\t ") || strings.Contains(repository, "://") || strings.HasPrefix(repository, "/") {
		return fmt.Errorf("%s repository %q is not a safe OCI repository", scope, repository)
	}
	return nil
}

func validateExtraArgs(args []string) error {
	managed := map[string]struct{}{
		"--builder":    {},
		"--platform":   {},
		"--progress":   {},
		"--cache-from": {},
		"--cache-to":   {},
		"--push":       {},
		"--load":       {},
		"--output":     {},
		"-o":           {},
	}
	for _, arg := range args {
		name, _, _ := strings.Cut(arg, "=")
		if _, found := managed[name]; found {
			return fmt.Errorf("managed buildx flag %q cannot be supplied in extra arguments", name)
		}
	}
	return nil
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, command Command, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, command.Path, command.Args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func writerOrDiscard(writer io.Writer) io.Writer {
	if writer == nil {
		return io.Discard
	}
	return writer
}
