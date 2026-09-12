package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/compatibility"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/project"
)

const maximumVMRouteLifetime = time.Hour

type vmRouteResponse struct {
	Endpoint      string            `json:"endpoint"`
	Project       string            `json:"project"`
	Compatibility string            `json:"compatibility"`
	ExpiresAt     time.Time         `json:"expiresAt"`
	Environment   map[string]string `json:"environment"`
}

type vmActionsScope struct {
	Repository   string
	Ref          string
	DefaultRef   string
	SourceCommit string
}

func runVMRoute(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "issue" {
		return errors.New("vm-route requires the issue subcommand")
	}
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("vm-route issue", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultPath, "configuration file")
	endpointValue := flags.String("endpoint", "", "runtime URL reachable from the VM")
	lifetime := flags.Duration("ttl", 15*time.Minute, "route credential lifetime (maximum 1h)")
	compatibilityID := flags.String("compatibility", "", "VM output compatibility identity (defaults to the host configuration)")
	integration := flags.String("integration", "all", "turbo, actions, or all")
	actionsRepository := flags.String("actions-repository", "", "Actions repository scope (owner/repository)")
	actionsRef := flags.String("actions-ref", "", "Actions current Git ref")
	actionsDefaultRef := flags.String("actions-default-ref", "", "Actions default branch ref")
	actionsSourceCommit := flags.String("actions-source-commit", "", "Actions immutable source commit")
	outputPath := flags.String("output", "", "exclusive owner-only JSON credential file")
	readOnly := flags.Bool("read-only", false, "issue read-only credentials")
	allowInsecureHTTP := flags.Bool("allow-insecure-http", false, "allow cleartext HTTP to a non-loopback VM route")
	jsonOutput := flags.Bool("json", false, "print credential JSON to stdout")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("vm-route issue does not accept positional arguments")
	}
	jsonRequested := *jsonOutput && flagWasSet(flags, "json")
	if *outputPath == "" && !jsonRequested {
		return errors.New("vm-route issue requires --output FILE; use explicit --json only when credential JSON on stdout is intended")
	}
	if *outputPath != "" && jsonRequested {
		return errors.New("--output and --json cannot be used together")
	}
	if *lifetime < time.Minute || *lifetime > maximumVMRouteLifetime {
		return errors.New("--ttl must be between 1m and 1h")
	}
	if *integration != "all" && *integration != "turbo" && *integration != "actions" {
		return fmt.Errorf("unsupported VM route integration %q", *integration)
	}
	explicitActionsScope, err := vmActionsScopeFlagSet(flags)
	if err != nil {
		return err
	}
	if *integration == "turbo" && explicitActionsScope {
		return errors.New("Actions scope flags require --integration actions or --integration all")
	}
	endpoint, err := advertisedVMEndpoint(*endpointValue, *allowInsecureHTTP)
	if err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load Layer Cache configuration: %w", err)
	}
	selectedCompatibility := *compatibilityID
	if selectedCompatibility == "" {
		selectedCompatibility = cfg.CompatibilityID
	}
	if err := compatibility.Validate(selectedCompatibility); err != nil {
		return fmt.Errorf("invalid VM compatibility identity: %w", err)
	}
	var actionsScope vmActionsScope
	if *integration == "all" || *integration == "actions" {
		actionsScope, err = resolveVMActionsScope(ctx, cfg, vmActionsScope{
			Repository:   *actionsRepository,
			Ref:          *actionsRef,
			DefaultRef:   *actionsDefaultRef,
			SourceCommit: *actionsSourceCommit,
		}, explicitActionsScope)
		if err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	expiresAt := now.Add(*lifetime)
	random, err := config.NewToken()
	if err != nil {
		return err
	}
	runID := "vm-route-" + random[:20]
	capability := access.CapabilityWrite
	if *readOnly {
		capability = access.CapabilityRead
	}
	baseClaims := access.Claims{
		Subject: "vm-route:" + cfg.InstallationID, RunID: runID,
		WorkspaceID: "vm:" + random[20:40], Project: cfg.ProjectID,
		Compatibility: selectedCompatibility,
		Toolchain:     selectedCompatibility, Builder: "vm-route",
		Capabilities: []access.Capability{capability}, ExpiresAt: expiresAt,
	}
	environment := map[string]string{
		"LAYER_CACHE_RUN_ID": runID,
	}
	if *integration == "all" || *integration == "turbo" {
		claims := baseClaims
		claims.Integration = "turbo"
		token, err := access.MintCapabilityToken(cfg.LocalToken, claims, now)
		if err != nil {
			return fmt.Errorf("issue Turbo VM route credential: %w", err)
		}
		environment["TURBO_API"] = endpoint
		environment["TURBO_TEAM"] = cfg.ProjectID
		environment["TURBO_TOKEN"] = token
	}
	if *integration == "all" || *integration == "actions" {
		claims := baseClaims
		claims.Integration = "actions"
		claims.Repository = actionsScope.Repository
		claims.Ref = actionsScope.Ref
		claims.DefaultRef = actionsScope.DefaultRef
		claims.SourceCommit = actionsScope.SourceCommit
		token, err := access.MintCapabilityToken(cfg.LocalToken, claims, now)
		if err != nil {
			return fmt.Errorf("issue Actions VM route credential: %w", err)
		}
		environment["ACTIONS_CACHE_URL"] = endpoint + "/"
		environment["ACTIONS_RUNTIME_TOKEN"] = token
		environment["ACTIONS_CACHE_SERVICE_V2"] = ""
	}
	response := vmRouteResponse{
		Endpoint: endpoint, Project: cfg.ProjectID, Compatibility: selectedCompatibility,
		ExpiresAt: expiresAt, Environment: environment,
	}
	if *outputPath != "" {
		writtenPath, err := writeExclusiveVMRouteOutput(*outputPath, response)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "Layer Cache VM route credential written to %s\n", writtenPath)
		return err
	}
	return printResult(stdout, true, response, "")
}

