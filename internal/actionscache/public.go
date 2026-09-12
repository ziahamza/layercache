package actionscache

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
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
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/publictrust"
)

const actionsPublicIdentityVersion = "layercache/actions-cache/public-identity/v1"
const legacyTarRegularFile byte = 0

// PublicResolveRequest is the exact, server-owned coordinate a Public Cache
// resolver must resolve. Identity is the publictrust identity derived from
// Expected; the remaining fields let an HTTP adapter send an auditable request
// without reconstructing cache authority from client input.
type PublicResolveRequest struct {
	CacheIdentity    string               `json:"cacheIdentity"`
	Expected         publictrust.Expected `json:"expected"`
	Repository       string               `json:"repository"`
	SourceRepository string               `json:"sourceRepository"`
	Ref              string               `json:"ref"`
	Key              string               `json:"key"`
	Version          string               `json:"version"`
	Compatibility    string               `json:"compatibility"`
	SourceCommit     string               `json:"sourceCommit"`
	RecipeDigest     string               `json:"recipeDigest"`
	Target           string               `json:"target"`
	Platform         string               `json:"platform"`
	Toolchain        string               `json:"toolchain"`
	Builder          string               `json:"builder"`
}

// PublicEntryMetadata is the signed trust record persisted beside an archive
// warmed into Local Cache. ExpiresAt, Digest, and Size are derived from the
// verified envelope and checked against it again before offline use.
type PublicEntryMetadata struct {
	Request        PublicResolveRequest `json:"request"`
	Envelope       publictrust.Envelope `json:"envelope"`
	Digest         string               `json:"digest"`
	Size           int64                `json:"size"`
	ExpiresAt      time.Time            `json:"expiresAt"`
	PublicIdentity string               `json:"publicIdentity"`
}

// PublicResolution contains untrusted input. PublicCacheIndex verifies the DSSE
// envelope and every archive byte before making the result observable.
type PublicResolution struct {
	Envelope publictrust.Envelope
	Archive  io.ReadCloser
}

// PublicResolver obtains one signed publication and its archive. A resolver
// should return publictrust.ErrNotFound, ErrRevoked, or ErrAmbiguous for an
// unusable exact identity; PublicCacheIndex will then try the next ordered exact
// coordinate.
type PublicResolver interface {
	Resolve(context.Context, PublicResolveRequest) (PublicResolution, error)
	Revalidate(context.Context, PublicResolveRequest) (publictrust.Envelope, error)
}

type PublicCacheConfig struct {
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
	// RequireSafeArchive rejects archives whose members can escape the Actions
	// workspace or whose links can redirect later extraction through another
	// archive member. Production Actions adapters must enable it.
	RequireSafeArchive bool
	Now                func() time.Time
}

