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

func TestPublicStorageReturnsOnlyFullyVerifiedExactArchives(t *testing.T) {
	t.Parallel()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	body := []byte("verified Actions cache archive")
	resolver := &signedPublicResolver{privateKey: privateKey, now: now, archives: map[publicArchiveCoordinate][]byte{
		{ref: "refs/heads/feature", key: "pnpm-linux", version: "v1"}: body,
	}}
	storage, err := actionscache.NewPublicStorage(actionscache.PublicStorageConfig{
		VerificationKey:  publicKey,
		StagingDirectory: t.TempDir(),
		Now:              func() time.Time { return now.Add(time.Hour) },
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/feature",
		DefaultRef: "refs/heads/main", Compatibility: "linux-amd64-node24",
	}
	result, err := storage.Lookup(context.Background(), actionscache.LookupRequest{
		Scope: scope, Keys: []string{"pnpm-linux", "pnpm-"}, Version: "v1",
	})
	if err != nil {
		t.Fatalf("lookup verified Public Cache archive: %v", err)
	}
	if result.Source != actionscache.SourcePublicCache || result.Match != actionscache.MatchExact ||
		result.RefScope != actionscache.RefScopeCurrent || result.RequestedKey != "pnpm-linux" {
		t.Fatalf("lookup result = %#v, want exact current-ref Public Cache hit", result)
	}
	archive, err := storage.Open(context.Background(), actionscache.OpenRequest{Scope: scope, ID: result.Entry.ID})
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(archive.Body)
	closeErr := archive.Body.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, body) {
		t.Fatalf("archive = %q, read error = %v, close error = %v", got, readErr, closeErr)
	}

	if _, err := storage.Reserve(context.Background(), actionscache.ReserveRequest{}); !errors.Is(err, actionscache.ErrReadOnly) {
		t.Fatalf("Public Cache reserve error = %v, want ErrReadOnly", err)
	}
	if err := storage.Upload(context.Background(), actionscache.UploadRequest{}); !errors.Is(err, actionscache.ErrReadOnly) {
		t.Fatalf("Public Cache upload error = %v, want ErrReadOnly", err)
	}
	if _, err := storage.Commit(context.Background(), actionscache.CommitRequest{}); !errors.Is(err, actionscache.ErrReadOnly) {
		t.Fatalf("Public Cache commit error = %v, want ErrReadOnly", err)
	}
}

