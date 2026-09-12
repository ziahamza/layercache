package actionscache

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/layercache/layercache/internal/compatibility"
	"github.com/layercache/layercache/internal/measurement"
)

const (
	// Match the stock v1 client's per-archive limit and maximum configurable
	// upload chunk size when a caller does not supply stricter limits.
	defaultMaxArtifactBytes = int64(10 * 1024 * 1024 * 1024)
	defaultMaxChunkBytes    = int64(128 * 1024 * 1024)
	actionsCorrelationTTL   = 24 * time.Hour
	maxActionsCorrelations  = 4_096
)

var (
	ErrNotFound         = errors.New("GitHub Actions cache entry not found")
	ErrAlreadyExists    = errors.New("GitHub Actions cache entry already exists")
	ErrInvalidUpload    = errors.New("invalid GitHub Actions cache upload")
	ErrIncompleteUpload = errors.New("incomplete GitHub Actions cache upload")
	ErrPublicOffline    = errors.New("Public Cache is offline")
)

type Config struct {
	Project                      string
	Repository                   string
	Ref                          string
	DefaultRef                   string
	Compatibility                string
	RequireCompatibilitySelector bool
	MaxArtifactBytes             int64
	ArtifactLimit                func(context.Context) (int64, error)
	MaxChunkBytes                int64
	ArchiveBaseURL               string
	ArchiveURLSigner             ArchiveURLSigner
	ArchiveURLTTL                time.Duration
	RecordOutcome                func(measurement.FinalOutcome) error
	EnrichActionsMiss            func(measurement.ActionsMissCompletion) error
	// RequireRequestAuthority rejects static process-wide scope. Team Cache
	// deployments should enable it and inject RequestAuthority only after
	// authenticating the caller's capability token.
	RequireRequestAuthority bool
}

type Scope struct {
	Project       string
	Repository    string
	Ref           string
	DefaultRef    string
	Compatibility string
	SourceCommit  string
	RecipeDigest  string
	Target        string
	Platform      string
	Toolchain     string
	Builder       string
	RunID         string
	WorkspaceID   string
}

// RequestAuthority is the cache identity authorized for one HTTP request. It
// must come from authenticated token claims, never from client headers.
type RequestAuthority = Scope

type requestAuthorityContextKey struct{}

func WithRequestAuthority(ctx context.Context, authority RequestAuthority) context.Context {
	return context.WithValue(ctx, requestAuthorityContextKey{}, authority)
}

func RequestAuthorityFromContext(ctx context.Context) (RequestAuthority, bool) {
	authority, ok := ctx.Value(requestAuthorityContextKey{}).(RequestAuthority)
	return authority, ok
}

type LookupRequest struct {
	Scope   Scope
	Keys    []string
	Version string
}

type MatchKind string

const (
	MatchExact  MatchKind = "exact"
	MatchPrefix MatchKind = "prefix"
)

type RefScope string

const (
	RefScopeCurrent RefScope = "current"
	RefScopeDefault RefScope = "default"
)

type CacheSource string

const (
	SourceLocalCache  CacheSource = "localCache"
	SourceTeamCache   CacheSource = "teamCache"
	SourcePublicCache CacheSource = "publicCache"
)

type Entry struct {
	ID        int64
	Key       string
	Version   string
	Ref       string
	Size      int64
	CreatedAt time.Time
	// ProducerDuration is the observed work interval between a cache miss and
	// the later save reservation. Nil means the runtime could not correlate the
	// two authenticated requests and must not estimate the value.
	ProducerDuration *time.Duration
	// Origin records who produced the bytes. Source on LookupResult records
	// which cache served this lookup, so a warmed Public artifact has a Local
	// source and a Public origin.
	Origin CacheSource
	Public *PublicEntryMetadata
}

type LookupResult struct {
	Entry        Entry
	Match        MatchKind
	RequestedKey string
	RefScope     RefScope
	Source       CacheSource
	// Degraded reports that this result was served only after a higher cache
	// tier failed. It does not change the fail-open Actions cache semantics.
	Degraded bool
}

type ReserveRequest struct {
	Scope            Scope
	Key              string
	Version          string
	CacheSize        *int64
	MaxArtifactBytes int64
}

type Reservation struct {
	ID int64
}

type UploadRequest struct {
	ReservationID int64
	Scope         *Scope
	Start         int64
	End           int64
	Body          io.Reader
}

type CommitRequest struct {
	ReservationID    int64
	Scope            *Scope
	Size             int64
	ProducerDuration *time.Duration
	Origin           CacheSource
	Public           *PublicEntryMetadata
}

type Archive struct {
	Entry Entry
	Body  io.ReadCloser
}

type OpenRequest struct {
	Scope Scope
	ID    int64
}

func reservationScopeMatches(scope *Scope, repository, compatibilityID, ref string) bool {
	return scope == nil || scope.Repository == repository &&
		scope.Compatibility == compatibilityID && scope.Ref == ref
}

