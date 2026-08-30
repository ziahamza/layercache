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
	"strconv"
	"strings"

	"github.com/layercache/layercache/internal/config"
)

func runPublic(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("a public command is required")
	}
	switch args[0] {
	case "trust-key":
		return runPublicTrustKey(args[1:], stdout, stderr)
	case "publish":
		return runPublicPublish(ctx, args[1:], stdout, stderr)
	case "revoke":
		return runPublicRevoke(ctx, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown public command %q", args[0])
	}
}

func runPublicRevoke(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("public revoke", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "Public Cache configuration file")
	hash := flags.String("hash", "", "Turbo artifact hash")
	reason := flags.String("reason", "", "revocation reason")
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *hash == "" || *reason == "" {
		return errors.New("--hash and --reason are required")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{
		"integration": "turbo", "project": cfg.ProjectID,
		"compatibility": cfg.CompatibilityID, "nativeKey": *hash, "reason": *reason,
	})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+cfg.Listen+"/v1/public/revoke", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+cfg.PublisherToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := newCLIHTTPClient(controlRequestTimeout).Do(request)
	if err != nil {
		return fmt.Errorf("revoke Public Cache artifact: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return fmt.Errorf("Public Cache revocation returned HTTP %d: %s", response.StatusCode, message)
	}
	return printResult(stdout, *jsonOutput, map[string]any{"revoked": true, "hash": *hash}, "Public Cache artifact revoked")
}

func runPublicTrustKey(args []string, stdout, stderr io.Writer) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("public trust-key", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "Public Cache configuration file")
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.Role != "public" || cfg.PublicTrustKey == "" {
		return errors.New("configuration does not own a Public Cache trust key")
	}
	return printResult(stdout, *jsonOutput, map[string]string{"publicKey": cfg.PublicTrustKey}, cfg.PublicTrustKey)
}

func runPublicPublish(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("public publish", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "Public Cache configuration file")
	filePath := flags.String("file", "", "artifact file")
	hash := flags.String("hash", "", "Turbo artifact hash")
	repository := flags.String("repository", "", "canonical source repository")
	commit := flags.String("commit", "", "immutable source commit")
	recipe := flags.String("recipe", "", "recipe digest")
	platform := flags.String("platform", "", "target platform")
	toolchain := flags.String("toolchain", "", "toolchain identity")
	builder := flags.String("builder", "", "builder identity")
	buildID := flags.String("build-id", "", "Public Build identity")
	duration := flags.Int64("duration", 0, "producer duration in milliseconds")
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *filePath == "" || *hash == "" {
		return errors.New("--file and --hash are required")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.Role != "public" || cfg.PublisherToken == "" {
		return errors.New("configuration is not a Public Cache publisher")
	}
	file, err := os.Open(*filePath)
	if err != nil {
		return fmt.Errorf("open public artifact: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect public artifact: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+cfg.Listen+"/v1/public/publish", file)
	if err != nil {
		return err
	}
	request.ContentLength = info.Size()
	request.Header.Set("Authorization", "Bearer "+cfg.PublisherToken)
	request.Header.Set("x-layercache-integration", "turbo")
	request.Header.Set("x-layercache-project", cfg.ProjectID)
	request.Header.Set("x-layercache-compatibility", cfg.CompatibilityID)
	request.Header.Set("x-layercache-native-key", *hash)
	request.Header.Set("x-layercache-repository", *repository)
	request.Header.Set("x-layercache-commit", *commit)
	request.Header.Set("x-layercache-recipe", *recipe)
	request.Header.Set("x-layercache-platform", *platform)
	request.Header.Set("x-layercache-toolchain", *toolchain)
	request.Header.Set("x-layercache-builder", *builder)
	request.Header.Set("x-layercache-build-id", *buildID)
	request.Header.Set("x-layercache-duration", strconv.FormatInt(*duration, 10))
	response, err := newCLIHTTPClient(publishRequestTimeout).Do(request)
	if err != nil {
		return fmt.Errorf("publish Public Cache artifact: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return fmt.Errorf("Public Cache publication returned HTTP %d: %s", response.StatusCode, message)
	}
	var result map[string]any
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil {
		return fmt.Errorf("decode Public Cache publication: %w", err)
	}
	return printResult(stdout, *jsonOutput, result, "Public Cache artifact published")
}
