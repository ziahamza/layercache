package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/buildkit"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/measurement"
	"github.com/layercache/layercache/internal/project"
	"github.com/layercache/layercache/internal/publictrust"
	"github.com/layercache/layercache/internal/remote"
)

const buildkitPublicResolveTimeout = 3 * time.Second

func runBuildx(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || (args[0] != "plan" && args[0] != "build") {
		return errors.New("use buildx plan or buildx build")
	}
	operation := args[0]
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("buildx "+operation, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "configuration file")
	platform := flags.String("platform", "linux/amd64", "target platform")
	teamExport := flags.String("team-export", "", "label for a unique immutable Team Cache export")
	teamPromote := flags.String("team-promote", "", "mutable Team Cache tag promoted after a successful export")
	load := flags.Bool("load", false, "load one platform into the local image store")
	push := flags.Bool("push", false, "push the build output")
	jsonOutput := flags.Bool("json", false, "print the result as JSON")
	var teamImports stringList
	flags.Var(&teamImports, "team-import", "Team Cache tag to import, repeatable")
	var publicImportSelectors stringList
	flags.Var(&publicImportSelectors, "public-import", "trusted Public Cache selector native-key=publication-identity, repeatable")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	remaining := flags.Args()
	if len(remaining) == 0 {
		return errors.New("buildx arguments must end with a build context")
	}
	if *load && *push {
		return errors.New("choose only one of --load or --push")
	}
	output := buildkit.OutputLoad
	if *push {
		output = buildkit.OutputPush
	}
	contextPath := remaining[len(remaining)-1]
	extraArgs := append([]string(nil), remaining[:len(remaining)-1]...)
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	scopeDirectory := ""
	if info, err := os.Stat(contextPath); err == nil && info.IsDir() {
		scopeDirectory = contextPath
	}
	_, scopeErr := discoverConfiguredProject(ctx, cfg, scopeDirectory, flagWasSet(flags, "config") || cfg.ProjectRoot == "")
	if scopeErr != nil {
		cfg.BypassAdapters = append(cfg.BypassAdapters, "buildkit")
		_, _ = fmt.Fprintf(stderr, "Layer Cache warning: %v; bypassing Layer Cache for this build\n", scopeErr)
	}
	preBuildDegraded := false
	// Buildx commands are short-lived clients, so they do not benefit from the
	// daemon's in-memory capability rotation. Refresh an expiring Team
	// capability for this invocation before constructing the promotion client.
	// Keep the refreshed capability in memory so this command cannot overwrite
	// concurrent setup changes with a stale configuration snapshot.
	if !isAdapterBypassed(cfg, "buildkit") {
		if err := refreshAndPersistTeamCapability(ctx, *configPath, &cfg); err != nil {
			preBuildDegraded = true
			_, _ = fmt.Fprintln(stderr, "Layer Cache BuildKit: Team capability refresh failed; the build will continue and remote cache operations may degrade:", err)
		}
	}
	if isAdapterBypassed(cfg, "buildkit") {
		teamImports = nil
		*teamExport = ""
		*teamPromote = ""
		publicImportSelectors = nil
		extraArgs = append(extraArgs, "--no-cache")
		_, _ = fmt.Fprintln(stderr, "Layer Cache BuildKit bypass is active; remote cache imports and Team Cache publication are disabled")
	}
	teamExportID := ""
	if *teamExport != "" {
		teamExportID, err = buildkit.NewTeamExportID(*teamExport)
		if err != nil {
			return err
		}
		if !flagWasSet(flags, "team-promote") {
			*teamPromote = cfg.BuildkitBranch
		}
		if *teamPromote == "" {
			return errors.New("Team Cache export requires --team-promote or a configured BuildKit branch")
		}
	}
	publicImports, publicWarnings, err := resolveBuildkitPublicImports(ctx, cfg, *platform, publicImportSelectors)
	if err != nil {
		return err
	}
	for _, warning := range publicWarnings {
		preBuildDegraded = true
		_, _ = fmt.Fprintln(stderr, "Layer Cache BuildKit:", warning)
	}
	localUsage, usageErr := buildkitLocalUsage(ctx, cfg)
	if usageErr != nil {
		localUsage = cfg.MaxBytes
		_, _ = fmt.Fprintln(stderr, "Layer Cache BuildKit: could not read Local Cache usage; native BuildKit cache is limited to one byte:", usageErr)
	}
	buildkitStateDir := filepath.Join(cfg.DataDir, "buildkit")
	var promotionCoordinator buildkit.PromotionCoordinator
	if cfg.TeamURL != "" {
		promotionCoordinator, err = buildkit.NewHTTPPromotionCoordinator(cfg.TeamURL, cfg.TeamToken)
		if err != nil {
			// A configured Team Cache requires cross-host coordination. Keep the
			// build available, but make mutable promotion fail closed at its seam.
			promotionCoordinator = unavailablePromotionCoordinator{cause: err}
		}
	}
	adapter, err := buildkit.New(buildkit.Config{
		BuilderName: cfg.BuildkitBuilder, TeamRepository: cfg.BuildkitTeamRepository,
		PromotionLockDir: filepath.Join(buildkitStateDir, "promotion-locks"), PromotionCoordinator: promotionCoordinator,
		LocalBudget: &buildkit.LocalCacheBudget{
			StateDir: buildkitStateDir, MaxBytes: cfg.MaxBytes, BuildkitMaxBytes: cfg.BuildkitGCBytes,
			UsedBytes: localUsage, MinFreeBytes: cfg.MinFreeBytes,
		},
	})
	if err != nil {
		return err
	}
	request := buildkit.BuildRequest{
		ContextPath: contextPath, TargetPlatform: *platform,
		TeamImportTags: teamImports, TeamExportID: teamExportID, TeamPromoteTag: *teamPromote,
		PublicImports: publicImports,
		Output:        output, ExtraArgs: extraArgs,
	}
	if operation == "plan" {
		plan, err := adapter.Plan(request)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(plan)
	}
	buildOutput := stdout
	if *jsonOutput {
		// Keep stdout machine-readable. Buildx builder/bootstrap diagnostics are
		// still visible on stderr alongside the native raw progress stream.
		buildOutput = stderr
	}
	result, err := adapter.Execute(ctx, request, buildOutput, stderr)
	if err != nil {
		return err
	}
	measurementRunID, workspaceID, correlationErr := "", "", scopeErr
	if correlationErr == nil {
		measurementRunID, workspaceID, correlationErr = buildkitMeasurementCorrelation(ctx, cfg)
	}
	if correlationErr == nil {
		correlationErr = recordBuildkitMeasurement(
			ctx, cfg, measurementRunID, workspaceID, strings.ReplaceAll(*platform, "/", "-"),
			result.BuildStartedAt, result.BuildFinishedAt, result.Metrics,
			preBuildDegraded || result.ProgressWarning != "" || len(result.CacheWarnings) > 0,
		)
	}
	if correlationErr != nil {
		_, _ = fmt.Fprintln(stderr, "Layer Cache warning: BuildKit measurement was not recorded:", correlationErr)
		measurementRunID = ""
	} else {
		_, _ = fmt.Fprintln(stderr, "Layer Cache BuildKit measurement run:", measurementRunID)
	}
	if *jsonOutput {
		return json.NewEncoder(stdout).Encode(struct {
			buildkit.ExecutionResult
			MeasurementRunID string `json:"measurementRunId,omitempty"`
			WorkspaceID      string `json:"workspaceId,omitempty"`
		}{ExecutionResult: result, MeasurementRunID: measurementRunID, WorkspaceID: workspaceID})
	}
	_, err = fmt.Fprintf(stderr, "Layer Cache BuildKit: %d/%d completed vertices were cached\n", result.Metrics.CachedVertices, result.Metrics.CompletedVertices)
	return err
}

