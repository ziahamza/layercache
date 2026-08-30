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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/config"
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
		Platform:     "linux/amd64", Toolchain: "turbo@2.10.9",
		Builder: "layercache-builder@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		Digest:  hex.EncodeToString(digest[:]), Size: int64(len(want) + 1),
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

	_, file, _, err := instance.resolveTurbo(context.Background(), key)
	if file != nil {
		_ = file.Close()
	}
	if !errors.Is(err, artifact.ErrCorrupt) {
		t.Fatalf("signed size mismatch error = %v, want ErrCorrupt", err)
	}
	if _, err := instance.store.Head(context.Background(), key); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("mismatched public artifact remained in Local Cache: %v", err)
	}
}

func TestRejectedPublicPublicationDoesNotClaimCacheIdentity(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Version: 1, Role: "public", DataDir: t.TempDir(), Listen: "127.0.0.1:7437",
		MaxBytes: 1 << 20, ProjectID: "github.com/acme/widget",
		PublicTrustKey:   publictrust.EncodePublicKey(publicKey),
		PublicPrivateKey: publictrust.EncodePrivateKey(privateKey), PublisherToken: "publisher-token",
		LocalToken: "consumer-token", CompatibilityID: "linux-amd64-schema1",
		ActionsRepository: "acme/widget", ActionsRef: "refs/heads/main",
		ActionsDefaultRef: "refs/heads/main", BuildkitBuilder: "layercache-test",
	}
	instance, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	endpoint := httptest.NewServer(instance.Handler())
	defer endpoint.Close()

	publish := func(body []byte, repository string) *http.Response {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, endpoint.URL+"/v1/public/publish", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer publisher-token")
		request.Header.Set("x-layercache-integration", "turbo")
		request.Header.Set("x-layercache-project", cfg.ProjectID)
		request.Header.Set("x-layercache-compatibility", cfg.CompatibilityID)
		request.Header.Set("x-layercache-native-key", "retry-valid-publication")
		request.Header.Set("x-layercache-repository", repository)
		request.Header.Set("x-layercache-commit", "0123456789abcdef0123456789abcdef01234567")
		request.Header.Set("x-layercache-recipe", "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
		request.Header.Set("x-layercache-platform", "linux/amd64")
		request.Header.Set("x-layercache-toolchain", "turbo@2.10.9")
		request.Header.Set("x-layercache-builder", "layercache-builder@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")
		request.Header.Set("x-layercache-build-id", "public-build-1")
		request.Header.Set("x-layercache-duration", "1200")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	rejected := publish([]byte("invalid-first-bytes"), "")
	rejected.Body.Close()
	if rejected.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid publication status = %d, want 400", rejected.StatusCode)
	}
	accepted := publish([]byte("valid-replacement-bytes"), "https://github.com/acme/widget")
	accepted.Body.Close()
	if accepted.StatusCode != http.StatusCreated {
		t.Fatalf("valid replacement status = %d, want 201", accepted.StatusCode)
	}
}
