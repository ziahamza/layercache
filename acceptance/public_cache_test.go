package acceptance_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

type publicTrustResult struct {
	PublicKey string `json:"publicKey"`
}

func TestVerifiedPublicCacheWarmsLocalAndRejectsClientWrites(t *testing.T) {
	root := t.TempDir()
	binary := buildLayerCache(t)
	publicAddress := availableAddress(t)
	publicConfig := filepath.Join(root, "public.json")
	githubAPI := acceptingGitHubAPI(t)
	const target = "@acme/widget#turbo-cache"
	recipe := publicFixtureRecipe("turbo", target)
	runLayerCache(t,
		"setup", "--config", publicConfig,
		"--data-dir", filepath.Join(root, "public-cache"),
		"--listen", publicAddress,
		"--role", "public",
		"--project", "github.com/acme/widget",
		"--publisher-token", "publisher-secret",
		"--github-api-url", githubAPI,
		"--public-build-repository", publicFixtureRepository,
		"--public-build-approved-ref", "refs/heads/main",
		"--public-build-recipe", recipe,
		"--max-size", "10485760",
		"--non-interactive", "--json",
	)
	var trust publicTrustResult
	if err := json.Unmarshal(runLayerCache(t, "public", "trust-key", "--config", publicConfig, "--json"), &trust); err != nil {
		t.Fatal(err)
	}
	if trust.PublicKey == "" {
		t.Fatal("public trust key is empty")
	}
	publicServer := startLayerCache(t, binary, publicConfig, publicAddress)
	buildID, workerID, leaseToken := requestAndLeasePublicBuild(t, publicConfig, "turbo", target, "linux/amd64")

	want := []byte("artifact-built-by-layercache-public-build")
	artifactFile := filepath.Join(root, "public-artifact.bin")
	if err := os.WriteFile(artifactFile, want, 0o600); err != nil {
		t.Fatal(err)
	}
	runLayerCache(t,
		"public", "publish",
		"--config", publicConfig,
		"--file", artifactFile,
		"--hash", "trusted-public-hash",
		"--repository", publicFixtureRepository,
		"--commit", publicFixtureCommit,
		"--recipe", recipe,
		"--target", target,
		"--platform", "linux/amd64",
		"--toolchain", "turbo@2.10.9",
		"--builder", "layercache-builder@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		"--builder-image-digest", "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		"--build-id", buildID,
		"--worker-id", workerID,
		"--lease-token", leaseToken,
		"--duration", "9500",
		"--json",
	)

	publicConnection := captureTurboConnection(t, binary, publicConfig)
	request, err := http.NewRequest(http.MethodPut, publicConnection.APIURL+"/v8/artifacts/trusted-public-hash", bytes.NewReader([]byte("client-poison")))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+publicConnection.Token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("client Public Cache PUT status = %d, want 405", response.StatusCode)
	}

	hostAddress := availableAddress(t)
	hostConfig := filepath.Join(root, "host.json")
	runLayerCache(t,
		"setup", "--config", hostConfig,
		"--data-dir", filepath.Join(root, "host-cache"),
		"--listen", hostAddress,
		"--project", "github.com/acme/widget",
		"--public-url", "http://"+publicAddress,
		"--public-trust-key", trust.PublicKey,
		"--max-size", "10485760",
		"--non-interactive", "--json",
	)
	hostServer := startLayerCache(t, binary, hostConfig, hostAddress)
	defer hostServer.stop(t)
	host := captureTurboConnection(t, binary, hostConfig)

	got, source := fetchTurboArtifact(t, host.APIURL+"/v8/artifacts/trusted-public-hash", host.Token)
	if !bytes.Equal(got, want) || source != "public" {
		t.Fatalf("first public restore got %q from %q, want %q from public", got, source, want)
	}

	publicServer.stop(t)
	got, source = fetchTurboArtifact(t, host.APIURL+"/v8/artifacts/trusted-public-hash", host.Token)
	if !bytes.Equal(got, want) || source != "local" {
		t.Fatalf("offline warmed restore got %q from %q, want %q from local", got, source, want)
	}

	publicServer = startLayerCache(t, binary, publicConfig, publicAddress)
	defer publicServer.stop(t)
	runLayerCache(t,
		"public", "revoke",
		"--config", publicConfig,
		"--hash", "trusted-public-hash",
		"--reason", "fixture revoked",
		"--json",
	)
	request, err = http.NewRequest(http.MethodGet, host.APIURL+"/v8/artifacts/trusted-public-hash", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+host.Token)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("warmed artifact after online revocation status = %d, want 404", response.StatusCode)
	}
}
