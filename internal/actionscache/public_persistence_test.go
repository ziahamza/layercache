package actionscache_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/actionscache"
	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/publictrust"
)

func TestWarmedPublicArchivePersistsTrustAndUsesSignedLeaseOffline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuedAt := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	expiresAt := issuedAt.Add(4 * time.Hour)
	clock := &testClock{now: issuedAt.Add(time.Hour)}
	body := []byte("persistent verified Public Cache archive")
	resolver := newStatefulPublicResolver(privateKey, issuedAt, expiresAt, body)
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-amd64-node24",
	}
	lookup := actionscache.LookupRequest{Scope: scope, Keys: []string{"pnpm-public"}, Version: "v1"}

	artifacts, local, public, hierarchy := openPersistentPublicHierarchy(t, ctx, root, publicKey, resolver, clock.Now)
	first, err := hierarchy.Lookup(ctx, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if first.Source != actionscache.SourcePublicCache || first.Entry.Public == nil {
		t.Fatalf("first lookup = %#v, want Public Cache origin metadata", first)
	}
	if first.Entry.Public.ExpiresAt != expiresAt || first.Entry.Public.Digest == "" || first.Entry.Public.Envelope.Payload == "" {
		t.Fatalf("persisted Public Cache metadata = %#v", first.Entry.Public)
	}
	closePersistentPublicHierarchy(t, artifacts, local, public)

	resolver.SetMode(publicResolverOffline)
	clock.Set(issuedAt.Add(2 * time.Hour))
	artifacts, local, public, hierarchy = openPersistentPublicHierarchy(t, ctx, root, publicKey, resolver, clock.Now)
	offlineHit, err := hierarchy.Lookup(ctx, lookup)
	if err != nil {
		t.Fatalf("offline lookup within signed lease: %v", err)
	}
	if offlineHit.Source != actionscache.SourceLocalCache || offlineHit.Entry.Public == nil {
		t.Fatalf("offline lookup = %#v, want trusted warmed Local Cache hit", offlineHit)
	}
	archive, err := hierarchy.Open(ctx, actionscache.OpenRequest{Scope: scope, ID: offlineHit.Entry.ID})
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(archive.Body)
	archive.Body.Close()
	if readErr != nil || !bytes.Equal(got, body) {
		t.Fatalf("offline archive = %q, err = %v", got, readErr)
	}

	clock.Set(expiresAt.Add(time.Nanosecond))
	if _, err := hierarchy.Lookup(ctx, lookup); !errors.Is(err, actionscache.ErrNotFound) {
		t.Fatalf("expired offline lookup error = %v, want ErrNotFound", err)
	}
	if _, err := local.Lookup(ctx, lookup); !errors.Is(err, actionscache.ErrNotFound) {
		t.Fatalf("expired warmed metadata remained locally: %v", err)
	}
	closePersistentPublicHierarchy(t, artifacts, local, public)
}

func TestOnlineRevocationInvalidatesWarmedPublicArchiveBeforeFallback(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuedAt := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	clock := &testClock{now: issuedAt.Add(time.Hour)}
	resolver := newStatefulPublicResolver(privateKey, issuedAt, issuedAt.Add(24*time.Hour), []byte("later revoked"))
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-amd64-node24",
	}
	lookup := actionscache.LookupRequest{Scope: scope, Keys: []string{"revoked-public"}, Version: "v1"}
	artifacts, local, public, hierarchy := openPersistentPublicHierarchy(t, ctx, root, publicKey, resolver, clock.Now)
	defer closePersistentPublicHierarchy(t, artifacts, local, public)
	if _, err := hierarchy.Lookup(ctx, lookup); err != nil {
		t.Fatal(err)
	}

	resolver.SetMode(publicResolverRevoked)
	if _, err := hierarchy.Lookup(ctx, lookup); !errors.Is(err, actionscache.ErrNotFound) {
		t.Fatalf("revoked lookup error = %v, want ErrNotFound", err)
	}
	if resolver.RevalidationCount() != 1 {
		t.Fatalf("online revalidations = %d, want one", resolver.RevalidationCount())
	}
	if _, err := local.Lookup(ctx, lookup); !errors.Is(err, actionscache.ErrNotFound) {
		t.Fatalf("revoked warmed entry remained locally: %v", err)
	}
}

