package actionscache

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/publictrust"
)

const actionsPublicIdentityVersion = "layercache/actions-cache/public-identity/v1"

// PublicResolveRequest is the exact, server-owned coordinate a Public Cache
// resolver must resolve. Identity is the publictrust identity derived from
// Expected; the remaining fields let an HTTP adapter send an auditable request
// without reconstructing cache authority from client input.
type PublicResolveRequest struct {
	Identity         string               `json:"identity"`
	Expected         publictrust.Expected `json:"expected"`
	Repository       string               `json:"repository"`
	SourceRepository string               `json:"sourceRepository"`
	Ref              string               `json:"ref"`
	Key              string               `json:"key"`
	Version          string               `json:"version"`
	Compatibility    string               `json:"compatibility"`
}

// PublicEntryMetadata is the signed trust record persisted beside an archive
// warmed into Local Cache. ExpiresAt, Digest, and Size are derived from the
// verified envelope and checked against it again before offline use.
type PublicEntryMetadata struct {
	Request   PublicResolveRequest `json:"request"`
	Envelope  publictrust.Envelope `json:"envelope"`
	Digest    string               `json:"digest"`
	Size      int64                `json:"size"`
	ExpiresAt time.Time            `json:"expiresAt"`
}

// PublicResolution contains untrusted input. PublicStorage verifies the DSSE
// envelope and every archive byte before making the result observable.
type PublicResolution struct {
	Envelope publictrust.Envelope
	Archive  io.ReadCloser
}

// PublicResolver obtains one signed publication and its archive. A resolver
// should return publictrust.ErrNotFound, ErrRevoked, or ErrAmbiguous for an
// unusable exact identity; PublicStorage will then try the next ordered exact
// coordinate.
type PublicResolver interface {
	Resolve(context.Context, PublicResolveRequest) (PublicResolution, error)
	Revalidate(context.Context, PublicResolveRequest) (publictrust.Envelope, error)
}

type PublicStorageConfig struct {
	VerificationKey  ed25519.PublicKey
	StagingDirectory string
	// ArtifactStore accounts verified downloads against the same transient
	// staging and free-space limits as Local Cache uploads.
	ArtifactStore    *artifact.Store
	MaxArtifactBytes int64
	// SourceRepository is the canonical HTTPS clone URL asserted by Public
	// Build provenance. When empty, owner/repo Actions namespaces are safely
	// derived as https://github.com/owner/repo. Set it explicitly for GHES.
	SourceRepository string
	Now              func() time.Time
}

// PublicStorage is a read-only Actions v1 StorageIndex. It intentionally
// supports exact identities only: current ref then default ref, and within a
// ref, keys in caller order. It never interprets or extracts archive bytes.
type PublicStorage struct {
	resolver         PublicResolver
	verificationKey  ed25519.PublicKey
	now              func() time.Time
	stagingRoot      string
	sourceRepository string
	artifacts        *artifact.Store
	maxArtifactBytes int64

	mu         sync.Mutex
	nextID     int64
	byIdentity map[string]*verifiedPublicArchive
	byID       map[int64]*verifiedPublicArchive
	inflight   map[string]*publicResolveCall
	closed     bool
}

type verifiedPublicArchive struct {
	entry         Entry
	repository    string
	compatibility string
	identity      string
	digest        string
	expiresAt     time.Time
	path          string
	lease         *artifact.StagingLease
}

type publicResolveCall struct {
	done    chan struct{}
	archive *verifiedPublicArchive
	err     error
}

