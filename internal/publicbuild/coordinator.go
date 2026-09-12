package publicbuild

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/layercache/layercache/internal/compatibility"
)

var (
	ErrRejected               = errors.New("Public Build request rejected")
	ErrNotFound               = errors.New("Public Build not found")
	ErrNoWork                 = errors.New("no compatible Public Build is queued")
	ErrLeaseLost              = errors.New("Public Build lease is no longer active")
	ErrPublicationLost        = errors.New("Public Build publication permit is no longer active")
	ErrPublicationNotFound    = errors.New("no valid Public Cache publication exists")
	ErrPublicationUnavailable = fmt.Errorf(
		"Public Cache publication is no longer usable: %w", ErrPublicationNotFound,
	)
	ErrPublicationPending = errors.New("Public Cache publication registration is still pending")
	ErrInvalidTransition  = errors.New("invalid Public Build state transition")
)

const publicationRegistrationGrace = 5 * time.Minute

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

// DeclaredInput is one non-secret, recipe-supported input that can affect a
// Public Build output. Admission canonicalizes inputs by name before they are
// persisted, dispatched, or included in provenance.
type DeclaredInput struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type BuildRequest struct {
	Repository       string
	Commit           string
	Ref              string
	Integration      Integration
	Target           string
	RecipeDigest     string
	Platform         Platform
	Inputs           []DeclaredInput
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

type SourcePolicy interface {
	Approve(context.Context, string, string) error
}

type SourcePolicyFunc func(context.Context, string, string) error

func (policy SourcePolicyFunc) Approve(ctx context.Context, repository, commit string) error {
	return policy(ctx, repository, commit)
}

type RecipePolicy interface {
	Approve(context.Context, Integration, string, string) error
}

type RecipePolicyFunc func(context.Context, Integration, string, string) error

func (policy RecipePolicyFunc) Approve(ctx context.Context, integration Integration, target, digest string) error {
	return policy(ctx, integration, target, digest)
}

type ExistingPublication struct {
	Identity         string
	BuildID          string
	Digest           string
	SizeBytes        int64
	MediaType        string
	ProducerDuration time.Duration
}

type PublicationIndex interface {
	Find(context.Context, BuildRequest) (ExistingPublication, error)
}

type PublicationIndexFunc func(context.Context, BuildRequest) (ExistingPublication, error)

func (index PublicationIndexFunc) Find(ctx context.Context, request BuildRequest) (ExistingPublication, error) {
	return index(ctx, request)
}

type Config struct {
	AllowlistedRepositories []string
	Limits                  Resources
	SanitizeLog             func(string) string
	SourcePolicy            SourcePolicy
	RecipePolicy            RecipePolicy
	Publications            PublicationIndex
	LeaseDuration           time.Duration
	PublicationDuration     time.Duration
	Now                     func() time.Time
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
	Recipes      []WorkerRecipeCapability
}

type WorkerRecipeCapability struct {
	Integration  Integration `json:"integration"`
	Target       string      `json:"target"`
	RecipeDigest string      `json:"recipeDigest"`
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
	Token     string
	WorkerID  string
	Build     Build
	LeasedAt  time.Time
	ExpiresAt time.Time
}

type PublicationPermit struct {
	Token string
	Build Build
}

type Coordinator interface {
	Request(context.Context, BuildRequest) (RequestResult, error)
	Inspect(context.Context, string) (Build, error)
	Logs(context.Context, string) ([]LogEntry, error)
	LeaseNext(context.Context, Worker) (Lease, error)
	Renew(context.Context, Lease) (Lease, error)
	AppendLog(context.Context, Lease, string) error
	Complete(context.Context, Lease, Publication) (Build, error)
	Fail(context.Context, Lease, string) (Build, error)
	Cancel(context.Context, string) (Build, error)
	BeginLeasedPublication(context.Context, Lease) (PublicationPermit, error)
	CommitPublication(context.Context, PublicationPermit, Publication) (Build, error)
	AbortPublication(context.Context, PublicationPermit, string) (Build, error)
}

type memoryCoordinator struct {
	mu              sync.Mutex
	config          Config
	allowlist       map[string]struct{}
	nextID          uint64
	nextLease       uint64
	nextPublication uint64
	builds          map[string]*buildRecord
	identity        map[string]string
	queue           []string
}

type buildRecord struct {
	build            Build
	identity         string
	leaseToken       string
	leaseExpires     time.Time
	publicationToken string
	logs             []LogEntry
}

var (
	commitPattern       = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
	digestPattern       = regexp.MustCompile(`^sha256:[0-9a-fA-F]{64}$`)
	targetPattern       = regexp.MustCompile(`^[A-Za-z0-9@][A-Za-z0-9._/@#+:-]{0,127}$`)
	actionsJobPattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,99}$`)
	actionsWorkflowPath = regexp.MustCompile(`^\.github/workflows/[A-Za-z0-9][A-Za-z0-9._/-]*\.ya?ml$`)
	actionsWorkflowRef  = regexp.MustCompile(`^refs/(?:heads|tags|pull)/[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$`)
	turboPackagePattern = regexp.MustCompile(`^(?:[A-Za-z0-9][A-Za-z0-9._-]*|@[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*)$`)
	turboTaskPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	inputNamePattern    = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
)

const (
	maximumDeclaredInputs     = 32
	maximumDeclaredInputValue = 1024
	maximumDeclaredInputBytes = 16 << 10
)

func NewCoordinator(config Config) (Coordinator, error) {
	allowlist, err := prepareConfig(&config)
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

func prepareConfig(config *Config) (map[string]struct{}, error) {
	if len(config.AllowlistedRepositories) == 0 {
		return nil, errors.New("at least one Public Build repository must be allowlisted")
	}
	if err := validateResources(config.Limits); err != nil {
		return nil, fmt.Errorf("invalid Public Build limits: %w", err)
	}
	if config.SanitizeLog == nil {
		return nil, errors.New("Public Build log sanitizer is required")
	}
	if config.SourcePolicy == nil {
		return nil, errors.New("Public Build source policy is required")
	}
	if config.RecipePolicy == nil {
		return nil, errors.New("Public Build recipe policy is required")
	}
	if config.LeaseDuration <= 0 {
		config.LeaseDuration = 2 * time.Minute
	}
	if config.PublicationDuration <= 0 {
		config.PublicationDuration = config.Limits.Timeout
	}
	if config.Now == nil {
		config.Now = time.Now
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
	normalized, err := coordinator.admit(ctx, request)
	if err != nil {
		return RequestResult{}, err
	}
	publicationMissing := false
	publicationUnavailable := false
	if coordinator.config.Publications != nil {
		existing, lookupErr := coordinator.config.Publications.Find(ctx, normalized)
		if lookupErr == nil {
			build, err := buildFromExistingPublication(normalized, existing, coordinator.config.Now().UTC())
			if err != nil {
				return RequestResult{}, err
			}
			return RequestResult{Build: build, Reused: true}, nil
		}
		if !errors.Is(lookupErr, ErrPublicationNotFound) {
			return RequestResult{}, fmt.Errorf("find existing Public Cache publication: %w", lookupErr)
		}
		publicationMissing = true
		publicationUnavailable = errors.Is(lookupErr, ErrPublicationUnavailable)
	}
	identity := buildIdentity(normalized)
	now := coordinator.config.Now().UTC()

	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if id, found := coordinator.identity[identity]; found {
		record := coordinator.builds[id]
		if record.build.State != StateSucceeded || !publicationMissing {
			return RequestResult{Build: cloneBuild(record.build), Reused: true}, nil
		}
		if !shouldRetireMissingPublication(record.build, now, publicationMissing, publicationUnavailable) {
			return RequestResult{}, ErrPublicationPending
		}
		record.build.State = StateFailed
		record.build.Publication = nil
		record.build.Failure = "Public Cache publication is unavailable"
		delete(coordinator.identity, identity)
	}
	coordinator.nextID++
	id := fmt.Sprintf("public-build-%d", coordinator.nextID)
	build := Build{
		ID:          id,
		Request:     normalized,
		State:       StateQueued,
		RequestedAt: now,
	}
	coordinator.builds[id] = &buildRecord{
		build:    build,
		identity: identity,
	}
	coordinator.identity[identity] = id
	coordinator.queue = append(coordinator.queue, id)
	return RequestResult{Build: cloneBuild(build)}, nil
}

func shouldRetireMissingPublication(
	build Build,
	now time.Time,
	publicationMissing bool,
	publicationUnavailable bool,
) bool {
	if !publicationMissing || build.State != StateSucceeded {
		return false
	}
	if publicationUnavailable || build.FinishedAt.IsZero() {
		return true
	}
	return !now.Before(build.FinishedAt.Add(publicationRegistrationGrace))
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
	coordinator.recoverExpiredLocked(coordinator.config.Now().UTC())
	for _, id := range coordinator.queue {
		record := coordinator.builds[id]
		if record.build.State != StateQueued || !supports(capabilities, record.build.Request) {
			continue
		}
		now := coordinator.config.Now().UTC()
		coordinator.nextLease++
		record.leaseToken = fmt.Sprintf("public-build-lease-%d", coordinator.nextLease)
		record.build.State = StateRunning
		record.build.WorkerID = workerID
		record.build.StartedAt = now
		record.leaseExpires = now.Add(coordinator.config.LeaseDuration)
		return Lease{
			Token:     record.leaseToken,
			WorkerID:  workerID,
			Build:     cloneBuild(record.build),
			LeasedAt:  now,
			ExpiresAt: record.leaseExpires,
		}, nil
	}
	return Lease{}, ErrNoWork
}

func (coordinator *memoryCoordinator) Renew(ctx context.Context, lease Lease) (Lease, error) {
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	record, err := coordinator.activeLease(lease)
	if err != nil {
		return Lease{}, err
	}
	now := coordinator.config.Now().UTC()
	record.leaseExpires = now.Add(coordinator.config.LeaseDuration)
	lease.Build = cloneBuild(record.build)
	lease.LeasedAt = now
	lease.ExpiresAt = record.leaseExpires
	return lease, nil
}

func (coordinator *memoryCoordinator) AppendLog(ctx context.Context, lease Lease, message string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sanitized := sanitizePublicBuildLog(coordinator.config, message, lease.Token)

	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	record, err := coordinator.activeLease(lease)
	if err != nil {
		return err
	}
	record.logs = append(record.logs, LogEntry{
		Sequence:  uint64(len(record.logs) + 1),
		Timestamp: coordinator.config.Now().UTC(),
		Message:   sanitized,
	})
	return nil
}

func (coordinator *memoryCoordinator) Complete(ctx context.Context, lease Lease, publication Publication) (Build, error) {
	if err := ctx.Err(); err != nil {
		return Build{}, err
	}
	if err := validateCredentialFreePublication(publication, lease.Token); err != nil {
		return Build{}, reject(err.Error())
	}
	publication = clonePublication(publication)

	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	record, err := coordinator.activeLease(lease)
	if err != nil {
		return Build{}, err
	}
	now := coordinator.config.Now().UTC()
	record.build.State = StateSucceeded
	record.build.FinishedAt = now
	record.build.Publication = &publication
	record.leaseToken = ""
	record.leaseExpires = time.Time{}
	return cloneBuild(record.build), nil
}

func (coordinator *memoryCoordinator) Fail(ctx context.Context, lease Lease, reason string) (Build, error) {
	if err := ctx.Err(); err != nil {
		return Build{}, err
	}
	if strings.TrimSpace(reason) == "" {
		return Build{}, reject("failure reason is required")
	}
	sanitized := sanitizePublicBuildLog(coordinator.config, reason, lease.Token)

	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	record, err := coordinator.activeLease(lease)
	if err != nil {
		return Build{}, err
	}
	now := coordinator.config.Now().UTC()
	record.build.State = StateFailed
	record.build.FinishedAt = now
	record.build.Failure = sanitized
	record.leaseToken = ""
	record.leaseExpires = time.Time{}
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
	switch {
	case canTransition(record.build.State, StateCancelled):
		if record.publicationToken != "" {
			return Build{}, fmt.Errorf("%w: publication already began", ErrInvalidTransition)
		}
		record.build.State = StateCancelled
		record.build.FinishedAt = coordinator.config.Now().UTC()
		record.leaseToken = ""
		record.leaseExpires = time.Time{}
		coordinator.releaseIdentity(record)
	case record.build.State == StateCancelled:
		// Cancellation is idempotent after it has been accepted.
	default:
		return Build{}, fmt.Errorf("%w: cannot cancel build in %s state", ErrInvalidTransition, record.build.State)
	}
	return cloneBuild(record.build), nil
}

// canTransition is the state policy shared by coordinator adapters. Storage
// adapters still make each transition atomic in their own transaction.
func canTransition(from, to State) bool {
	switch from {
	case StateQueued:
		return to == StateRunning || to == StateCancelled
	case StateRunning:
		return to == StateSucceeded || to == StateFailed || to == StateCancelled
	default:
		return false
	}
}

func (coordinator *memoryCoordinator) activeLease(lease Lease) (*buildRecord, error) {
	record, found := coordinator.builds[lease.Build.ID]
	if !found {
		return nil, ErrNotFound
	}
	if record.build.State == StateRunning && !record.leaseExpires.After(coordinator.config.Now().UTC()) {
		coordinator.requeue(record)
		return nil, ErrLeaseLost
	}
	if record.build.State != StateRunning || record.publicationToken != "" || lease.Token == "" || lease.Token != record.leaseToken ||
		lease.WorkerID == "" || lease.WorkerID != record.build.WorkerID {
		return nil, ErrLeaseLost
	}
	return record, nil
}

// BeginLeasedPublication atomically fences trusted collection to the exact
// live worker lease. The collector credential authorizes publication, while
// this lease check prevents a cancelled or replaced worker from winning a
// later publication race.
func (coordinator *memoryCoordinator) BeginLeasedPublication(ctx context.Context, lease Lease) (PublicationPermit, error) {
	if err := ctx.Err(); err != nil {
		return PublicationPermit{}, err
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	record, err := coordinator.activeLease(lease)
	if err != nil {
		if errors.Is(err, ErrLeaseLost) {
			return PublicationPermit{}, ErrPublicationLost
		}
		return PublicationPermit{}, err
	}
	coordinator.nextPublication++
	record.publicationToken = fmt.Sprintf("public-build-publication-%d", coordinator.nextPublication)
	record.leaseExpires = coordinator.config.Now().UTC().Add(coordinator.config.PublicationDuration)
	return PublicationPermit{Token: record.publicationToken, Build: cloneBuild(record.build)}, nil
}

func (coordinator *memoryCoordinator) CommitPublication(ctx context.Context, permit PublicationPermit, publication Publication) (Build, error) {
	if err := ctx.Err(); err != nil {
		return Build{}, err
	}
	if err := validateCredentialFreePublication(publication, permit.Token); err != nil {
		return Build{}, reject(err.Error())
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	record, err := coordinator.activePublication(permit)
	if err != nil {
		return Build{}, err
	}
	now := coordinator.config.Now().UTC()
	publication = clonePublication(publication)
	record.build.State = StateSucceeded
	record.build.FinishedAt = now
	record.build.Publication = &publication
	record.leaseToken = ""
	record.leaseExpires = time.Time{}
	record.publicationToken = ""
	return cloneBuild(record.build), nil
}

func (coordinator *memoryCoordinator) AbortPublication(ctx context.Context, permit PublicationPermit, reason string) (Build, error) {
	if err := ctx.Err(); err != nil {
		return Build{}, err
	}
	if strings.TrimSpace(reason) == "" {
		return Build{}, reject("publication failure reason is required")
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	record, err := coordinator.activePublication(permit)
	if err != nil {
		return Build{}, err
	}
	record.build.State = StateFailed
	record.build.FinishedAt = coordinator.config.Now().UTC()
	record.build.Failure = sanitizePublicBuildLog(coordinator.config, reason, permit.Token)
	record.leaseToken = ""
	record.leaseExpires = time.Time{}
	record.publicationToken = ""
	coordinator.releaseIdentity(record)
	return cloneBuild(record.build), nil
}

func (coordinator *memoryCoordinator) activePublication(permit PublicationPermit) (*buildRecord, error) {
	record, found := coordinator.builds[permit.Build.ID]
	if !found {
		return nil, ErrNotFound
	}
	if record.build.State == StateRunning && !record.leaseExpires.After(coordinator.config.Now().UTC()) {
		coordinator.requeue(record)
		return nil, ErrPublicationLost
	}
	if record.build.State != StateRunning || permit.Token == "" || permit.Token != record.publicationToken {
		return nil, ErrPublicationLost
	}
	return record, nil
}

func (coordinator *memoryCoordinator) recoverExpiredLocked(now time.Time) {
	for _, record := range coordinator.builds {
		if record.build.State == StateRunning && !record.leaseExpires.After(now) {
			coordinator.requeue(record)
		}
	}
}

func (coordinator *memoryCoordinator) requeue(record *buildRecord) {
	record.build.State = StateQueued
	record.build.WorkerID = ""
	record.build.StartedAt = time.Time{}
	record.leaseToken = ""
	record.leaseExpires = time.Time{}
	record.publicationToken = ""
}

func (coordinator *memoryCoordinator) releaseIdentity(record *buildRecord) {
	if coordinator.identity[record.identity] == record.build.ID {
		delete(coordinator.identity, record.identity)
	}
}

func (coordinator *memoryCoordinator) admit(ctx context.Context, request BuildRequest) (BuildRequest, error) {
	return admit(ctx, request, coordinator.allowlist, coordinator.config)
}

func admit(ctx context.Context, request BuildRequest, allowlist map[string]struct{}, config Config) (BuildRequest, error) {
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
	case IntegrationTurbo, IntegrationActions, IntegrationBuildKit:
	default:
		return BuildRequest{}, reject("unsupported integration")
	}
	if err := ValidateMaintainedTarget(request.Integration, request.Target); err != nil {
		return BuildRequest{}, reject(err.Error())
	}
	if !digestPattern.MatchString(request.RecipeDigest) {
		return BuildRequest{}, reject("recipe digest must be a complete SHA-256 digest")
	}
	if request.Platform != PlatformLinuxAMD64 && request.Platform != PlatformLinuxARM64 {
		return BuildRequest{}, reject("unsupported Public Build platform")
	}
	inputs, err := canonicalDeclaredInputs(request.Inputs)
	if err != nil {
		return BuildRequest{}, reject(err.Error())
	}
	request.Inputs = inputs
	if err := validateMaintainedIntegrationIdentity(request); err != nil {
		return BuildRequest{}, reject(err.Error())
	}
	if _, err := CompatibilityIdentity(request); err != nil {
		return BuildRequest{}, reject(err.Error())
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
	if exceeds(request.Resources, config.Limits) {
		return BuildRequest{}, reject("requested resources exceed Public Build limits")
	}

	request.Repository = repository
	request.Commit = strings.ToLower(request.Commit)
	request.RecipeDigest = strings.ToLower(request.RecipeDigest)
	request.Secrets = nil
	if err := config.SourcePolicy.Approve(ctx, request.Repository, request.Commit); err != nil {
		if ctx.Err() != nil {
			return BuildRequest{}, ctx.Err()
		}
		return BuildRequest{}, reject("commit is not reachable from an approved branch or release tag: " + err.Error())
	}
	if err := config.RecipePolicy.Approve(ctx, request.Integration, request.Target, request.RecipeDigest); err != nil {
		if ctx.Err() != nil {
			return BuildRequest{}, ctx.Err()
		}
		return BuildRequest{}, reject("recipe is not maintained by Layer Cache: " + err.Error())
	}
	return request, nil
}

func validateMaintainedIntegrationIdentity(request BuildRequest) error {
	switch request.Integration {
	case IntegrationActions:
		required := []string{"actions.key", "actions.ref", "actions.version", "compatibility"}
		if len(request.Inputs) != len(required) {
			return errors.New("Actions Public Build requires exactly actions.key, actions.ref, actions.version, and compatibility")
		}
		for index, name := range required {
			if request.Inputs[index].Name != name || request.Inputs[index].Value == "" {
				return errors.New("Actions Public Build requires exactly actions.key, actions.ref, actions.version, and compatibility")
			}
		}
		values := map[string]string{}
		for _, input := range request.Inputs {
			values[input.Name] = input.Value
		}
		if !strings.HasPrefix(values["actions.ref"], "refs/") ||
			len(values["actions.key"]) > 512 || strings.Contains(values["actions.key"], ",") ||
			len(values["actions.version"]) != 64 ||
			values["actions.version"] != strings.ToLower(values["actions.version"]) {
			return errors.New("Actions Public Build ref or cache version is invalid")
		}
		if _, err := hex.DecodeString(values["actions.version"]); err != nil {
			return errors.New("Actions Public Build cache version must be a lowercase SHA-256 digest")
		}
	}
	return nil
}

// ValidateMaintainedTarget enforces the target shape implemented by each
// pinned guest executor before a build can enter the queue.
func ValidateMaintainedTarget(integration Integration, target string) error {
	if integration == IntegrationActions {
		_, _, err := ParseActionsWorkflowTarget(target)
		return err
	}
	if !targetPattern.MatchString(target) || strings.Contains(target, "..") {
		return errors.New("target must be a safe name")
	}
	switch integration {
	case IntegrationTurbo:
		packageName, task, found := strings.Cut(target, "#")
		if !found || strings.Contains(task, "#") ||
			!turboPackagePattern.MatchString(packageName) || !turboTaskPattern.MatchString(task) {
			return errors.New("Turbo Public Build target must be one fully qualified package#task selector")
		}
	case IntegrationBuildKit:
	default:
		return errors.New("unsupported integration")
	}
	return nil
}

// ActionsWorkflowTarget derives the maintained Actions target from the signed
// GitHub workflow_ref claim and a literal GITHUB_JOB identifier. The target is
// bound into the recipe digest and selects exactly one workflow file and job.
func ActionsWorkflowTarget(repository, workflowRef, job string) (string, error) {
	repository = strings.TrimSpace(repository)
	prefix := repository + "/"
	if repository == "" || len(workflowRef) <= len(prefix) ||
		!strings.EqualFold(workflowRef[:len(prefix)], prefix) {
		return "", errors.New("GitHub workflow_ref does not match the Actions repository")
	}
	workflowIdentity := workflowRef[len(prefix):]
	separator := strings.LastIndex(workflowIdentity, "@")
	if separator <= 0 || separator == len(workflowIdentity)-1 {
		return "", errors.New("GitHub workflow_ref is not canonical")
	}
	workflowPath, reference := workflowIdentity[:separator], workflowIdentity[separator+1:]
	if !actionsWorkflowRef.MatchString(reference) || strings.Contains(reference, "..") ||
		strings.Contains(reference, "//") || strings.HasSuffix(reference, "/") || strings.HasSuffix(reference, ".") {
		return "", errors.New("GitHub workflow_ref uses an unsafe ref")
	}
	target := workflowPath + "#" + job
	if _, _, err := ParseActionsWorkflowTarget(target); err != nil {
		return "", err
	}
	return target, nil
}

// ParseActionsWorkflowTarget returns the exact workflow path and job bound
// into a maintained Actions target.
func ParseActionsWorkflowTarget(target string) (workflowPath, job string, err error) {
	if len(target) == 0 || len(target) > 512 || strings.TrimSpace(target) != target ||
		strings.ContainsAny(target, "\x00\r\n\\") {
		return "", "", errors.New("Actions Public Build target must bind one canonical workflow path and job")
	}
	workflowPath, job, found := strings.Cut(target, "#")
	if !found || strings.Contains(job, "#") || !actionsJobPattern.MatchString(job) {
		return "", "", errors.New("Actions Public Build target must bind one literal workflow job identifier")
	}
	if !actionsWorkflowPath.MatchString(workflowPath) || strings.Contains(workflowPath, "..") ||
		strings.Contains(workflowPath, "//") {
		return "", "", errors.New("Actions Public Build target must bind one safe .github/workflows file")
	}
	return workflowPath, job, nil
}

func buildFromExistingPublication(request BuildRequest, publication ExistingPublication, now time.Time) (Build, error) {
	if publication.Identity == "" || publication.BuildID == "" || !digestPattern.MatchString(publication.Digest) ||
		publication.SizeBytes < 0 || publication.MediaType == "" || publication.ProducerDuration < 0 {
		return Build{}, errors.New("Public Cache publication index returned an incomplete publication")
	}
	output := OutputDescriptor{
		Name: publication.Identity, Digest: publication.Digest,
		SizeBytes: publication.SizeBytes, MediaType: publication.MediaType,
	}
	return Build{
		ID: publication.BuildID, Request: request, State: StateSucceeded,
		RequestedAt: now, StartedAt: now, FinishedAt: now,
		Publication: &Publication{Outputs: []OutputDescriptor{output}, ProducerDuration: publication.ProducerDuration},
	}, nil
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
	parts := []string{
		request.Repository,
		request.Commit,
		string(request.Integration),
		request.Target,
		request.RecipeDigest,
		string(request.Platform),
		strconv.FormatInt(request.Resources.CPUMillis, 10),
		strconv.FormatInt(request.Resources.MemoryBytes, 10),
		strconv.FormatInt(request.Resources.DiskBytes, 10),
		strconv.FormatInt(request.Resources.Timeout.Nanoseconds(), 10),
	}
	for _, input := range request.Inputs {
		parts = append(parts, input.Name, input.Value)
	}
	return strings.Join(parts, "\x00")
}

// CompatibilityIdentity returns the Public Cache compatibility namespace for
// an admitted build. Public workers and consumers derive this value from the
// requested target platform, never from the control-plane host.
func CompatibilityIdentity(request BuildRequest) (string, error) {
	switch request.Integration {
	case IntegrationTurbo, IntegrationActions:
		for _, input := range request.Inputs {
			if input.Name != "compatibility" {
				continue
			}
			if err := compatibility.Validate(input.Value); err != nil {
				return "", fmt.Errorf("declared compatibility is invalid: %w", err)
			}
			platformPrefix := strings.ReplaceAll(string(request.Platform), "/", "-")
			if input.Value != platformPrefix && !strings.HasPrefix(input.Value, platformPrefix+"-") {
				return "", errors.New("declared compatibility does not match the Public Build platform")
			}
			return input.Value, nil
		}
		return "", errors.New("turbo and actions Public Builds require a declared compatibility input")
	case IntegrationBuildKit:
	default:
		return "", errors.New("unsupported Public Build integration")
	}
	switch request.Platform {
	case PlatformLinuxAMD64:
		return "linux-amd64", nil
	case PlatformLinuxARM64:
		return "linux-arm64", nil
	default:
		return "", errors.New("unsupported Public Build platform")
	}
}

// PublicationProjectIdentity returns the project coordinate consumed by the
// native cache protocol. Actions uses its owner/repository namespace, while
// Turbo and BuildKit use the connected Layer Cache project identity.
func PublicationProjectIdentity(
	integration Integration,
	projectID string,
	actionsRepository string,
	sourceRepository string,
) (string, error) {
	switch integration {
	case IntegrationActions:
		repository, err := normalizeRepository(sourceRepository)
		if err != nil {
			return "", err
		}
		actionsRepository = strings.ToLower(strings.TrimSpace(actionsRepository))
		parts := strings.Split(actionsRepository, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" ||
			actionsRepository != parts[0]+"/"+parts[1] {
			return "", errors.New("Actions publication project must be an owner/repository namespace")
		}
		if repository != "https://github.com/"+actionsRepository {
			return "", errors.New("Actions publication project does not match the Public Build source repository")
		}
		return actionsRepository, nil
	case IntegrationTurbo, IntegrationBuildKit:
		if strings.TrimSpace(projectID) != projectID || projectID == "" {
			return "", errors.New("Layer Cache publication project identity is invalid")
		}
		return projectID, nil
	default:
		return "", errors.New("unsupported Public Build integration")
	}
}

func canonicalDeclaredInputs(inputs []DeclaredInput) ([]DeclaredInput, error) {
	if len(inputs) > maximumDeclaredInputs {
		return nil, fmt.Errorf("Public Build inputs exceed the %d-input limit", maximumDeclaredInputs)
	}
	canonical := append([]DeclaredInput(nil), inputs...)
	slices.SortFunc(canonical, func(left, right DeclaredInput) int {
		return strings.Compare(left.Name, right.Name)
	})
	totalBytes := 0
	for index, input := range canonical {
		if !inputNamePattern.MatchString(input.Name) {
			return nil, errors.New("Public Build input names must be lowercase safe names no longer than 64 bytes")
		}
		if index > 0 && canonical[index-1].Name == input.Name {
			return nil, fmt.Errorf("Public Build input %q is declared more than once", input.Name)
		}
		if !utf8.ValidString(input.Value) || len(input.Value) > maximumDeclaredInputValue ||
			strings.IndexFunc(input.Value, func(character rune) bool {
				return character == 0 || character == '\r' || character == '\n' || character < 0x20 || character == 0x7f
			}) >= 0 {
			return nil, fmt.Errorf("Public Build input %q has an invalid value", input.Name)
		}
		totalBytes += len(input.Name) + len(input.Value)
	}
	if totalBytes > maximumDeclaredInputBytes {
		return nil, fmt.Errorf("Public Build inputs exceed the %d-byte limit", maximumDeclaredInputBytes)
	}
	return canonical, nil
}

func encodeDeclaredInputs(inputs []DeclaredInput) ([]byte, error) {
	if inputs == nil {
		inputs = []DeclaredInput{}
	}
	encoded, err := json.Marshal(inputs)
	if err != nil {
		return nil, fmt.Errorf("encode Public Build inputs: %w", err)
	}
	return encoded, nil
}

func decodeDeclaredInputs(encoded []byte) ([]DeclaredInput, error) {
	var inputs []DeclaredInput
	if err := json.Unmarshal(encoded, &inputs); err != nil {
		return nil, fmt.Errorf("decode Public Build inputs: %w", err)
	}
	canonical, err := canonicalDeclaredInputs(inputs)
	if err != nil {
		return nil, fmt.Errorf("corrupt Public Build inputs: %w", err)
	}
	if !slices.Equal(inputs, canonical) {
		return nil, errors.New("corrupt Public Build inputs are not canonical")
	}
	return canonical, nil
}

func supports(capabilities WorkerCapabilities, request BuildRequest) bool {
	if len(capabilities.Recipes) > 0 {
		recipeSupported := false
		for _, recipe := range capabilities.Recipes {
			if recipe.Integration == request.Integration && recipe.RecipeDigest == request.RecipeDigest &&
				(recipe.Target == "*" || recipe.Target == request.Target) {
				recipeSupported = true
				break
			}
		}
		if !recipeSupported {
			return false
		}
		for _, platform := range capabilities.Platforms {
			if platform == request.Platform {
				return true
			}
		}
		return false
	}
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

func validateCredentialFreePublication(publication Publication, credential string) error {
	if err := validatePublication(publication); err != nil {
		return err
	}
	if credential == "" {
		return nil
	}
	for _, output := range publication.Outputs {
		if strings.Contains(output.Name, credential) || strings.Contains(output.Digest, credential) ||
			strings.Contains(output.MediaType, credential) {
			return errors.New("publication output cannot contain an active credential")
		}
	}
	return nil
}

func cloneBuild(build Build) Build {
	build.Request.Secrets = append([]string(nil), build.Request.Secrets...)
	build.Request.Inputs = append([]DeclaredInput(nil), build.Request.Inputs...)
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

func sanitizePublicBuildLog(config Config, message string, dynamicCredentials ...string) string {
	for _, credential := range dynamicCredentials {
		if credential != "" {
			message = strings.ReplaceAll(message, credential, "[REDACTED]")
		}
	}
	return config.SanitizeLog(message)
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
		Recipes:      append([]WorkerRecipeCapability(nil), worker.Supported.Recipes...),
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
