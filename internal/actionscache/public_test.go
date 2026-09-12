package actionscache_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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

func TestPublicCacheIndexRejectsUnsafeArchiveMembersBeforeRestore(t *testing.T) {
	t.Parallel()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	scope := publicScope("acme/widgets", "refs/heads/main", "refs/heads/main", "linux-amd64-node24")
	for _, test := range []struct {
		name                 string
		member               string
		absoluteSymlink      bool
		nonDirectoryAncestor bool
		ancestorLast         bool
		sparseMetadata       bool
		wantMiss             bool
	}{
		{name: "workspace file", member: "node_modules/pkg/index.js"},
		{name: "parent traversal", member: "../../.ssh/authorized_keys", wantMiss: true},
		{name: "absolute path", member: "/tmp/layercache-escape", wantMiss: true},
		{name: "absolute symlink target", absoluteSymlink: true, wantMiss: true},
		{name: "file ancestor before child", nonDirectoryAncestor: true, wantMiss: true},
		{name: "file ancestor after child", nonDirectoryAncestor: true, ancestorLast: true, wantMiss: true},
		{name: "sparse file metadata", sparseMetadata: true, wantMiss: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var body []byte
			switch {
			case test.absoluteSymlink:
				body = gzipTarWithAbsoluteSymlink(t)
			case test.nonDirectoryAncestor:
				body = gzipTarWithNonDirectoryAncestor(t, test.ancestorLast)
			case test.sparseMetadata:
				body = gzipTarWithSparseMetadata(t)
			default:
				body = gzipTarArchive(t, test.member, []byte("content"))
			}
			resolver := &signedPublicResolver{privateKey: privateKey, now: now, archives: map[publicArchiveCoordinate][]byte{
				{ref: scope.Ref, key: "safe-members", version: "v1"}: body,
			}}
			storage, err := actionscache.NewPublicCacheIndex(actionscache.PublicCacheConfig{
				VerificationKey: publicKey, StagingDirectory: t.TempDir(), RequireSafeArchive: true,
				Now: func() time.Time { return now.Add(time.Hour) },
			}, resolver)
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			_, err = storage.Lookup(context.Background(), actionscache.LookupRequest{
				Scope: scope, Keys: []string{"safe-members"}, Version: "v1",
			})
			if test.wantMiss && err == nil {
				t.Fatal("unsafe signed archive became visible")
			}
			if !test.wantMiss && err != nil {
				t.Fatalf("safe signed archive was rejected: %v", err)
			}
		})
	}
}

