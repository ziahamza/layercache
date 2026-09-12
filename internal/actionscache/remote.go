package actionscache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/layercache/layercache/internal/compatibility"
)

const (
	defaultResponseHeaderTimeout = 2 * time.Second
	defaultTransferIdleTimeout   = 30 * time.Second
	maxRemoteResponseSize        = 1 << 20
	maxRemoteEntryMetadata       = 4_096
	maxRemoteReservationMetadata = 4_096
	remoteLookupMetadataTTL      = 10 * time.Minute
	remoteEntryMetadataTTL       = 24 * time.Hour
	remoteReservationMetadataTTL = 24 * time.Hour
)

var ErrTransferIdleTimeout = errors.New("remote cache transfer made no progress before the idle timeout")

type RemoteStorageConfig struct {
	Endpoint              string
	Token                 string
	ResponseHeaderTimeout time.Duration
	TransferIdleTimeout   time.Duration
}

// RemoteStorage implements StorageIndex against the GitHub Actions v1 cache
// protocol. Repository, compatibility, and ref authority remain server-owned;
// the client sends the complete ordered key request so the remote index applies
// its native exact, prefix, and ref matching rules.
type RemoteStorage struct {
	endpoint            *url.URL
	client              *http.Client
	transferIdleTimeout time.Duration
	tokenMu             sync.RWMutex
	token               string

	mu           sync.Mutex
	locations    map[remoteEntryIdentity]remoteLocationMetadata
	entries      map[remoteEntryIdentity]remoteEntryMetadata
	scopes       map[remoteEntryIdentity]time.Time
	reservations map[int64]remoteReservationMetadata
}

type remoteEntryIdentity struct {
	id    int64
	scope Scope
}

type remoteLocationMetadata struct {
	location *url.URL
	expires  time.Time
}

type remoteEntryMetadata struct {
	entry   Entry
	expires time.Time
}

type remoteReservationMetadata struct {
	request ReserveRequest
	expires time.Time
}

func NewRemoteStorage(config RemoteStorageConfig) (*RemoteStorage, error) {
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return nil, errors.New("remote Actions cache endpoint must be an absolute HTTP or HTTPS URL")
	}
	if endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && isLoopbackHost(endpoint.Hostname())) {
		return nil, errors.New("remote Actions cache endpoint must use HTTPS except on loopback")
	}
	if endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("remote Actions cache endpoint cannot contain credentials, a query, or a fragment")
	}
	if strings.TrimSpace(config.Token) == "" {
		return nil, errors.New("remote Actions cache bearer token is required")
	}

	responseHeaderTimeout := config.ResponseHeaderTimeout
	if responseHeaderTimeout == 0 {
		responseHeaderTimeout = defaultResponseHeaderTimeout
	}
	if responseHeaderTimeout < 0 {
		return nil, errors.New("remote Actions cache response-header timeout must be positive")
	}
	transferIdleTimeout := config.TransferIdleTimeout
	if transferIdleTimeout == 0 {
		transferIdleTimeout = defaultTransferIdleTimeout
	}
	if transferIdleTimeout < 0 {
		return nil, errors.New("remote Actions cache transfer idle timeout must be positive")
	}

	dialer := &net.Dialer{Timeout: responseHeaderTimeout, KeepAlive: 30 * time.Second}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = dialer.DialContext
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	transport.TLSHandshakeTimeout = responseHeaderTimeout

	endpoint.Path = strings.TrimRight(endpoint.Path, "/")
	return &RemoteStorage{
		endpoint: endpoint,
		token:    strings.TrimSpace(config.Token),
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("remote Actions cache redirect limit exceeded")
				}
				if request.URL.Scheme != "https" && !(request.URL.Scheme == "http" && isLoopbackHost(request.URL.Hostname())) {
					return errors.New("remote Actions cache redirect must use HTTPS except on loopback")
				}
				if request.URL.User != nil || request.URL.Fragment != "" {
					return errors.New("remote Actions cache redirect target is unsafe")
				}
				if request.URL.Scheme != endpoint.Scheme || request.URL.Host != endpoint.Host {
					if len(via) > 0 && via[0].Header.Get("Authorization") != "" {
						return errors.New("authenticated remote Actions cache redirect changed origin")
					}
					request.Header.Del("Authorization")
					request.Header.Del(compatibility.Header)
				}
				return nil
			},
		},
		transferIdleTimeout: transferIdleTimeout,
		locations:           make(map[remoteEntryIdentity]remoteLocationMetadata),
		entries:             make(map[remoteEntryIdentity]remoteEntryMetadata),
		scopes:              make(map[remoteEntryIdentity]time.Time),
		reservations:        make(map[int64]remoteReservationMetadata),
	}, nil
}