// CacheReader is the read seam for the v1 protocol. Lookup must search
// the current ref before the default ref. Within each ref it must try keys in
// request order, preferring an exact match and then the newest prefix match for
// each key. Open must reject entries outside the supplied scope.
type CacheReader interface {
	Lookup(context.Context, LookupRequest) (LookupResult, error)
	Open(context.Context, OpenRequest) (Archive, error)
}

// CacheWriter is the write seam for the v1 protocol. Reserve must be
// first-writer-wins for one repository, compatibility, ref, key, and version
// identity. Commit publishes an entry atomically.
type CacheWriter interface {
	Reserve(context.Context, ReserveRequest) (Reservation, error)
	Upload(context.Context, UploadRequest) error
	Commit(context.Context, CommitRequest) (Entry, error)
}

// StorageIndex is a writable cache index. Public Cache intentionally satisfies
// CacheReader only, so callers cannot publish to it through this interface.
type StorageIndex interface {
	CacheReader
	CacheWriter
}

type Handler struct {
	config              Config
	storage             StorageIndex
	archiveURLSigner    ArchiveURLSigner
	archiveURLTTL       time.Duration
	mux                 *http.ServeMux
	measurementMu       sync.Mutex
	measurementSequence uint64
	missCorrelations    map[[sha256.Size]byte]actionsMissCorrelation
	saveCorrelations    map[int64]actionsSaveCorrelation
}

type actionsMissCorrelation struct {
	runID      string
	workID     string
	finishedAt time.Time
	expiresAt  time.Time
}

type actionsSaveCorrelation struct {
	runID            string
	workID           string
	scopeIdentity    [sha256.Size]byte
	producerDuration time.Duration
	saveStartedAt    time.Time
	expiresAt        time.Time
}

func NewHandler(config Config, storage StorageIndex) (*Handler, error) {
	if storage == nil {
		return nil, errors.New("GitHub Actions cache storage is required")
	}
	if strings.TrimSpace(config.Project) == "" {
		config.Project = config.Repository
	}
	for name, value := range map[string]string{
		"repository":    config.Repository,
		"ref":           config.Ref,
		"default ref":   config.DefaultRef,
		"compatibility": config.Compatibility,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("GitHub Actions cache %s is required", name)
		}
	}
	if err := compatibility.Validate(config.Compatibility); err != nil {
		return nil, fmt.Errorf("invalid GitHub Actions cache compatibility: %w", err)
	}
	archiveURLTTL := config.ArchiveURLTTL
	if archiveURLTTL == 0 {
		archiveURLTTL = 5 * time.Minute
	}
	if archiveURLTTL < 0 {
		return nil, errors.New("GitHub Actions cache archive URL TTL must be positive")
	}
	if config.MaxArtifactBytes == 0 {
		config.MaxArtifactBytes = defaultMaxArtifactBytes
	}
	if config.MaxArtifactBytes < 0 {
		return nil, errors.New("GitHub Actions cache maximum artifact size must be positive")
	}
	if config.MaxChunkBytes == 0 {
		config.MaxChunkBytes = defaultMaxChunkBytes
	}
	if config.MaxChunkBytes < 0 {
		return nil, errors.New("GitHub Actions cache maximum chunk size must be positive")
	}
	archiveURLSigner := config.ArchiveURLSigner
	if archiveURLSigner == nil {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, fmt.Errorf("create GitHub Actions archive URL signer: %w", err)
		}
		archiveURLSigner = newHMACArchiveURLSigner(secret)
	}

	handler := &Handler{
		config: config, storage: storage, archiveURLSigner: archiveURLSigner,
		archiveURLTTL: archiveURLTTL, mux: http.NewServeMux(),
		missCorrelations: make(map[[sha256.Size]byte]actionsMissCorrelation),
		saveCorrelations: make(map[int64]actionsSaveCorrelation),
	}
	handler.mux.HandleFunc("GET /_apis/artifactcache/cache", handler.lookup)
	handler.mux.HandleFunc("POST /_apis/artifactcache/caches", handler.reserve)
	handler.mux.HandleFunc("PATCH /_apis/artifactcache/caches/{id}", handler.upload)
	handler.mux.HandleFunc("POST /_apis/artifactcache/caches/{id}", handler.commit)
	handler.mux.HandleFunc("DELETE /_apis/artifactcache/caches/{id}", handler.abort)
	handler.mux.HandleFunc("GET /_apis/artifactcache/caches/{id}/archive", handler.download)
	const compatibilityPrefix = "/_layercache/compatibility/{compatibility}/_apis/artifactcache"
	handler.mux.HandleFunc("GET "+compatibilityPrefix+"/cache", handler.lookup)
	handler.mux.HandleFunc("POST "+compatibilityPrefix+"/caches", handler.reserve)
	handler.mux.HandleFunc("PATCH "+compatibilityPrefix+"/caches/{id}", handler.upload)
	handler.mux.HandleFunc("POST "+compatibilityPrefix+"/caches/{id}", handler.commit)
	handler.mux.HandleFunc("DELETE "+compatibilityPrefix+"/caches/{id}", handler.abort)
	handler.mux.HandleFunc("GET "+compatibilityPrefix+"/caches/{id}/archive", handler.download)
	return handler, nil
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	handler.mux.ServeHTTP(writer, request)
}

