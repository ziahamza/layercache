package cli

import (
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
	"strconv"
	"strings"
	"syscall"

	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/publictrust"
	"github.com/layercache/layercache/internal/server"
)

const usage = `Layer Cache

Usage:
  layercache setup [options]
  layercache start [options]
  layercache stop [options]
  layercache status [options]
  layercache doctor [options]
  layercache repair [options]
  layercache gc [options]
  layercache bypass --adapter turbo|actions|buildkit|all [options]
  layercache uninstall --preserve-cache|--delete-cache [options]
  layercache report --run RUN_ID [options]
  layercache report --from RFC3339 --to RFC3339 [options]
  layercache backtest --input FILE --retention DURATION --source CACHE [options]
  layercache integration turbo [options]
  layercache integration actions [options]
  layercache buildx plan|build [options]
  layercache public-build request|status|logs|cancel [options]
  layercache public-build worker lease|append-log|complete|fail [options]
  layercache run [options] -- COMMAND [ARG...]
  layercache serve [options]
`

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, _ = io.WriteString(stderr, usage)
		return errors.New("a command is required")
	}
	switch args[0] {
	case "setup":
		return runSetup(args[1:], stdout, stderr)
	case "status":
		return runStatus(ctx, args[1:], stdout, stderr)
	case "start":
		return runStart(args[1:], stdout, stderr)
	case "stop":
		return runStop(args[1:], stdout, stderr)
	case "doctor":
		return runDoctor(args[1:], stdout, stderr)
	case "repair":
		return runRepair(args[1:], stdout, stderr)
	case "gc":
		return runGC(ctx, args[1:], stdout, stderr)
	case "bypass":
		return runBypass(args[1:], stdout, stderr)
	case "uninstall":
		return runUninstall(args[1:], stdout, stderr)
	case "integration":
		return runIntegration(args[1:], stdout, stderr)
	case "serve":
		return runServe(ctx, args[1:], stderr)
	case "public":
		return runPublic(ctx, args[1:], stdout, stderr)
	case "public-build":
		return runPublicBuild(ctx, args[1:], stdout, stderr)
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

func runIntegration(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("an integration name is required")
	}
	if args[0] != "turbo" && args[0] != "actions" {
		return fmt.Errorf("unknown integration %q", args[0])
	}
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("integration "+args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "configuration file")
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load Layer Cache configuration: %w", err)
	}
	if args[0] == "actions" {
		result := map[string]any{
			"cacheUrl":   "http://" + cfg.Listen + "/",
			"token":      cfg.LocalToken,
			"repository": cfg.ActionsRepository,
			"ref":        cfg.ActionsRef,
		}
		if *jsonOutput {
			return printResult(stdout, true, result, "")
		}
		_, err = fmt.Fprintf(stdout, "ACTIONS_CACHE_URL=%s\nACTIONS_RUNTIME_TOKEN=%s\n", result["cacheUrl"], result["token"])
		return err
	}
	result := map[string]any{"apiUrl": "http://" + cfg.Listen, "token": cfg.LocalToken, "team": cfg.ProjectID}
	if *jsonOutput {
		return printResult(stdout, true, result, "")
	}
	_, err = fmt.Fprintf(stdout, "TURBO_API=%s\nTURBO_TOKEN=%s\nTURBO_TEAM=%s\n", result["apiUrl"], result["token"], result["team"])
	return err
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
	serveContext, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if !strings.HasPrefix(cfg.Listen, "127.0.0.1:") && !strings.HasPrefix(cfg.Listen, "[::1]:") && cfg.LocalToken == "" {
		return errors.New("refusing to expose a cache runtime without authentication")
	}
	_, _ = fmt.Fprintf(stderr, "Layer Cache listening on http://%s\n", cfg.Listen)
	return server.RunHTTP(serveContext, cfg)
}

