package cli

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/measurement"
	"github.com/layercache/layercache/internal/project"
	"github.com/layercache/layercache/internal/publicbuild"
	"github.com/layercache/layercache/internal/uploadqueue"
)

const recentDegradedWindow = 24 * time.Hour

type operationalStatus struct {
	Configured           bool                            `json:"configured"`
	Running              bool                            `json:"running"`
	Role                 string                          `json:"role"`
	ProjectID            string                          `json:"projectId"`
	DataDir              string                          `json:"dataDir"`
	Listen               string                          `json:"listen"`
	CompatibilityID      string                          `json:"compatibilityId"`
	MaxBytes             int64                           `json:"maxBytes"`
	EvictionPolicy       string                          `json:"evictionPolicy"`
	UsageBytes           int64                           `json:"usageBytes"`
	Artifacts            int64                           `json:"artifacts"`
	Entries              int64                           `json:"entries"`
	PendingUploads       int64                           `json:"pendingUploads"`
	PendingUploadBytes   int64                           `json:"pendingUploadBytes"`
	RuntimePID           int                             `json:"runtimePid"`
	RuntimeInstanceID    string                          `json:"runtimeInstanceId"`
	RuntimeStartedAt     time.Time                       `json:"runtimeStartedAt,omitempty"`
	BypassAdapters       []string                        `json:"bypassAdapters"`
	DetectedIntegrations []string                        `json:"detectedIntegrations"`
	ActiveIntegrations   []string                        `json:"activeIntegrations"`
	Integrations         map[string]integrationCondition `json:"integrations"`
	RemoteReachability   map[string]remoteCondition      `json:"remoteReachability"`
	Credentials          map[string]credentialCondition  `json:"credentials"`
	RecentDegraded       degradedCondition               `json:"recentDegraded"`
	PublicBuild          publicBuildCondition            `json:"publicBuild"`
	Degraded             bool                            `json:"degraded"`
	DegradedReasons      []string                        `json:"degradedReasons"`
}

type integrationCondition struct {
	Detected            bool       `json:"detected"`
	Configured          bool       `json:"configured"`
	Bypassed            bool       `json:"bypassed"`
	Active              bool       `json:"active"`
	State               string     `json:"state"`
	ConfigurationPath   string     `json:"configurationPath,omitempty"`
	CredentialExpiresAt *time.Time `json:"credentialExpiresAt,omitempty"`
}

type remoteCondition struct {
	Configured bool   `json:"configured"`
	Reachable  bool   `json:"reachable"`
	State      string `json:"state"`
	LatencyMS  int64  `json:"latencyMs,omitempty"`
}

type credentialCondition struct {
	Configured       bool       `json:"configured"`
	State            string     `json:"state"`
	ExpiresAt        *time.Time `json:"expiresAt,omitempty"`
	RemainingSeconds int64      `json:"remainingSeconds,omitempty"`
}

type degradedCondition struct {
	Available bool          `json:"available"`
	Observed  bool          `json:"observed"`
	Window    time.Duration `json:"window"`
	Runs      int           `json:"runs,omitempty"`
	State     string        `json:"state"`
}

type publicBuildCondition struct {
	Enabled            bool                      `json:"enabled"`
	EndpointConfigured bool                      `json:"endpointConfigured"`
	CredentialRequired bool                      `json:"credentialRequired"`
	Authenticated      bool                      `json:"authenticated"`
	CapabilityState    string                    `json:"capabilityState,omitempty"`
	Mode               string                    `json:"mode"`
	StateAvailable     bool                      `json:"stateAvailable"`
	Queue              publicbuild.StatusSummary `json:"queue"`
	State              string                    `json:"state"`
}