func gzipTarWithSparseMetadata(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	compressor := gzip.NewWriter(&encoded)
	archive := tar.NewWriter(compressor)
	header := &tar.Header{
		Name:       "node_modules/pkg/sparse.bin",
		Mode:       0o644,
		Size:       1,
		Typeflag:   tar.TypeReg,
		PAXRecords: map[string]string{"SCHILY.realsize": "1099511627776"},
	}
	if err := archive.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write([]byte{'x'}); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func gzipTarWithAbsoluteSymlink(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	compressor := gzip.NewWriter(&encoded)
	archive := tar.NewWriter(compressor)
	for _, header := range []*tar.Header{
		{Name: "node_modules/pkg/etc/passwd", Mode: 0o644, Size: 7, Typeflag: tar.TypeReg},
		{Name: "node_modules/pkg/link", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
	} {
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := archive.Write([]byte("content")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func gzipTarWithNonDirectoryAncestor(t *testing.T, ancestorLast bool) []byte {
	t.Helper()
	ancestor := &tar.Header{Name: "node_modules/pkg", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg}
	child := &tar.Header{Name: "node_modules/pkg/index.js", Mode: 0o644, Size: 5, Typeflag: tar.TypeReg}
	headers := []*tar.Header{ancestor, child}
	if ancestorLast {
		headers = []*tar.Header{child, ancestor}
	}
	var encoded bytes.Buffer
	compressor := gzip.NewWriter(&encoded)
	archive := tar.NewWriter(compressor)
	for _, header := range headers {
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(bytes.Repeat([]byte{'x'}, int(header.Size))); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestPublicCacheIndexReturnsOnlyFullyVerifiedExactArchives(t *testing.T) {
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
	storage, err := actionscache.NewPublicCacheIndex(actionscache.PublicCacheConfig{
		VerificationKey:  publicKey,
		StagingDirectory: t.TempDir(),
		Now:              func() time.Time { return now.Add(time.Hour) },
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	scope := publicScope("acme/widgets", "refs/heads/feature", "refs/heads/main", "linux-amd64-node24")
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

}

func TestPublicCacheIndexRejectsTamperedBytesAndWrongSignedIdentity(t *testing.T) {
	t.Parallel()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	scope := publicScope("acme/widgets", "refs/heads/main", "refs/heads/main", "linux-amd64-node24")
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
			storage, err := actionscache.NewPublicCacheIndex(actionscache.PublicCacheConfig{
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

func TestPublicCacheIndexRejectsSignedOversizeBeforeReadingArchive(t *testing.T) {
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
	storage, err := actionscache.NewPublicCacheIndex(actionscache.PublicCacheConfig{
		VerificationKey: publicKey, StagingDirectory: t.TempDir(),
		ArtifactStore: artifacts, Now: func() time.Time { return now.Add(time.Hour) },
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	scope := publicScope("acme/widgets", "refs/heads/main", "refs/heads/main", "linux-amd64-node24")
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
	scope := publicScope("acme/widgets", "refs/heads/main", "refs/heads/main", "linux-amd64-node24")
	resolver := &signedPublicResolver{privateKey: privateKey, now: now, archives: map[publicArchiveCoordinate][]byte{
		{ref: scope.Ref, key: "full-size", version: "v1"}: []byte("1234"),
	}}
	public, err := actionscache.NewPublicCacheIndex(actionscache.PublicCacheConfig{
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

func TestPublicCacheIndexUsesOnlyExactPrimaryKeyOnCurrentRef(t *testing.T) {
	t.Parallel()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	resolver := &signedPublicResolver{privateKey: privateKey, now: now, archives: map[publicArchiveCoordinate][]byte{
		{ref: "refs/heads/main", key: "restore-primary", version: "v7"}: []byte("default-ref exact archive"),
	}}
	storage, err := actionscache.NewPublicCacheIndex(actionscache.PublicCacheConfig{
		VerificationKey: publicKey, StagingDirectory: t.TempDir(), Now: func() time.Time { return now.Add(time.Hour) },
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	scope := publicScope("acme/widgets", "refs/heads/feature", "refs/heads/main", "linux-amd64-node24")
	lookup := actionscache.LookupRequest{
		Scope: scope, Keys: []string{"restore-primary", "restore-secondary"}, Version: "v7",
	}
	if _, err := storage.Lookup(context.Background(), lookup); !errors.Is(err, actionscache.ErrNotFound) {
		t.Fatalf("default-ref-only Public lookup error = %v, want ErrNotFound", err)
	}
	resolver.mu.Lock()
	resolver.archives[publicArchiveCoordinate{
		ref: "refs/heads/feature", key: "restore-primary", version: "v7",
	}] = []byte("current-ref exact archive")
	resolver.mu.Unlock()
	result, err := storage.Lookup(context.Background(), lookup)
	if err != nil {
		t.Fatal(err)
	}
	if result.RefScope != actionscache.RefScopeCurrent || result.RequestedKey != "restore-primary" || result.Match != actionscache.MatchExact {
		t.Fatalf("result = %#v, want exact primary key on current ref", result)
	}

	resolver.mu.Lock()
	requests := append([]actionscache.PublicResolveRequest(nil), resolver.requests...)
	resolver.mu.Unlock()
	wantCoordinates := []publicArchiveCoordinate{
		{ref: "refs/heads/feature", key: "restore-primary", version: "v7"},
		{ref: "refs/heads/feature", key: "restore-primary", version: "v7"},
	}
	if len(requests) != len(wantCoordinates) {
		t.Fatalf("resolve requests = %d, want %d", len(requests), len(wantCoordinates))
	}
	for index, request := range requests {
		want := wantCoordinates[index]
		if request.Repository != scope.Repository || request.Compatibility != scope.Compatibility ||
			request.Ref != want.ref || request.Key != want.key || request.Version != want.version {
			t.Fatalf("resolve request %d = %#v, want coordinate %#v and server scope %#v", index, request, want, scope)
		}
		if request.SourceRepository != "https://github.com/acme/widgets" {
			t.Fatalf("source repository = %q, want canonical GitHub URL", request.SourceRepository)
		}
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

func TestPublicCacheIndexFailsClosedForMalformedOrHostQualifiedActionsRepository(t *testing.T) {
	t.Parallel()

	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	resolver := publicResolverFunc(func(context.Context, actionscache.PublicResolveRequest) (actionscache.PublicResolution, error) {
		t.Fatal("resolver called for repository namespace without safe provenance derivation")
		return actionscache.PublicResolution{}, nil
	})
	storage, err := actionscache.NewPublicCacheIndex(actionscache.PublicCacheConfig{
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
				Scope: publicScope(repository, "refs/heads/main", "refs/heads/main", "linux-amd64"),
				Keys:  []string{"cache-key"}, Version: "v1",
			})
			if !errors.Is(err, actionscache.ErrNotFound) {
				t.Fatalf("lookup error = %v, want safe Public Cache miss", err)
			}
		})
	}
}

func TestPublicCacheIndexUsesExplicitCanonicalGHESSourceRepository(t *testing.T) {
	t.Parallel()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	resolver := &signedPublicResolver{privateKey: privateKey, now: now, archives: map[publicArchiveCoordinate][]byte{
		{ref: "refs/heads/main", key: "cache-key", version: "v1"}: []byte("GHES archive"),
	}}
	storage, err := actionscache.NewPublicCacheIndex(actionscache.PublicCacheConfig{
		VerificationKey: publicKey, StagingDirectory: t.TempDir(),
		SourceRepository: "https://GitHub.Corp.Example/acme/widgets.git",
		Now:              func() time.Time { return now.Add(time.Hour) },
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	_, err = storage.Lookup(context.Background(), actionscache.LookupRequest{
		Scope: publicScope("acme/widgets", "refs/heads/main", "refs/heads/main", "linux-amd64"),
		Keys:  []string{"cache-key"}, Version: "v1",
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
	scope := publicScope("acme/widgets", "refs/heads/feature", "refs/heads/main", "linux-amd64-node24")
	local := actionscache.NewMemoryStorage()
	putArchive(t, local, scope, "restore-primary-old", "v1", []byte("local prefix"))
	team := actionscache.NewMemoryStorage()
	putArchive(t, team, scope, "restore-secondary", "v1", []byte("team second-key exact"))
	publicBody := []byte("public first-key exact")
	resolver := &signedPublicResolver{privateKey: privateKey, now: now, archives: map[publicArchiveCoordinate][]byte{
		{ref: scope.Ref, key: "restore-primary", version: "v1"}: publicBody,
	}}
	public, err := actionscache.NewPublicCacheIndex(actionscache.PublicCacheConfig{
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

func TestWarmedPublicEntryCannotMatchRestoreKey(t *testing.T) {
	t.Parallel()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	scope := publicScope("acme/widgets", "refs/heads/main", "refs/heads/main", "linux-amd64-node24")
	local := actionscache.NewMemoryStorage()
	resolver := &signedPublicResolver{privateKey: privateKey, now: now, archives: map[publicArchiveCoordinate][]byte{
		{ref: scope.Ref, key: "public-exact", version: "v1"}: []byte("public archive"),
	}}
	public, err := actionscache.NewPublicCacheIndex(actionscache.PublicCacheConfig{
		VerificationKey: publicKey, StagingDirectory: t.TempDir(), Now: func() time.Time { return now.Add(time.Hour) },
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	hierarchy, err := actionscache.NewCacheChainWithPublic(local, nil, public)
	if err != nil {
		t.Fatal(err)
	}
	exact := actionscache.LookupRequest{Scope: scope, Keys: []string{"public-exact"}, Version: "v1"}
	if _, err := hierarchy.Lookup(context.Background(), exact); err != nil {
		t.Fatal(err)
	}
	if err := public.Close(); err != nil {
		t.Fatal(err)
	}

	restoreKey := actionscache.LookupRequest{
		Scope: scope, Keys: []string{"different-primary", "public-exact"}, Version: "v1",
	}
	if _, err := hierarchy.Lookup(context.Background(), restoreKey); !errors.Is(err, actionscache.ErrNotFound) {
		t.Fatalf("warmed Public entry restore-key lookup error = %v, want ErrNotFound", err)
	}
}

func TestCacheHierarchyConcurrentPublicHitsExposeOneCompleteLocalArchive(t *testing.T) {
	t.Parallel()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	scope := publicScope("acme/widgets", "refs/heads/main", "refs/heads/main", "linux-amd64-node24")
	want := bytes.Repeat([]byte("complete-public-archive-"), 1024)
	resolver := &signedPublicResolver{
		privateKey: privateKey, now: now, delay: 40 * time.Millisecond,
		archives: map[publicArchiveCoordinate][]byte{{ref: scope.Ref, key: "concurrent", version: "v1"}: want},
	}
	public, err := actionscache.NewPublicCacheIndex(actionscache.PublicCacheConfig{
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

func TestCacheHierarchyNeverSharesConcurrentPublicBytesAcrossSourceCommits(t *testing.T) {
	t.Parallel()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	firstScope := publicScope("acme/widgets", "refs/heads/main", "refs/heads/main", "linux-amd64-node24")
	firstScope.SourceCommit = "1111111111111111111111111111111111111111"
	secondScope := firstScope
	secondScope.SourceCommit = "2222222222222222222222222222222222222222"
	resolver := &signedPublicResolver{
		privateKey: privateKey, now: now, delay: 30 * time.Millisecond,
		archives: map[publicArchiveCoordinate][]byte{
			{ref: firstScope.Ref, key: "same-native-key", version: "v1", sourceCommit: firstScope.SourceCommit}:   []byte("first-commit-bytes"),
			{ref: secondScope.Ref, key: "same-native-key", version: "v1", sourceCommit: secondScope.SourceCommit}: []byte("second-commit-bytes"),
		},
	}
	public, err := actionscache.NewPublicCacheIndex(actionscache.PublicCacheConfig{
		VerificationKey: publicKey, StagingDirectory: t.TempDir(), Now: func() time.Time { return now.Add(time.Hour) },
	}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = public.Close() })
	hierarchy, err := actionscache.NewCacheChainWithPublic(actionscache.NewMemoryStorage(), nil, public)
	if err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		want []byte
		got  []byte
		err  error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	for _, candidate := range []struct {
		scope actionscache.Scope
		want  []byte
	}{{firstScope, []byte("first-commit-bytes")}, {secondScope, []byte("second-commit-bytes")}} {
		candidate := candidate
		go func() {
			<-start
			result, lookupErr := hierarchy.Lookup(context.Background(), actionscache.LookupRequest{
				Scope: candidate.scope, Keys: []string{"same-native-key"}, Version: "v1",
			})
			if lookupErr != nil {
				results <- outcome{want: candidate.want, err: lookupErr}
				return
			}
			archive, openErr := hierarchy.Open(context.Background(), actionscache.OpenRequest{Scope: candidate.scope, ID: result.Entry.ID})
			if openErr != nil {
				results <- outcome{want: candidate.want, err: openErr}
				return
			}
			got, readErr := io.ReadAll(archive.Body)
			_ = archive.Body.Close()
			results <- outcome{want: candidate.want, got: got, err: readErr}
		}()
	}
	close(start)
	succeeded := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			if !errors.Is(result.err, actionscache.ErrNotFound) {
				t.Fatalf("concurrent distinct-provenance lookup failed unsafely: %v", result.err)
			}
			continue
		}
		succeeded++
		if !bytes.Equal(result.got, result.want) {
			t.Fatalf("lookup restored %q, want bytes for its own source commit %q", result.got, result.want)
		}
	}
	if succeeded == 0 {
		t.Fatal("both concurrent valid Public lookups missed")
	}
}

func TestCacheHierarchyUsesCacheOrderOnlyToBreakEqualNativeMatches(t *testing.T) {
	t.Parallel()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	scope := publicScope("acme/widgets", "refs/heads/main", "refs/heads/main", "linux-amd64-node24")
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
			public, err := actionscache.NewPublicCacheIndex(actionscache.PublicCacheConfig{
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
	ref          string
	key          string
	version      string
	sourceCommit string
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
	body, found := resolver.archive(request)
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
	body, found := resolver.archive(request)
	if !found {
		return publictrust.Envelope{}, publictrust.ErrNotFound
	}
	return signPublicTestEnvelope(resolver.privateKey, resolver.now, request, body)
}

func (resolver *signedPublicResolver) archive(request actionscache.PublicResolveRequest) ([]byte, bool) {
	body, found := resolver.archives[publicArchiveCoordinate{
		ref: request.Ref, key: request.Key, version: request.Version, sourceCommit: request.SourceCommit,
	}]
	if found {
		return body, true
	}
	body, found = resolver.archives[publicArchiveCoordinate{ref: request.Ref, key: request.Key, version: request.Version}]
	return body, found
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
		Repository: request.SourceRepository, Commit: request.SourceCommit,
		RecipeDigest: request.RecipeDigest, Target: request.Target,
		Platform: request.Platform, Inputs: append([]publictrust.DeclaredInput(nil), request.Expected.Inputs...),
		Toolchain: request.Toolchain, Builder: request.Builder,
		BuilderImageDigest: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		Digest:             hex.EncodeToString(digest[:]), Size: int64(len(body)), DurationMS: 500,
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
		Repository: request.SourceRepository, Commit: request.SourceCommit,
		RecipeDigest: request.RecipeDigest, Target: request.Target,
		Platform: request.Platform, Inputs: append([]publictrust.DeclaredInput(nil), request.Expected.Inputs...),
		Toolchain: request.Toolchain, Builder: request.Builder,
		BuilderImageDigest: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		Digest:             hex.EncodeToString(digest[:]), Size: int64(len(signedBody)), DurationMS: 500,
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

func publicScope(repository, ref, defaultRef, compatibility string) actionscache.Scope {
	return actionscache.Scope{
		Repository: repository, Ref: ref, DefaultRef: defaultRef, Compatibility: compatibility,
		SourceCommit: "0123456789abcdef0123456789abcdef01234567",
		RecipeDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Target:       ".github/workflows/public-cache.yml#public-cache", Platform: "linux/amd64", Toolchain: "actions/cache@v5",
		Builder: "layercache-public-builder@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
}

func gzipTarArchive(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	compressor := gzip.NewWriter(&encoded)
	archive := tar.NewWriter(compressor)
	if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}
