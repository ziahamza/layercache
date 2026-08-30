package publicbuild

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	ErrRejected          = errors.New("Public Build request rejected")
	ErrNotFound          = errors.New("Public Build not found")
	ErrNoWork            = errors.New("no compatible Public Build is queued")
	ErrLeaseLost         = errors.New("Public Build lease is no longer active")
	ErrInvalidTransition = errors.New("invalid Public Build state transition")
)

type Integration string

const (
	IntegrationTurbo    Integration = "turbo"
	IntegrationBuildKit Integration = "buildkit"
	IntegrationActions  Integration = "actions"
)

type Platform string

const (
	PlatformLinuxAMD64 Platform = "linux/amd64"
	PlatformLinuxARM64 Platform = "linux/arm64"
)

type State string

const (
	StateQueued    State = "queued"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

type Resources struct {
	CPUMillis   int64
	MemoryBytes int64
	DiskBytes   int64
	Timeout     time.Duration
}

type BuildRequest struct {
	Repository       string
	Commit           string
	Ref              string
	Integration      Integration
	Target           string
	RecipeDigest     string
	Platform         Platform
	Resources        Resources
	Secrets          []string
	Privileged       bool
	HostDockerSocket bool
}

type Build struct {
	ID          string
	Request     BuildRequest
	State       State
	WorkerID    string
	RequestedAt time.Time
	StartedAt   time.Time
	FinishedAt  time.Time
	Publication *Publication
	Failure     string
}

type RequestResult struct {
	Build  Build
	Reused bool
}

type Config struct {
	AllowlistedRepositories []string
	Limits                  Resources
	SanitizeLog             func(string) string
}

type OutputDescriptor struct {
	Name      string
	Digest    string
	SizeBytes int64
	MediaType string
}

type Publication struct {
	Outputs          []OutputDescriptor
	ProducerDuration time.Duration
}

type LogEntry struct {
	Sequence  uint64
	Timestamp time.Time
	Message   string
}

type WorkerCapabilities struct {
	Integrations []Integration
	Platforms    []Platform
}

type LogSink func(context.Context, string) error

// Worker is the production-isolation seam. Implementations execute each leased
// Public Build in an isolated environment and return immutable output
// descriptors; the coordinator remains the only component that can accept the
// result for publication.
type Worker interface {
	ID() string
	Capabilities() WorkerCapabilities
	Execute(context.Context, Build, LogSink) (Publication, error)
}

type Lease struct {
	Token    string
	WorkerID string
	Build    Build
	LeasedAt time.Time
}

type Coordinator interface {
	Request(context.Context, BuildRequest) (RequestResult, error)
	Inspect(context.Context, string) (Build, error)
	Logs(context.Context, string) ([]LogEntry, error)
	LeaseNext(context.Context, Worker) (Lease, error)
	AppendLog(context.Context, Lease, string) error
	Complete(context.Context, Lease, Publication) (Build, error)
	Fail(context.Context, Lease, string) (Build, error)
	Cancel(context.Context, string) (Build, error)
}

type memoryCoordinator struct {
	mu        sync.Mutex
	config    Config
	allowlist map[string]struct{}
	nextID    uint64
	nextLease uint64
	builds    map[string]*buildRecord
	identity  map[string]string
	queue     []string
}

type buildRecord struct {
	build      Build
	identity   string
	leaseToken string
	logs       []LogEntry
}

var (
	commitPattern = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-fA-F]{64}$`)
	targetPattern = regexp.MustCompile(`^[A-Za-z0-9@][A-Za-z0-9._/@#+:-]{0,127}$`)
)

func NewCoordinator(config Config) (Coordinator, error) {
	allowlist, err := prepareConfig(config)
	if err != nil {
		return nil, err
	}
	return &memoryCoordinator{
		config:    config,
		allowlist: allowlist,
		builds:    make(map[string]*buildRecord),
		identity:  make(map[string]string),
	}, nil
}

func prepareConfig(config Config) (map[string]struct{}, error) {
	if len(config.AllowlistedRepositories) == 0 {
		return nil, errors.New("at least one Public Build repository must be allowlisted")
	}
	if err := validateResources(config.Limits); err != nil {
		return nil, fmt.Errorf("invalid Public Build limits: %w", err)
	}
	if config.SanitizeLog == nil {
		return nil, errors.New("Public Build log sanitizer is required")
	}
	allowlist := make(map[string]struct{}, len(config.AllowlistedRepositories))
	for _, repository := range config.AllowlistedRepositories {
		normalized, err := normalizeRepository(repository)
		if err != nil {
			return nil, fmt.Errorf("invalid allowlisted Public Build repository %q: %w", repository, err)
		}
		allowlist[normalized] = struct{}{}
	}
	return allowlist, nil
}

func (coordinator *memoryCoordinator) Request(ctx context.Context, request BuildRequest) (RequestResult, error) {
	if err := ctx.Err(); err != nil {
		return RequestResult{}, err
	}
	normalized, err := coordinator.admit(request)
	if err != nil {
		return RequestResult{}, err
	}
	identity := buildIdentity(normalized)

	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if id, found := coordinator.identity[identity]; found {
		return RequestResult{Build: cloneBuild(coordinator.builds[id].build), Reused: true}, nil
	}
	coordinator.nextID++
	id := fmt.Sprintf("public-build-%d", coordinator.nextID)
	build := Build{
		ID:          id,
		Request:     normalized,
		State:       StateQueued,
		RequestedAt: time.Now().UTC(),
	}
	coordinator.builds[id] = &buildRecord{
		build:    build,
		identity: identity,
	}
	coordinator.identity[identity] = id
	coordinator.queue = append(coordinator.queue, id)
	return RequestResult{Build: cloneBuild(build)}, nil
}

func (coordinator *memoryCoordinator) Inspect(ctx context.Context, id string) (Build, error) {
	if err := ctx.Err(); err != nil {
		return Build{}, err
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	record, found := coordinator.builds[id]
	if !found {
		return Build{}, ErrNotFound
	}
	return cloneBuild(record.build), nil
}

func (coordinator *memoryCoordinator) Logs(ctx context.Context, id string) ([]LogEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	record, found := coordinator.builds[id]
	if !found {
		return nil, ErrNotFound
	}
	return append([]LogEntry(nil), record.logs...), nil
}

func (coordinator *memoryCoordinator) LeaseNext(ctx context.Context, worker Worker) (Lease, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	if worker == nil {
		return Lease{}, reject("worker is required")
	}
	workerID := strings.TrimSpace(worker.ID())
	if workerID == "" {
		return Lease{}, reject("worker ID is required")
	}
	capabilities := worker.Capabilities()

	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	for _, id := range coordinator.queue {
		record := coordinator.builds[id]
		if record.build.State != StateQueued || !supports(capabilities, record.build.Request) {
			continue
		}
		now := time.Now().UTC()
		coordinator.nextLease++
		record.leaseToken = fmt.Sprintf("public-build-lease-%d", coordinator.nextLease)
		record.build.State = StateRunning
		record.build.WorkerID = workerID
		record.build.StartedAt = now
		return Lease{
			Token:    record.leaseToken,
			WorkerID: workerID,
			Build:    cloneBuild(record.build),
			LeasedAt: now,
		}, nil
	}
	return Lease{}, ErrNoWork
}

func (coordinator *memoryCoordinator) AppendLog(ctx context.Context, lease Lease, message string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sanitized := coordinator.config.SanitizeLog(message)

	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	record, err := coordinator.activeLease(lease)
	if err != nil {
		return err
	}
	record.logs = append(record.logs, LogEntry{
		Sequence:  uint64(len(record.logs) + 1),
		Timestamp: time.Now().UTC(),
		Message:   sanitized,
	})
	return nil
}

func (coordinator *memoryCoordinator) Complete(ctx context.Context, lease Lease, publication Publication) (Build, error) {
	if err := ctx.Err(); err != nil {
		return Build{}, err
	}
	if err := validatePublication(publication); err != nil {
		return Build{}, reject(err.Error())
	}
	publication = clonePublication(publication)

	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	record, err := coordinator.activeLease(lease)
	if err != nil {
		return Build{}, err
	}
	now := time.Now().UTC()
	record.build.State = StateSucceeded
	record.build.FinishedAt = now
	record.build.Publication = &publication
	record.leaseToken = ""
	return cloneBuild(record.build), nil
}

func (coordinator *memoryCoordinator) Fail(ctx context.Context, lease Lease, reason string) (Build, error) {
	if err := ctx.Err(); err != nil {
		return Build{}, err
	}
	if strings.TrimSpace(reason) == "" {
		return Build{}, reject("failure reason is required")
	}
	sanitized := coordinator.config.SanitizeLog(reason)

	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	record, err := coordinator.activeLease(lease)
	if err != nil {
		return Build{}, err
	}
	now := time.Now().UTC()
	record.build.State = StateFailed
	record.build.FinishedAt = now
	record.build.Failure = sanitized
	record.leaseToken = ""
	coordinator.releaseIdentity(record)
	return cloneBuild(record.build), nil
}

func (coordinator *memoryCoordinator) Cancel(ctx context.Context, id string) (Build, error) {
	if err := ctx.Err(); err != nil {
		return Build{}, err
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	record, found := coordinator.builds[id]
	if !found {
		return Build{}, ErrNotFound
	}
	switch record.build.State {
	case StateQueued, StateRunning:
		record.build.State = StateCancelled
		record.build.FinishedAt = time.Now().UTC()
		record.leaseToken = ""
		coordinator.releaseIdentity(record)
	case StateCancelled:
		// Cancellation is idempotent after it has been accepted.
	default:
		return Build{}, fmt.Errorf("%w: cannot cancel build in %s state", ErrInvalidTransition, record.build.State)
	}
	return cloneBuild(record.build), nil
}

func (coordinator *memoryCoordinator) activeLease(lease Lease) (*buildRecord, error) {
	record, found := coordinator.builds[lease.Build.ID]
	if !found {
		return nil, ErrNotFound
	}
	if record.build.State != StateRunning || lease.Token == "" || lease.Token != record.leaseToken ||
		lease.WorkerID == "" || lease.WorkerID != record.build.WorkerID {
		return nil, ErrLeaseLost
	}
	return record, nil
}

func (coordinator *memoryCoordinator) releaseIdentity(record *buildRecord) {
	if coordinator.identity[record.identity] == record.build.ID {
		delete(coordinator.identity, record.identity)
	}
}

func (coordinator *memoryCoordinator) admit(request BuildRequest) (BuildRequest, error) {
	return admit(request, coordinator.allowlist, coordinator.config.Limits)
}

func admit(request BuildRequest, allowlist map[string]struct{}, limits Resources) (BuildRequest, error) {
	repository, err := normalizeRepository(request.Repository)
	if err != nil {
		return BuildRequest{}, reject(err.Error())
	}
	if _, allowed := allowlist[repository]; !allowed {
		return BuildRequest{}, reject("repository is not allowlisted")
	}
	if request.Ref != "" {
		return BuildRequest{}, reject("mutable source refs are not accepted")
	}
	if !commitPattern.MatchString(request.Commit) {
		return BuildRequest{}, reject("commit must be an immutable 40 or 64 character hexadecimal digest")
	}
	switch request.Integration {
	case IntegrationTurbo, IntegrationBuildKit, IntegrationActions:
	default:
		return BuildRequest{}, reject("unsupported integration")
	}
	if !targetPattern.MatchString(request.Target) || strings.Contains(request.Target, "..") {
		return BuildRequest{}, reject("target must be a safe name")
	}
	if !digestPattern.MatchString(request.RecipeDigest) {
		return BuildRequest{}, reject("recipe digest must be a complete SHA-256 digest")
	}
	if request.Platform != PlatformLinuxAMD64 && request.Platform != PlatformLinuxARM64 {
		return BuildRequest{}, reject("unsupported Public Build platform")
	}
	if len(request.Secrets) != 0 {
		return BuildRequest{}, reject("Public Builds cannot receive secrets")
	}
	if request.Privileged {
		return BuildRequest{}, reject("Public Builds cannot use privileged mode")
	}
	if request.HostDockerSocket {
		return BuildRequest{}, reject("Public Builds cannot mount the host Docker socket")
	}
	if err := validateResources(request.Resources); err != nil {
		return BuildRequest{}, reject(err.Error())
	}
	if exceeds(request.Resources, limits) {
		return BuildRequest{}, reject("requested resources exceed Public Build limits")
	}

	request.Repository = repository
	request.Commit = strings.ToLower(request.Commit)
	request.RecipeDigest = strings.ToLower(request.RecipeDigest)
	request.Secrets = nil
	return request, nil
}

func normalizeRepository(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("repository must be a valid HTTPS GitHub URL")
	}
	if parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, "github.com") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return "", errors.New("repository must be an HTTPS github.com URL")
	}
	path := strings.TrimSuffix(strings.TrimSuffix(parsed.Path, "/"), ".git")
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] == "." || parts[1] == "." ||
		parts[0] == ".." || parts[1] == ".." {
		return "", errors.New("repository must name one GitHub owner and repository")
	}
	return "https://github.com/" + strings.ToLower(parts[0]) + "/" + strings.ToLower(parts[1]), nil
}

