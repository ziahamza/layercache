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
	"github.com/layercache/layercache/internal/publicbuild"
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
	inputFlags := newRepeatableStringFlag(nil)
	flags.Var(inputFlags, "input", "declared non-secret recipe input as name=value (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *repository == "" || *commit == "" || *integration == "" || *target == "" || *recipe == "" || *platform == "" {
		return errors.New("--repository, --commit, --integration, --target, --recipe, and --platform are required")
	}
	if *timeout <= 0 || timeout.Milliseconds() <= 0 {
		return errors.New("--timeout must be at least one millisecond")
	}
	if flags.NArg() != 0 {
		return errors.New("public-build request does not accept positional arguments")
	}
	inputs, err := parsePublicBuildInputs(inputFlags.Values())
	if err != nil {
		return err
	}
	cfg, err := loadPublicBuildConfig(ctx, *configPath)
	if err != nil {
		return err
	}
	body := map[string]any{
		"repository": *repository, "commit": *commit, "integration": *integration,
		"target": *target, "recipeDigest": *recipe, "platform": *platform,
		"inputs": inputs,
		"resources": map[string]any{
			"cpuMillis": *cpuMillis, "memoryBytes": *memoryBytes,
			"diskBytes": *diskBytes, "timeoutMilliseconds": timeout.Milliseconds(),
		},
	}
	encoded, _, err := callPublicBuild(ctx, cfg, http.MethodPost, "/v1/public-builds", publicBuildClientToken(cfg), body)
	if err != nil {
		return err
	}
	return printPublicBuildResult(stdout, *jsonOutput, encoded, "Public Build requested")
}

func parsePublicBuildInputs(values []string) ([]publicbuild.DeclaredInput, error) {
	inputs := make([]publicbuild.DeclaredInput, 0, len(values))
	for _, value := range values {
		name, inputValue, found := strings.Cut(value, "=")
		if !found || name == "" {
			return nil, fmt.Errorf("invalid --input %q: use name=value", value)
		}
		inputs = append(inputs, publicbuild.DeclaredInput{Name: name, Value: inputValue})
	}
	return inputs, nil
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
	cfg, err := loadPublicBuildConfig(ctx, *configPath)
	if err != nil {
		return err
	}
	path := "/v1/public-builds/" + url.PathEscape(*id)
	message := "Public Build status"
	if command == "logs" {
		path += "/logs"
		message = "Public Build logs"
	}
	encoded, _, err := callPublicBuild(ctx, cfg, http.MethodGet, path, publicBuildClientToken(cfg), nil)
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
	cfg, err := loadPublicBuildConfig(ctx, *configPath)
	if err != nil {
		return err
	}
	encoded, _, err := callPublicBuild(ctx, cfg, http.MethodPost, "/v1/public-builds/"+url.PathEscape(*id)+"/cancel", publicBuildClientToken(cfg), map[string]any{})
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
	case "run":
		return runPublicBuildWorkerRun(ctx, args[1:], stdout, stderr)
	case "lease":
		return runPublicBuildWorkerLease(ctx, args[1:], stdout, stderr)
	case "append-log":
		return runPublicBuildWorkerAppendLog(ctx, args[1:], stdout, stderr)
	case "heartbeat":
		return runPublicBuildWorkerHeartbeat(ctx, args[1:], stdout, stderr)
	case "complete":
		return runPublicBuildWorkerComplete(ctx, args[1:], stdout, stderr)
	case "fail":
		return runPublicBuildWorkerFail(ctx, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown Public Build worker command %q", args[0])
	}
}