func NewPublicStorage(config PublicStorageConfig, resolver PublicResolver) (*PublicStorage, error) {
	if resolver == nil {
		return nil, errors.New("Public Cache resolver is required")
	}
	if len(config.VerificationKey) != ed25519.PublicKeySize {
		return nil, errors.New("Public Cache Ed25519 verification key is required")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.MaxArtifactBytes < 0 {
		return nil, errors.New("Public Cache maximum artifact bytes cannot be negative")
	}
	maxArtifactBytes := config.MaxArtifactBytes
	if config.ArtifactStore != nil && (maxArtifactBytes == 0 || maxArtifactBytes > config.ArtifactStore.MaxBytes()) {
		maxArtifactBytes = config.ArtifactStore.MaxBytes()
	}
	if maxArtifactBytes == 0 {
		maxArtifactBytes = defaultMaxArtifactBytes
	}
	var sourceRepository string
	if config.SourceRepository != "" {
		var err error
		sourceRepository, err = canonicalSourceRepository(config.SourceRepository)
		if err != nil {
			return nil, fmt.Errorf("invalid Public Cache source repository: %w", err)
		}
	}
	base := config.StagingDirectory
	if base == "" {
		base = os.TempDir()
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, fmt.Errorf("create Public Cache archive staging base: %w", err)
	}
	root, err := os.MkdirTemp(base, "layercache-actions-public-*")
	if err != nil {
		return nil, fmt.Errorf("create Public Cache archive staging directory: %w", err)
	}
	return &PublicStorage{
		resolver: resolver, verificationKey: append(ed25519.PublicKey(nil), config.VerificationKey...),
		now: config.Now, stagingRoot: root, sourceRepository: sourceRepository,
		artifacts: config.ArtifactStore, maxArtifactBytes: maxArtifactBytes,
		byIdentity: make(map[string]*verifiedPublicArchive),
		byID:       make(map[int64]*verifiedPublicArchive), inflight: make(map[string]*publicResolveCall),
	}, nil
}

func (storage *PublicStorage) Close() error {
	storage.mu.Lock()
	if storage.closed {
		storage.mu.Unlock()
		return nil
	}
	storage.closed = true
	root := storage.stagingRoot
	archives := make([]*verifiedPublicArchive, 0, len(storage.byIdentity))
	for _, archive := range storage.byIdentity {
		archives = append(archives, archive)
	}
	storage.byIdentity = make(map[string]*verifiedPublicArchive)
	storage.byID = make(map[int64]*verifiedPublicArchive)
	storage.mu.Unlock()
	err := os.RemoveAll(root)
	if err == nil {
		for _, archive := range archives {
			archive.lease.Release()
		}
	}
	return err
}

func (storage *PublicStorage) Lookup(ctx context.Context, request LookupRequest) (LookupResult, error) {
	sourceRepository, err := storage.sourceRepositoryFor(request.Scope.Repository)
	if err != nil {
		// A non-GitHub.com namespace requires an explicit server-owned source URL.
		// Fail closed as a miss rather than guessing signed provenance.
		return LookupResult{}, ErrNotFound
	}
	refs := []struct {
		value string
		scope RefScope
	}{{value: request.Scope.Ref, scope: RefScopeCurrent}}
	if request.Scope.DefaultRef != request.Scope.Ref {
		refs = append(refs, struct {
			value string
			scope RefScope
		}{value: request.Scope.DefaultRef, scope: RefScopeDefault})
	}
	for _, ref := range refs {
		for _, key := range request.Keys {
			resolveRequest := newPublicResolveRequest(request.Scope, sourceRepository, ref.value, key, request.Version)
			archive, err := storage.resolveOnce(ctx, resolveRequest)
			if err != nil {
				if isUnavailablePublicIdentity(err) {
					continue
				}
				return LookupResult{}, err
			}
			return LookupResult{
				Entry: cloneEntry(archive.entry), Match: MatchExact, RequestedKey: key,
				RefScope: ref.scope, Source: SourcePublicCache,
			}, nil
		}
	}
	return LookupResult{}, ErrNotFound
}

func (storage *PublicStorage) Reserve(context.Context, ReserveRequest) (Reservation, error) {
	return Reservation{}, ErrReadOnly
}

func (storage *PublicStorage) Upload(context.Context, UploadRequest) error {
	return ErrReadOnly
}

func (storage *PublicStorage) Commit(context.Context, CommitRequest) (Entry, error) {
	return Entry{}, ErrReadOnly
}

func (storage *PublicStorage) Open(_ context.Context, request OpenRequest) (Archive, error) {
	storage.mu.Lock()
	archive := storage.byID[request.ID]
	closed := storage.closed
	storage.mu.Unlock()
	if closed || archive == nil || archive.repository != request.Scope.Repository ||
		archive.compatibility != request.Scope.Compatibility ||
		(archive.entry.Ref != request.Scope.Ref && archive.entry.Ref != request.Scope.DefaultRef) {
		return Archive{}, ErrNotFound
	}
	body, err := os.Open(archive.path)
	if errors.Is(err, os.ErrNotExist) {
		return Archive{}, ErrNotFound
	}
	if err != nil {
		return Archive{}, err
	}
	return Archive{Entry: cloneEntry(archive.entry), Body: body}, nil
}

func (storage *PublicStorage) resolveOnce(ctx context.Context, request PublicResolveRequest) (*verifiedPublicArchive, error) {
	storage.mu.Lock()
	if storage.closed {
		storage.mu.Unlock()
		return nil, ErrNotFound
	}
	if cached := storage.byIdentity[request.Identity]; cached != nil && !storage.now().UTC().After(cached.expiresAt) {
		storage.mu.Unlock()
		refreshed, err := storage.RevalidateEntry(ctx, cached.entry.Public)
		if err != nil {
			return nil, err
		}
		storage.mu.Lock()
		if current := storage.byIdentity[request.Identity]; current != nil {
			current.entry.Public = clonePublicEntryMetadata(refreshed)
			current.expiresAt = refreshed.ExpiresAt
			cached = current
		}
		storage.mu.Unlock()
		return cached, nil
	}
	if running := storage.inflight[request.Identity]; running != nil {
		storage.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-running.done:
			return running.archive, running.err
		}
	}
	call := &publicResolveCall{done: make(chan struct{})}
	storage.inflight[request.Identity] = call
	storage.mu.Unlock()

	call.archive, call.err = storage.resolveAndVerify(ctx, request)
	storage.mu.Lock()
	delete(storage.inflight, request.Identity)
	var previous *verifiedPublicArchive
	if call.err == nil && !storage.closed {
		if previous = storage.byIdentity[request.Identity]; previous != nil {
			delete(storage.byID, previous.entry.ID)
		}
		storage.nextID++
		call.archive.entry.ID = storage.nextID
		storage.byIdentity[request.Identity] = call.archive
		storage.byID[call.archive.entry.ID] = call.archive
	} else if call.err == nil {
		call.err = ErrNotFound
	}
	close(call.done)
	storage.mu.Unlock()
	if call.err != nil && call.archive != nil {
		_ = discardVerifiedPublicArchive(call.archive, "")
		call.archive = nil
	} else if previous != nil {
		_ = discardVerifiedPublicArchive(previous, call.archive.path)
	}
	return call.archive, call.err
}

