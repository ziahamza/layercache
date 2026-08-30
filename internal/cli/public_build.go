package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/config"
)

func runPublicBuild(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("a Public Build command is required")
	}
	switch args[0] {
	case "request":
		return runPublicBuildRequest(ctx, args[1:], stdout, stderr)
	case "status":
		return runPublicBuildStatus(ctx, args[1:], stdout, stderr)
	case "logs":
		return runPublicBuildLogs(ctx, args[1:], stdout, stderr)
	case "cancel":
		return runPublicBuildCancel(ctx, args[1:], stdout, stderr)
	case "worker":
		return runPublicBuildWorker(ctx, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown Public Build command %q", args[0])
	}
}

func runPublicBuildRequest(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags, configPath, jsonOutput, err := newPublicBuildFlagSet("public-build request", stderr)
	if err != nil {
		return err
	}
	repository := flags.String("repository", "", "allowlisted public GitHub repository")
	commit := flags.String("commit", "", "immutable 40 or 64 character commit digest")
	integration := flags.String("integration", "", "turbo, buildkit, or actions")
	target := flags.String("target", "", "named recipe target")
	recipe := flags.String("recipe", "", "complete SHA-256 recipe digest")
	platform := flags.String("platform", "", "linux/amd64 or linux/arm64")
	cpuMillis := flags.Int64("cpu-millis", 2_000, "requested CPU in millicores")
	memoryBytes := flags.Int64("memory-bytes", 4<<30, "requested memory in bytes")
	diskBytes := flags.Int64("disk-bytes", 20<<30, "requested ephemeral disk in bytes")
	timeout := flags.Duration("timeout", 30*time.Minute, "requested build timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *repository == "" || *commit == "" || *integration == "" || *target == "" || *recipe == "" || *platform == "" {
		return errors.New("--repository, --commit, --integration, --target, --recipe, and --platform are required")
	}
	if *timeout <= 0 || timeout.Milliseconds() <= 0 {
		return errors.New("--timeout must be at least one millisecond")
	}
	cfg, err := loadPublicBuildConfig(*configPath)
	if err != nil {
		return err
	}
	body := map[string]any{
		"repository": *repository, "commit": *commit, "integration": *integration,
		"target": *target, "recipeDigest": *recipe, "platform": *platform,
		"resources": map[string]any{
			"cpuMillis": *cpuMillis, "memoryBytes": *memoryBytes,
			"diskBytes": *diskBytes, "timeoutMilliseconds": timeout.Milliseconds(),
		},
	}
	encoded, _, err := callPublicBuild(ctx, cfg, http.MethodPost, "/v1/public-builds", cfg.LocalToken, body)
	if err != nil {
		return err
	}
	return printPublicBuildResult(stdout, *jsonOutput, encoded, "Public Build requested")
}

func runPublicBuildStatus(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return runPublicBuildRead(ctx, "status", args, stdout, stderr)
}

func runPublicBuildLogs(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return runPublicBuildRead(ctx, "logs", args, stdout, stderr)
}

func runPublicBuildRead(ctx context.Context, command string, args []string, stdout, stderr io.Writer) error {
	flags, configPath, jsonOutput, err := newPublicBuildFlagSet("public-build "+command, stderr)
	if err != nil {
		return err
	}
	id := flags.String("id", "", "Public Build ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("--id is required")
	}
	cfg, err := loadPublicBuildConfig(*configPath)
	if err != nil {
		return err
	}
	path := "/v1/public-builds/" + url.PathEscape(*id)
	message := "Public Build status"
	if command == "logs" {
		path += "/logs"
		message = "Public Build logs"
	}
	encoded, _, err := callPublicBuild(ctx, cfg, http.MethodGet, path, cfg.LocalToken, nil)
	if err != nil {
		return err
	}
	return printPublicBuildResult(stdout, *jsonOutput, encoded, message)
}