func inspectOperationalStatus(ctx context.Context, cfg config.Config) operationalStatus {
	result := operationalStatus{
		Configured: true,
		Role:       cfg.Role, ProjectID: cfg.ProjectID, DataDir: cfg.DataDir, Listen: cfg.Listen,
		CompatibilityID: cfg.CompatibilityID, MaxBytes: cfg.MaxBytes,
		EvictionPolicy:     string(cfg.EvictionPolicy),
		BypassAdapters:     append([]string{}, cfg.BypassAdapters...),
		ActiveIntegrations: []string{},
		DegradedReasons:    []string{},
		Integrations:       make(map[string]integrationCondition),
		RemoteReachability: make(map[string]remoteCondition),
		Credentials:        make(map[string]credentialCondition),
		RecentDegraded:     degradedCondition{Window: recentDegradedWindow, State: "unavailable"},
	}
	if result.EvictionPolicy == "" {
		result.EvictionPolicy = "lru"
	}

	live, liveErr := probeRuntime(cfg)
	if liveErr == nil && live.Running {
		result.Running = true
		result.UsageBytes = live.UsageBytes
		if live.MaxBytes > 0 {
			result.MaxBytes = live.MaxBytes
		}
		if live.EvictionPolicy != "" {
			result.EvictionPolicy = live.EvictionPolicy
		}
		result.Artifacts = live.Artifacts
		result.Entries = live.Entries
		result.PendingUploads = live.PendingUploads
		result.PendingUploadBytes = live.PendingUploadBytes
		result.RuntimePID = live.RuntimePID
		result.RuntimeInstanceID = live.RuntimeInstanceID
		result.RuntimeStartedAt = live.StartedAt
	} else {
		if stats, err := artifact.ReadStats(ctx, cfg.DataDir); err == nil {
			result.UsageBytes = stats.UsageBytes
			result.Artifacts = stats.Artifacts
			result.Entries = stats.Entries
		} else {
			result.addDegraded("local-cache-status-unavailable")
		}
		queueStatusUnavailable := false
		if queued, err := uploadqueue.ReadPersistedStats(ctx, filepath.Join(cfg.DataDir, "team-uploads.db"), cfg.MaxBytes); err == nil {
			result.PendingUploads += queued.QueuedJobs
			result.PendingUploadBytes += queued.QueuedBytes
		} else {
			queueStatusUnavailable = true
		}
		if queued, err := readStoppedActionsTeamPublicationStats(ctx, filepath.Join(cfg.DataDir, "actions.db")); err == nil {
			result.PendingUploads += queued.PendingJobs
			result.PendingUploadBytes += queued.PendingBytes
		} else {
			queueStatusUnavailable = true
		}
		if queueStatusUnavailable {
			result.addDegraded("upload-queue-status-unavailable")
		}
	}

	result.inspectIntegrations(ctx, cfg)
	result.RemoteReachability["team"] = probeTeamRemote(ctx, cfg.TeamURL, cfg.TeamToken)
	result.RemoteReachability["public"] = probeRemote(ctx, cfg.PublicURL)
	result.Credentials["team"] = inspectCredential(cfg.TeamURL != "", cfg.TeamToken, cfg.TeamTokenExpiresAt)
	publicBuildToken, publicBuildExpiry, publicBuildRequired := publicBuildStatusCredential(cfg)
	result.Credentials["publicBuild"] = inspectCredential(publicBuildRequired, publicBuildToken, publicBuildExpiry)
	for name, remote := range result.RemoteReachability {
		if remote.Configured && !remote.Reachable {
			result.addDegraded(name + "-remote-unreachable")
		}
	}
	for name, credential := range result.Credentials {
		if credential.Configured && (credential.State == "missing" || credential.State == "expired") {
			result.addDegraded(name + "-credential-" + credential.State)
		}
	}
	if result.Running {
		result.RecentDegraded = inspectRecentDegraded(ctx, cfg)
		if result.RecentDegraded.Observed {
			result.addDegraded("recent-cache-operation-failure")
		}
	}
	result.PublicBuild = inspectPublicBuildStatus(
		ctx, cfg, result.RemoteReachability["public"], live.PublicBuild,
		publicBuildToken, publicBuildExpiry, publicBuildRequired,
	)
	if result.PublicBuild.Enabled && result.PublicBuild.State == "unavailable" {
		result.addDegraded("public-build-state-unavailable")
	}
	slices.Sort(result.ActiveIntegrations)
	slices.Sort(result.DegradedReasons)
	return result
}