func (storage *PublicStorage) resolveAndVerify(ctx context.Context, request PublicResolveRequest) (*verifiedPublicArchive, error) {
	resolved, err := storage.resolver.Resolve(ctx, request)
	if err != nil {
		return nil, err
	}
	if resolved.Archive == nil {
		return nil, errors.New("Public Cache resolver returned no archive")
	}
	defer resolved.Archive.Close()
	publication, err := storage.verifyEnvelope(resolved.Envelope, request, "", -1)
	if err != nil {
		return nil, fmt.Errorf("verify Actions Public Cache publication: %w", err)
	}
	if publication.Size < 0 {
		return nil, fmt.Errorf("verify Actions Public Cache publication size: %w", publictrust.ErrIdentity)
	}
	if publication.Size > storage.maxArtifactBytes {
		return nil, artifact.ErrStagingQuota
	}
	var lease *artifact.StagingLease
	if storage.artifacts != nil {
		lease, err = storage.artifacts.ReserveVerificationStaging(publication.Size)
		if err != nil {
			return nil, err
		}
		defer func() {
			if lease != nil {
				lease.Release()
			}
		}()
	}

	temporary, err := os.CreateTemp(storage.stagingRoot, ".archive-*")
	if err != nil {
		return nil, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	hasher := sha256.New()
	var destination io.Writer = temporary
	if storage.artifacts != nil {
		destination = storage.artifacts.SpaceCheckedWriter(ctx, temporary)
	}
	size, copyErr := copyExactPublicArchive(io.MultiWriter(destination, hasher), resolved.Archive, publication.Size)
	if copyErr != nil {
		temporary.Close()
		return nil, fmt.Errorf("verify Actions Public Cache archive: %w", copyErr)
	}
	actualDigest := hex.EncodeToString(hasher.Sum(nil))
	if size != publication.Size || actualDigest != publication.Digest {
		temporary.Close()
		return nil, fmt.Errorf("verify Actions Public Cache archive: %w", publictrust.ErrIdentity)
	}
	if storage.artifacts != nil {
		err = storage.artifacts.SyncStaged(ctx, temporary)
	} else {
		err = temporary.Sync()
	}
	if err != nil {
		temporary.Close()
		return nil, err
	}
	if err := temporary.Close(); err != nil {
		return nil, err
	}
	path := filepath.Join(storage.stagingRoot, request.Identity+"-"+publication.Digest)
	if err := os.Rename(temporaryPath, path); err != nil {
		return nil, err
	}
	publicMetadata := &PublicEntryMetadata{
		Request: request, Envelope: resolved.Envelope, Digest: publication.Digest,
		Size: publication.Size, ExpiresAt: publication.ExpiresAt,
	}
	archive := &verifiedPublicArchive{
		entry: Entry{
			Key: request.Key, Version: request.Version, Ref: request.Ref, Size: size,
			CreatedAt: publication.IssuedAt, Origin: SourcePublicCache, Public: publicMetadata,
		},
		repository: request.Repository, compatibility: request.Compatibility,
		identity: request.Identity, digest: publication.Digest, expiresAt: publication.ExpiresAt, path: path,
		lease: lease,
	}
	lease = nil
	return archive, nil
}

func copyExactPublicArchive(destination io.Writer, source io.Reader, expectedSize int64) (int64, error) {
	written, err := io.CopyN(destination, source, expectedSize)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return written, publictrust.ErrIdentity
		}
		return written, err
	}
	var extra [1]byte
	read, err := io.ReadFull(source, extra[:])
	if read != 0 || err == nil || errors.Is(err, io.ErrUnexpectedEOF) {
		return written + int64(read), publictrust.ErrIdentity
	}
	if errors.Is(err, io.EOF) {
		return written, nil
	}
	return written, err
}