func (storage *RemoteStorage) Lookup(ctx context.Context, request LookupRequest) (LookupResult, error) {
	if err := compatibility.Validate(request.Scope.Compatibility); err != nil {
		return LookupResult{}, fmt.Errorf("lookup remote Actions cache: %w", err)
	}
	query := make(url.Values, 2)
	query.Set("keys", strings.Join(request.Keys, ","))
	query.Set("version", request.Version)
	target := storage.route("/_apis/artifactcache/cache")
	target.RawQuery = query.Encode()

	response, err := storage.do(ctx, http.MethodGet, target, nil, "", request.Scope.Compatibility)
	if err != nil {
		return LookupResult{}, fmt.Errorf("lookup remote Actions cache: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent || response.StatusCode == http.StatusNotFound {
		return LookupResult{}, ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return LookupResult{}, remoteStatusError(response, "lookup remote Actions cache")
	}
	var body struct {
		CacheKey                              string    `json:"cacheKey"`
		Scope                                 string    `json:"scope"`
		CacheVersion                          string    `json:"cacheVersion"`
		CreationTime                          time.Time `json:"creationTime"`
		ArchiveLocation                       string    `json:"archiveLocation"`
		LayerCacheProducerDurationNanoseconds *int64    `json:"layerCacheProducerDurationNanoseconds"`
	}
	if err := decodeRemoteJSON(response.Body, &body); err != nil {
		return LookupResult{}, fmt.Errorf("decode remote Actions cache lookup: %w", err)
	}
	location, id, err := storage.archiveLocation(body.ArchiveLocation, request.Scope.Compatibility)
	if err != nil {
		return LookupResult{}, err
	}
	if body.LayerCacheProducerDurationNanoseconds != nil && *body.LayerCacheProducerDurationNanoseconds < 0 {
		return LookupResult{}, errors.New("remote Actions cache returned a negative producer duration")
	}
	var producerDuration *time.Duration
	if body.LayerCacheProducerDurationNanoseconds != nil {
		value := time.Duration(*body.LayerCacheProducerDurationNanoseconds)
		producerDuration = &value
	}
	entry := Entry{
		ID: id, Key: body.CacheKey, Version: body.CacheVersion, Ref: body.Scope,
		CreatedAt: body.CreationTime, ProducerDuration: producerDuration,
	}
	identity := remoteEntryIdentity{id: id, scope: request.Scope}
	storage.mu.Lock()
	storage.rememberEntryLocked(identity, entry, location, remoteLookupMetadataTTL, time.Now().UTC())
	storage.mu.Unlock()
	match, requestedKey := remoteMatchMetadata(response, request, entry.Key)
	refScope := RefScope(response.Header.Get("X-LayerCache-Ref-Scope"))
	if refScope == "" {
		switch entry.Ref {
		case request.Scope.Ref:
			refScope = RefScopeCurrent
		case request.Scope.DefaultRef:
			refScope = RefScopeDefault
		}
	}

	return LookupResult{
		Entry:        entry,
		Match:        match,
		RequestedKey: requestedKey,
		RefScope:     refScope,
		Source:       SourceTeamCache,
	}, nil
}

func remoteMatchMetadata(response *http.Response, request LookupRequest, matchedKey string) (MatchKind, string) {
	match := MatchKind(response.Header.Get("X-LayerCache-Match"))
	requestedKey := response.Header.Get("X-LayerCache-Requested-Key")
	if match != "" && requestedKey != "" {
		return match, requestedKey
	}
	for _, key := range request.Keys {
		if matchedKey == key {
			return MatchExact, key
		}
		if strings.HasPrefix(matchedKey, key) {
			return MatchPrefix, key
		}
	}
	return match, requestedKey
}

func (storage *RemoteStorage) Reserve(ctx context.Context, request ReserveRequest) (Reservation, error) {
	if err := compatibility.Validate(request.Scope.Compatibility); err != nil {
		return Reservation{}, fmt.Errorf("reserve remote Actions cache: %w", err)
	}
	body, err := json.Marshal(struct {
		Key       string `json:"key"`
		Version   string `json:"version"`
		CacheSize *int64 `json:"cacheSize,omitempty"`
	}{Key: request.Key, Version: request.Version, CacheSize: request.CacheSize})
	if err != nil {
		return Reservation{}, err
	}
	response, err := storage.do(ctx, http.MethodPost, storage.route("/_apis/artifactcache/caches"), strings.NewReader(string(body)), "application/json", request.Scope.Compatibility)
	if err != nil {
		return Reservation{}, fmt.Errorf("reserve remote Actions cache: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusConflict {
		return Reservation{}, ErrAlreadyExists
	}
	if response.StatusCode != http.StatusCreated {
		return Reservation{}, remoteStatusError(response, "reserve remote Actions cache")
	}
	var result struct {
		CacheID int64 `json:"cacheId"`
	}
	if err := decodeRemoteJSON(response.Body, &result); err != nil {
		return Reservation{}, fmt.Errorf("decode remote Actions cache reservation: %w", err)
	}
	if result.CacheID <= 0 {
		return Reservation{}, errors.New("remote Actions cache returned an invalid reservation ID")
	}
	storage.mu.Lock()
	storage.pruneMetadataLocked(time.Now().UTC())
	if len(storage.reservations) >= maxRemoteReservationMetadata {
		storage.mu.Unlock()
		abortErr := storage.abortRemoteReservation(ctx, result.CacheID, request.Scope.Compatibility)
		return Reservation{}, errors.Join(
			errors.New("remote Actions cache has too many unfinished reservations"), abortErr,
		)
	}
	storage.reservations[result.CacheID] = remoteReservationMetadata{
		request: cloneReserveRequest(request), expires: time.Now().UTC().Add(remoteReservationMetadataTTL),
	}
	storage.mu.Unlock()
	return Reservation{ID: result.CacheID}, nil
}

func (storage *RemoteStorage) Upload(ctx context.Context, request UploadRequest) error {
	if request.Body == nil {
		return ErrInvalidUpload
	}
	target := storage.route(fmt.Sprintf("/_apis/artifactcache/caches/%d", request.ReservationID))
	storage.mu.Lock()
	storage.pruneMetadataLocked(time.Now().UTC())
	metadata, found := storage.reservations[request.ReservationID]
	storage.mu.Unlock()
	if !found {
		return errors.New("upload remote Actions cache for an unknown reservation")
	}
	reservation := metadata.request
	requestContext, cancel := context.WithCancelCause(ctx)
	watch := newIdleWatch(cancel, storage.transferIdleTimeout)
	body := &progressReader{reader: request.Body, progress: watch.Progress}
	httpRequest, err := http.NewRequestWithContext(requestContext, http.MethodPatch, target.String(), body)
	if err != nil {
		watch.Stop()
		cancel(nil)
		return err
	}
	httpRequest.ContentLength = request.End - request.Start + 1
	httpRequest.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/*", request.Start, request.End))
	storage.authorize(httpRequest, reservation.Scope.Compatibility)
	response, err := storage.client.Do(httpRequest)
	watch.Stop()
	if err != nil {
		cause := context.Cause(requestContext)
		cancel(nil)
		if errors.Is(cause, ErrTransferIdleTimeout) {
			return ErrTransferIdleTimeout
		}
		return fmt.Errorf("upload remote Actions cache: %w", err)
	}
	cancel(nil)
	response.Body = newIdleTimeoutReadCloser(response.Body, storage.transferIdleTimeout)
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		storage.forgetReservation(request.ReservationID)
		return ErrNotFound
	}
	if response.StatusCode == http.StatusBadRequest {
		return ErrInvalidUpload
	}
	if response.StatusCode != http.StatusNoContent {
		return remoteStatusError(response, "upload remote Actions cache")
	}
	return nil
}

func (storage *RemoteStorage) Commit(ctx context.Context, request CommitRequest) (Entry, error) {
	if request.ProducerDuration != nil && *request.ProducerDuration < 0 {
		return Entry{}, ErrInvalidUpload
	}
	storage.mu.Lock()
	storage.pruneMetadataLocked(time.Now().UTC())
	metadata, found := storage.reservations[request.ReservationID]
	storage.mu.Unlock()
	if !found {
		return Entry{}, errors.New("commit remote Actions cache for an unknown reservation")
	}
	reservation := metadata.request
	body, err := json.Marshal(struct {
		Size                                  int64  `json:"size"`
		LayerCacheProducerDurationNanoseconds *int64 `json:"layerCacheProducerDurationNanoseconds,omitempty"`
	}{Size: request.Size, LayerCacheProducerDurationNanoseconds: durationNanoseconds(request.ProducerDuration)})
	if err != nil {
		return Entry{}, err
	}
	target := storage.route(fmt.Sprintf("/_apis/artifactcache/caches/%d", request.ReservationID))
	response, err := storage.do(ctx, http.MethodPost, target, strings.NewReader(string(body)), "application/json", reservation.Scope.Compatibility)
	if err != nil {
		return Entry{}, fmt.Errorf("commit remote Actions cache: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		storage.forgetReservation(request.ReservationID)
		return Entry{}, ErrNotFound
	}
	if response.StatusCode == http.StatusBadRequest {
		message := remoteErrorMessage(response.Body)
		if strings.Contains(message, ErrIncompleteUpload.Error()) {
			return Entry{}, ErrIncompleteUpload
		}
		return Entry{}, ErrInvalidUpload
	}
	if response.StatusCode != http.StatusNoContent {
		return Entry{}, remoteStatusError(response, "commit remote Actions cache")
	}

	storage.mu.Lock()
	delete(storage.reservations, request.ReservationID)
	storage.mu.Unlock()
	entry := Entry{
		ID: request.ReservationID, Key: reservation.Key, Version: reservation.Version,
		Ref: reservation.Scope.Ref, Size: request.Size, CreatedAt: time.Now().UTC(),
		ProducerDuration: cloneDuration(request.ProducerDuration),
	}
	identity := remoteEntryIdentity{id: entry.ID, scope: reservation.Scope}
	storage.mu.Lock()
	storage.rememberEntryLocked(identity, entry, nil, remoteEntryMetadataTTL, time.Now().UTC())
	storage.mu.Unlock()
	return entry, nil
}

func (storage *RemoteStorage) Abort(ctx context.Context, reservationID int64) error {
	storage.mu.Lock()
	storage.pruneMetadataLocked(time.Now().UTC())
	metadata, found := storage.reservations[reservationID]
	storage.mu.Unlock()
	if !found {
		return ErrNotFound
	}
	reservation := metadata.request
	target := storage.route(fmt.Sprintf("/_apis/artifactcache/caches/%d", reservationID))
	response, err := storage.do(ctx, http.MethodDelete, target, nil, "", reservation.Scope.Compatibility)
	if err != nil {
		return fmt.Errorf("abort remote Actions cache reservation: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusNotFound {
		return remoteStatusError(response, "abort remote Actions cache reservation")
	}
	storage.forgetReservation(reservationID)
	if response.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	return nil
}

func (storage *RemoteStorage) Open(ctx context.Context, request OpenRequest) (Archive, error) {
	if err := compatibility.Validate(request.Scope.Compatibility); err != nil {
		return Archive{}, fmt.Errorf("open remote Actions cache: %w", err)
	}
	identity := remoteEntryIdentity{id: request.ID, scope: request.Scope}
	storage.mu.Lock()
	storage.pruneMetadataLocked(time.Now().UTC())
	locationMetadata, knownLocation := storage.locations[identity]
	entryMetadata, knownEntry := storage.entries[identity]
	_, knownScope := storage.scopes[identity]
	knownID := knownScope
	if !knownID {
		for known := range storage.scopes {
			if known.id == request.ID {
				knownID = true
				break
			}
		}
	}
	storage.mu.Unlock()
	if knownID && !knownScope {
		return Archive{}, ErrNotFound
	}
	var location *url.URL
	if knownLocation {
		location = locationMetadata.location
	}
	entry := entryMetadata.entry
	if location == nil && knownEntry && entry.Key != "" && entry.Version != "" {
		lookup, err := storage.Lookup(ctx, LookupRequest{
			Scope: request.Scope, Keys: []string{entry.Key}, Version: entry.Version,
		})
		if err != nil {
			return Archive{}, fmt.Errorf("resolve archive URL for committed remote Actions cache: %w", err)
		}
		if lookup.Match != MatchExact || lookup.Entry.Key != entry.Key || lookup.Entry.ID != request.ID {
			return Archive{}, ErrNotFound
		}
		storage.mu.Lock()
		locationMetadata, knownLocation = storage.locations[identity]
		entryMetadata, knownEntry = storage.entries[identity]
		storage.mu.Unlock()
		if knownLocation {
			location = locationMetadata.location
		}
		if knownEntry {
			entry = entryMetadata.entry
		}
	}
	directRequest := location == nil
	if location == nil {
		location = storage.route(fmt.Sprintf("/_apis/artifactcache/caches/%d/archive", request.ID))
	}

	requestContext, cancel := context.WithCancelCause(ctx)
	httpRequest, err := http.NewRequestWithContext(requestContext, http.MethodGet, location.String(), nil)
	if err != nil {
		cancel(nil)
		return Archive{}, err
	}
	if directRequest {
		storage.authorize(httpRequest, request.Scope.Compatibility)
	}
	response, err := storage.client.Do(httpRequest)
	if err != nil {
		cancel(nil)
		return Archive{}, fmt.Errorf("open remote Actions cache: %w", err)
	}
	if response.StatusCode == http.StatusNotFound {
		response.Body.Close()
		cancel(nil)
		return Archive{}, ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		statusErr := remoteStatusError(response, "open remote Actions cache")
		response.Body.Close()
		cancel(nil)
		return Archive{}, statusErr
	}
	if response.ContentLength >= 0 {
		entry.Size = response.ContentLength
	}
	storage.mu.Lock()
	delete(storage.locations, identity)
	delete(storage.entries, identity)
	storage.mu.Unlock()
	watch := newIdleWatch(cancel, storage.transferIdleTimeout)
	return Archive{
		Entry: entry,
		Body: &progressReadCloser{
			body: response.Body, context: requestContext, cancel: cancel, watch: watch,
		},
	}, nil
}

func (storage *RemoteStorage) rememberEntryLocked(
	identity remoteEntryIdentity,
	entry Entry,
	location *url.URL,
	ttl time.Duration,
	now time.Time,
) {
	storage.pruneMetadataLocked(now)
	if _, known := storage.scopes[identity]; !known && len(storage.scopes) >= maxRemoteEntryMetadata {
		storage.evictOldestScopeLocked()
	}
	storage.scopes[identity] = now.Add(remoteEntryMetadataTTL)
	storage.entries[identity] = remoteEntryMetadata{entry: entry, expires: now.Add(ttl)}
	if location == nil {
		delete(storage.locations, identity)
	} else {
		storage.locations[identity] = remoteLocationMetadata{location: location, expires: now.Add(ttl)}
	}
}

func (storage *RemoteStorage) pruneMetadataLocked(now time.Time) {
	for identity, expires := range storage.scopes {
		if !now.Before(expires) {
			delete(storage.scopes, identity)
			delete(storage.entries, identity)
			delete(storage.locations, identity)
		}
	}
	for identity, metadata := range storage.entries {
		if !now.Before(metadata.expires) {
			delete(storage.entries, identity)
			delete(storage.locations, identity)
		}
	}
	for identity, metadata := range storage.locations {
		if !now.Before(metadata.expires) {
			delete(storage.locations, identity)
		}
	}
	for id, metadata := range storage.reservations {
		if !now.Before(metadata.expires) {
			delete(storage.reservations, id)
		}
	}
}

func (storage *RemoteStorage) evictOldestScopeLocked() {
	var oldest remoteEntryIdentity
	var oldestExpiry time.Time
	for identity, expires := range storage.scopes {
		if oldestExpiry.IsZero() || expires.Before(oldestExpiry) {
			oldest = identity
			oldestExpiry = expires
		}
	}
	if oldestExpiry.IsZero() {
		return
	}
	delete(storage.scopes, oldest)
	delete(storage.entries, oldest)
	delete(storage.locations, oldest)
}

func (storage *RemoteStorage) forgetReservation(id int64) {
	storage.mu.Lock()
	delete(storage.reservations, id)
	storage.mu.Unlock()
}

func (storage *RemoteStorage) abortRemoteReservation(ctx context.Context, id int64, compatibilityID string) error {
	response, err := storage.do(
		ctx, http.MethodDelete, storage.route(fmt.Sprintf("/_apis/artifactcache/caches/%d", id)), nil, "", compatibilityID,
	)
	if err != nil {
		return fmt.Errorf("abort untracked remote Actions cache reservation: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusNotFound {
		return remoteStatusError(response, "abort untracked remote Actions cache reservation")
	}
	return nil
}

func (storage *RemoteStorage) do(ctx context.Context, method string, target *url.URL, body io.Reader, contentType, compatibilityID string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	storage.authorize(request, compatibilityID)
	response, err := storage.client.Do(request)
	if err != nil {
		return nil, err
	}
	response.Body = newIdleTimeoutReadCloser(response.Body, storage.transferIdleTimeout)
	return response, nil
}

func (storage *RemoteStorage) authorize(request *http.Request, compatibilityID string) {
	if request.URL.Scheme == storage.endpoint.Scheme && request.URL.Host == storage.endpoint.Host {
		storage.tokenMu.RLock()
		token := storage.token
		storage.tokenMu.RUnlock()
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set(compatibility.Header, compatibilityID)
	}
}

// SetToken atomically rotates the bearer credential used by subsequent Team
// Cache requests. In-flight requests retain the credential with which they
// started.
func (storage *RemoteStorage) SetToken(token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("remote Actions cache bearer token is required")
	}
	storage.tokenMu.Lock()
	storage.token = token
	storage.tokenMu.Unlock()
	return nil
}

func (storage *RemoteStorage) route(path string) *url.URL {
	copy := *storage.endpoint
	copy.Path = strings.TrimRight(storage.endpoint.Path, "/") + path
	copy.RawPath = ""
	copy.RawQuery = ""
	copy.Fragment = ""
	return &copy
}

func (storage *RemoteStorage) archiveLocation(value, compatibilityID string) (*url.URL, int64, error) {
	location, err := url.Parse(value)
	if err != nil {
		return nil, 0, fmt.Errorf("remote Actions cache returned an invalid archive location: %w", err)
	}
	if !location.IsAbs() {
		location = storage.endpoint.ResolveReference(location)
	}
	if location.Scheme != "https" && !(location.Scheme == "http" && isLoopbackHost(location.Hostname())) {
		return nil, 0, errors.New("remote Actions cache archive location must use HTTPS except on loopback")
	}
	if location.User != nil || location.Fragment != "" {
		return nil, 0, errors.New("remote Actions cache returned an unsafe archive location")
	}
	parts := strings.Split(strings.Trim(location.Path, "/"), "/")
	if len(parts) < 2 || parts[len(parts)-1] != "archive" {
		return nil, 0, errors.New("remote Actions cache returned an invalid archive location")
	}
	if selected, found := compatibilityFromArchivePath(parts); found && selected != compatibilityID {
		return nil, 0, errors.New("remote Actions cache returned an archive for a different compatibility identity")
	}
	id, err := strconv.ParseInt(parts[len(parts)-2], 10, 64)
	if err != nil || id <= 0 {
		return nil, 0, errors.New("remote Actions cache returned an invalid archive ID")
	}
	return location, id, nil
}

func compatibilityFromArchivePath(parts []string) (string, bool) {
	for index := 0; index+2 < len(parts); index++ {
		if parts[index] == "_layercache" && parts[index+1] == "compatibility" {
			return parts[index+2], true
		}
	}
	return "", false
}

func remoteStatusError(response *http.Response, operation string) error {
	message := remoteErrorMessage(response.Body)
	if message == "" {
		return fmt.Errorf("%s: HTTP %d", operation, response.StatusCode)
	}
	return fmt.Errorf("%s: HTTP %d: %s", operation, response.StatusCode, message)
}

func remoteErrorMessage(reader io.Reader) string {
	var body struct {
		Message string `json:"message"`
	}
	if json.NewDecoder(io.LimitReader(reader, maxRemoteResponseSize)).Decode(&body) != nil {
		return ""
	}
	return body.Message
}

func decodeRemoteJSON(reader io.Reader, value any) error {
	return json.NewDecoder(io.LimitReader(reader, maxRemoteResponseSize)).Decode(value)
}

func cloneReserveRequest(request ReserveRequest) ReserveRequest {
	if request.CacheSize != nil {
		size := *request.CacheSize
		request.CacheSize = &size
	}
	return request
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type idleWatch struct {
	mu      sync.Mutex
	timer   *time.Timer
	timeout time.Duration
	stopped bool
}

type idleTimeoutReadCloser struct {
	body     io.ReadCloser
	timeout  time.Duration
	mu       sync.Mutex
	timer    *time.Timer
	timedOut bool
	closed   bool
}

func newIdleTimeoutReadCloser(body io.ReadCloser, timeout time.Duration) io.ReadCloser {
	reader := &idleTimeoutReadCloser{body: body, timeout: timeout}
	reader.timer = time.AfterFunc(timeout, reader.expire)
	return reader
}

func (reader *idleTimeoutReadCloser) expire() {
	reader.mu.Lock()
	if reader.closed {
		reader.mu.Unlock()
		return
	}
	reader.timedOut = true
	reader.mu.Unlock()
	_ = reader.body.Close()
}

func (reader *idleTimeoutReadCloser) Read(buffer []byte) (int, error) {
	count, err := reader.body.Read(buffer)
	reader.mu.Lock()
	if count > 0 && !reader.closed && !reader.timedOut {
		reader.timer.Reset(reader.timeout)
	}
	timedOut := reader.timedOut
	reader.mu.Unlock()
	if timedOut && err != nil {
		return count, ErrTransferIdleTimeout
	}
	return count, err
}

func (reader *idleTimeoutReadCloser) Close() error {
	reader.mu.Lock()
	if reader.closed {
		reader.mu.Unlock()
		return nil
	}
	reader.closed = true
	reader.timer.Stop()
	reader.mu.Unlock()
	return reader.body.Close()
}

func newIdleWatch(cancel context.CancelCauseFunc, timeout time.Duration) *idleWatch {
	watch := &idleWatch{timeout: timeout}
	watch.timer = time.AfterFunc(timeout, func() { cancel(ErrTransferIdleTimeout) })
	return watch
}

func (watch *idleWatch) Progress() {
	watch.mu.Lock()
	defer watch.mu.Unlock()
	if !watch.stopped {
		watch.timer.Reset(watch.timeout)
	}
}

func (watch *idleWatch) Stop() {
	watch.mu.Lock()
	defer watch.mu.Unlock()
	if !watch.stopped {
		watch.stopped = true
		watch.timer.Stop()
	}
}

type progressReader struct {
	reader   io.Reader
	progress func()
}

func (reader *progressReader) Read(buffer []byte) (int, error) {
	count, err := reader.reader.Read(buffer)
	if count > 0 {
		reader.progress()
	}
	return count, err
}

type progressReadCloser struct {
	body    io.ReadCloser
	context context.Context
	cancel  context.CancelCauseFunc
	watch   *idleWatch
	once    sync.Once
}

func (reader *progressReadCloser) Read(buffer []byte) (int, error) {
	count, err := reader.body.Read(buffer)
	if count > 0 {
		reader.watch.Progress()
	}
	if err != nil {
		reader.finish()
		if errors.Is(context.Cause(reader.context), ErrTransferIdleTimeout) {
			return count, ErrTransferIdleTimeout
		}
	}
	return count, err
}

func (reader *progressReadCloser) Close() error {
	reader.finish()
	return reader.body.Close()
}

func (reader *progressReadCloser) finish() {
	reader.once.Do(func() {
		reader.watch.Stop()
		reader.cancel(nil)
	})
}

var _ StorageIndex = (*RemoteStorage)(nil)