func (status *operationalStatus) inspectIntegrations(ctx context.Context, cfg config.Config) {
	detected := project.DetectIntegrations(cfg.ProjectRoot)
	status.DetectedIntegrations = append([]string{}, detected...)
	detectedSet := make(map[string]bool, len(detected))
	for _, name := range detected {
		detectedSet[name] = true
	}
	bypassed := make(map[string]bool, len(cfg.BypassAdapters))
	for _, name := range cfg.BypassAdapters {
		bypassed[name] = true
	}
	state, stateErr := loadIntegrationState(cfg)
	if stateErr != nil {
		status.addDegraded("integration-state-invalid")
		state.Records = make(map[string]integrationRecord)
	}

	turbo := integrationCondition{Detected: detectedSet["turbo"], Bypassed: bypassed["turbo"], State: "not-configured"}
	if record, ok := state.Records["turbo"]; ok {
		turbo.Configured = true
		turbo.ConfigurationPath = record.Path
		turbo.State = "configuration-missing"
		if data, exists, err := readJSONFile(record.Path, 1<<20); err == nil && exists {
			var values map[string]json.RawMessage
			var endpoint string
			if json.Unmarshal(data, &values) == nil && json.Unmarshal(values["apiUrl"], &endpoint) == nil && endpoint == record.Endpoint {
				if record.Endpoint != localRuntimeURL(cfg.Listen) {
					turbo.State = "endpoint-stale"
				} else {
					turbo.State = "active"
					turbo.Active = !turbo.Bypassed
				}
			} else {
				turbo.State = "configuration-drifted"
			}
		}
	}
	if turbo.Bypassed && turbo.Configured {
		turbo.State = "bypassed"
		turbo.Active = false
	}
	status.Integrations["turbo"] = turbo

	actions := integrationCondition{Detected: detectedSet["actions"], Bypassed: bypassed["actions"], State: "not-configured"}
	if record, ok := state.Records["local-ci"]; ok {
		actions.Configured = true
		actions.ConfigurationPath = record.Path
		expiresAt := record.CredentialExpiresAt
		actions.CredentialExpiresAt = &expiresAt
		actions.State = "handoff-missing"
		if data, exists, err := readRegularFile(record.Path, 1<<20); err == nil && exists && contentDigest(data) == record.Digest {
			if info, statErr := os.Stat(record.Path); statErr != nil || info.Mode().Perm()&0o077 != 0 {
				actions.State = "handoff-permissions-unsafe"
			} else if record.CredentialExpiresAt.Before(time.Now().UTC()) {
				actions.State = "credential-expired"
			} else {
				actions.State = inspectOwnedLocalCIHandoff(cfg, record, data, time.Now().UTC())
				if actions.State == "active" {
					actions.Active = !actions.Bypassed
				}
			}
		}
	}
	if actions.Bypassed && actions.Configured {
		actions.State = "bypassed"
		actions.Active = false
	}
	status.Integrations["actions"] = actions

	buildkit := integrationCondition{Detected: detectedSet["buildkit"], Bypassed: bypassed["buildkit"], State: "not-configured"}
	if record, ok := state.Records["buildkit"]; ok {
		buildkit.Configured = true
		buildkit.ConfigurationPath = record.Path
		buildkit.State = inspectOwnedBuildkitConfiguration(cfg, record)
		if buildkit.State == "current" {
			buildkit.State = "builder-unavailable"
			probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
			builders, err := listBuildxBuilders(probeCtx, "docker")
			cancel()
			if err == nil {
				if builder, exists := builders[record.Builder]; exists && builder.Driver == "docker-container" && builderHasNode(builder, record.BuilderNode) {
					if builder.Current {
						if record.State == "active" {
							buildkit.State = "active"
							buildkit.Active = !buildkit.Bypassed
						} else {
							buildkit.State = "application-incomplete"
						}
					} else {
						buildkit.State = "not-selected"
					}
				} else {
					buildkit.State = "builder-missing"
				}
			}
		}
	}
	if buildkit.Bypassed && buildkit.Configured {
		buildkit.State = "bypassed"
		buildkit.Active = false
	}
	status.Integrations["buildkit"] = buildkit

	for name, condition := range status.Integrations {
		if condition.Active {
			status.ActiveIntegrations = append(status.ActiveIntegrations, name)
		}
		if condition.Configured && condition.State != "active" && condition.State != "bypassed" {
			status.addDegraded(name + "-integration-" + condition.State)
		}
	}
}

func inspectOwnedBuildkitConfiguration(cfg config.Config, record integrationRecord) string {
	if record.Builder != cfg.BuildkitBuilder || record.BuilderNode != buildkitNodeName(cfg) {
		return "builder-identity-stale"
	}
	expectedPath, err := resolveThroughExistingAncestor(filepath.Join(cfg.DataDir, "buildkit", "buildkitd.toml"))
	if err != nil {
		return "configuration-unavailable"
	}
	if !record.OwnershipCaptured || record.Path != expectedPath || record.Digest == "" {
		return "configuration-path-drifted"
	}
	current, exists, err := readRegularFile(expectedPath, 1<<20)
	if err != nil {
		return "configuration-unreadable"
	}
	if !exists {
		return "configuration-missing"
	}
	if contentDigest(current) != record.Digest {
		return "configuration-drifted"
	}
	desired, _, err := buildkitGCConfiguration(cfg, expectedPath)
	if err != nil {
		return "configuration-unavailable"
	}
	if !bytes.Equal(current, desired) {
		return "configuration-policy-stale"
	}
	return "current"
}