// RevalidateEntry authorizes one warmed Public Cache entry. An authoritative
// online response always wins. When the network is explicitly offline, it
// verifies the persisted envelope and permits use only through its signed
// expiry. Every field used for restoration is checked against signed data.
func (storage *PublicStorage) RevalidateEntry(ctx context.Context, metadata *PublicEntryMetadata) (*PublicEntryMetadata, error) {
	if metadata == nil {
		return nil, publictrust.ErrIdentity
	}
	envelope, err := storage.resolver.Revalidate(ctx, metadata.Request)
	if err == nil {
		publication, verifyErr := storage.verifyEnvelope(envelope, metadata.Request, metadata.Digest, metadata.Size)
		if verifyErr != nil {
			storage.forgetPublicIdentity(metadata.Request.Identity)
			return nil, verifyErr
		}
		return &PublicEntryMetadata{
			Request: metadata.Request, Envelope: envelope, Digest: publication.Digest,
			Size: publication.Size, ExpiresAt: publication.ExpiresAt,
		}, nil
	}
	if !errors.Is(err, ErrPublicOffline) {
		storage.forgetPublicIdentity(metadata.Request.Identity)
		return nil, err
	}

	publication, err := storage.verifyEnvelope(metadata.Envelope, metadata.Request, metadata.Digest, metadata.Size)
	if err != nil {
		storage.forgetPublicIdentity(metadata.Request.Identity)
		return nil, err
	}
	if !publication.ExpiresAt.Equal(metadata.ExpiresAt) {
		storage.forgetPublicIdentity(metadata.Request.Identity)
		return nil, publictrust.ErrIdentity
	}
	return clonePublicEntryMetadata(metadata), nil
}

func (storage *PublicStorage) verifyEnvelope(
	envelope publictrust.Envelope,
	request PublicResolveRequest,
	expectedDigest string,
	expectedSize int64,
) (publictrust.Publication, error) {
	expected := request.Expected
	expected.Digest = expectedDigest
	publication, err := publictrust.Verify(storage.verificationKey, envelope, expected, storage.now().UTC())
	if err != nil {
		return publictrust.Publication{}, err
	}
	if publication.Identity() != request.Identity || publication.Repository != request.SourceRepository ||
		expectedSize >= 0 && publication.Size != expectedSize {
		return publictrust.Publication{}, publictrust.ErrIdentity
	}
	return publication, nil
}

func (storage *PublicStorage) forgetPublicIdentity(identity string) {
	storage.mu.Lock()
	archive := storage.byIdentity[identity]
	if archive != nil {
		delete(storage.byIdentity, identity)
		delete(storage.byID, archive.entry.ID)
	}
	storage.mu.Unlock()
	if archive != nil {
		_ = discardVerifiedPublicArchive(archive, "")
	}
}

// ReleaseEntry discards a verified Public Cache staging file after CacheChain
// has streamed it into Local Cache.
func (storage *PublicStorage) ReleaseEntry(_ context.Context, id int64) error {
	storage.mu.Lock()
	archive := storage.byID[id]
	if archive != nil {
		delete(storage.byID, id)
		if storage.byIdentity[archive.identity] == archive {
			delete(storage.byIdentity, archive.identity)
		}
	}
	storage.mu.Unlock()
	if archive == nil {
		return nil
	}
	return discardVerifiedPublicArchive(archive, "")
}

