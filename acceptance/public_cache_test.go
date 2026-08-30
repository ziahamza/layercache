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
	runLayerCache(t,
		"setup", "--config", publicConfig,
		"--data-dir", filepath.Join(root, "public-cache"),
		"--listen", publicAddress,
		"--role", "public",
		"--project", "github.com/acme/widget",
		"--publisher-token", "publisher-secret",
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
		"--repository", "https://github.com/acme/widget",
		"--commit", "0123456789abcdef0123456789abcdef01234567",
		"--recipe", "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		"--platform", "linux/amd64",
		"--toolchain", "turbo@2.10.9",
		"--builder", "layercache-builder@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		"--build-id", "build-123",
		"--duration", "9500",
		"--json",
	)

	var publicConnection turboConnection
	if err := json.Unmarshal(runLayerCache(t, "integration", "turbo", "--config", publicConfig, "--json"), &publicConnection); err != nil {
		t.Fatal(err)
	}
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
	var host turboConnection
	if err := json.Unmarshal(runLayerCache(t, "integration", "turbo", "--config", hostConfig, "--json"), &host); err != nil {
		t.Fatal(err)
	}
	hostServer := startLayerCache(t, binary, hostConfig, hostAddress)
	defer hostServer.stop(t)

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