func inspectOwnedLocalCIHandoff(cfg config.Config, record integrationRecord, data []byte, now time.Time) string {
	expectedEndpoint := localRuntimeURL(cfg.Listen) + "/"
	cacheURL, cacheURLFound := localCIHandoffAssignment(data, "ACTIONS_CACHE_URL")
	localCacheURL, localCacheURLFound := localCIHandoffAssignment(data, "LOCAL_CI_ACTIONS_CACHE_URL")
	token, tokenFound := localCIHandoffAssignment(data, "ACTIONS_RUNTIME_TOKEN")
	localToken, localTokenFound := localCIHandoffAssignment(data, "LOCAL_CI_ACTIONS_RUNTIME_TOKEN")
	if !cacheURLFound || !localCacheURLFound || !tokenFound || !localTokenFound || token != localToken {
		return "handoff-invalid"
	}
	if cacheURL != expectedEndpoint || localCacheURL != expectedEndpoint || record.Endpoint != expectedEndpoint {
		return "endpoint-stale"
	}
	claims, err := access.ParseCapabilityToken(cfg.LocalToken, token, now)
	if err != nil {
		return "credential-invalid"
	}
	if claims.Subject != cfg.InstallationID || claims.Project != cfg.ProjectID || claims.Integration != "actions" ||
		claims.Compatibility != cfg.CompatibilityID || !strings.EqualFold(claims.Repository, cfg.ActionsRepository) ||
		claims.Ref != cfg.ActionsRef || claims.DefaultRef != cfg.ActionsDefaultRef ||
		!claims.Allows(access.CapabilityWrite) || !claims.ExpiresAt.Equal(record.CredentialExpiresAt) {
		return "credential-scope-stale"
	}
	return "active"
}

func localCIHandoffAssignment(data []byte, name string) (string, bool) {
	prefix := name + "="
	value := ""
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		if found {
			return "", false
		}
		raw := strings.TrimPrefix(line, prefix)
		if len(raw) < 2 || raw[0] != '\'' || raw[len(raw)-1] != '\'' || strings.Contains(raw[1:len(raw)-1], "'") {
			return "", false
		}
		value = raw[1 : len(raw)-1]
		found = true
	}
	return value, found
}

type remoteProbeResponse struct {
	Condition  remoteCondition
	StatusCode int
	Body       []byte
}

func probeRemote(ctx context.Context, baseURL string) remoteCondition {
	probe := performRemoteProbe(ctx, baseURL, "/healthz", "")
	result := probe.Condition
	if probe.StatusCode == 0 {
		return result
	}
	if probe.StatusCode < http.StatusOK || probe.StatusCode >= http.StatusMultipleChoices {
		result.State = fmt.Sprintf("http-%d", probe.StatusCode)
		return result
	}
	result.Reachable = true
	result.State = "reachable"
	return result
}

func probeTeamRemote(ctx context.Context, baseURL, token string) remoteCondition {
	probe := performRemoteProbe(ctx, baseURL, "/v1/status", token)
	result := probe.Condition
	if probe.StatusCode == 0 {
		return result
	}
	switch probe.StatusCode {
	case http.StatusOK:
		result.Reachable = true
		result.State = "reachable"
	case http.StatusUnauthorized, http.StatusForbidden:
		result.State = "unauthorized"
	default:
		result.State = fmt.Sprintf("http-%d", probe.StatusCode)
	}
	return result
}

func probePublicBuildCapability(ctx context.Context, baseURL, token string) remoteCondition {
	// Public Build IDs are positive decimal sequences. public-build-0 can never
	// exist, so this proves Public Build read authority without reading or
	// mutating user data.
	probe := performRemoteProbe(ctx, baseURL, "/v1/public-builds/public-build-0", token)
	result := probe.Condition
	if probe.StatusCode == 0 {
		return result
	}
	switch probe.StatusCode {
	case http.StatusOK:
		result.Reachable = true
		result.State = "reachable"
	case http.StatusNotFound:
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(probe.Body, &failure) == nil && failure.Error == "Public Build not found" {
			result.Reachable = true
			result.State = "reachable"
		} else {
			result.State = "unsupported"
		}
	case http.StatusUnauthorized, http.StatusForbidden:
		result.State = "unauthorized"
	case http.StatusServiceUnavailable:
		result.State = "disabled"
	default:
		result.State = fmt.Sprintf("http-%d", probe.StatusCode)
	}
	return result
}

