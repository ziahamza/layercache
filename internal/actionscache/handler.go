package actionscache

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/compatibility"
)

const (
	// Match the stock v1 client's per-archive limit and maximum configurable
	// upload chunk size when a caller does not supply stricter limits.
	defaultMaxArtifactBytes = int64(10 * 1024 * 1024 * 1024)
	defaultMaxChunkBytes    = int64(128 * 1024 * 1024)
)

var (
	ErrNotFound         = errors.New("GitHub Actions cache entry not found")
	ErrAlreadyExists    = errors.New("GitHub Actions cache entry already exists")
	ErrInvalidUpload    = errors.New("invalid GitHub Actions cache upload")
	ErrIncompleteUpload = errors.New("incomplete GitHub Actions cache upload")
	ErrReadOnly         = errors.New("Public Cache is read-only")
	ErrPublicOffline    = errors.New("Public Cache is offline")
)

type Config struct {
	Repository                   string
	Ref                          string
	DefaultRef                   string
	Compatibility                string
	RequireCompatibilitySelector bool
	MaxArtifactBytes             int64
	MaxChunkBytes                int64
	ArchiveBaseURL               string
	ArchiveURLSigner             ArchiveURLSigner
	ArchiveURLTTL                time.Duration
}

type Scope struct {
	Repository    string
	Ref           string
	DefaultRef    string
	Compatibility string
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
	ReservationID int64
	Scope         *Scope
	Size          int64
	Origin        CacheSource
	Public        *PublicEntryMetadata
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

// StorageIndex is the persistence seam for the v1 protocol. Lookup must search
// the current ref before the default ref. Within each ref it must try keys in
// request order, preferring an exact match and then the newest prefix match for
// each key. Reserve must be first-writer-wins for one repository,
// compatibility, ref, key, and version identity. Commit publishes an entry
// atomically, and Open must reject entries outside the supplied scope.
type StorageIndex interface {
	Lookup(context.Context, LookupRequest) (LookupResult, error)
	Reserve(context.Context, ReserveRequest) (Reservation, error)
	Upload(context.Context, UploadRequest) error
	Commit(context.Context, CommitRequest) (Entry, error)
	Open(context.Context, OpenRequest) (Archive, error)
}

type Handler struct {
	config           Config
	storage          StorageIndex
	archiveURLSigner ArchiveURLSigner
	archiveURLTTL    time.Duration
	mux              *http.ServeMux
}

func NewHandler(config Config, storage StorageIndex) (*Handler, error) {
	if storage == nil {
		return nil, errors.New("GitHub Actions cache storage is required")
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
	}
	handler.mux.HandleFunc("GET /_apis/artifactcache/cache", handler.lookup)
	handler.mux.HandleFunc("POST /_apis/artifactcache/caches", handler.reserve)
	handler.mux.HandleFunc("PATCH /_apis/artifactcache/caches/{id}", handler.upload)
	handler.mux.HandleFunc("POST /_apis/artifactcache/caches/{id}", handler.commit)
	handler.mux.HandleFunc("GET /_apis/artifactcache/caches/{id}/archive", handler.download)
	const compatibilityPrefix = "/_layercache/compatibility/{compatibility}/_apis/artifactcache"
	handler.mux.HandleFunc("GET "+compatibilityPrefix+"/cache", handler.lookup)
	handler.mux.HandleFunc("POST "+compatibilityPrefix+"/caches", handler.reserve)
	handler.mux.HandleFunc("PATCH "+compatibilityPrefix+"/caches/{id}", handler.upload)
	handler.mux.HandleFunc("POST "+compatibilityPrefix+"/caches/{id}", handler.commit)
	handler.mux.HandleFunc("GET "+compatibilityPrefix+"/caches/{id}/archive", handler.download)
	return handler, nil
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	handler.mux.ServeHTTP(writer, request)
}

func (handler *Handler) lookup(writer http.ResponseWriter, request *http.Request) {
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
	archiveLocation, err := handler.archiveLocation(request, result.Entry.ID, scope)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "cache archive URL signing failed")
		return
	}
	writeJSON(writer, http.StatusOK, struct {
		CacheKey        string    `json:"cacheKey"`
		Scope           string    `json:"scope"`
		CacheVersion    string    `json:"cacheVersion"`
		CreationTime    time.Time `json:"creationTime"`
		ArchiveLocation string    `json:"archiveLocation"`
	}{
		CacheKey:        result.Entry.Key,
		Scope:           result.Entry.Ref,
		CacheVersion:    result.Entry.Version,
		CreationTime:    result.Entry.CreatedAt,
		ArchiveLocation: archiveLocation,
	})
}