func runPublicBuildCancel(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags, configPath, jsonOutput, err := newPublicBuildFlagSet("public-build cancel", stderr)
	if err != nil {
		return err
	}
	id := flags.String("id", "", "Public Build ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("--id is required")
	}
	cfg, err := loadPublicBuildConfig(*configPath)
	if err != nil {
		return err
	}
	encoded, _, err := callPublicBuild(ctx, cfg, http.MethodPost, "/v1/public-builds/"+url.PathEscape(*id)+"/cancel", cfg.LocalToken, map[string]any{})
	if err != nil {
		return err
	}
	return printPublicBuildResult(stdout, *jsonOutput, encoded, "Public Build cancelled")
}

func runPublicBuildWorker(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("a Public Build worker command is required")
	}
	switch args[0] {
	case "lease":
		return runPublicBuildWorkerLease(ctx, args[1:], stdout, stderr)
	case "append-log":
		return runPublicBuildWorkerAppendLog(ctx, args[1:], stdout, stderr)
	case "complete":
		return runPublicBuildWorkerComplete(ctx, args[1:], stdout, stderr)
	case "fail":
		return runPublicBuildWorkerFail(ctx, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown Public Build worker command %q", args[0])
	}
}

func runPublicBuildWorkerLease(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags, configPath, jsonOutput, err := newPublicBuildFlagSet("public-build worker lease", stderr)
	if err != nil {
		return err
	}
	workerID := flags.String("worker-id", "", "worker identity")
	integrations := newRepeatableStringFlag(nil)
	platforms := newRepeatableStringFlag(nil)
	flags.Var(integrations, "integration", "supported integration (repeatable)")
	flags.Var(platforms, "platform", "supported platform (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *workerID == "" || len(integrations.Values()) == 0 || len(platforms.Values()) == 0 {
		return errors.New("--worker-id and at least one --integration and --platform are required")
	}
	cfg, err := loadPublicBuildWorkerConfig(*configPath)
	if err != nil {
		return err
	}
	body := map[string]any{"workerId": *workerID, "integrations": integrations.Values(), "platforms": platforms.Values()}
	encoded, status, err := callPublicBuild(ctx, cfg, http.MethodPost, "/v1/public-build-worker/lease", cfg.PublisherToken, body)
	if err != nil {
		return err
	}
	if status == http.StatusNoContent {
		return printResult(stdout, *jsonOutput, map[string]bool{"leased": false}, "No compatible Public Build is queued")
	}
	return printPublicBuildResult(stdout, *jsonOutput, encoded, "Public Build leased")
}

func runPublicBuildWorkerAppendLog(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags, configPath, jsonOutput, id, workerID, leaseToken, err := publicBuildWorkerTransitionFlags("append-log", args, stderr)
	if err != nil {
		return err
	}
	message := flags.Lookup("message")
	if message == nil {
		return errors.New("internal Public Build log flag error")
	}
	cfg, err := loadPublicBuildWorkerConfig(*configPath)
	if err != nil {
		return err
	}
	body := map[string]string{"workerId": workerID, "leaseToken": leaseToken, "message": message.Value.String()}
	_, _, err = callPublicBuild(ctx, cfg, http.MethodPost, "/v1/public-build-worker/"+url.PathEscape(id)+"/logs", cfg.PublisherToken, body)
	if err != nil {
		return err
	}
	return printResult(stdout, *jsonOutput, map[string]any{"appended": true, "buildId": id}, "Public Build log appended")
}

func runPublicBuildWorkerComplete(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags, configPath, jsonOutput, id, workerID, leaseToken, err := publicBuildWorkerTransitionFlags("complete", args, stderr)
	if err != nil {
		return err
	}
	publication := flags.Lookup("publication-identity")
	if publication == nil || publication.Value.String() == "" {
		return errors.New("--publication-identity is required")
	}
	cfg, err := loadPublicBuildWorkerConfig(*configPath)
	if err != nil {
		return err
	}
	body := map[string]string{
		"workerId": workerID, "leaseToken": leaseToken,
		"publicationIdentity": publication.Value.String(),
	}
	encoded, _, err := callPublicBuild(ctx, cfg, http.MethodPost, "/v1/public-build-worker/"+url.PathEscape(id)+"/complete", cfg.PublisherToken, body)
	if err != nil {
		return err
	}
	return printPublicBuildResult(stdout, *jsonOutput, encoded, "Public Build completed")
}