func discardVerifiedPublicArchive(archive *verifiedPublicArchive, preservedPath string) error {
	if archive == nil {
		return nil
	}
	if archive.path != preservedPath {
		if err := os.Remove(archive.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	archive.lease.Release()
	archive.lease = nil
	return nil
}

func clonePublicEntryMetadata(metadata *PublicEntryMetadata) *PublicEntryMetadata {
	if metadata == nil {
		return nil
	}
	cloned := *metadata
	cloned.Envelope.Signatures = append([]publictrust.Signature(nil), metadata.Envelope.Signatures...)
	return &cloned
}

func cloneEntry(entry Entry) Entry {
	entry.Public = clonePublicEntryMetadata(entry.Public)
	return entry
}

func normalizedCommitTrust(request CommitRequest) (CacheSource, *PublicEntryMetadata, error) {
	origin := request.Origin
	if origin == "" {
		origin = SourceLocalCache
	}
	if origin != SourceLocalCache && origin != SourceTeamCache && origin != SourcePublicCache {
		return "", nil, errors.New("unknown Actions cache artifact origin")
	}
	publicMetadata := clonePublicEntryMetadata(request.Public)
	if origin == SourcePublicCache {
		if publicMetadata == nil || publicMetadata.Request.Identity == "" || publicMetadata.Envelope.Payload == "" ||
			publicMetadata.Digest == "" || publicMetadata.Size != request.Size || publicMetadata.ExpiresAt.IsZero() {
			return "", nil, errors.New("Public Cache commit requires complete signed origin metadata")
		}
	} else if publicMetadata != nil {
		return "", nil, errors.New("signed Public Cache metadata requires Public Cache origin")
	}
	return origin, publicMetadata, nil
}

func newPublicResolveRequest(scope Scope, sourceRepository, ref, key, version string) PublicResolveRequest {
	nativeKey := actionsPublicNativeKey(scope.Repository, ref, key, version, scope.Compatibility)
	expected := publictrust.Expected{
		Integration: "actions", Project: scope.Repository,
		Compatibility: scope.Compatibility, NativeKey: nativeKey,
	}
	identity := (publictrust.Publication{
		Integration: expected.Integration, Project: expected.Project,
		Compatibility: expected.Compatibility, NativeKey: expected.NativeKey,
	}).Identity()
	return PublicResolveRequest{
		Identity: identity, Expected: expected, Repository: scope.Repository, SourceRepository: sourceRepository,
		Ref: ref, Key: key, Version: version, Compatibility: scope.Compatibility,
	}
}

func (storage *PublicStorage) sourceRepositoryFor(repository string) (string, error) {
	owner, name, err := splitActionsRepository(repository)
	if err != nil {
		return "", err
	}
	if storage.sourceRepository == "" {
		return "https://github.com/" + owner + "/" + name, nil
	}
	parsed, err := url.Parse(storage.sourceRepository)
	if err != nil || parsed.EscapedPath() != "/"+owner+"/"+name {
		return "", errors.New("configured source repository does not match Actions repository namespace")
	}
	return storage.sourceRepository, nil
}

func canonicalSourceRepository(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", errors.New("source repository must be an absolute HTTPS URL without credentials, query, or fragment")
	}
	path := strings.TrimSuffix(parsed.EscapedPath(), ".git")
	path = strings.TrimSuffix(path, "/")
	owner, name, err := splitActionsRepository(strings.TrimPrefix(path, "/"))
	if err != nil || path != "/"+owner+"/"+name {
		return "", errors.New("source repository path must be exactly /owner/repo")
	}
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = "/" + owner + "/" + name
	parsed.RawPath = ""
	return parsed.String(), nil
}

func splitActionsRepository(repository string) (string, string, error) {
	if strings.TrimSpace(repository) != repository || strings.Count(repository, "/") != 1 {
		return "", "", errors.New("Actions repository must be owner/repo")
	}
	owner, name, _ := strings.Cut(repository, "/")
	if !validRepositoryComponent(owner, false) || !validRepositoryComponent(name, true) {
		return "", "", errors.New("Actions repository must be owner/repo")
	}
	return owner, name, nil
}

func validRepositoryComponent(value string, allowDotAndUnderscore bool) bool {
	if value == "" || value == "." || value == ".." || strings.HasPrefix(value, "-") || strings.HasSuffix(value, "-") {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' ||
			allowDotAndUnderscore && (character == '.' || character == '_') {
			continue
		}
		return false
	}
	return true
}

func actionsPublicNativeKey(repository, ref, key, version, compatibility string) string {
	hasher := sha256.New()
	for _, value := range []string{actionsPublicIdentityVersion, repository, ref, key, version, compatibility} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		hasher.Write(length[:])
		hasher.Write([]byte(value))
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil))
}

func isUnavailablePublicIdentity(err error) bool {
	return errors.Is(err, ErrPublicOffline) || errors.Is(err, publictrust.ErrNotFound) ||
		errors.Is(err, publictrust.ErrRevoked) || errors.Is(err, publictrust.ErrAmbiguous) ||
		errors.Is(err, publictrust.ErrExpired)
}

var _ StorageIndex = (*PublicStorage)(nil)