func validateResources(resources Resources) error {
	if resources.CPUMillis <= 0 || resources.MemoryBytes <= 0 || resources.DiskBytes <= 0 || resources.Timeout <= 0 {
		return errors.New("CPU, memory, disk, and timeout must be positive")
	}
	return nil
}

func exceeds(request, limit Resources) bool {
	return request.CPUMillis > limit.CPUMillis ||
		request.MemoryBytes > limit.MemoryBytes ||
		request.DiskBytes > limit.DiskBytes ||
		request.Timeout > limit.Timeout
}

func buildIdentity(request BuildRequest) string {
	return strings.Join([]string{
		request.Repository,
		request.Commit,
		string(request.Integration),
		request.Target,
		request.RecipeDigest,
		string(request.Platform),
	}, "\x00")
}

func supports(capabilities WorkerCapabilities, request BuildRequest) bool {
	integrationSupported := false
	for _, integration := range capabilities.Integrations {
		if integration == request.Integration {
			integrationSupported = true
			break
		}
	}
	if !integrationSupported {
		return false
	}
	for _, platform := range capabilities.Platforms {
		if platform == request.Platform {
			return true
		}
	}
	return false
}

func validatePublication(publication Publication) error {
	if len(publication.Outputs) == 0 {
		return errors.New("publication requires at least one output")
	}
	if publication.ProducerDuration < 0 {
		return errors.New("publication producer duration cannot be negative")
	}
	names := make(map[string]struct{}, len(publication.Outputs))
	for _, output := range publication.Outputs {
		if !targetPattern.MatchString(output.Name) || strings.Contains(output.Name, "..") {
			return errors.New("publication output must have a safe name")
		}
		if _, duplicate := names[output.Name]; duplicate {
			return errors.New("publication output names must be unique")
		}
		names[output.Name] = struct{}{}
		if !digestPattern.MatchString(output.Digest) {
			return errors.New("publication output must have a complete SHA-256 digest")
		}
		if output.SizeBytes < 0 {
			return errors.New("publication output size cannot be negative")
		}
		if strings.TrimSpace(output.MediaType) == "" {
			return errors.New("publication output media type is required")
		}
	}
	return nil
}

