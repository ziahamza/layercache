package cli

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/credentials"
	"github.com/layercache/layercache/internal/project"
	"github.com/layercache/layercache/internal/publictrust"
	"github.com/layercache/layercache/internal/retention"
	"github.com/layercache/layercache/internal/server"
)

const usage = `Layer Cache

Usage:
  layercache setup [options]
  layercache login [options]
  layercache start [options]
  layercache stop [options]
  layercache status [options]
  layercache doctor [options]
  layercache dashboard [--config FILE ...] [--listen 127.0.0.1:PORT]
  layercache repair [options]
  layercache gc [options]
  layercache cache quota|pins|pin|unpin [options]
  layercache disable [--adapter turbo|actions|buildkit|all] [options]
  layercache bypass --adapter turbo|actions|buildkit|all [options]
  layercache uninstall --preserve-cache|--delete-cache [options]
  layercache report --run RUN_ID [options]
  layercache report --from RFC3339 --to RFC3339 [options]
  layercache backtest --input FILE --retention DURATION --source CACHE [options]
  layercache integration turbo [--apply] [options]
  layercache integration buildkit [--apply] [options]
  layercache integration local-ci [--apply] [options]
  layercache buildx plan|build [options]
  layercache public-build request|status|logs|cancel [options]
  layercache public-build worker run|lease|append-log|heartbeat|complete|fail [options]
  layercache vm-route issue [options]
  layercache run [options] -- COMMAND [ARG...]
  layercache serve [options]
  layercache connect --cloud URL [--github-cli] [--team TEAM] [--project PROJECT]
  layercache serve-cloud --config FILE
  layercache serve-projects --config FILE
`

const (
	capabilityRefreshWindow = 5 * time.Minute
	capabilityRefreshRetry  = 30 * time.Second
)

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, _ = io.WriteString(stderr, usage)
		return errors.New("a command is required")
	}
	switch args[0] {
	case "dashboard":
		return runDashboard(ctx, args[1:], stdout, stderr)
	case "setup":
		return runSetup(ctx, args[1:], stdout, stderr)
	case "connect":
		return runConnect(ctx, args[1:], stdout, stderr)
	case "serve-cloud":
		return runServeCloud(ctx, args[1:], stderr)
	case "login":
		return runLogin(ctx, args[1:], stdout, stderr)
	case "status":
		return runStatus(ctx, args[1:], stdout, stderr)
	case "start":
		return runStart(ctx, args[1:], stdout, stderr)
	case "stop":
		return runStop(ctx, args[1:], stdout, stderr)
	case "doctor":
		return runDoctor(ctx, args[1:], stdout, stderr)
	case "repair":
		return runRepair(ctx, args[1:], stdout, stderr)
	case "gc":
		return runGC(ctx, args[1:], stdout, stderr)
	case "cache":
		return runCacheAdmin(ctx, args[1:], stdout, stderr)
	case "bypass":
		return runBypass(ctx, args[1:], stdout, stderr)
	case "disable":
		return runDisable(ctx, args[1:], stdout, stderr)
	case "uninstall":
		return runUninstall(ctx, args[1:], stdout, stderr)
	case "integration":
		return runIntegration(ctx, args[1:], stdout, stderr)
	case "serve":
		return runServe(ctx, args[1:], stderr)
	case "serve-projects":
		return runServeProjects(ctx, args[1:], stderr)
	case "public":
		return runPublic(ctx, args[1:], stdout, stderr)
	case "public-build":
		return runPublicBuild(ctx, args[1:], stdout, stderr)
	case "vm-route":
		return runVMRoute(ctx, args[1:], stdout, stderr)
	case "buildx":
		return runBuildx(ctx, args[1:], stdout, stderr)
	case "run":
		return runCommand(ctx, args[1:], stdout, stderr)
	case "report":
		return runReport(ctx, args[1:], stdout, stderr)
	case "backtest":
		return runBacktest(ctx, args[1:], stdout, stderr)
	case "help", "--help", "-h":
		_, err := io.WriteString(stdout, usage)
		return err
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// ExitCode preserves a child process's non-zero status through layercache run.
// All Layer Cache command and configuration failures use the conventional
// generic failure status instead.
func ExitCode(err error) int {
	type exitCoder interface {
		ExitCode() int
	}
	var coder exitCoder
	if errors.As(err, &coder) {
		if code := coder.ExitCode(); code > 0 && code <= 255 {
			return code
		}
	}
	return 1
}

func runServe(ctx context.Context, args []string, stderr io.Writer) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load Layer Cache configuration: %w", err)
	}
	if refreshErr := refreshAndPersistTeamCapability(ctx, *configPath, &cfg); refreshErr != nil {
		_, _ = fmt.Fprintf(stderr, "Layer Cache warning: %v; Local Cache will remain available\n", refreshErr)
	}
	serveContext, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if !strings.HasPrefix(cfg.Listen, "127.0.0.1:") && !strings.HasPrefix(cfg.Listen, "[::1]:") && cfg.LocalToken == "" {
		return errors.New("refusing to expose a cache runtime without authentication")
	}
	refreshContext, cancelRefresh := context.WithCancel(serveContext)
	refreshDone := make(chan struct{})
	refreshStarted := false
	err = server.RunHTTPWithReady(serveContext, cfg, func(runtime *server.Server) {
		_, _ = fmt.Fprintf(stderr, "Layer Cache listening on http://%s\n", cfg.Listen)
		refreshStarted = true
		go func() {
			defer close(refreshDone)
			maintainTeamCapability(refreshContext, *configPath, cfg, runtime, stderr)
		}()
	})
	cancelRefresh()
	if refreshStarted {
		<-refreshDone
	}
	return err
}