func TestOnlineRevalidationPersistsRenewedSignedLease(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuedAt := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	originalExpiry := issuedAt.Add(2 * time.Hour)
	renewedExpiry := issuedAt.Add(8 * time.Hour)
	clock := &testClock{now: issuedAt.Add(time.Hour)}
	resolver := newStatefulPublicResolver(privateKey, issuedAt, originalExpiry, []byte("renewed lease bytes"))
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-amd64-node24",
	}
	lookup := actionscache.LookupRequest{Scope: scope, Keys: []string{"renewed-public"}, Version: "v1"}
	artifacts, local, public, hierarchy := openPersistentPublicHierarchy(t, ctx, root, publicKey, resolver, clock.Now)
	if _, err := hierarchy.Lookup(ctx, lookup); err != nil {
		t.Fatal(err)
	}
	resolver.SetExpiry(renewedExpiry)
	renewed, err := hierarchy.Lookup(ctx, lookup)
	if err != nil {
		t.Fatalf("online lease renewal: %v", err)
	}
	if renewed.Entry.Public == nil || renewed.Entry.Public.ExpiresAt != renewedExpiry {
		t.Fatalf("renewed metadata = %#v, want expiry %s", renewed.Entry.Public, renewedExpiry)
	}
	closePersistentPublicHierarchy(t, artifacts, local, public)

	resolver.SetMode(publicResolverOffline)
	clock.Set(originalExpiry.Add(time.Hour))
	artifacts, local, public, hierarchy = openPersistentPublicHierarchy(t, ctx, root, publicKey, resolver, clock.Now)
	defer closePersistentPublicHierarchy(t, artifacts, local, public)
	offline, err := hierarchy.Lookup(ctx, lookup)
	if err != nil {
		t.Fatalf("offline use after original but before renewed signed expiry: %v", err)
	}
	if offline.Entry.Public == nil || offline.Entry.Public.ExpiresAt != renewedExpiry {
		t.Fatalf("persisted renewed metadata = %#v", offline.Entry.Public)
	}
}

func TestOnlineRevalidationRejectsChangedDigestAndSize(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuedAt := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	clock := &testClock{now: issuedAt.Add(time.Hour)}
	resolver := newStatefulPublicResolver(privateKey, issuedAt, issuedAt.Add(24*time.Hour), []byte("original bytes"))
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-amd64-node24",
	}
	lookup := actionscache.LookupRequest{Scope: scope, Keys: []string{"changed-public"}, Version: "v1"}
	artifacts, local, public, hierarchy := openPersistentPublicHierarchy(t, ctx, t.TempDir(), publicKey, resolver, clock.Now)
	defer closePersistentPublicHierarchy(t, artifacts, local, public)
	if _, err := hierarchy.Lookup(ctx, lookup); err != nil {
		t.Fatal(err)
	}

	resolver.SetMode(publicResolverChangedArtifact)
	if _, err := hierarchy.Lookup(ctx, lookup); !errors.Is(err, actionscache.ErrNotFound) {
		t.Fatalf("changed signed artifact lookup error = %v, want ErrNotFound", err)
	}
	if _, err := local.Lookup(ctx, lookup); !errors.Is(err, actionscache.ErrNotFound) {
		t.Fatalf("changed artifact left warmed entry locally: %v", err)
	}
}