func writeExclusiveVMRouteOutput(path string, response vmRouteResponse) (string, error) {
	if path == "-" || path == "" || strings.TrimSpace(path) != path {
		return "", errors.New("--output must be a non-empty filesystem path, not stdout")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve VM route output path: %w", err)
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", fmt.Errorf("resolve VM route output directory: %w", err)
	}
	parentInfo, err := os.Lstat(resolvedParent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("VM route output directory %s is not a real directory", resolvedParent)
	}
	resolvedPath := filepath.Join(resolvedParent, filepath.Base(absolute))
	if _, err := os.Lstat(resolvedPath); err == nil {
		return "", fmt.Errorf("VM route output %s already exists", resolvedPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect VM route output %s: %w", resolvedPath, err)
	}
	file, err := os.OpenFile(resolvedPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return "", fmt.Errorf("VM route output %s already exists", resolvedPath)
		}
		return "", fmt.Errorf("create VM route output %s: %w", resolvedPath, err)
	}
	committed := false
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
		if !committed {
			_ = os.Remove(resolvedPath)
		}
	}()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("VM route output is not a regular file")
	}
	if err := file.Chmod(0o600); err != nil {
		return "", fmt.Errorf("protect VM route output: %w", err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(response); err != nil {
		return "", fmt.Errorf("encode VM route output: %w", err)
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("sync VM route output: %w", err)
	}
	if err := file.Close(); err != nil {
		closed = true
		return "", fmt.Errorf("close VM route output: %w", err)
	}
	closed = true
	committed = true
	return resolvedPath, nil
}

func vmActionsScopeFlagSet(flags *flag.FlagSet) (bool, error) {
	set := make(map[string]bool, 4)
	flags.Visit(func(value *flag.Flag) {
		switch value.Name {
		case "actions-repository", "actions-ref", "actions-default-ref", "actions-source-commit":
			set[value.Name] = true
		}
	})
	if len(set) != 0 && len(set) != 4 {
		return false, errors.New("--actions-repository, --actions-ref, --actions-default-ref, and --actions-source-commit must be provided together")
	}
	return len(set) == 4, nil
}

func resolveVMActionsScope(
	ctx context.Context,
	cfg config.Config,
	explicit vmActionsScope,
	explicitSet bool,
) (vmActionsScope, error) {
	if explicitSet {
		if err := validateVMActionsScope(cfg, explicit); err != nil {
			return vmActionsScope{}, fmt.Errorf("invalid explicit Actions VM route scope: %w", err)
		}
		return explicit, nil
	}

	discovered, err := project.Discover(ctx, "")
	if err != nil {
		return vmActionsScope{}, actionsScopeDiscoveryError(err)
	}
	if discovered.Project != cfg.ProjectID ||
		!strings.EqualFold(discovered.ActionsRepository, cfg.ActionsRepository) {
		return vmActionsScope{}, actionsScopeDiscoveryError(fmt.Errorf(
			"checkout project %q does not match configured project %q", discovered.Project, cfg.ProjectID,
		))
	}
	if !discovered.DefaultRefKnown {
		return vmActionsScope{}, actionsScopeDiscoveryError(errors.New("origin/HEAD does not identify the default branch"))
	}
	scope := vmActionsScope{
		Repository:   strings.ToLower(discovered.ActionsRepository),
		Ref:          discovered.Ref,
		DefaultRef:   discovered.DefaultRef,
		SourceCommit: strings.ToLower(discovered.Commit),
	}
	if err := validateVMActionsScope(cfg, scope); err != nil {
		return vmActionsScope{}, actionsScopeDiscoveryError(err)
	}
	return scope, nil
}

func actionsScopeDiscoveryError(cause error) error {
	return fmt.Errorf(
		"cannot establish Actions VM route scope from the current Git checkout: %w; run this command inside the configured repository or provide --actions-repository, --actions-ref, --actions-default-ref, and --actions-source-commit",
		cause,
	)
}

func validateVMActionsScope(cfg config.Config, scope vmActionsScope) error {
	if scope.Repository != strings.ToLower(strings.TrimSpace(scope.Repository)) || !validVMActionsRepository(scope.Repository) {
		return errors.New("--actions-repository must be a lowercase owner/repository identity")
	}
	if scope.Repository != strings.ToLower(strings.TrimSpace(cfg.ActionsRepository)) {
		return fmt.Errorf("--actions-repository %q does not match configured repository %q", scope.Repository, cfg.ActionsRepository)
	}
	if !validVMActionsRef(scope.Ref, false) {
		return errors.New("--actions-ref must be a canonical branch, tag, or pull-request ref")
	}
	if !validVMActionsRef(scope.DefaultRef, true) {
		return errors.New("--actions-default-ref must be a canonical branch ref")
	}
	if scope.DefaultRef != cfg.ActionsDefaultRef {
		return fmt.Errorf("--actions-default-ref %q does not match configured default ref %q", scope.DefaultRef, cfg.ActionsDefaultRef)
	}
	if !immutableVMActionsCommit(scope.SourceCommit) {
		return errors.New("--actions-source-commit must be a lowercase 40- or 64-character hexadecimal commit")
	}
	return nil
}

func validVMActionsRepository(repository string) bool {
	owner, name, found := strings.Cut(repository, "/")
	return found && !strings.Contains(name, "/") && validVMActionsRepositoryPart(owner) && validVMActionsRepositoryPart(name)
}

func validVMActionsRepositoryPart(part string) bool {
	if part == "" || part == "." || part == ".." {
		return false
	}
	for _, character := range part {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' ||
			character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func validVMActionsRef(ref string, defaultOnly bool) bool {
	if ref == "" || len(ref) > 1024 || strings.TrimSpace(ref) != ref ||
		strings.ContainsAny(ref, "\x00\r\n ~^:?*[\\") || strings.Contains(ref, "..") ||
		strings.Contains(ref, "//") || strings.Contains(ref, "@{") || strings.HasSuffix(ref, "/") ||
		strings.HasSuffix(ref, ".") || strings.HasSuffix(ref, ".lock") {
		return false
	}
	if defaultOnly {
		return len(ref) > len("refs/heads/") && strings.HasPrefix(ref, "refs/heads/")
	}
	for _, prefix := range []string{"refs/heads/", "refs/tags/", "refs/pull/"} {
		if len(ref) > len(prefix) && strings.HasPrefix(ref, prefix) {
			return true
		}
	}
	return false
}

func immutableVMActionsCommit(commit string) bool {
	if len(commit) != 40 && len(commit) != 64 {
		return false
	}
	for _, character := range commit {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

func advertisedVMEndpoint(raw string, allowInsecureHTTP bool) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("--endpoint must be an absolute HTTP or HTTPS URL reachable from the VM")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", errors.New("VM route endpoint must use HTTP or HTTPS")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.EscapedPath() != "" && parsed.EscapedPath() != "/") {
		return "", errors.New("VM route endpoint cannot contain credentials, a path, query, or fragment")
	}
	host := parsed.Hostname()
	if host == "0.0.0.0" || host == "::" || host == "" {
		return "", errors.New("VM route endpoint must use an advertised host, not an unspecified listen address")
	}
	if parsed.Scheme == "http" && !allowInsecureHTTP && !vmLoopbackHost(host) {
		return "", errors.New("non-loopback VM routes require HTTPS or explicit --allow-insecure-http")
	}
	parsed.Path = ""
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func vmLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
