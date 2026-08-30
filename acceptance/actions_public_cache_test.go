package acceptance_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/layercache/layercache/internal/config"
)

func TestGitHubActionsPublicCacheVerifiesWarmsAndHonorsRevocation(t *testing.T) {
	root := t.TempDir()
	binary := buildLayerCache(t)

	publicAddress := availableAddress(t)
	publicConfig := filepath.Join(root, "public.json")
	runLayerCache(t,
		"setup", "--config", publicConfig,
		"--data-dir", filepath.Join(root, "public-cache"),
		"--listen", publicAddress,
		"--role", "public",
		"--project", "acme/widget",
		"--publisher-token", "actions-public-publisher",
		"--max-size", "10485760",
		"--non-interactive", "--json",
	)
	var trust publicTrustResult
	if err := json.Unmarshal(runLayerCache(t, "public", "trust-key", "--config", publicConfig, "--json"), &trust); err != nil {
		t.Fatal(err)
	}
	publicServer := startLayerCache(t, binary, publicConfig, publicAddress)
	publicRunning := true
	defer func() {
		if publicRunning {
			publicServer.stop(t)
		}
	}()

	hostAddress := availableAddress(t)
	hostConfigPath := filepath.Join(root, "host.json")
	dataDir := filepath.Join(root, "host-cache")
	runLayerCache(t,
		"setup", "--config", hostConfigPath,
		"--data-dir", dataDir,
		"--listen", hostAddress,
		"--project", "github.com/acme/widget",
		"--actions-repository", "acme/widget",
		"--actions-ref", "refs/heads/main",
		"--actions-default-ref", "refs/heads/main",
		"--public-url", "http://"+publicAddress,
		"--public-trust-key", trust.PublicKey,
		"--max-size", "10485760",
		"--non-interactive", "--json",
	)
	hostConfig, err := config.Load(hostConfigPath)
	if err != nil {
		t.Fatal(err)
	}

	const key = "pnpm-public"
	const version = "paths-v1"
	nativeKey := actionsPublicNativeKeyFixture(
		hostConfig.ActionsRepository,
		hostConfig.ActionsRef,
		key,
		version,
		hostConfig.CompatibilityID,
	)
	want := []byte("Actions archive produced by a Public Build")
	publishActionsPublicFixture(t, publicAddress, "actions-public-publisher", hostConfig, nativeKey, want)

	connection := actionsConnection(t, hostConfigPath)
	host := startLayerCache(t, binary, hostConfigPath, hostAddress)
	hostRunning := true
	defer func() {
		if hostRunning {
			host.stop(t)
		}
	}()
	got, source := getActionsArchive(t, connection.CacheURL, connection.Token, key, version)
	if !bytes.Equal(got, want) || source != "publicCache" {
		t.Fatalf("first restore = %q from %q, want verified Public Cache archive", got, source)
	}
	host.stop(t)
	hostRunning = false
	publicServer.stop(t)
	publicRunning = false

	host = startLayerCache(t, binary, hostConfigPath, hostAddress)
	hostRunning = true
	got, source = getActionsArchive(t, connection.CacheURL, connection.Token, key, version)
	if !bytes.Equal(got, want) || source != "localCache" {
		t.Fatalf("offline restore = %q from %q, want warmed Local Cache archive", got, source)
	}

	publicServer = startLayerCache(t, binary, publicConfig, publicAddress)
	publicRunning = true
	revokeActionsPublicFixture(t, publicAddress, "actions-public-publisher", hostConfig, nativeKey)
	lookupURL := connection.CacheURL + "_apis/artifactcache/cache?keys=" + url.QueryEscape(key) + "&version=" + url.QueryEscape(version)
	response := actionsRequest(t, http.MethodGet, lookupURL, connection.Token, nil, "")
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("lookup after online revocation status = %d, want 204", response.StatusCode)
	}

	publicServer.stop(t)
	publicRunning = false
	response = actionsRequest(t, http.MethodGet, lookupURL, connection.Token, nil, "")
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("revoked archive remained in Local Cache, lookup status = %d", response.StatusCode)
	}

	host.stop(t)
	hostRunning = false
	entries, err := os.ReadDir(filepath.Join(dataDir, "actions-public-staging"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("Public Cache staging retained %d entries after shutdown", len(entries))
	}
}

func publishActionsPublicFixture(
	t *testing.T,
	address string,
	publisherToken string,
	cfg config.Config,
	nativeKey string,
	body []byte,
) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "http://"+address+"/v1/public/publish", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+publisherToken)
	request.Header.Set("x-layercache-integration", "actions")
	request.Header.Set("x-layercache-project", cfg.ActionsRepository)
	request.Header.Set("x-layercache-compatibility", cfg.CompatibilityID)
	request.Header.Set("x-layercache-native-key", nativeKey)
	request.Header.Set("x-layercache-repository", "https://github.com/acme/widget")
	request.Header.Set("x-layercache-commit", "0123456789abcdef0123456789abcdef01234567")
	request.Header.Set("x-layercache-recipe", "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	request.Header.Set("x-layercache-platform", "linux/amd64")
	request.Header.Set("x-layercache-toolchain", "actions/cache@v4")
	request.Header.Set("x-layercache-builder", "layercache-public-worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	request.Header.Set("x-layercache-build-id", "public-build-actions-1")
	request.Header.Set("x-layercache-duration", "1200")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		message, _ := io.ReadAll(response.Body)
		t.Fatalf("seed Actions Public Cache status = %d: %s", response.StatusCode, message)
	}
}

func revokeActionsPublicFixture(
	t *testing.T,
	address string,
	publisherToken string,
	cfg config.Config,
	nativeKey string,
) {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"integration": "actions", "project": cfg.ActionsRepository,
		"compatibility": cfg.CompatibilityID, "nativeKey": nativeKey,
		"reason": "Actions fixture revoked",
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, "http://"+address+"/v1/public/revoke", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+publisherToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		message, _ := io.ReadAll(response.Body)
		t.Fatalf("revoke Actions Public Cache status = %d: %s", response.StatusCode, message)
	}
}

func actionsPublicNativeKeyFixture(repository, ref, key, version, compatibility string) string {
	hasher := sha256.New()
	for _, value := range []string{
		"layercache/actions-cache/public-identity/v1",
		repository,
		ref,
		key,
		version,
		compatibility,
	} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		hasher.Write(length[:])
		hasher.Write([]byte(value))
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil))
}
