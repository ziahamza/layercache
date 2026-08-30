package remote_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/publictrust"
	"github.com/layercache/layercache/internal/remote"
)

func TestPublicClientRejectsArtifactRedirectToAnotherOrigin(t *testing.T) {
	t.Parallel()

	var attackerRequests atomic.Int64
	attacker := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attackerRequests.Add(1)
		_, _ = writer.Write([]byte("attacker-controlled bytes"))
	}))
	defer attacker.Close()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	expected := publictrust.Expected{
		Integration: "turbo", Project: "github.com/acme/widget",
		Compatibility: "linux-amd64-schema1", NativeKey: "task-hash",
	}
	bodyDigest := sha256.Sum256([]byte("attacker-controlled bytes"))
	publication := publictrust.Publication{
		Integration: expected.Integration, Project: expected.Project,
		Compatibility: expected.Compatibility, NativeKey: expected.NativeKey,
		Repository:   "https://github.com/acme/widget",
		Commit:       "0123456789abcdef0123456789abcdef01234567",
		RecipeDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		Platform:     "linux/amd64", Toolchain: "turbo@2.10.9",
		Builder: "builder@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		Digest:  hex.EncodeToString(bodyDigest[:]), Size: 25, DurationMS: 100,
		BuildID: "public-build-1", IssuedAt: time.Now().UTC().Add(-time.Minute),
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	envelope, err := publictrust.Sign(privateKey, publication)
	if err != nil {
		t.Fatal(err)
	}

	publicServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/public/resolve":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"envelope": envelope, "artifactUrl": "/artifact",
			})
		case "/artifact":
			http.Redirect(writer, request, attacker.URL+"/stolen", http.StatusFound)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer publicServer.Close()

	client, err := remote.NewPublicClient(publicServer.URL, publictrust.EncodePublicKey(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get(context.Background(), expected); err == nil {
		t.Fatal("Public Cache followed an artifact redirect to another origin")
	}
	if attackerRequests.Load() != 0 {
		t.Fatalf("attacker origin received %d requests, want none", attackerRequests.Load())
	}
}

func TestPublicClientRequiresHTTPSExceptOnLoopback(t *testing.T) {
	t.Parallel()
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.NewPublicClient("http://cache.example.test", publictrust.EncodePublicKey(publicKey)); err == nil {
		t.Fatal("insecure non-loopback Public Cache URL was accepted")
	}
}