func (handler *Handler) reserve(writer http.ResponseWriter, request *http.Request) {
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
	if body.CacheSize != nil && *body.CacheSize > handler.config.MaxArtifactBytes {
		writeError(writer, http.StatusBadRequest, "cache size exceeds the configured maximum artifact size")
		return
	}
	reservation, err := handler.storage.Reserve(request.Context(), ReserveRequest{
		Scope:            scope,
		Key:              body.Key,
		Version:          body.Version,
		CacheSize:        body.CacheSize,
		MaxArtifactBytes: handler.config.MaxArtifactBytes,
	})
	if errors.Is(err, ErrAlreadyExists) {
		writeError(writer, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "cache reservation failed")
		return
	}
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
	if end >= handler.config.MaxArtifactBytes {
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
		Size *int64 `json:"size"`
	}
	if err := json.NewDecoder(io.LimitReader(request.Body, 1<<20)).Decode(&body); err != nil || body.Size == nil {
		writeError(writer, http.StatusBadRequest, "invalid commit request")
		return
	}
	if *body.Size < 0 {
		writeError(writer, http.StatusBadRequest, "cache size cannot be negative")
		return
	}
	if *body.Size > handler.config.MaxArtifactBytes {
		writeError(writer, http.StatusBadRequest, "cache size exceeds the configured maximum artifact size")
		return
	}
	_, err = handler.storage.Commit(request.Context(), CommitRequest{ReservationID: id, Scope: &scope, Size: *body.Size})
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
	scope, err := handler.scope(request)
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
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Content-Length", strconv.FormatInt(archive.Entry.Size, 10))
	writer.WriteHeader(http.StatusOK)
	_, _ = io.Copy(writer, archive.Body)
}

func (handler *Handler) scope(request *http.Request) (Scope, error) {
	scope := Scope{
		Repository:    handler.config.Repository,
		Ref:           handler.config.Ref,
		DefaultRef:    handler.config.DefaultRef,
		Compatibility: handler.config.Compatibility,
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
	if selected == "" && !handler.config.RequireCompatibilitySelector {
		return scope, nil
	}
	if err := compatibility.Validate(selected); err != nil {
		return Scope{}, err
	}
	scope.Compatibility = selected
	return scope, nil
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
	expiresValues, signatureValues := request.URL.Query()[archiveExpiryQuery], request.URL.Query()[archiveSignatureQuery]
	if len(expiresValues) != 1 || len(signatureValues) != 1 {
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
	return handler.archiveURLSigner.VerifyArchiveURL(request.URL.EscapedPath(), expiresAt, signatureValues[0]) == nil
}

func (handler *Handler) archiveLocation(request *http.Request, id int64, scope Scope) (string, error) {
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
	signature, err := handler.archiveURLSigner.SignArchiveURL(location.EscapedPath(), expiresAt)
	if err != nil {
		return "", err
	}
	query := location.Query()
	query.Set(archiveExpiryQuery, strconv.FormatInt(expiresAt.UnixMilli(), 10))
	query.Set(archiveSignatureQuery, signature)
	location.RawQuery = query.Encode()
	return location.String(), nil
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