func TestPublicStorageRejectsTamperedBytesAndWrongSignedIdentity(t *testing.T) {
	t.Parallel()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-amd64-node24",
	}
	request := actionscache.LookupRequest{Scope: scope, Keys: []string{"pnpm-linux"}, Version: "v1"}

	tests := []struct {
		name       string
		resolution func(context.Context, actionscache.PublicResolveRequest) (actionscache.PublicResolution, error)
		want       error
	}{
		{
			name: "archive bytes changed after signing",
			resolution: func(_ context.Context, resolveRequest actionscache.PublicResolveRequest) (actionscache.PublicResolution, error) {
				return signedResolution(t, privateKey, now, resolveRequest, []byte("trusted"), []byte("tampered"), nil), nil
			},
			want: publictrust.ErrIdentity,
		},
		{
			name: "signed native identity belongs to another key",
			resolution: func(_ context.Context, resolveRequest actionscache.PublicResolveRequest) (actionscache.PublicResolution, error) {
				return signedResolution(t, privateKey, now, resolveRequest, []byte("trusted"), []byte("trusted"), func(publication *publictrust.Publication) {
					publication.NativeKey = "sha256:wrong-native-identity"
				}), nil
			},
			want: publictrust.ErrIdentity,
		},
		{
			name: "signed provenance names another repository",
			resolution: func(_ context.Context, resolveRequest actionscache.PublicResolveRequest) (actionscache.PublicResolution, error) {
				return signedResolution(t, privateKey, now, resolveRequest, []byte("trusted"), []byte("trusted"), func(publication *publictrust.Publication) {
					publication.Repository = "https://github.com/attacker/widgets"
				}), nil
			},
			want: publictrust.ErrIdentity,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			storage, err := actionscache.NewPublicStorage(actionscache.PublicStorageConfig{
				VerificationKey: publicKey, StagingDirectory: t.TempDir(), Now: func() time.Time { return now.Add(time.Hour) },
			}, publicResolverFunc(test.resolution))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = storage.Close() })
			if _, err := storage.Lookup(context.Background(), request); !errors.Is(err, test.want) {
				t.Fatalf("lookup error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestPublicStorageRejectsSignedOversizeBeforeReadingArchive(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	artifacts, err := artifact.Open(ctx, t.TempDir(), 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.Close()
	reads := 0
	resolver := publicResolverFunc(func(_ context.Context, request actionscache.PublicResolveRequest) (actionscache.PublicResolution, error) {
		resolution := signedResolution(t, privateKey, now, request, []byte("12345"), []byte("12345"), nil)
		resolution.Archive = &countingReadCloser{
			reader: bytes.NewReader([]byte("12345")),
			onRead: func() { reads++ },
		}
		return resolution, nil
	})
	storage, err := actionscache.NewPublicStorage(actionscache.PublicStorageConfig{
		VerificationKey: publicKey, StagingDirectory: t.TempDir(),
		ArtifactStore: artifacts, Now: func() time.Time { return now.Add(time.Hour) },
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-amd64-node24",
	}
	_, err = storage.Lookup(ctx, actionscache.LookupRequest{Scope: scope, Keys: []string{"oversize"}, Version: "v1"})
	if !errors.Is(err, artifact.ErrStagingQuota) {
		t.Fatalf("oversize Public Cache error = %v, want artifact.ErrStagingQuota", err)
	}
	if reads != 0 {
		t.Fatalf("oversize Public Cache body read %d times, want 0", reads)
	}
}

func TestPublicWarmReleasesVerifiedStagingBeforeLocalCommitAssembly(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	artifacts, err := artifact.Open(ctx, filepath.Join(root, "cas"), 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer artifacts.Close()
	local, err := actionscache.OpenPersistentStorage(ctx, filepath.Join(root, "actions"), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-amd64-node24",
	}
	resolver := &signedPublicResolver{privateKey: privateKey, now: now, archives: map[publicArchiveCoordinate][]byte{
		{ref: scope.Ref, key: "full-size", version: "v1"}: []byte("1234"),
	}}
	public, err := actionscache.NewPublicStorage(actionscache.PublicStorageConfig{
		VerificationKey: publicKey, StagingDirectory: filepath.Join(root, "public"),
		ArtifactStore: artifacts, Now: func() time.Time { return now.Add(time.Hour) },
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	defer public.Close()
	chain, err := actionscache.NewCacheChainWithPublic(local, actionscache.NewMemoryStorage(), public)
	if err != nil {
		t.Fatal(err)
	}
	result, err := chain.Lookup(ctx, actionscache.LookupRequest{Scope: scope, Keys: []string{"full-size"}, Version: "v1"})
	if err != nil {
		t.Fatalf("warm full-size Public Cache archive: %v", err)
	}
	archive, err := chain.Open(ctx, actionscache.OpenRequest{Scope: scope, ID: result.Entry.ID})
	if err != nil {
		t.Fatal(err)
	}
	contents, readErr := io.ReadAll(archive.Body)
	closeErr := archive.Body.Close()
	if readErr != nil || closeErr != nil || string(contents) != "1234" {
		t.Fatalf("warmed archive = %q, read error = %v, close error = %v", contents, readErr, closeErr)
	}
}

func TestPublicStorageUsesExactCurrentThenDefaultRefIdentityOrderAndScopesOpen(t *testing.T) {
	t.Parallel()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	resolver := &signedPublicResolver{privateKey: privateKey, now: now, archives: map[publicArchiveCoordinate][]byte{
		{ref: "refs/heads/main", key: "restore-secondary", version: "v7"}: []byte("default-ref exact archive"),
	}}
	storage, err := actionscache.NewPublicStorage(actionscache.PublicStorageConfig{
		VerificationKey: publicKey, StagingDirectory: t.TempDir(), Now: func() time.Time { return now.Add(time.Hour) },
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/feature",
		DefaultRef: "refs/heads/main", Compatibility: "linux-amd64-node24",
	}
	result, err := storage.Lookup(context.Background(), actionscache.LookupRequest{
		Scope: scope, Keys: []string{"restore-primary", "restore-secondary"}, Version: "v7",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RefScope != actionscache.RefScopeDefault || result.RequestedKey != "restore-secondary" || result.Match != actionscache.MatchExact {
		t.Fatalf("result = %#v, want second ordered key on default ref", result)
	}

	resolver.mu.Lock()
	requests := append([]actionscache.PublicResolveRequest(nil), resolver.requests...)
	resolver.mu.Unlock()
	wantCoordinates := []publicArchiveCoordinate{
		{ref: "refs/heads/feature", key: "restore-primary", version: "v7"},
		{ref: "refs/heads/feature", key: "restore-secondary", version: "v7"},
		{ref: "refs/heads/main", key: "restore-primary", version: "v7"},
		{ref: "refs/heads/main", key: "restore-secondary", version: "v7"},
	}
	if len(requests) != len(wantCoordinates) {
		t.Fatalf("resolve requests = %d, want %d", len(requests), len(wantCoordinates))
	}
	identities := make(map[string]struct{}, len(requests))
	for index, request := range requests {
		want := wantCoordinates[index]
		if request.Repository != scope.Repository || request.Compatibility != scope.Compatibility ||
			request.Ref != want.ref || request.Key != want.key || request.Version != want.version {
			t.Fatalf("resolve request %d = %#v, want coordinate %#v and server scope %#v", index, request, want, scope)
		}
		if request.SourceRepository != "https://github.com/acme/widgets" {
			t.Fatalf("source repository = %q, want canonical GitHub URL", request.SourceRepository)
		}
		if _, duplicate := identities[request.Identity]; duplicate {
			t.Fatalf("distinct exact coordinate reused public identity %q", request.Identity)
		}
		identities[request.Identity] = struct{}{}
	}

	wrongScope := scope
	wrongScope.Ref = "refs/heads/unrelated"
	wrongScope.DefaultRef = wrongScope.Ref
	if _, err := storage.Open(context.Background(), actionscache.OpenRequest{Scope: wrongScope, ID: result.Entry.ID}); !errors.Is(err, actionscache.ErrNotFound) {
		t.Fatalf("cross-ref open error = %v, want ErrNotFound", err)
	}
	wrongScope = scope
	wrongScope.Repository = "acme/other"
	if _, err := storage.Open(context.Background(), actionscache.OpenRequest{Scope: wrongScope, ID: result.Entry.ID}); !errors.Is(err, actionscache.ErrNotFound) {
		t.Fatalf("cross-repository open error = %v, want ErrNotFound", err)
	}
}

func TestPublicStorageFailsClosedForMalformedOrHostQualifiedActionsRepository(t *testing.T) {
	t.Parallel()

	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	resolver := publicResolverFunc(func(context.Context, actionscache.PublicResolveRequest) (actionscache.PublicResolution, error) {
		t.Fatal("resolver called for repository namespace without safe provenance derivation")
		return actionscache.PublicResolution{}, nil
	})
	storage, err := actionscache.NewPublicStorage(actionscache.PublicStorageConfig{
		VerificationKey: publicKey, StagingDirectory: t.TempDir(),
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	for _, repository := range []string{
		"github.corp.example/acme/widgets",
		"https://github.com/acme/widgets",
		"acme/widgets/extra",
		"acme/..",
		" acme/widgets",
	} {
		t.Run(repository, func(t *testing.T) {
			_, err := storage.Lookup(context.Background(), actionscache.LookupRequest{
				Scope: actionscache.Scope{
					Repository: repository, Ref: "refs/heads/main", DefaultRef: "refs/heads/main", Compatibility: "linux-amd64",
				},
				Keys: []string{"cache-key"}, Version: "v1",
			})
			if !errors.Is(err, actionscache.ErrNotFound) {
				t.Fatalf("lookup error = %v, want safe Public Cache miss", err)
			}
		})
	}
}

func TestPublicStorageUsesExplicitCanonicalGHESSourceRepository(t *testing.T) {
	t.Parallel()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	resolver := &signedPublicResolver{privateKey: privateKey, now: now, archives: map[publicArchiveCoordinate][]byte{
		{ref: "refs/heads/main", key: "cache-key", version: "v1"}: []byte("GHES archive"),
	}}
	storage, err := actionscache.NewPublicStorage(actionscache.PublicStorageConfig{
		VerificationKey: publicKey, StagingDirectory: t.TempDir(),
		SourceRepository: "https://GitHub.Corp.Example/acme/widgets.git",
		Now:              func() time.Time { return now.Add(time.Hour) },
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	_, err = storage.Lookup(context.Background(), actionscache.LookupRequest{
		Scope: actionscache.Scope{
			Repository: "acme/widgets", Ref: "refs/heads/main", DefaultRef: "refs/heads/main", Compatibility: "linux-amd64",
		},
		Keys: []string{"cache-key"}, Version: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver.mu.Lock()
	request := resolver.requests[0]
	resolver.mu.Unlock()
	if request.SourceRepository != "https://github.corp.example/acme/widgets" {
		t.Fatalf("GHES source repository = %q, want canonical server-owned URL", request.SourceRepository)
	}
}

func TestCacheHierarchyChoosesStrongerPublicMatchWarmsLocalAndNeverPublishesPublic(t *testing.T) {
	t.Parallel()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/feature",
		DefaultRef: "refs/heads/main", Compatibility: "linux-amd64-node24",
	}
	local := actionscache.NewMemoryStorage()
	putArchive(t, local, scope, "restore-primary-old", "v1", []byte("local prefix"))
	team := actionscache.NewMemoryStorage()
	putArchive(t, team, scope, "restore-secondary", "v1", []byte("team second-key exact"))
	publicBody := []byte("public first-key exact")
	resolver := &signedPublicResolver{privateKey: privateKey, now: now, archives: map[publicArchiveCoordinate][]byte{
		{ref: scope.Ref, key: "restore-primary", version: "v1"}: publicBody,
	}}
	public, err := actionscache.NewPublicStorage(actionscache.PublicStorageConfig{
		VerificationKey: publicKey, StagingDirectory: t.TempDir(), Now: func() time.Time { return now.Add(time.Hour) },
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = public.Close() })
	hierarchy, err := actionscache.NewCacheChainWithPublic(local, team, public)
	if err != nil {
		t.Fatal(err)
	}
	lookup := actionscache.LookupRequest{Scope: scope, Keys: []string{"restore-primary", "restore-secondary"}, Version: "v1"}
	result, err := hierarchy.Lookup(context.Background(), lookup)
	if err != nil {
		t.Fatal(err)
	}
	if result.Source != actionscache.SourcePublicCache || result.Entry.Key != "restore-primary" || result.Match != actionscache.MatchExact {
		t.Fatalf("hierarchy result = %#v, want stronger Public Cache native match", result)
	}

	// The returned ID is local: Public Cache can disappear immediately after
	// Lookup and Open still observes the entire verified archive.
	if err := public.Close(); err != nil {
		t.Fatal(err)
	}
	archive, err := hierarchy.Open(context.Background(), actionscache.OpenRequest{Scope: scope, ID: result.Entry.ID})
	if err != nil {
		t.Fatalf("open warmed Local Cache archive: %v", err)
	}
	got, readErr := io.ReadAll(archive.Body)
	archive.Body.Close()
	if readErr != nil || !bytes.Equal(got, publicBody) {
		t.Fatalf("warmed archive = %q, err = %v", got, readErr)
	}
	warmed, err := hierarchy.Lookup(context.Background(), lookup)
	if err != nil {
		t.Fatal(err)
	}
	if warmed.Source != actionscache.SourceLocalCache || warmed.Entry.ID != result.Entry.ID {
		t.Fatalf("second lookup = %#v, want warmed Local Cache entry", warmed)
	}

	// Client publication still targets only Local Cache and Team Cache. A
	// closed Public Cache would fail the commit if hierarchy wrote to it.
	committed := putArchive(t, hierarchy, scope, "client-created", "v1", []byte("private client bytes"))
	if committed.Key != "client-created" {
		t.Fatalf("commit = %#v", committed)
	}
	teamHit, err := team.Lookup(context.Background(), actionscache.LookupRequest{Scope: scope, Keys: []string{"client-created"}, Version: "v1"})
	if err != nil || teamHit.Entry.Key != "client-created" {
		t.Fatalf("Team Cache publication = %#v, err = %v", teamHit, err)
	}
}

func TestCacheHierarchyConcurrentPublicHitsExposeOneCompleteLocalArchive(t *testing.T) {
	t.Parallel()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-amd64-node24",
	}
	want := bytes.Repeat([]byte("complete-public-archive-"), 1024)
	resolver := &signedPublicResolver{
		privateKey: privateKey, now: now, delay: 40 * time.Millisecond,
		archives: map[publicArchiveCoordinate][]byte{{ref: scope.Ref, key: "concurrent", version: "v1"}: want},
	}
	public, err := actionscache.NewPublicStorage(actionscache.PublicStorageConfig{
		VerificationKey: publicKey, StagingDirectory: t.TempDir(), Now: func() time.Time { return now.Add(time.Hour) },
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = public.Close() })
	local := &delayedCommitStorage{StorageIndex: actionscache.NewMemoryStorage(), delay: 40 * time.Millisecond}
	hierarchy, err := actionscache.NewCacheChainWithPublic(local, actionscache.NewMemoryStorage(), public)
	if err != nil {
		t.Fatal(err)
	}

	const readers = 32
	start := make(chan struct{})
	errorsByReader := make(chan error, readers)
	var wait sync.WaitGroup
	for range readers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			result, err := hierarchy.Lookup(context.Background(), actionscache.LookupRequest{
				Scope: scope, Keys: []string{"concurrent"}, Version: "v1",
			})
			if err != nil {
				errorsByReader <- err
				return
			}
			archive, err := hierarchy.Open(context.Background(), actionscache.OpenRequest{Scope: scope, ID: result.Entry.ID})
			if err != nil {
				errorsByReader <- err
				return
			}
			got, readErr := io.ReadAll(archive.Body)
			archive.Body.Close()
			if readErr != nil {
				errorsByReader <- readErr
				return
			}
			if !bytes.Equal(got, want) {
				errorsByReader <- errors.New("reader observed incomplete warmed Public Cache archive")
			}
		}()
	}
	close(start)
	wait.Wait()
	close(errorsByReader)
	for err := range errorsByReader {
		t.Errorf("concurrent Public Cache hit: %v", err)
	}
	resolver.mu.Lock()
	resolveCount := len(resolver.requests)
	resolver.mu.Unlock()
	if resolveCount != 1 {
		t.Fatalf("Public Cache resolved %d times, want one concurrent exact resolution", resolveCount)
	}
}

func TestCacheHierarchyUsesCacheOrderOnlyToBreakEqualNativeMatches(t *testing.T) {
	t.Parallel()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	scope := actionscache.Scope{
		Repository: "acme/widgets", Ref: "refs/heads/main",
		DefaultRef: "refs/heads/main", Compatibility: "linux-amd64-node24",
	}
	lookup := actionscache.LookupRequest{Scope: scope, Keys: []string{"same-native-match"}, Version: "v1"}

	for _, test := range []struct {
		name       string
		seedLocal  bool
		wantSource actionscache.CacheSource
	}{
		{name: "Local Cache beats equal Team and Public matches", seedLocal: true, wantSource: actionscache.SourceLocalCache},
		{name: "Team Cache beats equal Public match", seedLocal: false, wantSource: actionscache.SourceTeamCache},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			local := actionscache.NewMemoryStorage()
			if test.seedLocal {
				putArchive(t, local, scope, "same-native-match", "v1", []byte("local"))
			}
			team := actionscache.NewMemoryStorage()
			putArchive(t, team, scope, "same-native-match", "v1", []byte("team"))
			resolver := &signedPublicResolver{privateKey: privateKey, now: now, archives: map[publicArchiveCoordinate][]byte{
				{ref: scope.Ref, key: "same-native-match", version: "v1"}: []byte("public"),
			}}
			public, err := actionscache.NewPublicStorage(actionscache.PublicStorageConfig{
				VerificationKey: publicKey, StagingDirectory: t.TempDir(), Now: func() time.Time { return now.Add(time.Hour) },
			}, resolver)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = public.Close() })
			hierarchy, err := actionscache.NewCacheChainWithPublic(local, team, public)
			if err != nil {
				t.Fatal(err)
			}
			result, err := hierarchy.Lookup(context.Background(), lookup)
			if err != nil {
				t.Fatal(err)
			}
			if result.Source != test.wantSource {
				t.Fatalf("source = %q, want %q for equal native matches", result.Source, test.wantSource)
			}
			resolver.mu.Lock()
			resolveCount := len(resolver.requests)
			resolver.mu.Unlock()
			if resolveCount != 0 {
				t.Fatalf("Public Cache was resolved %d times after an equal stronger-cache match", resolveCount)
			}
		})
	}
}

type publicArchiveCoordinate struct {
	ref     string
	key     string
	version string
}

type signedPublicResolver struct {
	privateKey ed25519.PrivateKey
	now        time.Time
	delay      time.Duration
	archives   map[publicArchiveCoordinate][]byte

	mu       sync.Mutex
	requests []actionscache.PublicResolveRequest
}

type publicResolverFunc func(context.Context, actionscache.PublicResolveRequest) (actionscache.PublicResolution, error)

func (resolve publicResolverFunc) Resolve(ctx context.Context, request actionscache.PublicResolveRequest) (actionscache.PublicResolution, error) {
	return resolve(ctx, request)
}

func (publicResolverFunc) Revalidate(context.Context, actionscache.PublicResolveRequest) (publictrust.Envelope, error) {
	return publictrust.Envelope{}, actionscache.ErrPublicOffline
}

func (resolver *signedPublicResolver) Resolve(ctx context.Context, request actionscache.PublicResolveRequest) (actionscache.PublicResolution, error) {
	if resolver.delay > 0 {
		timer := time.NewTimer(resolver.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return actionscache.PublicResolution{}, ctx.Err()
		case <-timer.C:
		}
	}
	resolver.mu.Lock()
	resolver.requests = append(resolver.requests, request)
	resolver.mu.Unlock()
	body, found := resolver.archives[publicArchiveCoordinate{ref: request.Ref, key: request.Key, version: request.Version}]
	if !found {
		return actionscache.PublicResolution{}, publictrust.ErrNotFound
	}
	envelope, err := signPublicTestEnvelope(resolver.privateKey, resolver.now, request, body)
	if err != nil {
		return actionscache.PublicResolution{}, err
	}
	return actionscache.PublicResolution{Envelope: envelope, Archive: io.NopCloser(bytes.NewReader(body))}, nil
}

func (resolver *signedPublicResolver) Revalidate(_ context.Context, request actionscache.PublicResolveRequest) (publictrust.Envelope, error) {
	body, found := resolver.archives[publicArchiveCoordinate{ref: request.Ref, key: request.Key, version: request.Version}]
	if !found {
		return publictrust.Envelope{}, publictrust.ErrNotFound
	}
	return signPublicTestEnvelope(resolver.privateKey, resolver.now, request, body)
}

func signPublicTestEnvelope(
	privateKey ed25519.PrivateKey,
	now time.Time,
	request actionscache.PublicResolveRequest,
	body []byte,
) (publictrust.Envelope, error) {
	digest := sha256.Sum256(body)
	publication := publictrust.Publication{
		Integration: request.Expected.Integration, Project: request.Expected.Project,
		Compatibility: request.Expected.Compatibility, NativeKey: request.Expected.NativeKey,
		Repository: request.SourceRepository, Commit: "0123456789abcdef0123456789abcdef01234567",
		RecipeDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Platform:     "linux/amd64", Toolchain: "actions/cache@v5",
		Builder: "layercache-public-builder@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Digest:  hex.EncodeToString(digest[:]), Size: int64(len(body)), DurationMS: 500,
		BuildID: "build-123", IssuedAt: now, ExpiresAt: now.Add(24 * time.Hour),
	}
	return publictrust.Sign(privateKey, publication)
}

func signedResolution(
	t *testing.T,
	privateKey ed25519.PrivateKey,
	now time.Time,
	request actionscache.PublicResolveRequest,
	signedBody []byte,
	servedBody []byte,
	mutate func(*publictrust.Publication),
) actionscache.PublicResolution {
	t.Helper()
	digest := sha256.Sum256(signedBody)
	publication := publictrust.Publication{
		Integration: request.Expected.Integration, Project: request.Expected.Project,
		Compatibility: request.Expected.Compatibility, NativeKey: request.Expected.NativeKey,
		Repository: request.SourceRepository, Commit: "0123456789abcdef0123456789abcdef01234567",
		RecipeDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Platform:     "linux/amd64", Toolchain: "actions/cache@v5",
		Builder: "layercache-public-builder@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Digest:  hex.EncodeToString(digest[:]), Size: int64(len(signedBody)), DurationMS: 500,
		BuildID: "build-123", IssuedAt: now, ExpiresAt: now.Add(24 * time.Hour),
	}
	if mutate != nil {
		mutate(&publication)
	}
	envelope, err := publictrust.Sign(privateKey, publication)
	if err != nil {
		t.Fatal(err)
	}
	return actionscache.PublicResolution{Envelope: envelope, Archive: io.NopCloser(bytes.NewReader(servedBody))}
}
