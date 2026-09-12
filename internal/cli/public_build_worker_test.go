package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/publicbuild"
	"github.com/layercache/layercache/internal/publicbuild/sandbox"
	"github.com/layercache/layercache/internal/publictrust"
)

func TestQEMUSystemBinaryUsesDistributionArchitectureNames(t *testing.T) {
	t.Parallel()
	for architecture, want := range map[string]string{
		"amd64": "qemu-system-x86_64",
		"arm64": "qemu-system-aarch64",
	} {
		if got := qemuSystemBinary(architecture); got != want {
			t.Fatalf("qemuSystemBinary(%q) = %q, want %q", architecture, got, want)
		}
	}
}

func TestReconcileCommittedPublicBuildAfterLostPublishResponse(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	publication := publictrust.Publication{
		Integration: "turbo", Project: "github.com/acme/widget",
		Compatibility: "linux-amd64-schema1", NativeKey: "native-key",
		Repository: "https://github.com/acme/widget", Commit: strings.Repeat("a", 40),
		RecipeDigest: "sha256:" + strings.Repeat("b", 64), Target: "@acme/widget#build",
		Platform: "linux/amd64", Inputs: []publictrust.DeclaredInput{{Name: "compatibility", Value: "linux-amd64-schema1"}},
		Toolchain: "turbo@2.10.12", Builder: "layercache-turbo-builder-v1",
		BuilderImageDigest: "sha256:" + strings.Repeat("c", 64),
		Digest:             strings.Repeat("d", 64), Size: 42, DurationMS: 100,
		BuildID: "build-1", IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	}
	envelope, err := publictrust.Sign(privateKey, publication)
	if err != nil {
		t.Fatal(err)
	}
	size := publication.Size
	expected := trustedPublicationResult{
		Identity: publication.Identity(), NativeKey: publication.NativeKey,
		Digest: "sha256:" + publication.Digest, SizeBytes: publication.Size,
		Coordinate: publication.Coordinate(),
		Verification: publictrust.Expected{
			Integration: publication.Integration, Project: publication.Project,
			Compatibility: publication.Compatibility, NativeKey: publication.NativeKey,
			PublicIdentity: publication.Identity(), Digest: publication.Digest, Size: &size,
			BuilderImageDigest: publication.BuilderImageDigest, BuildID: publication.BuildID,
		},
	}
	resolveAvailable := true
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v1/public-build-worker/build-1" &&
			request.Header.Get("Authorization") == "Bearer worker-token":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"state": "succeeded",
				"publication": map[string]any{
					"publicCachePublication": expected.Identity,
					"nativeKey":              expected.NativeKey,
					"digest":                 expected.Digest,
					"sizeBytes":              expected.SizeBytes,
				},
			})
		case request.Method == http.MethodPost && request.URL.Path == "/v1/public/resolve":
			if !resolveAvailable {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"envelope": envelope})
		default:
			http.Error(writer, "unexpected reconciliation request", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	reconciled, err := reconcileCommittedPublicBuild(context.Background(), config.Config{
		PublicURL: server.URL, PublicBuildWorkerToken: "worker-token",
		PublicTrustKey: publictrust.EncodePublicKey(publicKey),
	}, "build-1", expected)
	if err != nil || !reconciled {
		t.Fatalf("reconcile committed publication = %v, %v", reconciled, err)
	}
	resolveAvailable = false
	reconciled, err = reconcileCommittedPublicBuild(context.Background(), config.Config{
		PublicURL: server.URL, PublicBuildWorkerToken: "worker-token",
		PublicTrustKey: publictrust.EncodePublicKey(publicKey),
	}, "build-1", expected)
	if err == nil || reconciled {
		t.Fatalf("missing registry publication reconciled as success = %v, %v", reconciled, err)
	}
}

func TestWorkerAndCollectorConfigDoNotRequireClientAuthority(t *testing.T) {
	cfg, err := config.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Role = "local"
	cfg.PublicURL = "https://public-cache.example"
	cfg.PublicTrustKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	cfg.TeamToken = ""
	cfg.PublicAccessToken = ""
	cfg.PublicBuildWorkerToken = "worker-only"
	cfg.PublicCollectorToken = "collector-only"
	path := filepath.Join(t.TempDir(), "worker.json")
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPublicBuildWorkerConfig(path); err != nil {
		t.Fatalf("load worker-only Public Build config: %v", err)
	}
	if _, err := loadPublicBuildCollectorConfig(path); err != nil {
		t.Fatalf("load collector-only Public Build config: %v", err)
	}
	if _, err := loadPublicBuildConfig(context.Background(), path); err == nil {
		t.Fatal("client Public Build command accepted a config without client authority")
	}
}

func TestTrustedCollectorRefusesOpaqueBuildKitOutput(t *testing.T) {
	t.Parallel()
	_, err := publishCollectedPublicBuild(
		context.Background(),
		config.Config{ProjectID: "github.com/acme/widget"},
		sandbox.GuestContract{},
		"sha256:"+strings.Repeat("a", 64),
		publicBuildWorkerLease{Build: publicbuild.Build{Request: publicbuild.BuildRequest{
			Integration: publicbuild.IntegrationBuildKit,
			Platform:    publicbuild.PlatformLinuxAMD64,
			Repository:  "https://github.com/acme/widget",
		}}},
		sandbox.CollectedOutput{},
		0,
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "open collected Public Build output") {
		t.Fatalf("BuildKit collector error = %v", err)
	}
}
