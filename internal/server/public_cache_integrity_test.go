package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/publicbuild"
	"github.com/layercache/layercache/internal/publictrust"
)

func TestResolveTurboRejectsSignedSizeMismatch(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("complete-public-artifact")
	digest := sha256.Sum256(want)
	now := time.Now().UTC()
	publication := publictrust.Publication{
		Integration: "turbo", Project: "github.com/acme/widget",
		Compatibility: "linux-amd64-schema1", NativeKey: "signed-wrong-size",
		Repository:   "https://github.com/acme/widget",
		Commit:       "0123456789abcdef0123456789abcdef01234567",
		RecipeDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		Target:       "@acme/widget#test",
		Platform:     "linux/amd64", Toolchain: "turbo@2.10.9",
		Builder:            "layercache-builder@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		BuilderImageDigest: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		Digest:             hex.EncodeToString(digest[:]), Size: int64(len(want) + 1),
		DurationMS: 9000, BuildID: "public-build-1",
		IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	}
	envelope, err := publictrust.Sign(privateKey, publication)
	if err != nil {
		t.Fatal(err)
	}
	publicServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/public/resolve":
			_ = json.NewEncoder(writer).Encode(publicResolveResponse{
				Envelope: envelope, ArtifactURL: "/v1/public/artifacts/" + publication.Identity(),
			})
		case "/v1/public/artifacts/" + publication.Identity():
			_, _ = writer.Write(want)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer publicServer.Close()

	cfg := config.Config{
		Version: 1, Role: "local", DataDir: t.TempDir(), Listen: "127.0.0.1:7437",
		MaxBytes: 1 << 20, ProjectID: publication.Project, PublicURL: publicServer.URL,
		PublicTrustKey: publictrust.EncodePublicKey(publicKey), LocalToken: "local-token",
		CompatibilityID: publication.Compatibility, ActionsRepository: "acme/widget",
		ActionsRef: "refs/heads/main", ActionsDefaultRef: "refs/heads/main",
		BuildkitBuilder: "layercache-test",
	}
	instance, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	key := artifact.Key{
		Integration: publication.Integration, Project: publication.Project,
		Compatibility: publication.Compatibility, Native: publication.NativeKey,
	}

	resolution, err := instance.resolveTurbo(context.Background(), key)
	if resolution.Body != nil {
		_ = resolution.Body.Close()
	}
	if !errors.Is(err, artifact.ErrCorrupt) {
		t.Fatalf("signed size mismatch error = %v, want ErrCorrupt", err)
	}
	if _, err := instance.store.Head(context.Background(), key); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("mismatched public artifact remained in Local Cache: %v", err)
	}
}