func runPublicBuildWorkerFail(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags, configPath, jsonOutput, id, workerID, leaseToken, err := publicBuildWorkerTransitionFlags("fail", args, stderr)
	if err != nil {
		return err
	}
	reason := flags.Lookup("reason")
	if reason == nil || strings.TrimSpace(reason.Value.String()) == "" {
		return errors.New("--reason is required")
	}
	cfg, err := loadPublicBuildWorkerConfig(*configPath)
	if err != nil {
		return err
	}
	body := map[string]string{"workerId": workerID, "leaseToken": leaseToken, "reason": reason.Value.String()}
	encoded, _, err := callPublicBuild(ctx, cfg, http.MethodPost, "/v1/public-build-worker/"+url.PathEscape(id)+"/fail", cfg.PublisherToken, body)
	if err != nil {
		return err
	}
	return printPublicBuildResult(stdout, *jsonOutput, encoded, "Public Build failed")
}

func publicBuildWorkerTransitionFlags(command string, args []string, stderr io.Writer) (*flag.FlagSet, *string, *bool, string, string, string, error) {
	flags, configPath, jsonOutput, err := newPublicBuildFlagSet("public-build worker "+command, stderr)
	if err != nil {
		return nil, nil, nil, "", "", "", err
	}
	id := flags.String("id", "", "Public Build ID")
	workerID := flags.String("worker-id", "", "worker identity")
	leaseToken := flags.String("lease-token", "", "active worker lease token")
	switch command {
	case "append-log":
		flags.String("message", "", "sanitized worker log message")
	case "complete":
		flags.String("publication-identity", "", "existing Public Cache publication identity")
	case "fail":
		flags.String("reason", "", "failure reason")
	}
	if err := flags.Parse(args); err != nil {
		return nil, nil, nil, "", "", "", err
	}
	if *id == "" || *workerID == "" || *leaseToken == "" {
		return nil, nil, nil, "", "", "", errors.New("--id, --worker-id, and --lease-token are required")
	}
	return flags, configPath, jsonOutput, *id, *workerID, *leaseToken, nil
}

func newPublicBuildFlagSet(name string, stderr io.Writer) (*flag.FlagSet, *string, *bool, error) {
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return nil, nil, nil, err
	}
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "Public Cache configuration file")
	jsonOutput := flags.Bool("json", false, "print JSON")
	return flags, configPath, jsonOutput, nil
}

func loadPublicBuildConfig(path string) (config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return config.Config{}, err
	}
	if cfg.Role != "public" {
		return config.Config{}, errors.New("configuration does not address a Public Cache API")
	}
	return cfg, nil
}

func loadPublicBuildWorkerConfig(path string) (config.Config, error) {
	cfg, err := loadPublicBuildConfig(path)
	if err != nil {
		return config.Config{}, err
	}
	if cfg.PublisherToken == "" {
		return config.Config{}, errors.New("configuration has no Public Build worker token")
	}
	return cfg, nil
}

func callPublicBuild(ctx context.Context, cfg config.Config, method, path, token string, body any) ([]byte, int, error) {
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("encode Public Build request: %w", err)
		}
		requestBody = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://"+cfg.Listen+path, requestBody)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := newCLIHTTPClient(controlRequestTimeout).Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("call Public Build API: %w", err)
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, response.StatusCode, fmt.Errorf("read Public Build response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(encoded, &failure) == nil && failure.Error != "" {
			return nil, response.StatusCode, fmt.Errorf("Public Build API returned HTTP %d: %s", response.StatusCode, failure.Error)
		}
		return nil, response.StatusCode, fmt.Errorf("Public Build API returned HTTP %d", response.StatusCode)
	}
	return encoded, response.StatusCode, nil
}

func printPublicBuildResult(stdout io.Writer, jsonOutput bool, encoded []byte, message string) error {
	if !jsonOutput {
		_, err := fmt.Fprintln(stdout, message)
		return err
	}
	var value any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return fmt.Errorf("decode Public Build response: %w", err)
	}
	return printResult(stdout, true, value, "")
}