func runSetup(args []string, stdout, stderr io.Writer) error {
	base, err := config.Defaults()
	if err != nil {
		return err
	}
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	requestedConfigPath := setupConfigPath(args, defaultPath)
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
	original := base
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", requestedConfigPath, "configuration file")
	dataDir := flags.String("data-dir", base.DataDir, "host Local Cache directory")
	listen := flags.String("listen", base.Listen, "loopback HTTP address")
	maxSize := flags.String("max-size", strconv.FormatInt(base.MaxBytes, 10), "Local Cache byte limit")
	minFreeBytesFlag := flags.String("min-free-bytes", strconv.FormatInt(base.MinFreeBytes, 10), "upward Local Cache free-space reserve override in bytes")
	role := flags.String("role", base.Role, "runtime role: local, team, or public")
	project := flags.String("project", base.ProjectID, "project identity")
	compatibilityID := flags.String("compatibility-id", base.CompatibilityID, "output compatibility identity (OS, architecture, ABI, and toolchain as needed)")
	localToken := flags.String("local-token", "", "bearer token accepted by this runtime (preserved when omitted)")
	teamURL := flags.String("team-url", base.TeamURL, "Team Cache URL")
	teamToken := flags.String("team-token", "", "Team Cache bearer token (preserved when omitted)")
	publicURL := flags.String("public-url", base.PublicURL, "Public Cache URL")
	publicTrustKey := flags.String("public-trust-key", base.PublicTrustKey, "pinned Public Cache Ed25519 key")
	publisherToken := flags.String("publisher-token", "", "trusted Public Build publisher token (preserved when omitted)")
	actionsRepositoryDefault := base.ActionsRepository
	if !existing {
		actionsRepositoryDefault = ""
	}
	actionsRepository := flags.String("actions-repository", actionsRepositoryDefault, "GitHub Actions owner/repository scope")
	actionsRef := flags.String("actions-ref", base.ActionsRef, "GitHub Actions current ref")
	actionsDefaultRef := flags.String("actions-default-ref", base.ActionsDefaultRef, "GitHub Actions default branch ref")
	actionsArchiveBaseURL := flags.String("actions-archive-base-url", base.ActionsArchiveBaseURL, "external HTTPS base URL for signed Actions archives")
	buildkitBuilder := flags.String("buildkit-builder", base.BuildkitBuilder, "persistent BuildKit builder name")
	buildkitTeamRepository := flags.String("buildkit-team-repository", base.BuildkitTeamRepository, "Team Cache OCI repository")
	publicBuildRepositories := newRepeatableStringFlag(base.PublicBuildRepositories)
	flags.Var(publicBuildRepositories, "public-build-repository", "allowlisted immutable Public Build repository (repeatable)")
	nonInteractive := flags.Bool("non-interactive", false, "accept supplied settings")
	preview := flags.Bool("preview", false, "show intended changes without writing")
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*nonInteractive && !*preview {
		return errors.New("interactive setup is not available yet; pass --non-interactive")
	}
	maxBytes, err := strconv.ParseInt(*maxSize, 10, 64)
	if err != nil || maxBytes <= 0 {
		return fmt.Errorf("invalid --max-size %q: use a positive byte count", *maxSize)
	}
	minFreeBytes, err := strconv.ParseInt(*minFreeBytesFlag, 10, 64)
	if err != nil || minFreeBytes < 0 {
		return fmt.Errorf("invalid --min-free-bytes %q: use a non-negative byte count", *minFreeBytesFlag)
	}
	absoluteDataDir, err := filepath.Abs(*dataDir)
	if err != nil {
		return fmt.Errorf("resolve data directory: %w", err)
	}
	base.DataDir = absoluteDataDir
	base.Listen = *listen
	base.MaxBytes = maxBytes
	base.MinFreeBytes = minFreeBytes
	base.Role = *role
	base.ProjectID = *project
	base.CompatibilityID = *compatibilityID
	if flagWasSet(flags, "local-token") {
		base.LocalToken = *localToken
	}
	base.TeamURL = strings.TrimRight(*teamURL, "/")
	if flagWasSet(flags, "team-token") {
		base.TeamToken = *teamToken
	}
	base.PublicURL = strings.TrimRight(*publicURL, "/")
	base.PublicTrustKey = *publicTrustKey
	if flagWasSet(flags, "publisher-token") {
		base.PublisherToken = *publisherToken
	}
	base.ActionsRepository = *actionsRepository
	if base.ActionsRepository == "" {
		base.ActionsRepository = strings.TrimPrefix(base.ProjectID, "github.com/")
	}
	base.ActionsRef = *actionsRef
	base.ActionsDefaultRef = *actionsDefaultRef
	base.ActionsArchiveBaseURL = strings.TrimRight(*actionsArchiveBaseURL, "/")
	base.BuildkitBuilder = *buildkitBuilder
	base.BuildkitTeamRepository = strings.TrimRight(*buildkitTeamRepository, "/")
	base.PublicBuildRepositories = publicBuildRepositories.Values()
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
		if base.PublisherToken == "" {
			base.PublisherToken, err = config.NewToken()
			if err != nil {
				return err
			}
		}
		if base.PublicTrustKey == "" || base.PublicPrivateKey == "" {
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
	result := map[string]any{
		"configured":    !*preview,
		"preview":       *preview,
		"configPath":    *configPath,
		"dataDir":       base.DataDir,
		"maxBytes":      base.MaxBytes,
		"minFreeBytes":  base.MinFreeBytes,
		"role":          base.Role,
		"projectId":     base.ProjectID,
		"configuration": redactedConfiguration(base),
	}
	if *preview {
		return printResult(stdout, *jsonOutput, result, "Layer Cache setup preview")
	}
	if err := validateDeletionTarget(base.DataDir); err != nil {
		return fmt.Errorf("unsafe Local Cache directory: %w", err)
	}
	if err := os.MkdirAll(base.DataDir, 0o700); err != nil {
		return fmt.Errorf("create Local Cache directory: %w", err)
	}
	if err := rejectOwnershipConflict(base, *configPath); err != nil {
		return err
	}
	if !existing || !reflect.DeepEqual(original, base) || !configFileIsProtected(*configPath) {
		if err := config.Save(*configPath, base); err != nil {
			return err
		}
	}
	if err := ensureOwnershipMarker(base, *configPath); err != nil {
		return err
	}
	return printResult(stdout, *jsonOutput, result, "Layer Cache configured at "+*configPath)
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

func flagWasSet(flags *flag.FlagSet, name string) bool {
	found := false
	flags.Visit(func(setFlag *flag.Flag) {
		if setFlag.Name == name {
			found = true
		}
	})
	return found
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
	live, liveErr := probeRuntime(cfg)
	if liveErr != nil {
		stats, statsErr := artifact.ReadStats(ctx, cfg.DataDir)
		if statsErr != nil {
			return fmt.Errorf("read stopped Local Cache status: %w", statsErr)
		}
		live.UsageBytes = stats.UsageBytes
		live.Artifacts = stats.Artifacts
		live.Entries = stats.Entries
	}
	result := map[string]any{
		"configured":         true,
		"running":            liveErr == nil && live.Running,
		"dataDir":            cfg.DataDir,
		"listen":             cfg.Listen,
		"maxBytes":           cfg.MaxBytes,
		"compatibilityId":    cfg.CompatibilityID,
		"usageBytes":         live.UsageBytes,
		"artifacts":          live.Artifacts,
		"entries":            live.Entries,
		"pendingUploads":     live.PendingUploads,
		"pendingUploadBytes": live.PendingUploadBytes,
		"bypassAdapters":     cfg.BypassAdapters,
		"runtimePid":         live.RuntimePID,
		"runtimeInstanceId":  live.RuntimeInstanceID,
	}
	return printResult(stdout, *jsonOutput, result, "Layer Cache is configured")
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