func performRemoteProbe(ctx context.Context, baseURL, path, token string) remoteProbeResponse {
	if strings.TrimSpace(baseURL) == "" {
		return remoteProbeResponse{Condition: remoteCondition{State: "not-configured"}}
	}
	result := remoteCondition{Configured: true, State: "unavailable"}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
		result.State = "invalid-url"
		return remoteProbeResponse{Condition: result}
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + path
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		result.State = "invalid-url"
		return remoteProbeResponse{Condition: result}
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := newCLIHTTPClient(2 * time.Second)
	client.CheckRedirect = func(next *http.Request, previous []*http.Request) error {
		if len(previous) >= 3 || !sameHTTPOrigin(next.URL, parsed) {
			return errors.New("remote health redirect rejected")
		}
		return nil
	}
	startedAt := time.Now()
	response, err := client.Do(request)
	result.LatencyMS = time.Since(startedAt).Milliseconds()
	if err != nil {
		result.State = classifyReachabilityError(err)
		return remoteProbeResponse{Condition: result}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return remoteProbeResponse{Condition: result}
	}
	return remoteProbeResponse{Condition: result, StatusCode: response.StatusCode, Body: body}
}

func sameHTTPOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func classifyReachabilityError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return "dns-failure"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection-refused"
	}
	var certificateError x509.UnknownAuthorityError
	if errors.As(err, &certificateError) {
		return "tls-untrusted"
	}
	return "unavailable"
}

func inspectCredential(required bool, token string, expiresAt time.Time) credentialCondition {
	result := credentialCondition{Configured: required, State: "not-required"}
	if !required {
		return result
	}
	if token == "" {
		result.State = "missing"
		return result
	}
	if expiresAt.IsZero() {
		result.State = "static"
		return result
	}
	expiresAt = expiresAt.UTC()
	result.ExpiresAt = &expiresAt
	result.RemainingSeconds = int64(time.Until(expiresAt).Seconds())
	switch {
	case !expiresAt.After(time.Now()):
		result.State = "expired"
	case expiresAt.Before(time.Now().Add(30 * time.Minute)):
		result.State = "expiring"
	default:
		result.State = "valid"
	}
	return result
}

func inspectRecentDegraded(ctx context.Context, cfg config.Config) degradedCondition {
	result := degradedCondition{Available: false, Window: recentDegradedWindow, State: "unavailable"}
	now := time.Now().UTC()
	query := make(url.Values)
	query.Set("from", now.Add(-recentDegradedWindow).Format(time.RFC3339Nano))
	query.Set("to", now.Format(time.RFC3339Nano))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, localRuntimeURL(cfg.Listen)+"/v1/reports?"+query.Encode(), nil)
	if err != nil {
		return result
	}
	request.Header.Set("Authorization", "Bearer "+cfg.LocalToken)
	response, err := newLocalCLIHTTPClient(2 * time.Second).Do(request)
	if err != nil {
		return result
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result
	}
	var report measurement.PeriodReport
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&report); err != nil {
		return result
	}
	result.Available = true
	result.Observed = report.Degraded
	result.Runs = report.Runs
	if report.Degraded {
		result.State = "degraded"
	} else {
		result.State = "healthy"
	}
	return result
}

func publicBuildStatusCredential(cfg config.Config) (string, time.Time, bool) {
	if cfg.Role == "public" || cfg.PublicURL == "" {
		return "", time.Time{}, false
	}
	if cfg.PublicAccessToken != "" || !cfg.PublicAccessTokenExpiresAt.IsZero() {
		return cfg.PublicAccessToken, cfg.PublicAccessTokenExpiresAt, true
	}
	if sameConfiguredRemoteEndpoint(cfg.TeamURL, cfg.PublicURL) {
		return cfg.TeamToken, cfg.TeamTokenExpiresAt, true
	}
	// A Public Cache endpoint is globally readable. Its presence alone does not
	// opt the installation into authenticated Public Build requests.
	return "", time.Time{}, false
}