func TestEmbeddedPublicPublicationPinsSurviveEvictionAndRestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	body := bytes.Repeat([]byte("p"), 128)
	digest := sha256.Sum256(body)
	now := time.Now().UTC()
	publication := publictrust.Publication{
		Integration: "turbo", Project: "github.com/acme/widget",
		Compatibility: "linux-amd64-schema1", NativeKey: "pinned-public-entry",
		Repository: "https://github.com/acme/widget", Commit: strings.Repeat("a", 40),
		RecipeDigest: "sha256:" + strings.Repeat("b", 64), Target: "@acme/widget#build",
		Platform: "linux/amd64", Inputs: []publictrust.DeclaredInput{{Name: "compatibility", Value: "linux-amd64-schema1"}},
		Toolchain: "turbo@2.10.12", Builder: "layercache-turbo-builder-v1",
		BuilderImageDigest: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		Digest:             hex.EncodeToString(digest[:]), Size: int64(len(body)), DurationMS: 1200,
		BuildID: "public-build-1", IssuedAt: now, ExpiresAt: now.Add(24 * time.Hour),
	}

	store, err := artifact.Open(ctx, root, 1<<10, 0)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := publictrust.OpenRegistry(ctx, root)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	key := publicArtifactKey(publication)
	if _, _, err := store.Put(ctx, key, artifact.Metadata{}, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if err := registry.Publish(ctx, publication); err != nil {
		t.Fatal(err)
	}
	instance := &Server{store: store, localStore: store}
	if err := instance.reconcilePublicPins(ctx, registry); err != nil {
		t.Fatal(err)
	}
	if err := store.GC(ctx, 0); !errors.Is(err, artifact.ErrQuota) {
		t.Fatalf("GC with active publication pin = %v, want ErrQuota", err)
	}
	if _, err := store.Head(ctx, key); err != nil {
		t.Fatalf("pinned public artifact was evicted: %v", err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = artifact.Open(ctx, root, 1<<10, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	registry, err = publictrust.OpenRegistry(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	instance = &Server{store: store, localStore: store}
	if err := instance.reconcilePublicPins(ctx, registry); err != nil {
		t.Fatal(err)
	}
	if err := store.GC(ctx, 0); !errors.Is(err, artifact.ErrQuota) {
		t.Fatalf("GC after pin recovery = %v, want ErrQuota", err)
	}
	if _, err := store.Head(ctx, key); err != nil {
		t.Fatalf("reconciled public artifact was evicted: %v", err)
	}

	if err := registry.Revoke(ctx, publication.CacheIdentity(), "manual QA revocation", now); err != nil {
		t.Fatal(err)
	}
	if err := instance.releasePublicArtifact(ctx, publication.CacheIdentity(), key); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Head(ctx, key); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("revoked public artifact remained available: %v", err)
	}
}

func TestResolveTurboFallsBackToVerifiedPublicAfterCorruptTeamHit(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("verified-public-repair")
	digest := sha256.Sum256(want)
	now := time.Now().UTC()
	publication := publictrust.Publication{
		Integration: "turbo", Project: "github.com/acme/widget",
		Compatibility: "linux-amd64-schema1", NativeKey: "team-poisoned-key",
		Repository:   "https://github.com/acme/widget",
		Commit:       "0123456789abcdef0123456789abcdef01234567",
		RecipeDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		Target:       "@acme/widget#test", Platform: "linux/amd64", Toolchain: "turbo@2.10.9",
		Builder: "layercache-builder-v1", Digest: hex.EncodeToString(digest[:]), Size: int64(len(want)),
		BuilderImageDigest: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		DurationMS:         100, BuildID: "public-build-1", IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	}
	envelope, err := publictrust.Sign(privateKey, publication)
	if err != nil {
		t.Fatal(err)
	}
	team := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("x-layercache-digest", strings.Repeat("0", 64))
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("poisoned-team-bytes"))
	}))
	defer team.Close()
	public := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/public/resolve":
			_ = json.NewEncoder(writer).Encode(publicResolveResponse{
				Envelope: envelope, ArtifactURL: "/v1/public/artifacts/" + publication.Identity(),
			})
		case "/v1/public/artifacts/" + publication.Identity():
			_, _ = writer.Write(want)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer public.Close()

	cfg := config.Config{
		Version: 1, Role: "local", DataDir: t.TempDir(), Listen: "127.0.0.1:7437", MaxBytes: 1 << 20,
		ProjectID: publication.Project, TeamURL: team.URL, TeamToken: "team-token",
		PublicURL: public.URL, PublicTrustKey: publictrust.EncodePublicKey(publicKey),
		LocalToken: "local-token", CompatibilityID: publication.Compatibility,
		ActionsRepository: "acme/widget", ActionsRef: "refs/heads/main", ActionsDefaultRef: "refs/heads/main",
		BuildkitBuilder: "layercache-test",
	}
	instance, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	key := artifact.Key{Integration: "turbo", Project: publication.Project, Compatibility: publication.Compatibility, Native: publication.NativeKey}
	resolution, err := instance.resolveTurbo(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer resolution.Body.Close()
	got, err := io.ReadAll(resolution.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Source != "public" || resolution.Entry.Digest != publication.Digest ||
		!bytes.Equal(got, want) || !resolution.Degraded {
		t.Fatalf("fallback = source %q digest %q degraded %t body %q", resolution.Source, resolution.Entry.Digest, resolution.Degraded, got)
	}
}

func TestResolveTurboPreservesRemoteOutageAsDegradedMiss(t *testing.T) {
	t.Parallel()

	team := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer team.Close()
	cfg := config.Config{
		Version: 1, Role: "local", DataDir: t.TempDir(), Listen: "127.0.0.1:7437", MaxBytes: 1 << 20,
		ProjectID: "github.com/acme/widget", TeamURL: team.URL, TeamToken: "team-token",
		LocalToken: "local-token", CompatibilityID: "linux-amd64-schema1",
		ActionsRepository: "acme/widget", ActionsRef: "refs/heads/main", ActionsDefaultRef: "refs/heads/main",
		BuildkitBuilder: "layercache-test",
	}
	instance, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	resolution, err := instance.resolveTurbo(context.Background(), artifact.Key{
		Integration: "turbo", Project: cfg.ProjectID,
		Compatibility: cfg.CompatibilityID, Native: "remote-outage",
	})
	if !errors.Is(err, artifact.ErrNotFound) || !resolution.Degraded || resolution.Body != nil {
		t.Fatalf("remote outage resolution = %#v, %v", resolution, err)
	}
}

func TestRejectedPublicPublicationDoesNotClaimCacheIdentity(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	headCommit := strings.Repeat("f", 40)
	githubAPI := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "/git/ref/heads/main") {
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"ref": "refs/heads/main", "object": map[string]string{"type": "commit", "sha": headCommit},
			})
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]string{"status": "ahead"})
	}))
	defer githubAPI.Close()
	recipe, err := publicbuild.MaintainedRecipeDigest(publicbuild.IntegrationTurbo, "@acme/widget#test")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Version: 1, Role: "public", DataDir: t.TempDir(), Listen: "127.0.0.1:7437",
		MaxBytes: 1 << 20, ProjectID: "github.com/acme/widget",
		PublicTrustKey:         publictrust.EncodePublicKey(publicKey),
		PublicPrivateKey:       publictrust.EncodePrivateKey(privateKey),
		PublicBuildWorkerToken: "worker-token", PublicCollectorToken: "collector-token",
		PublicBuildRepositories: []string{"https://github.com/acme/widget"},
		PublicBuildApprovedRefs: []string{"refs/heads/main"}, PublicBuildRecipeDigests: []string{recipe},
		GitHubAPIURL: githubAPI.URL,
		LocalToken:   "consumer-token", CompatibilityID: "linux-amd64-schema1",
		ActionsRepository: "acme/widget", ActionsRef: "refs/heads/main",
		ActionsDefaultRef: "refs/heads/main", BuildkitBuilder: "layercache-test",
	}
	instance, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	ambiguousRegistry := &commitThenFailPublicationRegistry{
		publicationRegistry: instance.publications,
		failNext:            true,
	}
	instance.publications = ambiguousRegistry
	endpoint := httptest.NewServer(instance.Handler())
	defer endpoint.Close()
	workerCannotPublish, err := http.NewRequest(http.MethodPost, endpoint.URL+"/v1/public/publish", bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	workerCannotPublish.Header.Set("Authorization", "Bearer worker-token")
	workerResponse, err := http.DefaultClient.Do(workerCannotPublish)
	if err != nil {
		t.Fatal(err)
	}
	workerResponse.Body.Close()
	if workerResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("worker publication status = %d, want 401", workerResponse.StatusCode)
	}
	collectorCannotLease, err := http.NewRequest(http.MethodPost, endpoint.URL+"/v1/public-build-worker/lease", bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	collectorCannotLease.Header.Set("Authorization", "Bearer collector-token")
	collectorResponse, err := http.DefaultClient.Do(collectorCannotLease)
	if err != nil {
		t.Fatal(err)
	}
	collectorResponse.Body.Close()
	if collectorResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("collector lease status = %d, want 401", collectorResponse.StatusCode)
	}
	requested, err := instance.publicBuilds.Request(context.Background(), publicbuild.BuildRequest{
		Repository: "https://github.com/acme/widget", Commit: "0123456789abcdef0123456789abcdef01234567",
		Integration: publicbuild.IntegrationTurbo, Target: "@acme/widget#test", RecipeDigest: recipe,
		Platform:  publicbuild.PlatformLinuxAMD64,
		Inputs:    []publicbuild.DeclaredInput{{Name: "compatibility", Value: "linux-amd64"}},
		Resources: publicbuild.Resources{CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 2 << 30, Timeout: time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := instance.publicBuilds.LeaseNext(context.Background(), &publicBuildCapabilityWorker{
		id: "worker", capabilities: publicbuild.WorkerCapabilities{
			Integrations: []publicbuild.Integration{publicbuild.IntegrationTurbo},
			Platforms:    []publicbuild.Platform{publicbuild.PlatformLinuxAMD64},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	publish := func(body []byte, repository string) *http.Response {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, endpoint.URL+"/v1/public/publish", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer collector-token")
		request.Header.Set("x-layercache-integration", "turbo")
		request.Header.Set("x-layercache-project", cfg.ProjectID)
		request.Header.Set("x-layercache-compatibility", "linux-amd64")
		request.Header.Set("x-layercache-native-key", "retry-valid-publication")
		request.Header.Set("x-layercache-repository", repository)
		request.Header.Set("x-layercache-commit", "0123456789abcdef0123456789abcdef01234567")
		request.Header.Set("x-layercache-recipe", recipe)
		request.Header.Set("x-layercache-target", "@acme/widget#test")
		request.Header.Set("x-layercache-platform", "linux/amd64")
		request.Header.Set("x-layercache-toolchain", "turbo@2.10.9")
		request.Header.Set("x-layercache-builder", "layercache-builder@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")
		request.Header.Set("x-layercache-builder-image-digest", "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
		request.Header.Set("x-layercache-build-id", requested.Build.ID)
		request.Header.Set("x-layercache-worker-id", lease.WorkerID)
		request.Header.Set("x-layercache-lease-token", lease.Token)
		request.Header.Set("x-layercache-duration", "1200")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	rejected := publish([]byte("invalid-first-bytes"), "")
	rejected.Body.Close()
	if rejected.StatusCode != http.StatusConflict {
		t.Fatalf("invalid publication status = %d, want 409", rejected.StatusCode)
	}
	liveLeaseToken := lease.Token
	lease.Token = "replaced-or-expired-lease"
	stale := publish([]byte("stale-worker-bytes"), "https://github.com/acme/widget")
	stale.Body.Close()
	if stale.StatusCode != http.StatusConflict {
		t.Fatalf("stale lease publication status = %d, want 409", stale.StatusCode)
	}
	lease.Token = liveLeaseToken
	accepted := publish([]byte("valid-replacement-bytes"), "https://github.com/acme/widget")
	accepted.Body.Close()
	if accepted.StatusCode != http.StatusCreated {
		t.Fatalf("valid replacement status = %d, want 201", accepted.StatusCode)
	}
	if !ambiguousRegistry.failedAfterCommit {
		t.Fatal("publication registry did not exercise the lost commit response path")
	}
	divergent := publish([]byte("different-trusted-rebuild"), "https://github.com/acme/widget")
	divergent.Body.Close()
	if divergent.StatusCode != http.StatusConflict {
		t.Fatalf("divergent publication status = %d, want 409", divergent.StatusCode)
	}
	coordinate := publictrust.CacheCoordinate{
		Integration: "turbo", Project: cfg.ProjectID,
		Compatibility: "linux-amd64", NativeKey: "retry-valid-publication",
	}
	publication, err := instance.publications.Resolve(context.Background(), coordinate.Identity())
	if err != nil {
		t.Fatalf("valid completed publication was poisoned by a mismatched retry: %v", err)
	}
	wantDigest := sha256.Sum256([]byte("valid-replacement-bytes"))
	if publication.Digest != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("publication digest = %s after mismatched retry", publication.Digest)
	}
	entry, file, err := instance.store.Get(context.Background(), artifact.Key{
		Integration: coordinate.Integration, Project: coordinate.Project,
		Compatibility: coordinate.Compatibility, Native: coordinate.NativeKey,
	})
	if err != nil {
		t.Fatalf("valid completed artifact was removed by a mismatched retry: %v", err)
	}
	defer file.Close()
	if entry.Digest != publication.Digest {
		t.Fatalf("stored digest = %s, publication digest = %s", entry.Digest, publication.Digest)
	}
}

type commitThenFailPublicationRegistry struct {
	publicationRegistry
	failNext          bool
	failedAfterCommit bool
}

func (registry *commitThenFailPublicationRegistry) Publish(ctx context.Context, publication publictrust.Publication) error {
	if err := registry.publicationRegistry.Publish(ctx, publication); err != nil {
		return err
	}
	if registry.failNext {
		registry.failNext = false
		registry.failedAfterCommit = true
		return errors.New("publication commit response was lost")
	}
	return nil
}

type failBeforeCommitPublicationRegistry struct {
	publicationRegistry
}

func (registry *failBeforeCommitPublicationRegistry) Publish(context.Context, publictrust.Publication) error {
	return errors.New("publication registry unavailable before commit")
}

func TestDefinitePublicationRegistryMissReleasesPinnedArtifact(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	const (
		project       = "github.com/acme/widget"
		repository    = "https://github.com/acme/widget"
		compatibility = "linux-amd64-schema1"
		nativeKey     = "definite-registry-miss"
	)
	recipe := "sha256:" + strings.Repeat("b", 64)
	resources := publicbuild.Resources{
		CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 2 << 30, Timeout: time.Minute,
	}
	coordinator, err := publicbuild.OpenSQLiteCoordinator(t.TempDir()+"/public-builds.db", publicbuild.Config{
		AllowlistedRepositories: []string{repository}, Limits: resources,
		SanitizeLog: func(message string) string { return message },
		SourcePolicy: publicbuild.SourcePolicyFunc(func(context.Context, string, string) error {
			return nil
		}),
		RecipePolicy: publicbuild.RecipePolicyFunc(func(context.Context, publicbuild.Integration, string, string) error {
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	requested, err := coordinator.Request(ctx, publicbuild.BuildRequest{
		Repository: repository, Commit: strings.Repeat("a", 40),
		Integration: publicbuild.IntegrationTurbo, Target: "@acme/widget#test",
		RecipeDigest: recipe, Platform: publicbuild.PlatformLinuxAMD64,
		Inputs:    []publicbuild.DeclaredInput{{Name: "compatibility", Value: compatibility}},
		Resources: resources,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := coordinator.LeaseNext(ctx, &publicBuildCapabilityWorker{
		id: "worker", capabilities: publicbuild.WorkerCapabilities{
			Integrations: []publicbuild.Integration{publicbuild.IntegrationTurbo},
			Platforms:    []publicbuild.Platform{publicbuild.PlatformLinuxAMD64},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.Open(ctx, t.TempDir(), 1<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	registry, err := publictrust.OpenRegistry(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	instance := &Server{
		config: config.Config{ProjectID: project, MaxBytes: 1 << 20},
		store:  store, localStore: store, publicBuilds: coordinator,
		publications: &failBeforeCommitPublicationRegistry{publicationRegistry: registry},
	}
	body := []byte("complete-but-unpublished")
	request := httptest.NewRequest(http.MethodPost, "/v1/public/publish", bytes.NewReader(body))
	request.Header.Set("x-layercache-integration", "turbo")
	request.Header.Set("x-layercache-project", project)
	request.Header.Set("x-layercache-compatibility", compatibility)
	request.Header.Set("x-layercache-native-key", nativeKey)
	request.Header.Set("x-layercache-repository", repository)
	request.Header.Set("x-layercache-commit", strings.Repeat("a", 40))
	request.Header.Set("x-layercache-recipe", recipe)
	request.Header.Set("x-layercache-target", "@acme/widget#test")
	request.Header.Set("x-layercache-platform", "linux/amd64")
	request.Header.Set("x-layercache-toolchain", "turbo@2.10.12")
	request.Header.Set("x-layercache-builder", "layercache-turbo-builder-v1")
	request.Header.Set("x-layercache-builder-image-digest", "sha256:"+strings.Repeat("e", 64))
	request.Header.Set("x-layercache-build-id", requested.Build.ID)
	request.Header.Set("x-layercache-worker-id", lease.WorkerID)
	request.Header.Set("x-layercache-lease-token", lease.Token)
	request.Header.Set("x-layercache-duration", "1200")
	response := httptest.NewRecorder()
	instance.publishPublic(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("publication status = %d, want 500", response.Code)
	}
	key := artifact.Key{
		Integration: "turbo", Project: project, Compatibility: compatibility, Native: nativeKey,
	}
	if _, err := store.Head(ctx, key); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("definitively unpublished artifact remained pinned: %v", err)
	}
}

func TestPublicBuildPublicationHeadersRequireCanonicalActionsNativeKey(t *testing.T) {
	const target = ".github/workflows/public-cache.yml#public-cache"
	recipe, err := publicbuild.MaintainedRecipeDigest(publicbuild.IntegrationActions, target)
	if err != nil {
		t.Fatal(err)
	}
	request := publicbuild.BuildRequest{
		Repository:   "https://github.com/acme/widget",
		Commit:       strings.Repeat("a", 40),
		Integration:  publicbuild.IntegrationActions,
		Target:       target,
		RecipeDigest: recipe,
		Platform:     publicbuild.PlatformLinuxAMD64,
		Inputs: []publicbuild.DeclaredInput{
			{Name: "actions.key", Value: "pnpm-linux"},
			{Name: "actions.ref", Value: "refs/heads/main"},
			{Name: "actions.version", Value: strings.Repeat("b", 64)},
			{Name: "compatibility", Value: "linux-amd64-schema1"},
		},
	}
	cfg := config.Config{
		ProjectID: "github.com/acme/widget", ActionsRepository: "acme/widget",
		ActionsPublicRecipeDigest: recipe, ActionsPublicBuilder: "layercache-actions-builder-v1",
	}
	nativeKey, err := publicbuild.ActionsPublicNativeKey(request, actionsPublicToolchain, cfg.ActionsPublicBuilder)
	if err != nil {
		t.Fatal(err)
	}
	header := http.Header{}
	header.Set("x-layercache-build-id", "build-1")
	header.Set("x-layercache-repository", request.Repository)
	header.Set("x-layercache-commit", request.Commit)
	header.Set("x-layercache-recipe", request.RecipeDigest)
	header.Set("x-layercache-target", request.Target)
	header.Set("x-layercache-platform", string(request.Platform))
	header.Set("x-layercache-toolchain", actionsPublicToolchain)
	header.Set("x-layercache-builder", cfg.ActionsPublicBuilder)
	header.Set("x-layercache-builder-image-digest", "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	build := publicbuild.Build{ID: "build-1", Request: request}
	key := artifact.Key{
		Integration: "actions", Project: "acme/widget",
		Compatibility: "linux-amd64-schema1", Native: nativeKey,
	}
	if !publicBuildPublicationHeadersMatch(cfg, build, key, header) {
		t.Fatal("canonical Actions native publication headers were rejected")
	}
	key.Native = "sha256:" + strings.Repeat("c", 64)
	if publicBuildPublicationHeadersMatch(cfg, build, key, header) {
		t.Fatal("arbitrary Actions native key was accepted")
	}
	key.Native = nativeKey
	header.Set("x-layercache-toolchain", "actions/cache@unexpected")
	if publicBuildPublicationHeadersMatch(cfg, build, key, header) {
		t.Fatal("unconfigured Actions toolchain was accepted")
	}
}

func TestPublicBuilderImagePolicyPinsConfiguredGuestAssets(t *testing.T) {
	t.Parallel()

	components := []string{
		"sha256:" + strings.Repeat("a", 64),
		"sha256:" + strings.Repeat("b", 64),
		"sha256:" + strings.Repeat("c", 64),
	}
	digest, err := publicbuild.BuilderImageDigest(components[0], components[1], components[2])
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		PublicBuildKernelSHA256: components[0], PublicBuildRootFSSHA256: components[1],
		PublicBuildContractSHA256: components[2],
	}
	if !publicBuilderImageMatchesConfig(cfg, digest) {
		t.Fatal("configured builder image digest was rejected")
	}
	if publicBuilderImageMatchesConfig(cfg, "sha256:"+strings.Repeat("d", 64)) {
		t.Fatal("unreviewed builder image digest was accepted")
	}
}

func TestPublicBuildViewExposesReadyBuildKitImportSelector(t *testing.T) {
	request := publicbuild.BuildRequest{
		Repository: "https://github.com/acme/widget", Commit: strings.Repeat("a", 40),
		Integration: publicbuild.IntegrationBuildKit, Target: "release",
		RecipeDigest: "sha256:" + strings.Repeat("b", 64), Platform: publicbuild.PlatformLinuxAMD64,
		Inputs: []publicbuild.DeclaredInput{{Name: "dockerfile", Value: "Dockerfile"}},
	}
	nativeKey, err := publicbuild.BuildKitPublicNativeKey(request)
	if err != nil {
		t.Fatal(err)
	}
	const publicationIdentity = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	build := publicbuild.Build{
		ID: "buildkit-public-build", Request: request, State: publicbuild.StateSucceeded,
		Publication: &publicbuild.Publication{Outputs: []publicbuild.OutputDescriptor{{
			Name: publicationIdentity, Digest: "sha256:" + strings.Repeat("d", 64),
			SizeBytes: 1234, MediaType: "application/vnd.oci.image.manifest.v1+json",
		}}},
	}
	view := (&Server{}).publicBuildView(build)
	if view.Publication == nil || view.Publication.NativeKey != nativeKey ||
		view.Publication.PublicImportSelector != nativeKey+"="+publicationIdentity {
		t.Fatalf("BuildKit publication handoff = %#v", view.Publication)
	}
}