func (handler *Handler) lookup(writer http.ResponseWriter, request *http.Request) {
	startedAt := time.Now().UTC()
	scope, err := handler.scope(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	keys, err := parseLookupKeys(request.URL.Query().Get("keys"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	version := request.URL.Query().Get("version")
	if strings.TrimSpace(version) == "" {
		writeError(writer, http.StatusBadRequest, "cache version is required")
		return
	}
	lookup := LookupRequest{
		Scope:   scope,
		Keys:    keys,
		Version: version,
	}
	result, err := handler.storage.Lookup(request.Context(), lookup)
	if errors.Is(err, ErrNotFound) {
		finishedAt := time.Now().UTC()
		requestIdentity := actionsArtifactIdentity(scope, keys[:1], version)
		outcome := handler.newActionsOutcome(scope, "restore", requestIdentity, requestIdentity)
		outcome.Result = measurement.ResultMiss
		outcome.Source = measurement.SourceNone
		outcome.StartedAt = startedAt
		outcome.FinishedAt = finishedAt
		outcome.Timing.Lookup = finishedAt.Sub(startedAt)
		outcome.Degraded = errors.Is(err, errRemoteDegraded)
		handler.recordOutcome(outcome)
		handler.rememberActionsMiss(scope, keys[0], version, outcome, finishedAt)
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "cache lookup failed")
		return
	}
	writer.Header().Set("X-LayerCache-Match", string(result.Match))
	writer.Header().Set("X-LayerCache-Requested-Key", result.RequestedKey)
	writer.Header().Set("X-LayerCache-Ref-Scope", string(result.RefScope))
	writer.Header().Set("X-LayerCache-Source", string(result.Source))
	finishedLookupAt := time.Now().UTC()
	requestIdentity := actionsArtifactIdentity(scope, keys[:1], version)
	outcome := handler.newActionsOutcome(
		scope, "restore", requestIdentity,
		actionsArtifactIdentity(scopeForEntry(scope, result.Entry.Ref), []string{result.Entry.Key}, result.Entry.Version),
	)
	outcome.Result = measurement.ResultHit
	outcome.Source = actionsMeasurementSource(result.Source)
	outcome.StartedAt = startedAt
	outcome.FinishedAt = finishedLookupAt
	outcome.Timing.Lookup = finishedLookupAt.Sub(startedAt)
	outcome.Bytes.Downloaded = result.Entry.Size
	outcome.ProducerDuration = cloneDuration(result.Entry.ProducerDuration)
	outcome.Degraded = result.Degraded
	archiveLocation, err := handler.archiveLocation(request, result.Entry.ID, scope, outcome)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "cache archive URL signing failed")
		return
	}
	writeJSON(writer, http.StatusOK, struct {
		CacheKey                              string               `json:"cacheKey"`
		Scope                                 string               `json:"scope"`
		CacheVersion                          string               `json:"cacheVersion"`
		CreationTime                          time.Time            `json:"creationTime"`
		ArchiveLocation                       string               `json:"archiveLocation"`
		LayerCacheOrigin                      CacheSource          `json:"layerCacheOrigin"`
		LayerCachePublic                      *PublicEntryMetadata `json:"layerCachePublic,omitempty"`
		LayerCacheProducerDurationNanoseconds *int64               `json:"layerCacheProducerDurationNanoseconds,omitempty"`
	}{
		CacheKey:                              result.Entry.Key,
		Scope:                                 result.Entry.Ref,
		CacheVersion:                          result.Entry.Version,
		CreationTime:                          result.Entry.CreatedAt,
		ArchiveLocation:                       archiveLocation,
		LayerCacheOrigin:                      result.Entry.Origin,
		LayerCachePublic:                      clonePublicEntryMetadata(result.Entry.Public),
		LayerCacheProducerDurationNanoseconds: durationNanoseconds(result.Entry.ProducerDuration),
	})
}

func (handler *Handler) reserve(writer http.ResponseWriter, request *http.Request) {
	startedAt := time.Now().UTC()
	scope, err := handler.scope(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	defer request.Body.Close()
	var body struct {
		Key       string `json:"key"`
		Version   string `json:"version"`
		CacheSize *int64 `json:"cacheSize"`
	}
	if err := json.NewDecoder(io.LimitReader(request.Body, 1<<20)).Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid reserve request")
		return
	}
	if err := validateKey(body.Key); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.Version) == "" {
		writeError(writer, http.StatusBadRequest, "cache version is required")
		return
	}
	if body.CacheSize != nil && *body.CacheSize < 0 {
		writeError(writer, http.StatusBadRequest, "cache size cannot be negative")
		return
	}
	maximum, err := handler.artifactLimit(request.Context())
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "cache quota is unavailable")
		return
	}
	if body.CacheSize != nil && *body.CacheSize > maximum {
		writeError(writer, http.StatusBadRequest, "cache size exceeds the configured maximum artifact size")
		return
	}
	reservation, err := handler.storage.Reserve(request.Context(), ReserveRequest{
		Scope:            scope,
		Key:              body.Key,
		Version:          body.Version,
		CacheSize:        body.CacheSize,
		MaxArtifactBytes: maximum,
	})
	if errors.Is(err, ErrAlreadyExists) {
		writeError(writer, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "cache reservation failed")
		return
	}
	handler.correlateActionsSave(reservation.ID, scope, body.Key, body.Version, startedAt)
	writeJSON(writer, http.StatusCreated, struct {
		CacheID int64 `json:"cacheId"`
	}{CacheID: reservation.ID})
}