// PublicCacheIndex is a read-only Actions v1 cache index. It intentionally
// supports exact identities only: the primary key on the current ref. Public
// fallback cannot verify a default-branch commit against the current workflow
// identity. Restore-key, prefix, and default-ref matching remain private Local
// and Team Cache behavior. It never extracts archive bytes.
type PublicCacheIndex struct {
	resolver           PublicResolver
	verificationKey    ed25519.PublicKey
	now                func() time.Time
	stagingRoot        string
	sourceRepository   string
	artifacts          *artifact.Store
	maxArtifactBytes   int64
	requireSafeArchive bool

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

func NewPublicCacheIndex(config PublicCacheConfig, resolver PublicResolver) (*PublicCacheIndex, error) {
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
	return &PublicCacheIndex{
		resolver: resolver, verificationKey: append(ed25519.PublicKey(nil), config.VerificationKey...),
		now: config.Now, stagingRoot: root, sourceRepository: sourceRepository,
		artifacts: config.ArtifactStore, maxArtifactBytes: maxArtifactBytes,
		requireSafeArchive: config.RequireSafeArchive,
		byIdentity:         make(map[string]*verifiedPublicArchive),
		byID:               make(map[int64]*verifiedPublicArchive), inflight: make(map[string]*publicResolveCall),
	}, nil
}

func (storage *PublicCacheIndex) Close() error {
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

func (storage *PublicCacheIndex) Lookup(ctx context.Context, request LookupRequest) (LookupResult, error) {
	if !request.Scope.hasCompletePublicIdentity() {
		return LookupResult{}, ErrNotFound
	}
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
	if len(request.Keys) == 0 {
		return LookupResult{}, ErrNotFound
	}
	primaryKey := request.Keys[0]
	for _, ref := range refs {
		resolveRequest := newPublicResolveRequest(request.Scope, sourceRepository, ref.value, primaryKey, request.Version)
		archive, err := storage.resolveOnce(ctx, resolveRequest)
		if err != nil {
			if isUnavailablePublicIdentity(err) {
				continue
			}
			return LookupResult{}, err
		}
		return LookupResult{
			Entry: cloneEntry(archive.entry), Match: MatchExact, RequestedKey: primaryKey,
			RefScope: ref.scope, Source: SourcePublicCache,
		}, nil
	}
	return LookupResult{}, ErrNotFound
}

func (storage *PublicCacheIndex) Open(_ context.Context, request OpenRequest) (Archive, error) {
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

func (storage *PublicCacheIndex) resolveOnce(ctx context.Context, request PublicResolveRequest) (*verifiedPublicArchive, error) {
	storage.mu.Lock()
	if storage.closed {
		storage.mu.Unlock()
		return nil, ErrNotFound
	}
	if cached := storage.byIdentity[request.CacheIdentity]; cached != nil && !storage.now().UTC().After(cached.expiresAt) {
		storage.mu.Unlock()
		refreshed, err := storage.RevalidateEntry(ctx, cached.entry.Public)
		if err != nil {
			return nil, err
		}
		storage.mu.Lock()
		if current := storage.byIdentity[request.CacheIdentity]; current != nil {
			current.entry.Public = clonePublicEntryMetadata(refreshed)
			current.expiresAt = refreshed.ExpiresAt
			cached = current
		}
		storage.mu.Unlock()
		return cached, nil
	}
	if running := storage.inflight[request.CacheIdentity]; running != nil {
		storage.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-running.done:
			return running.archive, running.err
		}
	}
	call := &publicResolveCall{done: make(chan struct{})}
	storage.inflight[request.CacheIdentity] = call
	storage.mu.Unlock()

	call.archive, call.err = storage.resolveAndVerify(ctx, request)
	storage.mu.Lock()
	delete(storage.inflight, request.CacheIdentity)
	var previous *verifiedPublicArchive
	if call.err == nil && !storage.closed {
		if previous = storage.byIdentity[request.CacheIdentity]; previous != nil {
			delete(storage.byID, previous.entry.ID)
		}
		storage.nextID++
		call.archive.entry.ID = storage.nextID
		storage.byIdentity[request.CacheIdentity] = call.archive
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

func (storage *PublicCacheIndex) resolveAndVerify(ctx context.Context, request PublicResolveRequest) (*verifiedPublicArchive, error) {
	resolved, err := storage.resolver.Resolve(ctx, request)
	if err != nil {
		return nil, err
	}
	if resolved.Archive == nil {
		return nil, errors.New("Public Cache resolver returned no archive")
	}
	defer resolved.Archive.Close()
	publication, err := storage.verifyEnvelope(resolved.Envelope, request, "", -1, "")
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
	if storage.requireSafeArchive {
		if err := validateSafeActionsArchive(temporaryPath); err != nil {
			return nil, fmt.Errorf("verify Actions Public Cache archive members: %w", err)
		}
	}
	path := filepath.Join(storage.stagingRoot, request.CacheIdentity+"-"+publication.Digest)
	if err := os.Rename(temporaryPath, path); err != nil {
		return nil, err
	}
	publicMetadata := &PublicEntryMetadata{
		Request: request, Envelope: resolved.Envelope, Digest: publication.Digest,
		Size: publication.Size, ExpiresAt: publication.ExpiresAt, PublicIdentity: publication.Identity(),
	}
	archive := &verifiedPublicArchive{
		entry: Entry{
			Key: request.Key, Version: request.Version, Ref: request.Ref, Size: size,
			CreatedAt: publication.IssuedAt, ProducerDuration: durationFromMilliseconds(publication.DurationMS),
			Origin: SourcePublicCache, Public: publicMetadata,
		},
		repository: request.Repository, compatibility: request.Compatibility,
		identity: request.CacheIdentity, digest: publication.Digest, expiresAt: publication.ExpiresAt, path: path,
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

const (
	maxActionsArchiveMembers       = 1_000_000
	maxActionsArchiveName          = 4 << 10
	maxActionsExpandedArchiveBytes = int64(100 << 30)
	minActionsExpandedArchiveBytes = int64(1 << 30)
)

func validateSafeActionsArchive(archivePath string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() < 0 {
		return publictrust.ErrIdentity
	}
	buffered := bufio.NewReader(file)
	magic, err := buffered.Peek(4)
	if err != nil {
		return publictrust.ErrIdentity
	}
	var reader io.Reader = buffered
	var closeDecoder func()
	switch {
	case magic[0] == 0x1f && magic[1] == 0x8b:
		gzipReader, err := gzip.NewReader(buffered)
		if err != nil {
			return publictrust.ErrIdentity
		}
		reader = gzipReader
		closeDecoder = func() { _ = gzipReader.Close() }
	case magic[0] == 0x28 && magic[1] == 0xb5 && magic[2] == 0x2f && magic[3] == 0xfd:
		zstdReader, err := zstd.NewReader(buffered, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(1<<30))
		if err != nil {
			return publictrust.ErrIdentity
		}
		reader = zstdReader
		closeDecoder = zstdReader.Close
	}
	if closeDecoder != nil {
		defer closeDecoder()
	}
	expandedLimit := maxActionsExpandedArchiveBytes
	if info.Size() <= maxActionsExpandedArchiveBytes/100 {
		expandedLimit = info.Size() * 100
		if expandedLimit < minActionsExpandedArchiveBytes {
			expandedLimit = minActionsExpandedArchiveBytes
		}
	}
	limited := &io.LimitedReader{R: reader, N: expandedLimit + 1}

	tape := tar.NewReader(limited)
	types := make(map[string]byte)
	links := make(map[string]string)
	var declaredFileBytes int64
	for count := 0; ; count++ {
		if count >= maxActionsArchiveMembers {
			return errors.New("Actions cache archive has too many members")
		}
		header, err := tape.Next()
		if errors.Is(err, io.EOF) {
			if limited.N == 0 {
				return errors.New("Actions cache archive expands beyond its safety limit")
			}
			break
		}
		if err != nil {
			return publictrust.ErrIdentity
		}
		name, err := safeArchivePath(header.Name)
		if err != nil {
			return fmt.Errorf("unsafe archive member %q: %w", header.Name, err)
		}
		for key := range header.PAXRecords {
			if strings.HasPrefix(key, "GNU.sparse.") || key == "SCHILY.realsize" {
				return fmt.Errorf("Actions cache archive member %q uses unsupported sparse metadata", name)
			}
		}
		if name == "." && header.Typeflag == tar.TypeDir {
			continue
		}
		if _, duplicate := types[name]; duplicate {
			return fmt.Errorf("duplicate Actions cache archive member %q", name)
		}
		if header.Mode&0o6000 != 0 {
			return fmt.Errorf("Actions cache archive member %q has set-ID mode", name)
		}
		switch header.Typeflag {
		case tar.TypeReg, legacyTarRegularFile:
			if header.Size > expandedLimit-declaredFileBytes {
				return errors.New("Actions cache archive file contents exceed the safety limit")
			}
			declaredFileBytes += header.Size
		case tar.TypeDir:
		case tar.TypeSymlink:
			target, err := safeArchiveSymlinkTarget(name, header.Linkname)
			if err != nil {
				return fmt.Errorf("unsafe archive symlink %q: %w", name, err)
			}
			links[name] = target
		case tar.TypeLink:
			target, err := safeArchivePath(header.Linkname)
			if err != nil {
				return fmt.Errorf("unsafe archive hard link %q: %w", name, err)
			}
			links[name] = target
		default:
			return fmt.Errorf("Actions cache archive member %q has unsupported type %d", name, header.Typeflag)
		}
		types[name] = header.Typeflag
	}
	if len(types) == 0 {
		return errors.New("Actions cache archive has no members")
	}
	for name := range types {
		for ancestor := path.Dir(name); ancestor != "." && ancestor != "/"; ancestor = path.Dir(ancestor) {
			if ancestorType, present := types[ancestor]; present && ancestorType != tar.TypeDir {
				return fmt.Errorf("Actions cache archive member %q descends through non-directory %q", name, ancestor)
			}
		}
	}
	for name, target := range links {
		targetType, found := types[target]
		if !found || targetType == tar.TypeSymlink || targetType == tar.TypeLink ||
			types[name] == tar.TypeLink && targetType != tar.TypeReg && targetType != legacyTarRegularFile {
			return fmt.Errorf("Actions cache archive link %q has an unsafe target", name)
		}
	}
	return nil
}

func safeArchiveSymlinkTarget(name, target string) (string, error) {
	if target == "" || len(target) > maxActionsArchiveName || strings.ContainsRune(target, 0) ||
		strings.Contains(target, `\`) || strings.HasPrefix(target, "/") {
		return "", publictrust.ErrIdentity
	}
	return safeArchivePath(path.Join(path.Dir(name), target))
}

func safeArchivePath(value string) (string, error) {
	if value == "" || len(value) > maxActionsArchiveName || strings.ContainsRune(value, 0) ||
		strings.Contains(value, `\`) || strings.HasPrefix(value, "/") {
		return "", publictrust.ErrIdentity
	}
	cleaned := path.Clean(value)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", publictrust.ErrIdentity
	}
	first, _, _ := strings.Cut(cleaned, "/")
	if strings.Contains(first, ":") {
		return "", publictrust.ErrIdentity
	}
	return cleaned, nil
}

// RevalidateEntry authorizes one warmed Public Cache entry. An authoritative
// online response always wins. When the network is explicitly offline, it
// verifies the persisted envelope and permits use only through its signed
// expiry. Every field used for restoration is checked against signed data.
func (storage *PublicCacheIndex) RevalidateEntry(ctx context.Context, metadata *PublicEntryMetadata) (*PublicEntryMetadata, error) {
	if metadata == nil {
		return nil, publictrust.ErrIdentity
	}
	envelope, err := storage.resolver.Revalidate(ctx, metadata.Request)
	if err == nil {
		publication, verifyErr := storage.verifyEnvelope(envelope, metadata.Request, metadata.Digest, metadata.Size, metadata.PublicIdentity)
		if verifyErr != nil {
			storage.forgetPublicIdentity(metadata.Request.CacheIdentity)
			return nil, verifyErr
		}
		return &PublicEntryMetadata{
			Request: metadata.Request, Envelope: envelope, Digest: publication.Digest,
			Size: publication.Size, ExpiresAt: publication.ExpiresAt, PublicIdentity: publication.Identity(),
		}, nil
	}
	if !errors.Is(err, ErrPublicOffline) {
		storage.forgetPublicIdentity(metadata.Request.CacheIdentity)
		return nil, err
	}

	publication, err := storage.verifyEnvelope(metadata.Envelope, metadata.Request, metadata.Digest, metadata.Size, metadata.PublicIdentity)
	if err != nil {
		storage.forgetPublicIdentity(metadata.Request.CacheIdentity)
		return nil, err
	}
	if !publication.ExpiresAt.Equal(metadata.ExpiresAt) {
		storage.forgetPublicIdentity(metadata.Request.CacheIdentity)
		return nil, publictrust.ErrIdentity
	}
	return clonePublicEntryMetadata(metadata), nil
}

func (storage *PublicCacheIndex) verifyEnvelope(
	envelope publictrust.Envelope,
	request PublicResolveRequest,
	expectedDigest string,
	expectedSize int64,
	expectedPublicIdentity string,
) (publictrust.Publication, error) {
	expected := request.Expected
	expected.Digest = expectedDigest
	expected.PublicIdentity = expectedPublicIdentity
	if expectedSize >= 0 {
		expected.Size = &expectedSize
	}
	publication, err := publictrust.Verify(storage.verificationKey, envelope, expected, storage.now().UTC())
	if err != nil {
		return publictrust.Publication{}, err
	}
	if publication.CacheIdentity() != request.CacheIdentity || publication.Repository != request.SourceRepository ||
		publication.Commit != request.SourceCommit || publication.RecipeDigest != request.RecipeDigest ||
		publication.Target != request.Target || publication.Platform != request.Platform ||
		publication.Toolchain != request.Toolchain || publication.Builder != request.Builder ||
		expectedSize >= 0 && publication.Size != expectedSize {
		return publictrust.Publication{}, publictrust.ErrIdentity
	}
	return publication, nil
}

func (storage *PublicCacheIndex) forgetPublicIdentity(identity string) {
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
func (storage *PublicCacheIndex) ReleaseEntry(_ context.Context, id int64) error {
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

// ClonePublicEntryMetadata returns an independent copy for durable storage
// adapters outside this package.
func ClonePublicEntryMetadata(metadata *PublicEntryMetadata) *PublicEntryMetadata {
	return clonePublicEntryMetadata(metadata)
}

func cloneEntry(entry Entry) Entry {
	entry.Public = clonePublicEntryMetadata(entry.Public)
	entry.ProducerDuration = cloneDuration(entry.ProducerDuration)
	return entry
}

func cloneDuration(duration *time.Duration) *time.Duration {
	if duration == nil {
		return nil
	}
	value := *duration
	return &value
}

func durationNanoseconds(duration *time.Duration) *int64 {
	if duration == nil {
		return nil
	}
	value := int64(*duration)
	return &value
}

func durationFromMilliseconds(milliseconds int64) *time.Duration {
	if milliseconds < 0 || milliseconds > int64((time.Duration(1<<63-1))/time.Millisecond) {
		return nil
	}
	value := time.Duration(milliseconds) * time.Millisecond
	return &value
}

func normalizedCommitTrust(request CommitRequest) (CacheSource, *PublicEntryMetadata, error) {
	if request.ProducerDuration != nil && *request.ProducerDuration < 0 {
		return "", nil, errors.New("Actions cache producer duration cannot be negative")
	}
	origin := request.Origin
	if origin == "" {
		origin = SourceLocalCache
	}
	if origin != SourceLocalCache && origin != SourceTeamCache && origin != SourcePublicCache {
		return "", nil, errors.New("unknown Actions cache artifact origin")
	}
	publicMetadata := clonePublicEntryMetadata(request.Public)
	if origin == SourcePublicCache {
		if publicMetadata == nil || publicMetadata.Request.CacheIdentity == "" || publicMetadata.Envelope.Payload == "" ||
			publicMetadata.PublicIdentity == "" || publicMetadata.Digest == "" || publicMetadata.Size != request.Size || publicMetadata.ExpiresAt.IsZero() {
			return "", nil, errors.New("Public Cache commit requires complete signed origin metadata")
		}
	} else if publicMetadata != nil {
		return "", nil, errors.New("signed Public Cache metadata requires Public Cache origin")
	}
	return origin, publicMetadata, nil
}

// NormalizeCommitTrust applies the same origin and signed-metadata checks to
// every Actions StorageIndex adapter.
func NormalizeCommitTrust(request CommitRequest) (CacheSource, *PublicEntryMetadata, error) {
	return normalizedCommitTrust(request)
}

func newPublicResolveRequest(scope Scope, sourceRepository, ref, key, version string) PublicResolveRequest {
	nativeKey := actionsPublicNativeKey(scope, ref, key, version)
	inputs := []publictrust.DeclaredInput{
		{Name: "actions.key", Value: key},
		{Name: "actions.ref", Value: ref},
		{Name: "actions.version", Value: version},
		{Name: "compatibility", Value: scope.Compatibility},
	}
	expected := publictrust.Expected{
		Integration: "actions", Project: scope.Repository,
		Compatibility: scope.Compatibility, NativeKey: nativeKey, Repository: sourceRepository,
		Commit: scope.SourceCommit, RecipeDigest: scope.RecipeDigest, Target: scope.Target,
		Platform:  scope.Platform,
		Inputs:    inputs,
		Toolchain: scope.Toolchain, Builder: scope.Builder,
	}
	cacheIdentity := (publictrust.Publication{
		Integration: expected.Integration, Project: expected.Project,
		Compatibility: expected.Compatibility, NativeKey: expected.NativeKey,
	}).CacheIdentity()
	return PublicResolveRequest{
		CacheIdentity: cacheIdentity, Expected: expected, Repository: scope.Repository, SourceRepository: sourceRepository,
		Ref: ref, Key: key, Version: version, Compatibility: scope.Compatibility,
		SourceCommit: scope.SourceCommit, RecipeDigest: scope.RecipeDigest, Target: scope.Target, Platform: scope.Platform,
		Toolchain: scope.Toolchain, Builder: scope.Builder,
	}
}

func (storage *PublicCacheIndex) sourceRepositoryFor(repository string) (string, error) {
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

func actionsPublicNativeKey(scope Scope, ref, key, version string) string {
	hasher := sha256.New()
	for _, value := range []string{
		actionsPublicIdentityVersion, scope.Repository, ref, key, version, scope.Compatibility,
		scope.SourceCommit, scope.RecipeDigest, scope.Platform, scope.Toolchain, scope.Builder,
	} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		hasher.Write(length[:])
		hasher.Write([]byte(value))
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil))
}

func (scope Scope) hasCompletePublicIdentity() bool {
	return scope.SourceCommit != "" && scope.RecipeDigest != "" && scope.Platform != "" &&
		scope.Target != "" && scope.Toolchain != "" && scope.Builder != ""
}

func isUnavailablePublicIdentity(err error) bool {
	return errors.Is(err, ErrPublicOffline) || errors.Is(err, publictrust.ErrNotFound) ||
		errors.Is(err, publictrust.ErrRevoked) || errors.Is(err, publictrust.ErrAmbiguous) ||
		errors.Is(err, publictrust.ErrExpired)
}

var _ CacheReader = (*PublicCacheIndex)(nil)