func inspectPublicBuildStatus(
	ctx context.Context,
	cfg config.Config,
	publicRemote remoteCondition,
	runtimeQueue *publicbuild.StatusSummary,
	token string,
	expiresAt time.Time,
	credentialRequired bool,
) publicBuildCondition {
	result := publicBuildCondition{State: "not-configured"}
	if cfg.Role == "public" {
		result.Mode = "coordinator"
		result.EndpointConfigured = true
		result.Enabled = len(cfg.PublicBuildRepositories) > 0
		if !result.Enabled {
			return result
		}
		if runtimeQueue == nil {
			result.State = "unavailable"
			return result
		}
		result.StateAvailable = true
		result.Queue = *runtimeQueue
		if runtimeQueue.Running > 0 {
			result.State = "running"
		} else if runtimeQueue.Queued > 0 {
			result.State = "queued"
		} else {
			result.State = "idle"
		}
		return result
	}
	result.Mode = "remote"
	result.EndpointConfigured = cfg.PublicURL != ""
	result.CredentialRequired = credentialRequired
	result.Enabled = result.EndpointConfigured && credentialRequired
	if !result.EndpointConfigured || !result.Enabled {
		return result
	}
	if token == "" {
		result.CapabilityState = "credential-missing"
		result.State = "unavailable"
		return result
	}
	if !expiresAt.IsZero() && !expiresAt.After(time.Now().UTC()) {
		result.CapabilityState = "credential-expired"
		result.State = "unavailable"
		return result
	}
	if !publicRemote.Reachable {
		result.CapabilityState = "remote-unreachable"
		result.State = "unavailable"
		return result
	}
	capability := probePublicBuildCapability(ctx, cfg.PublicURL, token)
	result.CapabilityState = capability.State
	result.Authenticated = capability.Reachable
	if !capability.Reachable {
		result.State = "unavailable"
		return result
	}
	result.State = "reachable"
	return result
}

func (status *operationalStatus) addDegraded(reason string) {
	status.Degraded = true
	if !slices.Contains(status.DegradedReasons, reason) {
		status.DegradedReasons = append(status.DegradedReasons, reason)
	}
}

func formatByteCount(value int64) string {
	const unit = int64(1024)
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	number := float64(value)
	index := -1
	for number >= float64(unit) && index+1 < len(units) {
		number /= float64(unit)
		index++
	}
	return fmt.Sprintf("%.1f %s", number, units[index])
}

func printOperationalStatus(output io.Writer, status operationalStatus) error {
	runtimeState := "stopped"
	if status.Running {
		runtimeState = "running"
	}
	if _, err := fmt.Fprintf(output, "Layer Cache %s for %s is %s\n", status.Role, status.ProjectID, runtimeState); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "Local Cache: %s of %s, %d entries; pending Team uploads: %d (%s)\n",
		formatByteCount(status.UsageBytes), formatByteCount(status.MaxBytes), status.Entries,
		status.PendingUploads, formatByteCount(status.PendingUploadBytes)); err != nil {
		return err
	}
	integrations := "none"
	if len(status.ActiveIntegrations) > 0 {
		integrations = strings.Join(status.ActiveIntegrations, ", ")
	}
	if _, err := fmt.Fprintf(output, "Active integrations: %s\n", integrations); err != nil {
		return err
	}
	for _, name := range []string{"team", "public"} {
		remote := status.RemoteReachability[name]
		if remote.Configured {
			if _, err := fmt.Fprintf(output, "%s Cache remote: %s\n", strings.ToUpper(name[:1])+name[1:], remote.State); err != nil {
				return err
			}
		}
	}
	for _, credential := range []struct {
		name  string
		label string
	}{
		{name: "team", label: "Team Cache credential"},
		{name: "publicBuild", label: "Public Build credential"},
	} {
		condition, present := status.Credentials[credential.name]
		if !present {
			continue
		}
		state := strings.ReplaceAll(condition.State, "-", " ")
		if condition.ExpiresAt != nil {
			state += "; expires " + condition.ExpiresAt.UTC().Format(time.RFC3339)
		}
		if _, err := fmt.Fprintf(output, "%s: %s\n", credential.label, state); err != nil {
			return err
		}
	}
	degraded := "no"
	if status.Degraded {
		degraded = strings.Join(status.DegradedReasons, ", ")
	}
	_, err := fmt.Fprintf(output, "Degraded: %s; Public Build: %s\n", degraded, status.PublicBuild.State)
	return err
}