func (handler *Handler) upload(writer http.ResponseWriter, request *http.Request) {
	scope, err := handler.scope(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	id, err := parseID(request.PathValue("id"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	start, end, err := parseContentRange(request.Header.Get("Content-Range"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	maximum, err := handler.artifactLimit(request.Context())
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "cache quota is unavailable")
		return
	}
	if end >= maximum {
		writeError(writer, http.StatusBadRequest, "cache upload exceeds the configured maximum artifact size")
		return
	}
	chunkSize := end - start + 1
	if chunkSize > handler.config.MaxChunkBytes {
		writeError(writer, http.StatusBadRequest, "cache upload exceeds the configured maximum chunk size")
		return
	}
	defer request.Body.Close()
	uploadBody := io.Reader(request.Body)
	if chunkSize < math.MaxInt64 {
		uploadBody = io.LimitReader(request.Body, chunkSize+1)
	}
	err = handler.storage.Upload(request.Context(), UploadRequest{
		ReservationID: id,
		Scope:         &scope,
		Start:         start,
		End:           end,
		Body:          uploadBody,
	})
	if errors.Is(err, ErrNotFound) {
		writeError(writer, http.StatusNotFound, err.Error())
		return
	}
	if errors.Is(err, ErrInvalidUpload) {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "cache upload failed")
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (handler *Handler) commit(writer http.ResponseWriter, request *http.Request) {
	scope, err := handler.scope(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	id, err := parseID(request.PathValue("id"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	defer request.Body.Close()
	var body struct {
		Size                                  *int64 `json:"size"`
		LayerCacheProducerDurationNanoseconds *int64 `json:"layerCacheProducerDurationNanoseconds,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(request.Body, 1<<20)).Decode(&body); err != nil || body.Size == nil {
		writeError(writer, http.StatusBadRequest, "invalid commit request")
		return
	}
	if *body.Size < 0 {
		writeError(writer, http.StatusBadRequest, "cache size cannot be negative")
		return
	}
	maximum, err := handler.artifactLimit(request.Context())
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "cache quota is unavailable")
		return
	}
	if *body.Size > maximum {
		writeError(writer, http.StatusBadRequest, "cache size exceeds the configured maximum artifact size")
		return
	}
	producerDuration := handler.actionsSaveProducerDuration(id, scope, time.Now().UTC())
	if producerDuration == nil && scope.RunID == "" && body.LayerCacheProducerDurationNanoseconds != nil {
		if *body.LayerCacheProducerDurationNanoseconds < 0 {
			writeError(writer, http.StatusBadRequest, "producer duration cannot be negative")
			return
		}
		value := time.Duration(*body.LayerCacheProducerDurationNanoseconds)
		producerDuration = &value
	}
	entry, err := handler.storage.Commit(request.Context(), CommitRequest{
		ReservationID: id, Scope: &scope, Size: *body.Size, ProducerDuration: producerDuration,
	})
	if errors.Is(err, ErrNotFound) {
		writeError(writer, http.StatusNotFound, err.Error())
		return
	}
	if errors.Is(err, ErrInvalidUpload) || errors.Is(err, ErrIncompleteUpload) {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "cache commit failed")
		return
	}
	handler.completeActionsSave(id, scope, entry.Size, time.Now().UTC())
	writer.WriteHeader(http.StatusNoContent)
}

func (handler *Handler) artifactLimit(ctx context.Context) (int64, error) {
	if handler.config.ArtifactLimit != nil {
		maximum, err := handler.config.ArtifactLimit(ctx)
		if err != nil {
			return 0, err
		}
		if maximum <= 0 {
			return 0, errors.New("invalid cache quota")
		}
		return maximum, nil
	}
	return handler.config.MaxArtifactBytes, nil
}

func (handler *Handler) abort(writer http.ResponseWriter, request *http.Request) {
	scope, err := handler.scope(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	id, err := parseID(request.PathValue("id"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	aborter, ok := handler.storage.(scopedReservationAborter)
	if !ok {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := aborter.AbortScoped(request.Context(), id, &scope); errors.Is(err, ErrNotFound) {
		writeError(writer, http.StatusNotFound, err.Error())
		return
	} else if err != nil {
		writeError(writer, http.StatusInternalServerError, "cache reservation abort failed")
		return
	}
	handler.forgetActionsSave(id, scope)
	writer.WriteHeader(http.StatusNoContent)
}

func (handler *Handler) download(writer http.ResponseWriter, request *http.Request) {
	if !handler.AuthorizesArchiveDownload(request) {
		writeError(writer, http.StatusNotFound, ErrNotFound.Error())
		return
	}
	id, err := parseID(request.PathValue("id"))
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	scope, err := handler.downloadScope(request)
	if err != nil {
		writeError(writer, http.StatusNotFound, ErrNotFound.Error())
		return
	}
	archive, err := handler.storage.Open(request.Context(), OpenRequest{Scope: scope, ID: id})
	if errors.Is(err, ErrNotFound) {
		writeError(writer, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "cache download failed")
		return
	}
	defer archive.Body.Close()
	measurementOutcome, measurementErr := decodeArchiveOutcome(request.URL.Query().Get(archiveOutcomeQuery))
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Content-Length", strconv.FormatInt(archive.Entry.Size, 10))
	writer.WriteHeader(http.StatusOK)
	downloadStartedAt := time.Now()
	written, copyErr := io.Copy(writer, archive.Body)
	if copyErr == nil && measurementErr == nil && written == archive.Entry.Size {
		measurementOutcome.FinishedAt = time.Now().UTC()
		measurementOutcome.Timing.Download = time.Since(downloadStartedAt)
		measurementOutcome.Bytes.Downloaded = written
		handler.recordOutcome(measurementOutcome)
	}
}

func (handler *Handler) downloadScope(request *http.Request) (Scope, error) {
	if encoded := request.URL.Query().Get(archiveAuthorityQuery); encoded != "" {
		if !handler.AuthorizesArchiveDownload(request) {
			return Scope{}, ErrNotFound
		}
		return decodeArchiveAuthority(encoded)
	}
	return handler.scope(request)
}

func (handler *Handler) scope(request *http.Request) (Scope, error) {
	scope, hasAuthority := RequestAuthorityFromContext(request.Context())
	if !hasAuthority {
		if handler.config.RequireRequestAuthority {
			return Scope{}, errors.New("authenticated GitHub Actions cache authority is required")
		}
		scope = Scope{
			Project:       handler.config.Project,
			Repository:    handler.config.Repository,
			Ref:           handler.config.Ref,
			DefaultRef:    handler.config.DefaultRef,
			Compatibility: handler.config.Compatibility,
		}
	}
	if err := validateScope(scope); err != nil {
		return Scope{}, err
	}
	headerValues := request.Header.Values(compatibility.Header)
	if len(headerValues) > 1 {
		return Scope{}, errors.New("send exactly one compatibility selector")
	}
	headerValue := ""
	if len(headerValues) == 1 {
		headerValue = headerValues[0]
	}
	pathValue := request.PathValue("compatibility")
	if headerValue != "" && pathValue != "" && headerValue != pathValue {
		return Scope{}, errors.New("compatibility selectors disagree")
	}
	selected := headerValue
	if selected == "" {
		selected = pathValue
	}
	if selected == "" && (!handler.config.RequireCompatibilitySelector || hasAuthority) {
		return scope, nil
	}
	if err := compatibility.Validate(selected); err != nil {
		return Scope{}, err
	}
	if hasAuthority && selected != scope.Compatibility {
		return Scope{}, errors.New("compatibility selector is outside the authenticated authority")
	}
	if !hasAuthority {
		scope.Compatibility = selected
	}
	return scope, nil
}

func validateScope(scope Scope) error {
	for name, value := range map[string]string{
		"repository":  scope.Repository,
		"ref":         scope.Ref,
		"default ref": scope.DefaultRef,
	} {
		if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
			return fmt.Errorf("GitHub Actions cache %s authority is invalid", name)
		}
	}
	if err := compatibility.Validate(scope.Compatibility); err != nil {
		return fmt.Errorf("GitHub Actions cache compatibility authority is invalid: %w", err)
	}
	for name, value := range map[string]string{
		"project": scope.Project, "run ID": scope.RunID, "workspace ID": scope.WorkspaceID,
	} {
		if value != "" && (strings.TrimSpace(value) != value || len(value) > 512 || strings.ContainsAny(value, "\x00\r\n")) {
			return fmt.Errorf("GitHub Actions cache %s authority is invalid", name)
		}
	}
	return nil
}

// AuthorizesArchiveDownload is the integration hook for bearer middleware.
// A server may bypass bearer authentication only when this returns true; the
// download handler independently repeats the same verification before serving
// bytes.
func (handler *Handler) AuthorizesArchiveDownload(request *http.Request) bool {
	if request.Method != http.MethodGet {
		return false
	}
	if _, err := archiveIDFromPath(request.URL.Path); err != nil {
		return false
	}
	query := request.URL.Query()
	expiresValues, signatureValues := query[archiveExpiryQuery], query[archiveSignatureQuery]
	authorityValues := query[archiveAuthorityQuery]
	outcomeValues := query[archiveOutcomeQuery]
	if len(query) != 4 || len(expiresValues) != 1 || len(signatureValues) != 1 || len(authorityValues) != 1 || len(outcomeValues) != 1 {
		return false
	}
	expiresMilliseconds, err := strconv.ParseInt(expiresValues[0], 10, 64)
	if err != nil {
		return false
	}
	expiresAt := time.UnixMilli(expiresMilliseconds).UTC()
	if !time.Now().UTC().Before(expiresAt) {
		return false
	}
	if _, err := decodeArchiveAuthority(authorityValues[0]); err != nil {
		return false
	}
	if _, err := decodeArchiveOutcome(outcomeValues[0]); err != nil {
		return false
	}
	resource := archiveSignatureResource(request.URL.EscapedPath(), authorityValues[0], outcomeValues[0])
	return handler.archiveURLSigner.VerifyArchiveURL(resource, expiresAt, signatureValues[0]) == nil
}

func (handler *Handler) archiveLocation(
	request *http.Request,
	id int64,
	scope Scope,
	outcome measurement.FinalOutcome,
) (string, error) {
	base := strings.TrimRight(handler.config.ArchiveBaseURL, "/")
	if base == "" {
		scheme := request.Header.Get("X-Forwarded-Proto")
		if scheme == "" {
			if request.TLS != nil {
				scheme = "https"
			} else {
				scheme = "http"
			}
		}
		base = scheme + "://" + request.Host
	}
	archivePath := fmt.Sprintf("/_apis/artifactcache/caches/%d/archive", id)
	if handler.config.RequireCompatibilitySelector || request.Header.Get(compatibility.Header) != "" || request.PathValue("compatibility") != "" {
		archivePath = fmt.Sprintf("/_layercache/compatibility/%s%s", url.PathEscape(scope.Compatibility), archivePath)
	}
	location, err := url.Parse(base + archivePath)
	if err != nil {
		return "", err
	}
	expiresAt := time.Now().UTC().Add(handler.archiveURLTTL).Truncate(time.Millisecond)
	authority, err := encodeArchiveAuthority(scope)
	if err != nil {
		return "", err
	}
	encodedOutcome, err := encodeArchiveOutcome(outcome)
	if err != nil {
		return "", err
	}
	signature, err := handler.archiveURLSigner.SignArchiveURL(
		archiveSignatureResource(location.EscapedPath(), authority, encodedOutcome), expiresAt,
	)
	if err != nil {
		return "", err
	}
	query := location.Query()
	query.Set(archiveExpiryQuery, strconv.FormatInt(expiresAt.UnixMilli(), 10))
	query.Set(archiveAuthorityQuery, authority)
	query.Set(archiveOutcomeQuery, encodedOutcome)
	query.Set(archiveSignatureQuery, signature)
	location.RawQuery = query.Encode()
	return location.String(), nil
}

func archiveSignatureResource(escapedPath, authority, outcome string) string {
	return escapedPath + "\nauthority=" + authority + "\noutcome=" + outcome
}

func encodeArchiveAuthority(scope Scope) (string, error) {
	encoded, err := json.Marshal(scope)
	if err != nil {
		return "", fmt.Errorf("encode GitHub Actions archive authority: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeArchiveAuthority(encoded string) (Scope, error) {
	bytes, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(bytes) == 0 || len(bytes) > 16<<10 {
		return Scope{}, errors.New("GitHub Actions archive authority is invalid")
	}
	decoder := json.NewDecoder(strings.NewReader(string(bytes)))
	decoder.DisallowUnknownFields()
	var scope Scope
	if err := decoder.Decode(&scope); err != nil {
		return Scope{}, errors.New("GitHub Actions archive authority is invalid")
	}
	if err := validateScope(scope); err != nil {
		return Scope{}, err
	}
	return scope, nil
}

func encodeArchiveOutcome(outcome measurement.FinalOutcome) (string, error) {
	encoded, err := json.Marshal(outcome)
	if err != nil {
		return "", fmt.Errorf("encode GitHub Actions archive outcome: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeArchiveOutcome(encoded string) (measurement.FinalOutcome, error) {
	bytes, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(bytes) == 0 || len(bytes) > 16<<10 {
		return measurement.FinalOutcome{}, errors.New("GitHub Actions archive outcome is invalid")
	}
	decoder := json.NewDecoder(strings.NewReader(string(bytes)))
	decoder.DisallowUnknownFields()
	var outcome measurement.FinalOutcome
	if err := decoder.Decode(&outcome); err != nil || outcome.Integration != measurement.IntegrationActions ||
		outcome.Result != measurement.ResultHit || outcome.Source == measurement.SourceNone ||
		outcome.RunID == "" || outcome.WorkID == "" || outcome.StartedAt.IsZero() {
		return measurement.FinalOutcome{}, errors.New("GitHub Actions archive outcome is invalid")
	}
	return outcome, nil
}

func archiveIDFromPath(value string) (int64, error) {
	const prefix = "/_apis/artifactcache/caches/"
	const suffix = "/archive"
	if strings.HasPrefix(value, "/_layercache/compatibility/") {
		selectedAndPath := strings.TrimPrefix(value, "/_layercache/compatibility/")
		selected, path, found := strings.Cut(selectedAndPath, "/")
		if !found || compatibility.Validate(selected) != nil {
			return 0, ErrNotFound
		}
		value = "/" + path
	}
	idValue, found := strings.CutPrefix(value, prefix)
	if !found {
		return 0, ErrNotFound
	}
	idValue, found = strings.CutSuffix(idValue, suffix)
	if !found || strings.Contains(idValue, "/") {
		return 0, ErrNotFound
	}
	return parseID(idValue)
}

func parseLookupKeys(encoded string) ([]string, error) {
	if encoded == "" {
		return nil, errors.New("at least one cache key is required")
	}
	keys := strings.Split(encoded, ",")
	if len(keys) > 10 {
		return nil, errors.New("use at most 10 cache keys")
	}
	for _, key := range keys {
		if err := validateKey(key); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

func validateKey(key string) error {
	if key == "" {
		return errors.New("cache key cannot be empty")
	}
	if len(key) > 512 {
		return errors.New("cache key cannot exceed 512 characters")
	}
	if strings.Contains(key, ",") {
		return errors.New("cache key cannot contain commas")
	}
	return nil
}

func (handler *Handler) newActionsOutcome(scope Scope, operation, workIdentity, artifactID string) measurement.FinalOutcome {
	if scope.RunID != "" {
		identity := sha256.Sum256([]byte(strings.Join([]string{
			"actions-outcome-v2", scope.RunID, scope.WorkspaceID, operation, workIdentity,
		}, "\x00")))
		return measurement.FinalOutcome{
			RunID: scope.RunID, WorkspaceID: scope.WorkspaceID, Integration: measurement.IntegrationActions,
			WorkID: fmt.Sprintf("sha256:%x", identity[:]), ArtifactID: artifactID, CompatibilityID: scope.Compatibility,
		}
	}
	handler.measurementMu.Lock()
	handler.measurementSequence++
	sequence := handler.measurementSequence
	handler.measurementMu.Unlock()
	identity := sha256.Sum256([]byte(fmt.Sprintf(
		"actions-outcome\x00%s\x00%s\x00%d\x00%d", scope.RunID, operation, time.Now().UnixNano(), sequence,
	)))
	privateID := fmt.Sprintf("sha256:%x", identity[:])
	runID := scope.RunID
	if runID == "" {
		runID = "run-actions-" + fmt.Sprintf("%x", identity[:10])
	}
	return measurement.FinalOutcome{
		RunID: runID, WorkspaceID: scope.WorkspaceID, Integration: measurement.IntegrationActions,
		WorkID: privateID, ArtifactID: artifactID, CompatibilityID: scope.Compatibility,
	}
}

func (handler *Handler) rememberActionsMiss(
	scope Scope,
	key, version string,
	outcome measurement.FinalOutcome,
	now time.Time,
) {
	if scope.RunID == "" {
		return
	}
	identity := actionsCorrelationIdentity(scope, key, version)
	handler.measurementMu.Lock()
	defer handler.measurementMu.Unlock()
	handler.pruneActionsCorrelationsLocked(now)
	if _, replacing := handler.missCorrelations[identity]; !replacing {
		handler.makeActionsCorrelationRoomLocked()
	}
	handler.missCorrelations[identity] = actionsMissCorrelation{
		runID: outcome.RunID, workID: outcome.WorkID, finishedAt: outcome.FinishedAt,
		expiresAt: now.Add(actionsCorrelationTTL),
	}
}

func (handler *Handler) correlateActionsSave(
	reservationID int64,
	scope Scope,
	key, version string,
	startedAt time.Time,
) {
	if scope.RunID == "" {
		return
	}
	identity := actionsCorrelationIdentity(scope, key, version)
	handler.measurementMu.Lock()
	defer handler.measurementMu.Unlock()
	handler.pruneActionsCorrelationsLocked(startedAt)
	miss, found := handler.missCorrelations[identity]
	if !found || startedAt.Before(miss.finishedAt) {
		return
	}
	delete(handler.missCorrelations, identity)
	handler.saveCorrelations[reservationID] = actionsSaveCorrelation{
		runID: miss.runID, workID: miss.workID, scopeIdentity: actionsScopeCorrelationIdentity(scope),
		producerDuration: startedAt.Sub(miss.finishedAt), saveStartedAt: startedAt,
		expiresAt: startedAt.Add(actionsCorrelationTTL),
	}
}

func (handler *Handler) actionsSaveProducerDuration(
	reservationID int64,
	scope Scope,
	now time.Time,
) *time.Duration {
	handler.measurementMu.Lock()
	defer handler.measurementMu.Unlock()
	handler.pruneActionsCorrelationsLocked(now)
	correlation, found := handler.saveCorrelations[reservationID]
	if !found || correlation.scopeIdentity != actionsScopeCorrelationIdentity(scope) {
		return nil
	}
	value := correlation.producerDuration
	return &value
}

func (handler *Handler) completeActionsSave(
	reservationID int64,
	scope Scope,
	uploadedBytes int64,
	finishedAt time.Time,
) {
	handler.measurementMu.Lock()
	handler.pruneActionsCorrelationsLocked(finishedAt)
	correlation, found := handler.saveCorrelations[reservationID]
	if found && correlation.scopeIdentity == actionsScopeCorrelationIdentity(scope) {
		delete(handler.saveCorrelations, reservationID)
	} else {
		found = false
	}
	handler.measurementMu.Unlock()
	if !found || finishedAt.Before(correlation.saveStartedAt) || handler.config.EnrichActionsMiss == nil {
		return
	}
	_ = handler.config.EnrichActionsMiss(measurement.ActionsMissCompletion{
		RunID: correlation.runID, WorkID: correlation.workID, FinishedAt: finishedAt,
		ExecutionDuration: correlation.producerDuration,
		UploadDuration:    finishedAt.Sub(correlation.saveStartedAt), UploadedBytes: uploadedBytes,
	})
}

func (handler *Handler) forgetActionsSave(reservationID int64, scope Scope) {
	handler.measurementMu.Lock()
	defer handler.measurementMu.Unlock()
	correlation, found := handler.saveCorrelations[reservationID]
	if found && correlation.scopeIdentity == actionsScopeCorrelationIdentity(scope) {
		delete(handler.saveCorrelations, reservationID)
	}
}

func (handler *Handler) pruneActionsCorrelationsLocked(now time.Time) {
	for identity, correlation := range handler.missCorrelations {
		if !now.Before(correlation.expiresAt) {
			delete(handler.missCorrelations, identity)
		}
	}
	for id, correlation := range handler.saveCorrelations {
		if !now.Before(correlation.expiresAt) {
			delete(handler.saveCorrelations, id)
		}
	}
}

func (handler *Handler) makeActionsCorrelationRoomLocked() {
	if len(handler.missCorrelations)+len(handler.saveCorrelations) < maxActionsCorrelations {
		return
	}
	var oldestMiss [sha256.Size]byte
	var oldestSave int64
	var oldestExpiry time.Time
	isSave := false
	for identity, correlation := range handler.missCorrelations {
		if oldestExpiry.IsZero() || correlation.expiresAt.Before(oldestExpiry) {
			oldestMiss, oldestExpiry, isSave = identity, correlation.expiresAt, false
		}
	}
	for id, correlation := range handler.saveCorrelations {
		if oldestExpiry.IsZero() || correlation.expiresAt.Before(oldestExpiry) {
			oldestSave, oldestExpiry, isSave = id, correlation.expiresAt, true
		}
	}
	if isSave {
		delete(handler.saveCorrelations, oldestSave)
	} else if !oldestExpiry.IsZero() {
		delete(handler.missCorrelations, oldestMiss)
	}
}

func actionsCorrelationIdentity(scope Scope, key, version string) [sha256.Size]byte {
	return hashActionsCorrelation([]string{
		"actions-work-v1", scope.RunID, scope.WorkspaceID, scope.Project, scope.Repository,
		scope.Ref, scope.Compatibility, key, version,
	})
}

func actionsScopeCorrelationIdentity(scope Scope) [sha256.Size]byte {
	return hashActionsCorrelation([]string{
		"actions-scope-v1", scope.RunID, scope.WorkspaceID, scope.Project, scope.Repository,
		scope.Ref, scope.Compatibility,
	})
}

func hashActionsCorrelation(parts []string) [sha256.Size]byte {
	hasher := sha256.New()
	for _, part := range parts {
		_, _ = hasher.Write([]byte(part))
		_, _ = hasher.Write([]byte{0})
	}
	var identity [sha256.Size]byte
	copy(identity[:], hasher.Sum(nil))
	return identity
}

func (handler *Handler) recordOutcome(outcome measurement.FinalOutcome) {
	if handler.config.RecordOutcome != nil {
		_ = handler.config.RecordOutcome(outcome)
	}
}

func actionsArtifactIdentity(scope Scope, keys []string, version string) string {
	hasher := sha256.New()
	for _, value := range append([]string{
		"actions", scope.Project, scope.Repository, scope.Ref, scope.Compatibility, version,
	}, keys...) {
		_, _ = hasher.Write([]byte(value))
		_, _ = hasher.Write([]byte{0})
	}
	return fmt.Sprintf("sha256:%x", hasher.Sum(nil))
}

func scopeForEntry(scope Scope, ref string) Scope {
	scope.Ref = ref
	return scope
}

func actionsMeasurementSource(source CacheSource) measurement.Source {
	switch source {
	case SourceLocalCache:
		return measurement.SourceLocalCache
	case SourceTeamCache:
		return measurement.SourceTeamCache
	case SourcePublicCache:
		return measurement.SourcePublicCache
	default:
		return measurement.SourceUnattributed
	}
}

func parseID(value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("cache id must be a positive integer")
	}
	return id, nil
}

func parseContentRange(value string) (int64, int64, error) {
	rangeValue, ok := strings.CutPrefix(value, "bytes ")
	if !ok {
		return 0, 0, errors.New("Content-Range must use bytes start-end/*")
	}
	positions, total, ok := strings.Cut(rangeValue, "/")
	if !ok || total != "*" {
		return 0, 0, errors.New("Content-Range must use bytes start-end/*")
	}
	startValue, endValue, ok := strings.Cut(positions, "-")
	if !ok {
		return 0, 0, errors.New("Content-Range must use bytes start-end/*")
	}
	start, startErr := strconv.ParseInt(startValue, 10, 64)
	end, endErr := strconv.ParseInt(endValue, 10, 64)
	if startErr != nil || endErr != nil || start < 0 || end < start {
		return 0, 0, errors.New("Content-Range must use bytes start-end/*")
	}
	return start, end, nil
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{"message": message})
}
