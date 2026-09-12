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
	"time"
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
	DockerCommand        string
	BuilderName          string
	TeamRepository       string
	MaxTeamImports       int
	PromotionLockDir     string
	PromotionCoordinator PromotionCoordinator
	LocalBudget          *LocalCacheBudget
}

type LocalCacheBudget struct {
	StateDir         string
	MaxBytes         int64
	BuildkitMaxBytes int64
	UsedBytes        int64
	MinFreeBytes     int64
}

type PublicCache struct {
	Repository     string
	Digest         string
	PublicIdentity string
}

type BuildRequest struct {
	ContextPath    string
	TargetPlatform string
	TeamImportTags []string
	TeamExportID   string
	TeamPromoteTag string
	PublicImports  []PublicCache
	Output         Output
	ExtraArgs      []string
}

type Command struct {
	Path string
	Args []string
}

type Plan struct {
	Command      Command
	Promotion    *Command     `json:",omitempty"`
	CacheScopes  []CacheScope `json:",omitempty"`
	TeamExportID string       `json:",omitempty"`
}

type CacheScope struct {
	Tier           string `json:"tier"`
	Direction      string `json:"direction"`
	Reference      string `json:"reference"`
	PublicIdentity string `json:"publicIdentity,omitempty"`
}

type ExecutionResult struct {
	Plan            Plan            `json:"plan"`
	Metrics         ProgressMetrics `json:"metrics"`
	ProgressWarning string          `json:"progressWarning,omitempty"`
	CacheWarnings   []string        `json:"cacheWarnings,omitempty"`
	BuildStartedAt  time.Time       `json:"-"`
	BuildFinishedAt time.Time       `json:"-"`
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
	if config.LocalBudget != nil {
		if err := validateLocalCacheBudget(*config.LocalBudget); err != nil {
			return nil, err
		}
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
	warning, err := a.ensureBuilder(ctx, stdout, stderr)
	if warning != "" {
		_, _ = fmt.Fprintln(stderr, "Layer Cache BuildKit:", warning)
	}
	return err
}

func (a *Adapter) ensureBuilder(ctx context.Context, stdout, stderr io.Writer) (string, error) {
	buildkitdConfig, err := a.writeBuildkitdConfig()
	if err != nil {
		return "", err
	}
	list := Command{
		Path: a.config.DockerCommand,
		Args: []string{"buildx", "ls", "--format", "{{json .}}"},
	}
	var builders bytes.Buffer
	if err := a.runner.Run(ctx, list, &builders, io.Discard); err != nil {
		return "", fmt.Errorf("list BuildKit builders: %w", err)
	}
	driver, found, err := findBuilderDriver(&builders, a.config.BuilderName)
	if err != nil {
		return "", err
	}
	if !found {
		createArgs := []string{"buildx", "create", "--name", a.config.BuilderName, "--driver", "docker-container"}
		if buildkitdConfig != "" {
			createArgs = append(createArgs, "--buildkitd-config", buildkitdConfig)
		}
		createArgs = append(createArgs, "--use")
		create := Command{
			Path: a.config.DockerCommand,
			Args: createArgs,
		}
		if err := a.runner.Run(ctx, create, stdout, stderr); err != nil {
			return "", fmt.Errorf("create BuildKit builder %q: %w", a.config.BuilderName, err)
		}
	} else if driver != "docker-container" {
		return "", fmt.Errorf("BuildKit builder %q uses driver %q, want %q", a.config.BuilderName, driver, "docker-container")
	}

	bootstrap := Command{
		Path: a.config.DockerCommand,
		Args: []string{"buildx", "inspect", a.config.BuilderName, "--bootstrap"},
	}
	if err := a.runner.Run(ctx, bootstrap, stdout, stderr); err != nil {
		return "", fmt.Errorf("bootstrap BuildKit builder %q: %w", a.config.BuilderName, err)
	}
	if a.config.LocalBudget == nil {
		return "", nil
	}
	if err := a.pruneToLocalBudget(ctx, stderr); err != nil {
		return fmt.Sprintf("native cache GC did not complete: %v", err), nil
	}
	return "", nil
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
	result := ExecutionResult{Plan: plan}
	gcWarning, err := a.ensureBuilder(ctx, stdout, stderr)
	if err != nil {
		return result, err
	}
	if gcWarning != "" {
		result.CacheWarnings = append(result.CacheWarnings, gcWarning)
		_, _ = fmt.Fprintln(stderr, "Layer Cache BuildKit:", gcWarning)
	}

	progress := newProgressStreamWithScopes(stderr, plan.CacheScopes)
	started := time.Now()
	result.BuildStartedAt = started.UTC()
	runErr := a.runner.Run(ctx, plan.Command, stdout, progress)
	finished := time.Now()
	result.BuildFinishedAt = finished.UTC()
	buildDuration := finished.Sub(started)
	metrics, progressErr := progress.finish()
	metrics.BuildDurationMS = buildDuration.Milliseconds()
	metrics.RequestedCacheScopes = append([]CacheScope(nil), plan.CacheScopes...)
	metrics.CacheSource = cacheSource(metrics, plan.CacheScopes)
	result.Metrics = metrics
	if progressErr != nil {
		result.ProgressWarning = progressErr.Error()
	}
	if a.config.LocalBudget != nil {
		if err := a.pruneToLocalBudget(ctx, stderr); err != nil {
			warning := fmt.Sprintf("native cache post-build GC did not complete: %v", err)
			result.CacheWarnings = append(result.CacheWarnings, warning)
			_, _ = fmt.Fprintln(stderr, "Layer Cache BuildKit:", warning)
		}
	}
	if runErr != nil {
		return result, fmt.Errorf("run BuildKit build: %w", runErr)
	}
	if plan.Promotion != nil {
		result.Metrics.TeamPromotionAttempted = true
		if err := a.promote(ctx, *plan.Promotion, plan.promotionReference(), stderr); err != nil {
			warning := fmt.Sprintf("Team Cache promotion failed after the image build succeeded: %v", err)
			result.CacheWarnings = append(result.CacheWarnings, warning)
			_, _ = fmt.Fprintln(stderr, "Layer Cache BuildKit:", warning)
		} else {
			result.Metrics.TeamPromotionSucceeded = true
		}
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
	if (len(request.TeamImportTags) > 0 || request.TeamExportID != "" || request.TeamPromoteTag != "") && a.config.TeamRepository == "" {
		return Plan{}, errors.New("Team Cache repository is required for Team Cache imports or publication")
	}
	for _, tag := range request.TeamImportTags {
		if !ociTagPattern.MatchString(tag) {
			return Plan{}, fmt.Errorf("Team Cache import %q must be a safe OCI tag", tag)
		}
	}
	if request.TeamExportID != "" {
		if err := validateTeamExportID(request.TeamExportID); err != nil {
			return Plan{}, err
		}
	}
	if request.TeamPromoteTag != "" && !ociTagPattern.MatchString(request.TeamPromoteTag) {
		return Plan{}, fmt.Errorf("Team Cache promotion %q must be a safe OCI tag", request.TeamPromoteTag)
	}
	if request.TeamPromoteTag != "" && request.TeamExportID == "" {
		return Plan{}, errors.New("Team Cache promotion requires an immutable Team Cache export")
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
	plan := Plan{TeamExportID: request.TeamExportID}
	for _, tag := range request.TeamImportTags {
		reference := teamRepository + ":" + tag
		args = append(args, "--cache-from", "type=registry,ref="+reference)
		plan.CacheScopes = append(plan.CacheScopes, CacheScope{Tier: "Team Cache", Direction: "import", Reference: reference})
	}
	for _, public := range request.PublicImports {
		if err := validatePublicCache(public); err != nil {
			return Plan{}, err
		}
		repository := strings.TrimSuffix(public.Repository, "/") + "/" + platform
		reference := repository + "@" + public.Digest
		args = append(args, "--cache-from", "type=registry,ref="+reference)
		plan.CacheScopes = append(plan.CacheScopes, CacheScope{
			Tier: "Public Cache", Direction: "import", Reference: reference, PublicIdentity: public.PublicIdentity,
		})
	}
	if request.TeamExportID != "" {
		reference := teamRepository + ":build-" + request.TeamExportID
		cacheTo := "type=registry,ref=" + reference + ",mode=max,oci-mediatypes=true,image-manifest=true,ignore-error=true"
		args = append(args, "--cache-to", cacheTo)
		plan.CacheScopes = append(plan.CacheScopes, CacheScope{Tier: "Team Cache", Direction: "export", Reference: reference})
		if request.TeamPromoteTag != "" {
			promoted := teamRepository + ":" + request.TeamPromoteTag
			plan.Promotion = &Command{Path: a.config.DockerCommand, Args: []string{
				"buildx", "imagetools", "create", "--builder", a.config.BuilderName,
				"--prefer-index=false", "--progress=plain", "--tag", promoted, reference,
			}}
			plan.CacheScopes = append(plan.CacheScopes, CacheScope{Tier: "Team Cache", Direction: "promote", Reference: promoted})
		}
	}
	if request.Output == OutputPush {
		args = append(args, "--push")
	} else {
		args = append(args, "--load")
	}
	args = append(args, request.ExtraArgs...)
	args = append(args, request.ContextPath)

	plan.Command = Command{Path: a.config.DockerCommand, Args: args}
	return plan, nil
}

func (plan Plan) promotionReference() string {
	for _, scope := range plan.CacheScopes {
		if scope.Direction == "promote" {
			return scope.Reference
		}
	}
	return ""
}

func cacheSource(metrics ProgressMetrics, scopes []CacheScope) string {
	if metrics.CachedVertices == 0 {
		return "miss"
	}
	for _, scope := range scopes {
		if scope.Direction == "import" {
			return "unattributed"
		}
	}
	return "Local Cache"
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
	if len(public.PublicIdentity) != 64 || public.PublicIdentity != strings.ToLower(public.PublicIdentity) {
		return fmt.Errorf("Public Cache %q must include its complete signed publication identity", public.Repository)
	}
	if _, err := hex.DecodeString(public.PublicIdentity); err != nil {
		return fmt.Errorf("Public Cache %q must include its complete signed publication identity: %w", public.Repository, err)
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