type teamTokenUpdater interface {
	UpdateTeamToken(string) error
}

func maintainTeamCapability(
	ctx context.Context,
	configPath string,
	cfg config.Config,
	runtime teamTokenUpdater,
	stderr io.Writer,
) {
	retryDelay := time.Duration(0)
	for {
		delay, refreshable := nextCapabilityRefresh(cfg, time.Now().UTC())
		if !refreshable {
			<-ctx.Done()
			return
		}
		if retryDelay > 0 {
			delay = retryDelay
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		current, err := config.Load(configPath)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "Layer Cache warning: reload configuration before credential refresh: %v; Local Cache remains available\n", err)
			retryDelay = capabilityRefreshRetry
			continue
		}
		refreshed := current
		refreshContext, cancel := context.WithTimeout(ctx, controlRequestTimeout)
		err = refreshTeamCapability(refreshContext, &refreshed)
		cancel()
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "Layer Cache warning: %v; Team Cache will retry while Local Cache remains available\n", err)
			retryDelay = capabilityRefreshRetry
			continue
		}
		if refreshed.TeamToken == "" {
			_, _ = fmt.Fprintln(stderr, "Layer Cache warning: refreshed Team Cache credential is empty; Local Cache remains available")
			retryDelay = capabilityRefreshRetry
			continue
		}
		if refreshed.TeamToken != cfg.TeamToken {
			if err := runtime.UpdateTeamToken(refreshed.TeamToken); err != nil {
				_, _ = fmt.Fprintf(stderr, "Layer Cache warning: activate refreshed Team Cache capability: %v; Local Cache remains available\n", err)
				retryDelay = capabilityRefreshRetry
				continue
			}
		}
		if err := persistRefreshedCapabilities(ctx, configPath, current, refreshed); err != nil {
			_, _ = fmt.Fprintf(stderr, "Layer Cache warning: persist refreshed project capabilities: %v; runtime credentials were rotated and persistence will retry\n", err)
			cfg = current
			retryDelay = capabilityRefreshRetry
			continue
		}
		cfg = refreshed
		retryDelay = 0
	}
}