func cloneBuild(build Build) Build {
	build.Request.Secrets = append([]string(nil), build.Request.Secrets...)
	if build.Publication != nil {
		publication := clonePublication(*build.Publication)
		build.Publication = &publication
	}
	return build
}

func clonePublication(publication Publication) Publication {
	publication.Outputs = append([]OutputDescriptor(nil), publication.Outputs...)
	return publication
}

func reject(message string) error {
	return fmt.Errorf("%w: %s", ErrRejected, message)
}

// LocalFakeWorker runs Public Build callbacks in the caller's process. It has
// no filesystem, network, process, privilege, kernel, or tenant isolation and
// must never be used as a production Public Build worker.
type LocalFakeWorker struct {
	WorkerID    string
	Supported   WorkerCapabilities
	ExecuteFunc func(context.Context, Build, LogSink) (Publication, error)
}

func (worker *LocalFakeWorker) ID() string {
	if worker == nil {
		return ""
	}
	return worker.WorkerID
}

func (worker *LocalFakeWorker) Capabilities() WorkerCapabilities {
	if worker == nil {
		return WorkerCapabilities{}
	}
	return WorkerCapabilities{
		Integrations: append([]Integration(nil), worker.Supported.Integrations...),
		Platforms:    append([]Platform(nil), worker.Supported.Platforms...),
	}
}

func (worker *LocalFakeWorker) Execute(ctx context.Context, build Build, logs LogSink) (Publication, error) {
	if worker == nil || worker.ExecuteFunc == nil {
		return Publication{}, errors.New("local fake Public Build worker has no execute callback")
	}
	return worker.ExecuteFunc(ctx, cloneBuild(build), logs)
}

var _ Coordinator = (*memoryCoordinator)(nil)
var _ Worker = (*LocalFakeWorker)(nil)