func buildkitMeasurementCorrelation(ctx context.Context, cfg config.Config) (string, string, error) {
	now := time.Now().UTC()
	if token := strings.TrimSpace(os.Getenv(measurementCapabilityEnvironment)); token != "" {
		claims, err := access.ParseCapabilityToken(cfg.LocalToken, token, now)
		if err == nil && claims.Project == cfg.ProjectID && claims.Integration == "buildkit" && claims.RunID != "" {
			return claims.RunID, claims.WorkspaceID, nil
		}
	}
	random, err := config.NewToken()
	if err != nil {
		return "", "", fmt.Errorf("create BuildKit measurement run identity: %w", err)
	}
	discoveredRoot := ""
	if discovered, discoverErr := project.Discover(ctx, ""); discoverErr == nil || errors.Is(discoverErr, project.ErrNoRemote) {
		discoveredRoot = discovered.Root
	}
	return "run-buildkit-" + random[:20], workspaceMeasurementIdentity(cfg.ProjectID, discoveredRoot, cfg.ProjectRoot), nil
}

func recordBuildkitMeasurement(
	ctx context.Context,
	cfg config.Config,
	runID, workspaceID, compatibilityID string,
	startedAt, finishedAt time.Time,
	metrics buildkit.ProgressMetrics,
	degraded bool,
) error {
	type repository interface {
		Record(measurement.FinalOutcome) error
		LatestBuildkitBaseline(string, string) (*time.Duration, error)
		Close() error
	}
	var outcomes repository
	var err error
	if cfg.CloudPostgresURL != "" {
		outcomes, err = measurement.OpenPostgresRepository(ctx, cfg.CloudPostgresURL, cfg.ProjectID)
	} else {
		outcomes, err = measurement.OpenSQLiteRepository(filepath.Join(cfg.DataDir, "measurements.db"))
	}
	if err != nil {
		return err
	}
	random, err := config.NewToken()
	if err != nil {
		return errors.Join(err, outcomes.Close())
	}
	eligibleUnits := metrics.CompletedVertices
	graphDigest := metrics.GraphDigest
	if graphDigest == "" {
		graphDigest = "unobserved:" + random[:20]
	}
	duration := time.Duration(metrics.BuildDurationMS) * time.Millisecond
	if duration > 0 {
		startedAt = finishedAt.Add(-duration)
	} else {
		duration = finishedAt.Sub(startedAt)
	}
	artifactID := measurement.BuildkitArtifactIdentity(cfg.ProjectID, compatibilityID, graphDigest)
	outcome := measurement.FinalOutcome{
		RunID: runID, WorkspaceID: workspaceID, Integration: measurement.IntegrationBuildkit,
		WorkID:          "buildkit-" + random[:20],
		ArtifactID:      artifactID,
		CompatibilityID: compatibilityID,
		StartedAt:       startedAt, FinishedAt: finishedAt,
		Bytes: measurement.Bytes{
			Downloaded: metrics.RemoteDownloadedBytes,
			Uploaded:   metrics.RemoteUploadedBytes,
		},
		Degraded:      degraded || metrics.CompletedVertices == 0,
		EligibleUnits: eligibleUnits, HitUnits: metrics.CachedVertices,
	}
	if metrics.CompletedVertices == 0 {
		outcome.Result = measurement.ResultUnknown
		outcome.Source = measurement.SourceUnattributed
	} else if metrics.CachedVertices == 0 {
		outcome.Result = measurement.ResultMiss
		outcome.Source = measurement.SourceNone
		outcome.ExecutionDuration = &duration
	} else {
		outcome.Result = measurement.ResultHit
		switch metrics.CacheSource {
		case "Local Cache":
			outcome.Source = measurement.SourceLocalCache
		default:
			outcome.Source = measurement.SourceUnattributed
		}
		outcome.Timing.Restore = duration
		baseline, baselineErr := outcomes.LatestBuildkitBaseline(artifactID, compatibilityID)
		if baselineErr != nil {
			return errors.Join(baselineErr, outcomes.Close())
		}
		outcome.ProducerDuration = baseline
	}
	recordErr := outcomes.Record(outcome)
	return errors.Join(recordErr, outcomes.Close())
}