func nextCapabilityRefresh(cfg config.Config, now time.Time) (time.Duration, bool) {
	if cfg.TeamURL == "" || cfg.GitHubCredentialAccount == "" && cfg.GitHubCLIPath == "" {
		return 0, false
	}
	expiresAt := cfg.TeamTokenExpiresAt
	if cfg.PublicAccessToken != "" && !cfg.PublicAccessTokenExpiresAt.IsZero() &&
		(expiresAt.IsZero() || cfg.PublicAccessTokenExpiresAt.Before(expiresAt)) {
		expiresAt = cfg.PublicAccessTokenExpiresAt
	}
	if expiresAt.IsZero() {
		return 0, true
	}
	delay := expiresAt.Add(-capabilityRefreshWindow).Sub(now)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

func runSetup(ctx context.Context, args []string, stdout, stderr io.Writer) (returnErr error) {
	base, err := config.Defaults()
	if err != nil {
		return err
	}
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	requestedConfigPath, err := canonicalSetupConfigPath(setupConfigPath(args, defaultPath))
	if err != nil {
		return err
	}
	existing, err := config.Exists(requestedConfigPath)
	if err != nil {
		return fmt.Errorf("inspect Layer Cache configuration: %w", err)
	}
	if existing {
		base, err = config.Load(requestedConfigPath)
		if err != nil {
			return fmt.Errorf("load existing Layer Cache configuration: %w", err)
		}
	}
	localIdentityRoot := ""
	persistLocalIdentity := false
	if !existing {
		discovered, discoverErr := project.Discover(ctx, "")
		if discoverErr == nil {
			base.ProjectRoot = discovered.Root
			base.ProjectID = discovered.Project
			base.ActionsRepository = discovered.ActionsRepository
			base.ActionsRef = discovered.Ref
			base.ActionsDefaultRef = discovered.DefaultRef
		} else if errors.Is(discoverErr, project.ErrNoRemote) {
			base.ProjectRoot = discovered.Root
			localIdentityRoot = discovered.Root
			if discovered.Project != "" {
				base.ProjectID = discovered.Project
			} else {
				persistLocalIdentity = true
			}
			base.ActionsRef = discovered.Ref
			base.ActionsDefaultRef = discovered.DefaultRef
			base.ActionsRepository = "local/" + strings.TrimPrefix(base.ProjectID, "local-")
		}
	}
	original := base
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", requestedConfigPath, "configuration file")
	dataDir := flags.String("data-dir", base.DataDir, "host Local Cache directory")
	listen := flags.String("listen", base.Listen, "loopback HTTP address")
	maxSize := flags.String("max-size", strconv.FormatInt(base.MaxBytes, 10), "Local Cache byte limit")
	evictionPolicy := flags.String("eviction-policy", string(base.EvictionPolicy), "cache eviction policy: lru or impact")
	minFreeBytesFlag := flags.String("min-free-bytes", strconv.FormatInt(base.MinFreeBytes, 10), "upward Local Cache free-space reserve override in bytes")
	role := flags.String("role", base.Role, "runtime role: local, team, or public")
	projectID := flags.String("project", base.ProjectID, "project identity")
	compatibilityID := flags.String("compatibility-id", base.CompatibilityID, "output compatibility identity (OS, architecture, ABI, and toolchain as needed)")
	localToken := flags.String("local-token", "", "bearer token accepted by this runtime (prefer --local-token-file; preserved when omitted)")
	localTokenFile := flags.String("local-token-file", "", "owner-only file containing the runtime bearer token")
	teamURL := flags.String("team-url", base.TeamURL, "Team Cache URL")
	teamToken := flags.String("team-token", "", "Team Cache bearer token (prefer --team-token-file; preserved when omitted)")
	teamTokenFile := flags.String("team-token-file", "", "owner-only file containing the Team Cache bearer token")
	githubAPIURL := flags.String("github-api-url", base.GitHubAPIURL, "GitHub API URL used by Team Cache login")
	githubOIDCIssuer := flags.String("github-oidc-issuer", base.GitHubOIDCIssuer, "GitHub Actions OIDC issuer")
	teamMembers := newRepeatableStringFlag(teamMemberValues(base.TeamMembers))
	flags.Var(teamMembers, "team-member", "authorized GitHub login and role, LOGIN=reader|writer|admin (repeatable)")
	cloudPostgresURL := flags.String("cloud-postgres-url", "", "PostgreSQL connection URL for Team/Public cloud metadata (prefer --cloud-postgres-url-file; preserved when omitted)")
	cloudPostgresURLFile := flags.String("cloud-postgres-url-file", "", "owner-only file containing the PostgreSQL connection URL")
	cloudS3Endpoint := flags.String("cloud-s3-endpoint", base.CloudS3Endpoint, "S3-compatible object storage endpoint")
	cloudS3Bucket := flags.String("cloud-s3-bucket", base.CloudS3Bucket, "S3 bucket for immutable Team/Public artifacts")
	cloudS3Region := flags.String("cloud-s3-region", base.CloudS3Region, "S3 region")
	cloudS3AccessKey := flags.String("cloud-s3-access-key", "", "S3 access key (prefer --cloud-s3-access-key-file; preserved when omitted; AWS credential chain is the default)")
	cloudS3AccessKeyFile := flags.String("cloud-s3-access-key-file", "", "owner-only file containing the S3 access key")
	cloudS3SecretKey := flags.String("cloud-s3-secret-key", "", "S3 secret key (prefer --cloud-s3-secret-key-file; preserved when omitted; AWS credential chain is the default)")
	cloudS3SecretKeyFile := flags.String("cloud-s3-secret-key-file", "", "owner-only file containing the S3 secret key")
	cloudS3UsePathStyle := flags.Bool("cloud-s3-path-style", base.CloudS3UsePathStyle, "use path-style S3 bucket addressing")
	cloudStageTTL := flags.Duration("cloud-stage-ttl", base.CloudStageTTL, "expiry for abandoned cloud uploads")
	cloudBlobGrace := flags.Duration("cloud-blob-grace", base.CloudBlobGrace, "grace period before unreferenced cloud blobs are deleted")
	remoteMetadataTimeout := flags.Duration("remote-metadata-timeout", base.RemoteMetadataTimeout, "deadline for remote connection and response metadata")
	remoteTransferIdleTimeout := flags.Duration("remote-transfer-idle-timeout", base.RemoteTransferIdleTimeout, "maximum time a remote transfer may make no progress")
	publicURL := flags.String("public-url", base.PublicURL, "Public Cache URL")
	publicAccessToken := flags.String("public-access-token", "", "Public Build client credential (prefer --public-access-token-file; preserved when omitted)")
	publicAccessTokenFile := flags.String("public-access-token-file", "", "owner-only file containing the Public Build client credential")
	publicTrustKey := flags.String("public-trust-key", base.PublicTrustKey, "pinned Public Cache Ed25519 key")
	publisherToken := flags.String("publisher-token", "", "deprecated publisher token (prefer --publisher-token-file; used only to migrate separate worker and collector credentials)")
	publisherTokenFile := flags.String("publisher-token-file", "", "owner-only file containing the deprecated publisher token")
	publicBuildWorkerToken := flags.String("public-build-worker-token", "", "Public Build worker credential (prefer --public-build-worker-token-file; preserved when omitted)")
	publicBuildWorkerTokenFile := flags.String("public-build-worker-token-file", "", "owner-only file containing the Public Build worker credential")
	publicCollectorToken := flags.String("public-collector-token", "", "trusted Public Cache collector credential (prefer --public-collector-token-file; preserved when omitted)")
	publicCollectorTokenFile := flags.String("public-collector-token-file", "", "owner-only file containing the trusted collector credential")
	actionsRepositoryDefault := base.ActionsRepository
	actionsRepository := flags.String("actions-repository", actionsRepositoryDefault, "GitHub Actions owner/repository scope")
	actionsRef := flags.String("actions-ref", base.ActionsRef, "GitHub Actions current ref")
	actionsDefaultRef := flags.String("actions-default-ref", base.ActionsDefaultRef, "GitHub Actions default branch ref")
	actionsPublicRecipe := flags.String("actions-public-recipe", base.ActionsPublicRecipeDigest, "maintained Public Actions recipe digest")
	actionsPublicBuilder := flags.String("actions-public-builder", base.ActionsPublicBuilder, "trusted Public Actions builder identity")
	actionsArchiveBaseURL := flags.String("actions-archive-base-url", base.ActionsArchiveBaseURL, "external HTTPS base URL for signed Actions archives")
	buildkitBuilder := flags.String("buildkit-builder", base.BuildkitBuilder, "persistent BuildKit builder name")
	buildkitTeamRepository := flags.String("buildkit-team-repository", base.BuildkitTeamRepository, "Team Cache OCI repository")
	buildkitPublicRepository := flags.String("buildkit-public-repository", base.BuildkitPublicRepository, "trusted Public Cache OCI repository")
	buildkitBranch := flags.String("buildkit-branch", base.BuildkitBranch, "mutable Team Cache branch reference")
	buildkitGCBytes := flags.String("buildkit-gc-bytes", strconv.FormatInt(base.BuildkitGCBytes, 10), "BuildKit Local Cache byte budget")
	publicBuildRepositories := newRepeatableStringFlag(base.PublicBuildRepositories)
	flags.Var(publicBuildRepositories, "public-build-repository", "allowlisted immutable Public Build repository (repeatable)")
	publicBuildApprovedRefs := newRepeatableStringFlag(base.PublicBuildApprovedRefs)
	flags.Var(publicBuildApprovedRefs, "public-build-approved-ref", "approved Public Build branch or tag (repeatable)")
	publicBuildRecipeDigests := newRepeatableStringFlag(base.PublicBuildRecipeDigests)
	flags.Var(publicBuildRecipeDigests, "public-build-recipe", "maintained Public Build recipe digest (repeatable)")
	publicBuildKernel := flags.String("public-build-kernel", base.PublicBuildKernelPath, "immutable Public Build guest kernel")
	publicBuildKernelSHA256 := flags.String("public-build-kernel-sha256", base.PublicBuildKernelSHA256, "pinned Public Build guest kernel SHA-256")
	publicBuildRootFS := flags.String("public-build-rootfs", base.PublicBuildRootFSPath, "immutable raw Public Build guest rootfs")
	publicBuildRootFSSHA256 := flags.String("public-build-rootfs-sha256", base.PublicBuildRootFSSHA256, "pinned Public Build guest rootfs SHA-256")
	publicBuildGuestContract := flags.String("public-build-guest-contract", base.PublicBuildGuestContract, "pinned Public Build guest protocol contract")
	publicBuildContractSHA256 := flags.String("public-build-contract-sha256", base.PublicBuildContractSHA256, "pinned Public Build guest contract SHA-256")
	publicBuildCgroupRoot := flags.String("public-build-cgroup-root", base.PublicBuildCgroupRoot, "delegated Public Build cgroup v2 directory")
	publicBuildWorkRoot := flags.String("public-build-work-root", base.PublicBuildWorkRoot, "root-owned Public Build worker scratch directory")
	publicBuildMaxScratchBytes := flags.String("public-build-max-scratch-bytes", strconv.FormatInt(base.PublicBuildMaxScratchBytes, 10), "maximum aggregate scratch bytes for one Public Build")
	publicBuildSandboxUID := flags.Uint("public-build-sandbox-uid", uint(base.PublicBuildSandboxUID), "dedicated non-root QEMU UID")
	publicBuildSandboxGID := flags.Uint("public-build-sandbox-gid", uint(base.PublicBuildSandboxGID), "dedicated QEMU GID with KVM access")
	nonInteractive := flags.Bool("non-interactive", false, "accept supplied settings")
	preview := flags.Bool("preview", false, "show intended changes without writing")
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	localTokenSet, err := resolveSecretFlag(flags, "local-token", "local-token-file", localToken, *localTokenFile)
	if err != nil {
		return err
	}
	teamTokenSet, err := resolveSecretFlag(flags, "team-token", "team-token-file", teamToken, *teamTokenFile)
	if err != nil {
		return err
	}
	cloudPostgresURLSet, err := resolveSecretFlag(flags, "cloud-postgres-url", "cloud-postgres-url-file", cloudPostgresURL, *cloudPostgresURLFile)
	if err != nil {
		return err
	}
	cloudS3AccessKeySet, err := resolveSecretFlag(flags, "cloud-s3-access-key", "cloud-s3-access-key-file", cloudS3AccessKey, *cloudS3AccessKeyFile)
	if err != nil {
		return err
	}
	cloudS3SecretKeySet, err := resolveSecretFlag(flags, "cloud-s3-secret-key", "cloud-s3-secret-key-file", cloudS3SecretKey, *cloudS3SecretKeyFile)
	if err != nil {
		return err
	}
	publicAccessTokenSet, err := resolveSecretFlag(flags, "public-access-token", "public-access-token-file", publicAccessToken, *publicAccessTokenFile)
	if err != nil {
		return err
	}
	publisherTokenSet, err := resolveSecretFlag(flags, "publisher-token", "publisher-token-file", publisherToken, *publisherTokenFile)
	if err != nil {
		return err
	}
	publicBuildWorkerTokenSet, err := resolveSecretFlag(flags, "public-build-worker-token", "public-build-worker-token-file", publicBuildWorkerToken, *publicBuildWorkerTokenFile)
	if err != nil {
		return err
	}
	publicCollectorTokenSet, err := resolveSecretFlag(flags, "public-collector-token", "public-collector-token-file", publicCollectorToken, *publicCollectorTokenFile)
	if err != nil {
		return err
	}
	resolvedConfigPath, err := canonicalSetupConfigPath(*configPath)
	if err != nil {
		return err
	}
	*configPath = resolvedConfigPath
	if !*nonInteractive && !*preview {
		if err := runInteractiveSetup(base.ProjectRoot, role, projectID, maxSize, teamURL, stderr); err != nil {
			return err
		}
	}
	maxBytes, err := strconv.ParseInt(*maxSize, 10, 64)
	if err != nil || maxBytes <= 0 {
		return fmt.Errorf("invalid --max-size %q: use a positive byte count", *maxSize)
	}
	minFreeBytes, err := strconv.ParseInt(*minFreeBytesFlag, 10, 64)
	if err != nil || minFreeBytes < 0 {
		return fmt.Errorf("invalid --min-free-bytes %q: use a non-negative byte count", *minFreeBytesFlag)
	}
	buildkitBudget, err := strconv.ParseInt(*buildkitGCBytes, 10, 64)
	if err != nil || buildkitBudget <= 0 {
		return fmt.Errorf("invalid --buildkit-gc-bytes %q: use a positive byte count", *buildkitGCBytes)
	}
	workerScratchBudget, err := strconv.ParseInt(*publicBuildMaxScratchBytes, 10, 64)
	if err != nil || workerScratchBudget < 0 {
		return fmt.Errorf("invalid --public-build-max-scratch-bytes %q: use a non-negative byte count", *publicBuildMaxScratchBytes)
	}
	if uint64(*publicBuildSandboxUID) > uint64(^uint32(0)) || uint64(*publicBuildSandboxGID) > uint64(^uint32(0)) {
		return errors.New("Public Build sandbox UID or GID is outside the uint32 range")
	}
	absoluteDataDir, err := filepath.Abs(*dataDir)
	if err != nil {
		return fmt.Errorf("resolve data directory: %w", err)
	}
	base.DataDir, err = resolveThroughExistingAncestor(filepath.Clean(absoluteDataDir))
	if err != nil {
		return fmt.Errorf("resolve Local Cache directory symlinks: %w", err)
	}
	base.Listen = *listen
	base.MaxBytes = maxBytes
	base.EvictionPolicy, err = retention.Normalize(retention.Policy(*evictionPolicy))
	if err != nil {
		return err
	}
	base.MinFreeBytes = minFreeBytes
	base.Role = *role
	base.ProjectID = *projectID
	base.CompatibilityID = *compatibilityID
	if localTokenSet {
		base.LocalToken = *localToken
	}
	base.TeamURL = strings.TrimRight(*teamURL, "/")
	if teamTokenSet {
		base.TeamToken = *teamToken
		base.TeamTokenExpiresAt = time.Time{}
	}
	base.GitHubAPIURL = strings.TrimRight(*githubAPIURL, "/")
	base.GitHubOIDCIssuer = strings.TrimRight(*githubOIDCIssuer, "/")
	base.TeamMembers, err = parseTeamMembers(teamMembers.Values())
	if err != nil {
		return err
	}
	if cloudPostgresURLSet {
		base.CloudPostgresURL = strings.TrimSpace(*cloudPostgresURL)
	}
	base.CloudS3Endpoint = strings.TrimRight(*cloudS3Endpoint, "/")
	base.CloudS3Bucket = strings.TrimSpace(*cloudS3Bucket)
	base.CloudS3Region = strings.TrimSpace(*cloudS3Region)
	if cloudS3AccessKeySet {
		base.CloudS3AccessKey = *cloudS3AccessKey
	}
	if cloudS3SecretKeySet {
		base.CloudS3SecretKey = *cloudS3SecretKey
	}
	base.CloudS3UsePathStyle = *cloudS3UsePathStyle
	base.CloudStageTTL = *cloudStageTTL
	base.CloudBlobGrace = *cloudBlobGrace
	base.RemoteMetadataTimeout = *remoteMetadataTimeout
	base.RemoteTransferIdleTimeout = *remoteTransferIdleTimeout
	base.PublicURL = strings.TrimRight(*publicURL, "/")
	if publicAccessTokenSet {
		base.PublicAccessToken = *publicAccessToken
		base.PublicAccessTokenExpiresAt = time.Time{}
	}
	base.PublicTrustKey = *publicTrustKey
	if publisherTokenSet {
		base.PublisherToken = *publisherToken
	}
	if publicBuildWorkerTokenSet {
		base.PublicBuildWorkerToken = *publicBuildWorkerToken
	}
	if publicCollectorTokenSet {
		base.PublicCollectorToken = *publicCollectorToken
	}
	base.ActionsRepository = *actionsRepository
	if !flagWasSet(flags, "actions-repository") && base.ProjectID != original.ProjectID &&
		strings.HasPrefix(base.ProjectID, "github.com/") {
		base.ActionsRepository = strings.TrimPrefix(base.ProjectID, "github.com/")
	}
	if base.ActionsRepository == "" {
		base.ActionsRepository = strings.TrimPrefix(base.ProjectID, "github.com/")
	}
	base.ActionsRef = *actionsRef
	base.ActionsDefaultRef = *actionsDefaultRef
	base.ActionsPublicRecipeDigest = strings.TrimSpace(*actionsPublicRecipe)
	base.ActionsPublicBuilder = strings.TrimSpace(*actionsPublicBuilder)
	base.ActionsArchiveBaseURL = strings.TrimRight(*actionsArchiveBaseURL, "/")
	base.BuildkitBuilder = *buildkitBuilder
	base.BuildkitTeamRepository = strings.TrimRight(*buildkitTeamRepository, "/")
	base.BuildkitPublicRepository = strings.TrimRight(*buildkitPublicRepository, "/")
	base.BuildkitBranch = strings.TrimSpace(*buildkitBranch)
	base.BuildkitGCBytes = buildkitBudget
	base.PublicBuildRepositories = publicBuildRepositories.Values()
	base.PublicBuildApprovedRefs = publicBuildApprovedRefs.Values()
	base.PublicBuildRecipeDigests = publicBuildRecipeDigests.Values()
	base.PublicBuildKernelPath = strings.TrimSpace(*publicBuildKernel)
	base.PublicBuildKernelSHA256 = strings.TrimSpace(*publicBuildKernelSHA256)
	base.PublicBuildRootFSPath = strings.TrimSpace(*publicBuildRootFS)
	base.PublicBuildRootFSSHA256 = strings.TrimSpace(*publicBuildRootFSSHA256)
	base.PublicBuildGuestContract = strings.TrimSpace(*publicBuildGuestContract)
	base.PublicBuildContractSHA256 = strings.TrimSpace(*publicBuildContractSHA256)
	base.PublicBuildCgroupRoot = strings.TrimSpace(*publicBuildCgroupRoot)
	base.PublicBuildWorkRoot = strings.TrimSpace(*publicBuildWorkRoot)
	base.PublicBuildMaxScratchBytes = workerScratchBudget
	base.PublicBuildSandboxUID = uint32(*publicBuildSandboxUID)
	base.PublicBuildSandboxGID = uint32(*publicBuildSandboxGID)
	capabilityScopeChanged := original.ProjectID != base.ProjectID ||
		original.CompatibilityID != base.CompatibilityID ||
		original.ActionsRepository != base.ActionsRepository ||
		original.ActionsRef != base.ActionsRef ||
		original.ActionsDefaultRef != base.ActionsDefaultRef
	teamCapabilityScopeChanged := original.TeamURL != base.TeamURL || capabilityScopeChanged
	publicCapabilityScopeChanged := original.PublicURL != base.PublicURL || capabilityScopeChanged
	if teamCapabilityScopeChanged && !teamTokenSet {
		base.TeamToken = ""
		base.TeamTokenExpiresAt = time.Time{}
	}
	if publicCapabilityScopeChanged && !publicAccessTokenSet {
		base.PublicAccessToken = ""
		base.PublicAccessTokenExpiresAt = time.Time{}
	}
	credentialAccountToDelete := ""
	if original.TeamURL != base.TeamURL || original.PublicURL != base.PublicURL || original.ProjectID != base.ProjectID {
		base.GitHubCLIPath = ""
	}
	if original.GitHubCredentialAccount != "" &&
		(original.TeamURL != base.TeamURL || original.PublicURL != base.PublicURL || original.ProjectID != base.ProjectID) {
		credentialAccountToDelete = original.GitHubCredentialAccount
		base.GitHubCredentialAccount = ""
	}
	if base.BypassAdapters == nil {
		base.BypassAdapters = []string{}
	}
	if base.InstallationID == "" {
		installationToken, tokenErr := config.NewToken()
		if tokenErr != nil {
			return tokenErr
		}
		base.InstallationID = "install-" + installationToken[:24]
	}
	if base.Role == "public" {
		if base.PublicBuildWorkerToken == "" {
			base.PublicBuildWorkerToken, err = config.NewToken()
			if err != nil {
				return err
			}
		}
		if base.PublicCollectorToken == "" {
			base.PublicCollectorToken, err = config.NewToken()
			if err != nil {
				return err
			}
		}
		if base.PublicTrustKey == "" && base.PublicPrivateKey == "" {
			publicKey, privateKey, keyErr := ed25519.GenerateKey(rand.Reader)
			if keyErr != nil {
				return fmt.Errorf("generate Public Cache signing key: %w", keyErr)
			}
			base.PublicTrustKey = publictrust.EncodePublicKey(publicKey)
			base.PublicPrivateKey = publictrust.EncodePrivateKey(privateKey)
		}
	}
	if err := base.Validate(); err != nil {
		return fmt.Errorf("invalid setup: %w", err)
	}
	detectedIntegrations := project.DetectIntegrations(base.ProjectRoot)
	result := map[string]any{
		"configured":           !*preview,
		"preview":              *preview,
		"configPath":           *configPath,
		"dataDir":              base.DataDir,
		"maxBytes":             base.MaxBytes,
		"evictionPolicy":       base.EvictionPolicy,
		"minFreeBytes":         base.MinFreeBytes,
		"role":                 base.Role,
		"projectId":            base.ProjectID,
		"configuration":        redactedConfiguration(base),
		"detectedIntegrations": detectedIntegrations,
	}
	if *preview {
		if *jsonOutput {
			return printResult(stdout, true, result, "")
		}
		return printSetupPreview(stdout, *configPath, base, detectedIntegrations)
	}
	if existing && !reflect.DeepEqual(original, base) {
		if err := rejectConfigurationMutationWhileRuntimeActive(original); err != nil {
			return err
		}
	}
	if existing {
		if err := rejectOwnedDataDirectoryChange(original, base.DataDir); err != nil {
			return err
		}
	}
	unlockConfiguration, err := lockConfiguration(ctx, *configPath)
	if err != nil {
		return err
	}
	defer unlockConfiguration()
	var unlockIntegrations func()
	if existing {
		unlockIntegrations, err = lockIntegrationState(ctx, original)
		if err != nil {
			return err
		}
		defer unlockIntegrations()
	}
	if existing {
		current, loadErr := config.Load(*configPath)
		if loadErr != nil {
			return fmt.Errorf("reload Layer Cache configuration before setup commit: %w", loadErr)
		}
		if !reflect.DeepEqual(current, original) {
			return errors.New("Layer Cache configuration changed during setup; rerun setup against the current configuration")
		}
		if err := rejectOwnedIntegrationConfigChange(original, base); err != nil {
			return err
		}
		if err := rejectConfigurationMutationWhileRuntimeActive(current); err != nil {
			return err
		}
	} else if nowExists, existsErr := config.Exists(*configPath); existsErr != nil {
		return fmt.Errorf("recheck Layer Cache configuration before setup commit: %w", existsErr)
	} else if nowExists {
		return errors.New("Layer Cache configuration was created during setup; rerun setup against the current configuration")
	}
	if err := validateDeletionTarget(base.DataDir); err != nil {
		return fmt.Errorf("unsafe Local Cache directory: %w", err)
	}
	if err := rejectOwnershipConflict(base, *configPath); err != nil {
		return err
	}
	transaction, err := beginSetupTransaction(*configPath, base.DataDir)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rollbackErr := transaction.Rollback(); rollbackErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("roll back interrupted Layer Cache setup: %w", rollbackErr))
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(base.DataDir, 0o700); err != nil {
		return fmt.Errorf("create Local Cache directory: %w", err)
	}
	if err := os.Chmod(base.DataDir, 0o700); err != nil {
		return fmt.Errorf("protect Local Cache directory: %w", err)
	}
	if !existing || !reflect.DeepEqual(original, base) || !configFileIsProtected(*configPath) {
		if err := config.Save(*configPath, base); err != nil {
			return err
		}
	}
	if err := ensureOwnershipMarker(base, *configPath); err != nil {
		return err
	}
	if persistLocalIdentity && !flagWasSet(flags, "project") {
		if err := project.SaveLocalID(ctx, localIdentityRoot, base.ProjectID); err != nil {
			return err
		}
	}
	committed = true
	if credentialAccountToDelete != "" {
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), controlRequestTimeout)
		cleanupErr := (credentials.Store{}).Delete(cleanupContext, credentialAccountToDelete)
		cancel()
		if cleanupErr != nil {
			_, _ = fmt.Fprintf(stderr, "Layer Cache warning: configuration was safely rescoped, but the old protected GitHub session could not be removed: %v\n", cleanupErr)
		}
	}
	return printResult(stdout, *jsonOutput, result, "Layer Cache configured at "+*configPath)
}