func TestOnlineRevalidationRejectsDifferentExactIdentity(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuedAt := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	clock := &testClock{now: issuedAt.Add(time.Hour)}
	resolver := newStatefulPublicResolver(privateKey, issuedAt, issuedAt.Add(24*time.Hour), []byte("identity-bound bytes"))
	public, err := actionscache.NewPublicStorage(actionscache.PublicStorageConfig{
		VerificationKey: publicKey, StagingDirectory: t.TempDir(), Now: clock.Now,
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	defer public.Close()
	result, err := public.Lookup(ctx, actionscache.LookupRequest{
		Scope: actionscache.Scope{
			Repository: "acme/widgets", Ref: "refs/heads/main",
			DefaultRef: "refs/heads/main", Compatibility: "linux-amd64-node24",
		},
		Keys: []string{"identity-public"}, Version: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver.SetMode(publicResolverWrongIdentity)
	if _, err := public.RevalidateEntry(ctx, result.Entry.Public); !errors.Is(err, publictrust.ErrIdentity) {
		t.Fatalf("wrong exact identity revalidation error = %v, want ErrIdentity", err)
	}
}

func TestPersistentStorageCleansIndexAfterCASGarbageCollection(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	artifacts, err := artifact.Open(ctx, filepath.Join(root, "cas"), 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.Close()
	storage, err := actionscache.OpenPersistentStorage(ctx, filepath.Join(root, "actions"), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-amd64-node24",
	}
	putArchive(t, storage, scope, "gc-victim", "v1", []byte("discarded bytes"))
	if err := artifacts.GC(ctx, 0); err != nil {
		t.Fatal(err)
	}
	lookup := actionscache.LookupRequest{Scope: scope, Keys: []string{"gc-victim"}, Version: "v1"}
	if _, err := storage.Lookup(ctx, lookup); !errors.Is(err, actionscache.ErrNotFound) {
		t.Fatalf("lookup after CAS GC = %v, want clean miss", err)
	}
	size := int64(len("replacement"))
	if _, err := storage.Reserve(ctx, actionscache.ReserveRequest{
		Scope: scope, Key: "gc-victim", Version: "v1", CacheSize: &size,
	}); err != nil {
		t.Fatalf("stale identity metadata was not removed: %v", err)
	}
}

func openPersistentPublicHierarchy(
	t *testing.T,
	ctx context.Context,
	root string,
	publicKey ed25519.PublicKey,
	resolver actionscache.PublicResolver,
	now func() time.Time,
) (*artifact.Store, *actionscache.PersistentStorage, *actionscache.PublicStorage, *actionscache.CacheChain) {
	t.Helper()
	artifacts, err := artifact.Open(ctx, filepath.Join(root, "cas"), 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	local, err := actionscache.OpenPersistentStorage(ctx, filepath.Join(root, "actions"), artifacts)
	if err != nil {
		artifacts.Close()
		t.Fatal(err)
	}
	public, err := actionscache.NewPublicStorage(actionscache.PublicStorageConfig{
		VerificationKey: publicKey, StagingDirectory: filepath.Join(root, "public-staging"), Now: now,
	}, resolver)
	if err != nil {
		local.Close()
		artifacts.Close()
		t.Fatal(err)
	}
	hierarchy, err := actionscache.NewCacheChainWithPublic(local, actionscache.NewMemoryStorage(), public)
	if err != nil {
		public.Close()
		local.Close()
		artifacts.Close()
		t.Fatal(err)
	}
	return artifacts, local, public, hierarchy
}

func closePersistentPublicHierarchy(
	t *testing.T,
	artifacts *artifact.Store,
	local *actionscache.PersistentStorage,
	public *actionscache.PublicStorage,
) {
	t.Helper()
	if err := public.Close(); err != nil {
		t.Error(err)
	}
	if err := local.Close(); err != nil {
		t.Error(err)
	}
	if err := artifacts.Close(); err != nil {
		t.Error(err)
	}
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *testClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *testClock) Set(now time.Time) {
	clock.mu.Lock()
	clock.now = now
	clock.mu.Unlock()
}

type publicResolverMode int

const (
	publicResolverValid publicResolverMode = iota
	publicResolverOffline
	publicResolverRevoked
	publicResolverChangedArtifact
	publicResolverWrongIdentity
)

type statefulPublicResolver struct {
	mu            sync.Mutex
	privateKey    ed25519.PrivateKey
	issuedAt      time.Time
	expiresAt     time.Time
	body          []byte
	mode          publicResolverMode
	revalidations int
}

func newStatefulPublicResolver(
	privateKey ed25519.PrivateKey,
	issuedAt time.Time,
	expiresAt time.Time,
	body []byte,
) *statefulPublicResolver {
	return &statefulPublicResolver{
		privateKey: privateKey, issuedAt: issuedAt, expiresAt: expiresAt, body: append([]byte(nil), body...),
	}
}

func (resolver *statefulPublicResolver) SetMode(mode publicResolverMode) {
	resolver.mu.Lock()
	resolver.mode = mode
	resolver.mu.Unlock()
}

func (resolver *statefulPublicResolver) SetExpiry(expiresAt time.Time) {
	resolver.mu.Lock()
	resolver.expiresAt = expiresAt
	resolver.mu.Unlock()
}

func (resolver *statefulPublicResolver) RevalidationCount() int {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	return resolver.revalidations
}

func (resolver *statefulPublicResolver) Resolve(_ context.Context, request actionscache.PublicResolveRequest) (actionscache.PublicResolution, error) {
	resolver.mu.Lock()
	mode := resolver.mode
	body := append([]byte(nil), resolver.body...)
	resolver.mu.Unlock()
	switch mode {
	case publicResolverOffline:
		return actionscache.PublicResolution{}, actionscache.ErrPublicOffline
	case publicResolverRevoked, publicResolverChangedArtifact, publicResolverWrongIdentity:
		return actionscache.PublicResolution{}, publictrust.ErrNotFound
	}
	envelope, err := resolver.sign(request, body)
	if err != nil {
		return actionscache.PublicResolution{}, err
	}
	return actionscache.PublicResolution{Envelope: envelope, Archive: io.NopCloser(bytes.NewReader(body))}, nil
}

func (resolver *statefulPublicResolver) Revalidate(_ context.Context, request actionscache.PublicResolveRequest) (publictrust.Envelope, error) {
	resolver.mu.Lock()
	resolver.revalidations++
	mode := resolver.mode
	body := append([]byte(nil), resolver.body...)
	resolver.mu.Unlock()
	switch mode {
	case publicResolverOffline:
		return publictrust.Envelope{}, actionscache.ErrPublicOffline
	case publicResolverRevoked:
		return publictrust.Envelope{}, publictrust.ErrRevoked
	case publicResolverChangedArtifact:
		body = []byte("different signed bytes and size")
	case publicResolverWrongIdentity:
		request.Expected.NativeKey = "sha256:different-exact-actions-identity"
	}
	return resolver.sign(request, body)
}

func (resolver *statefulPublicResolver) sign(request actionscache.PublicResolveRequest, body []byte) (publictrust.Envelope, error) {
	digest := sha256.Sum256(body)
	resolver.mu.Lock()
	publication := publictrust.Publication{
		Integration: request.Expected.Integration, Project: request.Expected.Project,
		Compatibility: request.Expected.Compatibility, NativeKey: request.Expected.NativeKey,
		Repository: request.SourceRepository, Commit: "0123456789abcdef0123456789abcdef01234567",
		RecipeDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Platform:     "linux/amd64", Toolchain: "actions/cache@v5",
		Builder: "layercache-public-builder@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Digest:  hex.EncodeToString(digest[:]), Size: int64(len(body)), DurationMS: 500,
		BuildID: "public-build-123", IssuedAt: resolver.issuedAt, ExpiresAt: resolver.expiresAt,
	}
	privateKey := append(ed25519.PrivateKey(nil), resolver.privateKey...)
	resolver.mu.Unlock()
	return publictrust.Sign(privateKey, publication)
}