type unavailablePromotionCoordinator struct {
	cause error
}

func (coordinator unavailablePromotionCoordinator) Acquire(context.Context, string) (buildkit.PromotionLease, error) {
	return nil, coordinator.cause
}

type buildkitPublicSelector struct {
	nativeKey      string
	publicIdentity string
}

func parseBuildkitPublicSelector(value string) (buildkitPublicSelector, error) {
	nativeKey, publicIdentity, found := strings.Cut(value, "=")
	if !found || nativeKey == "" || len(publicIdentity) != 64 || publicIdentity != strings.ToLower(publicIdentity) {
		return buildkitPublicSelector{}, fmt.Errorf("Public Cache import %q must use native-key=64-hex-publication-identity", value)
	}
	if strings.ContainsAny(nativeKey, "\r\n\t ") {
		return buildkitPublicSelector{}, fmt.Errorf("Public Cache import %q has an invalid native key", value)
	}
	if _, err := hex.DecodeString(publicIdentity); err != nil {
		return buildkitPublicSelector{}, fmt.Errorf("Public Cache import %q has an invalid publication identity", value)
	}
	return buildkitPublicSelector{nativeKey: nativeKey, publicIdentity: publicIdentity}, nil
}

func resolveBuildkitPublicImports(
	ctx context.Context,
	cfg config.Config,
	platform string,
	values []string,
) ([]buildkit.PublicCache, []string, error) {
	if len(values) == 0 {
		return nil, nil, nil
	}
	if len(values) > 4 {
		return nil, nil, errors.New("use at most 4 Public Cache imports")
	}
	selectors := make([]buildkitPublicSelector, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		selector, err := parseBuildkitPublicSelector(value)
		if err != nil {
			return nil, nil, err
		}
		identity := selector.nativeKey + "\x00" + selector.publicIdentity
		if _, duplicate := seen[identity]; duplicate {
			return nil, nil, fmt.Errorf("duplicate Public Cache import %q", value)
		}
		seen[identity] = struct{}{}
		selectors = append(selectors, selector)
	}
	if cfg.PublicURL == "" || cfg.PublicTrustKey == "" || cfg.BuildkitPublicRepository == "" {
		return nil, []string{"Public Cache imports were requested but the Public Cache URL, trust key, or BuildKit OCI repository is not configured"}, nil
	}
	client, err := remote.NewPublicClientWithTimeouts(cfg.PublicURL, cfg.PublicTrustKey, remote.Timeouts{
		Metadata: cfg.RemoteMetadataTimeout, TransferIdle: cfg.RemoteTransferIdleTimeout,
	})
	if err != nil {
		return nil, []string{fmt.Sprintf("Public Cache imports were skipped: %v", err)}, nil
	}
	compatibility := strings.ReplaceAll(platform, "/", "-")
	imports := make([]buildkit.PublicCache, 0, len(selectors))
	warnings := make([]string, 0)
	for _, selector := range selectors {
		resolveCtx, cancel := context.WithTimeout(ctx, buildkitPublicResolveTimeout)
		publication, _, _, resolveErr := client.Resolve(resolveCtx, publictrust.Expected{
			Integration: "buildkit", Project: cfg.ProjectID, Compatibility: compatibility,
			NativeKey: selector.nativeKey, Platform: platform, PublicIdentity: selector.publicIdentity,
		})
		cancel()
		if resolveErr != nil {
			warnings = append(warnings, fmt.Sprintf(
				"Public Cache import %q was not approved and will be skipped: %v", selector.nativeKey, resolveErr,
			))
			continue
		}
		imports = append(imports, buildkit.PublicCache{
			Repository: cfg.BuildkitPublicRepository, Digest: "sha256:" + publication.Digest,
			PublicIdentity: selector.publicIdentity,
		})
	}
	return imports, warnings, nil
}

func buildkitLocalUsage(ctx context.Context, cfg config.Config) (int64, error) {
	if live, err := probeRuntime(cfg); err == nil {
		return live.UsageBytes, nil
	}
	stats, err := artifact.ReadStats(ctx, cfg.DataDir)
	if err != nil {
		return 0, err
	}
	return stats.UsageBytes, nil
}

type stringList []string

func (values *stringList) String() string {
	return strings.Join(*values, ",")
}

func (values *stringList) Set(value string) error {
	if value == "" {
		return errors.New("value cannot be empty")
	}
	*values = append(*values, value)
	return nil
}