func printSetupPreview(output io.Writer, configPath string, cfg config.Config, detected []string) error {
	if _, err := fmt.Fprintln(output, "Layer Cache setup preview"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "Configuration file: %s\n", configPath); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "Runtime: %s role listening on %s\n", cfg.Role, cfg.Listen); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "Project: %s\nCompatibility: %s\n", cfg.ProjectID, cfg.CompatibilityID); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "Local Cache: %s\n", cfg.DataDir); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "Capacity: %s limit; %s minimum filesystem reserve\n",
		formatByteCount(cfg.MaxBytes), formatByteCount(cfg.MinFreeBytes)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "BuildKit Local Cache: %s builder; %s budget\n",
		cfg.BuildkitBuilder, formatByteCount(cfg.BuildkitGCBytes)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "Team Cache: %s\n", setupRemoteDescription(cfg.TeamURL, cfg.Role == "team")); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "Public Cache: %s\n", setupRemoteDescription(cfg.PublicURL, cfg.Role == "public")); err != nil {
		return err
	}
	detected = append([]string(nil), detected...)
	slices.Sort(detected)
	detectedDescription := "none"
	if len(detected) > 0 {
		detectedDescription = strings.Join(detected, ", ")
	}
	if _, err := fmt.Fprintf(output, "Detected integrations: %s\n", detectedDescription); err != nil {
		return err
	}
	if cfg.Role == "public" || len(cfg.PublicBuildRepositories) > 0 || len(cfg.PublicBuildRecipeDigests) > 0 {
		if _, err := fmt.Fprintf(output, "Public Build policy: %d repositories; %d approved refs; %d recipes; %s scratch limit\n",
			len(cfg.PublicBuildRepositories), len(cfg.PublicBuildApprovedRefs), len(cfg.PublicBuildRecipeDigests),
			formatByteCount(cfg.PublicBuildMaxScratchBytes)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(output, "Preview only: no files or integrations were changed.")
	return err
}

func setupRemoteDescription(endpoint string, servedLocally bool) string {
	if endpoint != "" {
		return endpoint
	}
	if servedLocally {
		return "served by this runtime"
	}
	return "disabled"
}

func setupConfigPath(args []string, defaultPath string) string {
	result := defaultPath
	for index, arg := range args {
		if (arg == "--config" || arg == "-config") && index+1 < len(args) {
			result = args[index+1]
		}
		if strings.HasPrefix(arg, "--config=") || strings.HasPrefix(arg, "-config=") {
			result = strings.SplitN(arg, "=", 2)[1]
		}
	}
	return result
}

func rejectOwnedDataDirectoryChange(original config.Config, requested string) error {
	if original.DataDir == requested {
		return nil
	}
	for _, path := range []string{
		filepath.Join(original.DataDir, ownershipMarkerName),
		integrationStatePath(original),
	} {
		_, err := os.Lstat(path)
		if err == nil {
			return fmt.Errorf(
				"cannot change --data-dir from %s to %s while Layer Cache ownership metadata remains in the original directory; uninstall Layer Cache first",
				original.DataDir, requested,
			)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect existing Layer Cache ownership metadata at %s: %w", path, err)
		}
	}
	return nil
}

func canonicalSetupConfigPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve Layer Cache configuration path: %w", err)
	}
	absolute = filepath.Clean(absolute)
	if info, statErr := os.Lstat(absolute); statErr == nil {
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("Layer Cache configuration path %s is not a regular file", absolute)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", fmt.Errorf("inspect Layer Cache configuration path: %w", statErr)
	}
	resolvedParent, err := resolveThroughExistingAncestor(filepath.Dir(absolute))
	if err != nil {
		return "", fmt.Errorf("resolve Layer Cache configuration directory symlinks: %w", err)
	}
	return filepath.Join(resolvedParent, filepath.Base(absolute)), nil
}

