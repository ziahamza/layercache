package acceptance_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/config"
)

const (
	actionsPublicToolchainFixture = "actions/cache@6.2.0"
	actionsPublicBuilderFixture   = "layercache-public-builder-v1"
	actionsPublicTargetFixture    = ".github/workflows/public-cache.yml#public-cache"
)

func TestGitHubActionsPublicCacheVerifiesWarmsAndHonorsRevocation(t *testing.T) {
	const key = "pnpm-public"
	const version = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	recipe := publicFixtureRecipe("actions", actionsPublicTargetFixture)
	root := t.TempDir()
	binary := buildLayerCache(t)

	publicAddress := availableAddress(t)
	publicConfig := filepath.Join(root, "public.json")
	githubAPI := acceptingGitHubAPI(t)
	runLayerCache(t,
		"setup", "--config", publicConfig,
		"--data-dir", filepath.Join(root, "public-cache"),
		"--listen", publicAddress,
		"--role", "public",
		"--project", "acme/widget",
		"--compatibility-id", "linux-amd64-public-fixture-v1",
		"--actions-repository", "acme/widget",
		"--actions-ref", "refs/heads/main",
		"--actions-default-ref", "refs/heads/main",
		"--github-api-url", githubAPI,
		"--public-build-repository", publicFixtureRepository,
		"--public-build-approved-ref", "refs/heads/main",
		"--public-build-recipe", recipe,
		"--actions-public-recipe", recipe,
		"--actions-public-builder", actionsPublicBuilderFixture,
		"--max-size", "10485760",
		"--non-interactive", "--json",
	)
	publicRuntimeConfig, err := config.Load(publicConfig)
	if err != nil {
		t.Fatal(err)
	}
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
		"--compatibility-id", "linux-amd64-public-fixture-v1",
		"--actions-repository", "acme/widget",
		"--actions-ref", "refs/heads/main",
		"--actions-default-ref", "refs/heads/main",
		"--actions-public-recipe", recipe,
		"--actions-public-builder", actionsPublicBuilderFixture,
		"--public-url", "http://"+publicAddress,
		"--public-trust-key", trust.PublicKey,
		"--max-size", "10485760",
		"--non-interactive", "--json",
	)
	hostConfig, err := config.Load(hostConfigPath)
	if err != nil {
		t.Fatal(err)
	}

	nativeKey := actionsPublicNativeKeyFixture(
		hostConfig.ActionsRepository,
		hostConfig.ActionsRef,
		key,
		version,
		hostConfig.CompatibilityID,
		publicFixtureCommit,
		recipe,
		"linux/amd64",
		actionsPublicToolchainFixture,
		actionsPublicBuilderFixture,
	)
	want := safeActionsArchiveFixture(t)
	buildID, workerID, leaseToken := requestAndLeasePublicBuild(
		t, publicConfig, "actions", actionsPublicTargetFixture, "linux/amd64",
		"actions.key="+key,
		"actions.ref="+hostConfig.ActionsRef,
		"actions.version="+version,
	)
	publishActionsPublicFixture(
		t, publicAddress, publicRuntimeConfig.PublicCollectorToken, hostConfig, nativeKey, actionsPublicTargetFixture,
		buildID, workerID, leaseToken, want,
	)

	connection := actionsConnectionResult{
		CacheURL: "http://" + hostAddress + "/",
		Token:    actionsPublicCapabilityFixture(t, hostConfig),
	}
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
	revokeActionsPublicFixture(t, publicAddress, publicRuntimeConfig.LocalToken, hostConfig, nativeKey)
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
	target string,
	buildID string,
	workerID string,
	leaseToken string,
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
	request.Header.Set("x-layercache-repository", publicFixtureRepository)
	request.Header.Set("x-layercache-commit", publicFixtureCommit)
	request.Header.Set("x-layercache-recipe", publicFixtureRecipe("actions", target))
	request.Header.Set("x-layercache-target", target)
	request.Header.Set("x-layercache-platform", "linux/amd64")
	request.Header.Set("x-layercache-toolchain", actionsPublicToolchainFixture)
	request.Header.Set("x-layercache-builder", actionsPublicBuilderFixture)
	request.Header.Set("x-layercache-builder-image-digest", "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	request.Header.Set("x-layercache-build-id", buildID)
	request.Header.Set("x-layercache-worker-id", workerID)
	request.Header.Set("x-layercache-lease-token", leaseToken)
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

func actionsPublicNativeKeyFixture(
	repository, ref, key, version, compatibility, commit, recipe, platform, toolchain, builder string,
) string {
	hasher := sha256.New()
	for _, value := range []string{
		"layercache/actions-cache/public-identity/v1",
		repository,
		ref,
		key,
		version,
		compatibility,
		commit,
		recipe,
		platform,
		toolchain,
		builder,
	} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		hasher.Write(length[:])
		hasher.Write([]byte(value))
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil))
}

func actionsPublicCapabilityFixture(t *testing.T, cfg config.Config) string {
	t.Helper()
	now := time.Now().UTC()
	token, err := access.MintCapabilityToken(cfg.LocalToken, access.Claims{
		Subject:       "acceptance-public-actions",
		Project:       cfg.ProjectID,
		Integration:   "actions",
		Compatibility: cfg.CompatibilityID,
		Repository:    cfg.ActionsRepository,
		Ref:           cfg.ActionsRef,
		DefaultRef:    cfg.ActionsDefaultRef,
		SourceCommit:  publicFixtureCommit,
		RecipeDigest:  cfg.ActionsPublicRecipeDigest,
		Target:        actionsPublicTargetFixture,
		Platform:      "linux/amd64",
		Toolchain:     actionsPublicToolchainFixture,
		Builder:       actionsPublicBuilderFixture,
		Capabilities:  []access.Capability{access.CapabilityRead},
		ExpiresAt:     now.Add(time.Hour),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func safeActionsArchiveFixture(t *testing.T) []byte {
	t.Helper()
	var archive bytes.Buffer
	compressed := gzip.NewWriter(&archive)
	tape := tar.NewWriter(compressed)
	contents := []byte("Actions archive produced by a Public Build")
	if err := tape.WriteHeader(&tar.Header{
		Name: "cache/payload.txt", Mode: 0o600, Size: int64(len(contents)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tape.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := tape.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}