func runPublicBuildWorkerHeartbeat(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	_, configPath, jsonOutput, id, workerID, leaseToken, err := publicBuildWorkerTransitionFlags("heartbeat", args, stderr)
	if err != nil {
		return err
	}
	cfg, err := loadPublicBuildWorkerConfig(*configPath)
	if err != nil {
		return err
	}
	body := map[string]string{"workerId": workerID, "leaseToken": leaseToken}
	encoded, _, err := callPublicBuild(ctx, cfg, http.MethodPost, "/v1/public-build-worker/"+url.PathEscape(id)+"/heartbeat", cfg.PublicBuildWorkerToken, body)
	if err != nil {
		return err
	}
	return printPublicBuildResult(stdout, *jsonOutput, encoded, "Public Build worker lease renewed")
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
	encoded, status, err := callPublicBuild(ctx, cfg, http.MethodPost, "/v1/public-build-worker/lease", cfg.PublicBuildWorkerToken, body)
	if err != nil {
		return err
	}
	if status == http.StatusNoContent {
		if *jsonOutput {
			return printResult(stdout, true, map[string]bool{"leased": false}, "")
		}
		_, err := fmt.Fprintln(stdout, "No compatible Public Build is queued\nLeased: no")
		return err
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
	_, _, err = callPublicBuild(ctx, cfg, http.MethodPost, "/v1/public-build-worker/"+url.PathEscape(id)+"/logs", cfg.PublicBuildWorkerToken, body)
	if err != nil {
		return err
	}
	result := map[string]any{"appended": true, "buildId": id}
	if *jsonOutput {
		return printResult(stdout, true, result, "")
	}
	_, err = fmt.Fprintf(stdout, "Public Build log appended\nBuild: %s\n", id)
	return err
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
	cfg, err := loadPublicBuildCollectorConfig(*configPath)
	if err != nil {
		return err
	}
	body := map[string]string{
		"workerId": workerID, "leaseToken": leaseToken,
		"publicationIdentity": publication.Value.String(),
	}
	encoded, _, err := callPublicBuild(ctx, cfg, http.MethodPost, "/v1/public-build-worker/"+url.PathEscape(id)+"/complete", cfg.PublicCollectorToken, body)
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
	encoded, _, err := callPublicBuild(ctx, cfg, http.MethodPost, "/v1/public-build-worker/"+url.PathEscape(id)+"/fail", cfg.PublicBuildWorkerToken, body)
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
	leaseToken := flags.String("lease-token", "", "active worker lease token (prefer --lease-token-file)")
	leaseTokenFile := flags.String("lease-token-file", "", "owner-only file containing the active worker lease token")
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
	leaseTokenSet, err := resolveSecretFlag(flags, "lease-token", "lease-token-file", leaseToken, *leaseTokenFile)
	if err != nil {
		return nil, nil, nil, "", "", "", err
	}
	if *id == "" || *workerID == "" || !leaseTokenSet || *leaseToken == "" {
		return nil, nil, nil, "", "", "", errors.New("--id, --worker-id, and one of --lease-token or --lease-token-file are required")
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

func loadPublicBuildConfig(ctx context.Context, path string) (config.Config, error) {
	cfg, err := loadPublicBuildEndpointConfig(path)
	if err != nil {
		return config.Config{}, err
	}
	refreshErr := refreshAndPersistTeamCapability(ctx, path, &cfg)
	if publicBuildClientToken(cfg) == "" {
		if refreshErr != nil {
			return config.Config{}, refreshErr
		}
		return config.Config{}, errors.New("configuration has no Public Build client credential; run layercache login or set --public-access-token")
	}
	_, expiresAt := publicBuildClientCredential(cfg)
	if !expiresAt.IsZero() && !expiresAt.After(time.Now().UTC()) {
		if refreshErr != nil {
			return config.Config{}, refreshErr
		}
		return config.Config{}, errors.New("Public Build client credential has expired; run layercache login")
	}
	// A still-valid capability remains usable when its proactive refresh or
	// persistence fails. The remote request remains authoritative, and the next
	// invocation retries refresh instead of turning a control-plane hiccup into
	// an early client outage.
	return cfg, nil
}

func loadPublicBuildEndpointConfig(path string) (config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return config.Config{}, err
	}
	if cfg.Role != "public" && cfg.PublicURL == "" {
		return config.Config{}, errors.New("configuration does not address a Public Cache API")
	}
	return cfg, nil
}

func loadPublicBuildWorkerConfig(path string) (config.Config, error) {
	cfg, err := loadPublicBuildEndpointConfig(path)
	if err != nil {
		return config.Config{}, err
	}
	if cfg.PublicBuildWorkerToken == "" {
		return config.Config{}, errors.New("configuration has no Public Build worker token")
	}
	return cfg, nil
}

func loadPublicBuildCollectorConfig(path string) (config.Config, error) {
	cfg, err := loadPublicBuildEndpointConfig(path)
	if err != nil {
		return config.Config{}, err
	}
	if cfg.PublicCollectorToken == "" {
		return config.Config{}, errors.New("configuration has no trusted Public Cache collector credential")
	}
	return cfg, nil
}

func publicBuildClientToken(cfg config.Config) string {
	token, _ := publicBuildClientCredential(cfg)
	return token
}

func publicBuildClientCredential(cfg config.Config) (string, time.Time) {
	if cfg.PublicAccessToken != "" {
		return cfg.PublicAccessToken, cfg.PublicAccessTokenExpiresAt
	}
	if sameConfiguredRemoteEndpoint(cfg.TeamURL, cfg.PublicURL) && cfg.TeamToken != "" {
		return cfg.TeamToken, cfg.TeamTokenExpiresAt
	}
	if cfg.Role == "public" {
		return cfg.LocalToken, time.Time{}
	}
	return "", time.Time{}
}

func sameConfiguredRemoteEndpoint(left, right string) bool {
	return left != "" && right != "" && strings.TrimRight(left, "/") == strings.TrimRight(right, "/")
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
	baseURL := localRuntimeURL(cfg.Listen)
	client := newLocalCLIHTTPClient(controlRequestTimeout)
	if cfg.PublicURL != "" {
		baseURL = strings.TrimRight(cfg.PublicURL, "/")
		client = newCLIHTTPClient(controlRequestTimeout)
	}
	request, err := http.NewRequestWithContext(ctx, method, baseURL+path, requestBody)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
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
	var value any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return fmt.Errorf("decode Public Build response: %w", err)
	}
	if jsonOutput {
		return printResult(stdout, true, value, "")
	}
	return printPublicBuildHumanResult(stdout, encoded, message)
}

type publicBuildHumanRequest struct {
	Repository  string `json:"repository"`
	Commit      string `json:"commit"`
	Integration string `json:"integration"`
	Target      string `json:"target"`
	Platform    string `json:"platform"`
}

type publicBuildHumanPublication struct {
	Identity  string `json:"publicCachePublication"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
}

type publicBuildHumanBuild struct {
	ID          string                       `json:"id"`
	Request     publicBuildHumanRequest      `json:"request"`
	State       string                       `json:"state"`
	WorkerID    string                       `json:"workerId"`
	RequestedAt string                       `json:"requestedAt"`
	StartedAt   string                       `json:"startedAt"`
	FinishedAt  string                       `json:"finishedAt"`
	Publication *publicBuildHumanPublication `json:"publication"`
	Failure     string                       `json:"failure"`
}

type publicBuildHumanLog struct {
	Sequence  uint64 `json:"sequence"`
	Timestamp string `json:"timestamp"`
	Message   string `json:"message"`
}

type publicBuildHumanEnvelope struct {
	Build      *publicBuildHumanBuild `json:"build"`
	Reused     *bool                  `json:"reused"`
	LeaseToken string                 `json:"leaseToken"`
	WorkerID   string                 `json:"workerId"`
	LeasedAt   string                 `json:"leasedAt"`
	ExpiresAt  string                 `json:"expiresAt"`
	BuildID    string                 `json:"buildId"`
	Logs       []publicBuildHumanLog  `json:"logs"`
}

func printPublicBuildHumanResult(output io.Writer, encoded []byte, message string) error {
	var envelope publicBuildHumanEnvelope
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		return fmt.Errorf("decode Public Build response: %w", err)
	}
	if _, err := fmt.Fprintln(output, message); err != nil {
		return err
	}
	if envelope.BuildID != "" {
		return printPublicBuildLogs(output, envelope.BuildID, envelope.Logs)
	}
	if envelope.Build != nil {
		if envelope.Reused != nil {
			if _, err := fmt.Fprintf(output, "Reused: %s\n", yesNo(*envelope.Reused)); err != nil {
				return err
			}
		}
		if envelope.WorkerID != "" {
			if _, err := fmt.Fprintf(output, "Worker: %s\n", envelope.WorkerID); err != nil {
				return err
			}
		}
		if envelope.LeasedAt != "" {
			if _, err := fmt.Fprintf(output, "Leased: %s\n", envelope.LeasedAt); err != nil {
				return err
			}
		}
		if envelope.ExpiresAt != "" {
			if _, err := fmt.Fprintf(output, "Lease expires: %s\n", envelope.ExpiresAt); err != nil {
				return err
			}
		}
		if envelope.LeaseToken != "" {
			if _, err := fmt.Fprintf(output, "Lease token: %s\n", envelope.LeaseToken); err != nil {
				return err
			}
		}
		return printPublicBuild(output, *envelope.Build)
	}
	var build publicBuildHumanBuild
	if err := json.Unmarshal(encoded, &build); err != nil {
		return fmt.Errorf("decode Public Build response: %w", err)
	}
	if build.ID == "" {
		return errors.New("Public Build response did not identify a build")
	}
	return printPublicBuild(output, build)
}

func printPublicBuild(output io.Writer, build publicBuildHumanBuild) error {
	if _, err := fmt.Fprintf(output, "Build: %s\nState: %s\n", build.ID, build.State); err != nil {
		return err
	}
	if build.Request.Repository != "" {
		if _, err := fmt.Fprintf(output, "Source: %s@%s\n", build.Request.Repository, build.Request.Commit); err != nil {
			return err
		}
	}
	if build.Request.Integration != "" || build.Request.Target != "" || build.Request.Platform != "" {
		if _, err := fmt.Fprintf(output, "Request: %s %s on %s\n",
			build.Request.Integration, build.Request.Target, build.Request.Platform); err != nil {
			return err
		}
	}
	if build.WorkerID != "" {
		if _, err := fmt.Fprintf(output, "Worker: %s\n", build.WorkerID); err != nil {
			return err
		}
	}
	for _, timestamp := range []struct {
		label string
		value string
	}{
		{label: "Requested", value: build.RequestedAt},
		{label: "Started", value: build.StartedAt},
		{label: "Finished", value: build.FinishedAt},
	} {
		if timestamp.value == "" {
			continue
		}
		if _, err := fmt.Fprintf(output, "%s: %s\n", timestamp.label, timestamp.value); err != nil {
			return err
		}
	}
	if build.Publication != nil {
		if _, err := fmt.Fprintf(output, "Publication: %s (%s, %s)\n", build.Publication.Identity,
			build.Publication.Digest, formatByteCount(build.Publication.SizeBytes)); err != nil {
			return err
		}
	}
	if build.Failure != "" {
		_, err := fmt.Fprintf(output, "Failure: %s\n", build.Failure)
		return err
	}
	return nil
}

func printPublicBuildLogs(output io.Writer, buildID string, logs []publicBuildHumanLog) error {
	if _, err := fmt.Fprintf(output, "Build: %s\n", buildID); err != nil {
		return err
	}
	if len(logs) == 0 {
		_, err := fmt.Fprintln(output, "Logs: none")
		return err
	}
	for _, entry := range logs {
		if _, err := fmt.Fprintf(output, "[%s] #%d %s\n", entry.Timestamp, entry.Sequence, entry.Message); err != nil {
			return err
		}
	}
	return nil
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