func flagWasSet(flags *flag.FlagSet, name string) bool {
	found := false
	flags.Visit(func(setFlag *flag.Flag) {
		if setFlag.Name == name {
			found = true
		}
	})
	return found
}

func teamMemberValues(members map[string]string) []string {
	values := make([]string, 0, len(members))
	for login, role := range members {
		values = append(values, login+"="+role)
	}
	slices.Sort(values)
	return values
}

func parseTeamMembers(values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	members := make(map[string]string, len(values))
	for _, value := range values {
		login, role, found := strings.Cut(value, "=")
		login = strings.ToLower(strings.TrimSpace(login))
		role = strings.ToLower(strings.TrimSpace(role))
		if !found || login == "" || (role != "reader" && role != "writer" && role != "admin") {
			return nil, fmt.Errorf("invalid --team-member %q: use LOGIN=reader|writer|admin", value)
		}
		if _, exists := members[login]; exists {
			return nil, fmt.Errorf("duplicate --team-member login %q", login)
		}
		members[login] = role
	}
	return members, nil
}

func runInteractiveSetup(root string, role, projectID, maxSize, teamURL *string, output io.Writer) error {
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return errors.New("interactive setup needs a terminal; use --preview or --non-interactive")
	}
	reader := bufio.NewReader(os.Stdin)
	if *role, err = promptSetupValue(reader, output, "Runtime role (local, team, public)", *role); err != nil {
		return err
	}
	if *projectID, err = promptSetupValue(reader, output, "Project identity", *projectID); err != nil {
		return err
	}
	if *maxSize, err = promptSetupValue(reader, output, "Local Cache byte budget", *maxSize); err != nil {
		return err
	}
	if *teamURL, err = promptSetupValue(reader, output, "Team Cache URL (leave blank for Local-only)", *teamURL); err != nil {
		return err
	}
	detected := project.DetectIntegrations(root)
	if len(detected) > 0 {
		_, _ = fmt.Fprintf(output, "Detected integrations: %s\n", strings.Join(detected, ", "))
	}
	if *teamURL != "" {
		_, _ = fmt.Fprintln(output, "Team Cache may receive opaque build outputs and product-owned cache timing metadata. Raw paths, environment values, and unhashed cache keys are excluded from telemetry.")
	}
	confirmation, err := promptSetupValue(reader, output, "Apply this configuration? (yes/no)", "no")
	if err != nil {
		return err
	}
	if !strings.EqualFold(confirmation, "yes") && !strings.EqualFold(confirmation, "y") {
		return errors.New("setup cancelled without changes")
	}
	return nil
}

func promptSetupValue(reader *bufio.Reader, output io.Writer, label, current string) (string, error) {
	if _, err := fmt.Fprintf(output, "%s [%s]: ", label, current); err != nil {
		return "", err
	}
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return current, nil
	}
	return line, nil
}

func configFileIsProtected(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o077 == 0
}

func runStatus(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "configuration file")
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load Layer Cache status: %w", err)
	}
	status := inspectOperationalStatus(ctx, cfg)
	if *jsonOutput {
		return json.NewEncoder(stdout).Encode(status)
	}
	return printOperationalStatus(stdout, status)
}

func printResult(stdout io.Writer, jsonOutput bool, value any, message string) error {
	if !jsonOutput {
		_, err := fmt.Fprintln(stdout, message)
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}
