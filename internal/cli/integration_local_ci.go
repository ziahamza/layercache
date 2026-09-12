package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/config"
)

func runLocalCIIntegration(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("integration local-ci", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "configuration file")
	envPathFlag := flags.String("env-file", "", "protected Local CI environment handoff file")
	apply := flags.Bool("apply", false, "write or refresh the Local CI environment handoff")
	force := flags.Bool("force", false, "replace an unowned environment handoff file")
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("integration local-ci accepts no positional arguments")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load Layer Cache configuration: %w", err)
	}
	envPath := *envPathFlag
	if _, err := discoverConfiguredProject(ctx, cfg, "", flagWasSet(flags, "config") || cfg.ProjectRoot == ""); err != nil {
		return err
	}
	if envPath == "" {
		envPath = filepath.Join(cfg.DataDir, "integrations", "local-ci.env")
	}
	envPath, err = filepath.Abs(envPath)
	if err != nil {
		return fmt.Errorf("resolve Local CI environment path: %w", err)
	}
	envPath, err = resolveThroughExistingAncestor(filepath.Clean(envPath))
	if err != nil {
		return fmt.Errorf("resolve Local CI environment path symlinks: %w", err)
	}
	if *apply {
		unlock, lockErr := lockIntegrationApplication(ctx, cfg, *configPath, "Local CI")
		if lockErr != nil {
			return lockErr
		}
		defer unlock()
	}
	state, err := loadIntegrationState(cfg)
	if err != nil {
		return err
	}
	record, owned := state.Records["local-ci"]
	if *apply {
		if err := validateOwnedIntegrationTarget(state, "local-ci", envPath); err != nil {
			return err
		}
	}
	current, exists, err := readRegularFile(envPath, 1<<20)
	if err != nil {
		return fmt.Errorf("inspect Local CI environment handoff: %w", err)
	}
	owned = owned && record.OwnershipCaptured && record.Path == envPath && record.Digest != "" && contentDigest(current) == record.Digest
	endpoint := localRuntimeURL(cfg.Listen) + "/"
	reusable := exists && owned && record.Endpoint == endpoint && record.CredentialExpiresAt.After(time.Now().UTC().Add(5*time.Minute))
	if exists && !owned && *apply && !*force {
		return fmt.Errorf("%s is not an unchanged Layer Cache-owned handoff; choose another --env-file or rerun with --force", envPath)
	}
	result := map[string]any{
		"integration": "local-ci", "preview": !*apply, "envFile": envPath,
		"cacheUrl": endpoint, "changed": !reusable, "credentialWrittenToStdout": false,
		"upstreamConfiguration": "explicit-env-file-seam",
	}
	if reusable {
		result["credentialExpiresAt"] = record.CredentialExpiresAt
	}
	if !*apply {
		return printIntegrationResult(stdout, *jsonOutput, result,
			fmt.Sprintf("Local CI preview: write a protected endpoint and short-lived credential handoff to %s", envPath))
	}
	if reusable {
		result["active"] = true
		return printIntegrationResult(stdout, *jsonOutput, result,
			fmt.Sprintf("Local CI handoff is current at %s", envPath))
	}
	token, expiresAt, err := mintLocalIntegrationCapability(ctx, cfg, "actions")
	if err != nil {
		return fmt.Errorf("issue short-lived Actions capability: %w", err)
	}
	contents := localCIEnvironment(endpoint, token, expiresAt)
	now := time.Now().UTC()
	record.Name = "local-ci"
	record.Path = envPath
	record.Endpoint = endpoint
	record.AppliedAt = now
	record.CredentialExpiresAt = expiresAt
	state, err = commitOwnedFile(cfg, "local-ci", state, record, contents)
	if err != nil {
		return err
	}
	result["active"] = true
	result["credentialExpiresAt"] = expiresAt
	return printIntegrationResult(stdout, *jsonOutput, result,
		fmt.Sprintf("Local CI handoff written to %s with mode 0600; configure Local CI to load this file through its endpoint seam", envPath))
}

func localCIEnvironment(endpoint, token string, expiresAt time.Time) []byte {
	return []byte(fmt.Sprintf(
		"# Layer Cache Local CI handoff. Refresh with: layercache integration local-ci --apply\n"+
			"# Credential expires at %s. Do not commit this file.\n"+
			"ACTIONS_CACHE_URL=%s\n"+
			"ACTIONS_RUNTIME_TOKEN=%s\n"+
			"ACTIONS_CACHE_SERVICE_V2=\n"+
			"LOCAL_CI_ACTIONS_CACHE_URL=%s\n"+
			"LOCAL_CI_ACTIONS_RUNTIME_TOKEN=%s\n",
		expiresAt.UTC().Format(time.RFC3339), shellQuote(endpoint), shellQuote(token),
		shellQuote(endpoint), shellQuote(token),
	))
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
