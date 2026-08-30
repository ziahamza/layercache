package acceptance_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
)

func TestGitHubActionsV1CacheSurvivesRuntimeRestart(t *testing.T) {
	root := t.TempDir()
	binary := buildLayerCache(t)
	address := availableAddress(t)
	configPath := filepath.Join(root, "config.json")
	runLayerCache(t,
		"setup", "--config", configPath,
		"--data-dir", filepath.Join(root, "cache"),
		"--listen", address,
		"--project", "github.com/acme/widget",
		"--actions-repository", "acme/widget",
		"--actions-ref", "refs/heads/feature",
		"--actions-default-ref", "refs/heads/main",
		"--max-size", "10485760",
		"--non-interactive", "--json",
	)
	var connection struct {
		CacheURL string `json:"cacheUrl"`
		Token    string `json:"token"`
	}
	if err := json.Unmarshal(runLayerCache(t, "integration", "actions", "--config", configPath, "--json"), &connection); err != nil {
		t.Fatal(err)
	}
	server := startLayerCache(t, binary, configPath, address)

	want := []byte("github-actions-tar-zstd-archive")
	reserveBody := fmt.Sprintf(`{"key":"pnpm-linux-feature-abc","version":"paths-v1","cacheSize":%d}`, len(want))
	response := actionsRequest(t, http.MethodPost, connection.CacheURL+"_apis/artifactcache/caches", connection.Token, []byte(reserveBody), "")
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("reserve status = %d, want 201: %s", response.StatusCode, body)
	}
	var reservation struct {
		CacheID int64 `json:"cacheId"`
	}
	if err := json.NewDecoder(response.Body).Decode(&reservation); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	uploadURL := fmt.Sprintf("%s_apis/artifactcache/caches/%d", connection.CacheURL, reservation.CacheID)
	contentRange := fmt.Sprintf("bytes 0-%d/*", len(want)-1)
	response = actionsRequest(t, http.MethodPatch, uploadURL, connection.Token, want, contentRange)
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("upload status = %d, want 204", response.StatusCode)
	}
	response = actionsRequest(t, http.MethodPost, uploadURL, connection.Token, []byte(fmt.Sprintf(`{"size":%d}`, len(want))), "")
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("commit status = %d, want 204", response.StatusCode)
	}

	server.stop(t)
	server = startLayerCache(t, binary, configPath, address)
	defer server.stop(t)

	lookupURL := connection.CacheURL + "_apis/artifactcache/cache?keys=" + url.QueryEscape("pnpm-linux-feature-abc,pnpm-linux-") + "&version=paths-v1"
	response = actionsRequest(t, http.MethodGet, lookupURL, connection.Token, nil, "")
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("lookup status = %d, want 200: %s", response.StatusCode, body)
	}
	if response.Header.Get("X-LayerCache-Match") != "exact" {
		t.Fatalf("match = %q, want exact", response.Header.Get("X-LayerCache-Match"))
	}
	var hit struct {
		CacheKey        string `json:"cacheKey"`
		ArchiveLocation string `json:"archiveLocation"`
	}
	if err := json.NewDecoder(response.Body).Decode(&hit); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if hit.CacheKey != "pnpm-linux-feature-abc" {
		t.Fatalf("matched key = %q", hit.CacheKey)
	}
	response = actionsRequest(t, http.MethodGet, hit.ArchiveLocation, connection.Token, nil, "")
	got, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !bytes.Equal(got, want) {
		t.Fatalf("archive status/body = %d/%q, want 200/%q", response.StatusCode, got, want)
	}
}

func TestGitHubActionsV1TeamCacheWarmsFreshHost(t *testing.T) {
	root := t.TempDir()
	binary := buildLayerCache(t)
	teamAddress := availableAddress(t)
	teamConfig := filepath.Join(root, "team.json")
	runLayerCache(t,
		"setup", "--config", teamConfig,
		"--data-dir", filepath.Join(root, "team-cache"),
		"--listen", teamAddress,
		"--role", "team", "--project", "github.com/acme/widget",
		"--local-token", "team-actions-secret",
		"--actions-repository", "acme/widget",
		"--max-size", "10485760", "--non-interactive", "--json",
	)
	team := startLayerCache(t, binary, teamConfig, teamAddress)
	defer team.stop(t)

	hostAAddress := availableAddress(t)
	hostAConfig := filepath.Join(root, "host-a.json")
	runLayerCache(t,
		"setup", "--config", hostAConfig,
		"--data-dir", filepath.Join(root, "host-a-cache"),
		"--listen", hostAAddress,
		"--project", "github.com/acme/widget",
		"--actions-repository", "acme/widget",
		"--team-url", "http://"+teamAddress, "--team-token", "team-actions-secret",
		"--max-size", "10485760", "--non-interactive", "--json",
	)
	hostAToken := actionsConnection(t, hostAConfig).Token
	hostA := startLayerCache(t, binary, hostAConfig, hostAAddress)
	want := []byte("actions-team-cache-archive")
	putActionsArchive(t, "http://"+hostAAddress+"/", hostAToken, "pnpm-shared-team", "paths-v1", want)
	hostA.stop(t)

	hostBAddress := availableAddress(t)
	hostBConfig := filepath.Join(root, "host-b.json")
	runLayerCache(t,
		"setup", "--config", hostBConfig,
		"--data-dir", filepath.Join(root, "host-b-cache"),
		"--listen", hostBAddress,
		"--project", "github.com/acme/widget",
		"--actions-repository", "acme/widget",
		"--team-url", "http://"+teamAddress, "--team-token", "team-actions-secret",
		"--max-size", "10485760", "--non-interactive", "--json",
	)
	hostBToken := actionsConnection(t, hostBConfig).Token
	hostB := startLayerCache(t, binary, hostBConfig, hostBAddress)
	defer hostB.stop(t)

	got, source := getActionsArchive(t, "http://"+hostBAddress+"/", hostBToken, "pnpm-shared-team", "paths-v1")
	if !bytes.Equal(got, want) || source != "teamCache" {
		t.Fatalf("fresh host restored %q from %q", got, source)
	}
	team.stop(t)
	got, source = getActionsArchive(t, "http://"+hostBAddress+"/", hostBToken, "pnpm-shared-team", "paths-v1")
	if !bytes.Equal(got, want) || source != "localCache" {
		t.Fatalf("warmed host restored %q from %q", got, source)
	}
}

type actionsConnectionResult struct {
	CacheURL string `json:"cacheUrl"`
	Token    string `json:"token"`
}

func actionsConnection(t *testing.T, configPath string) actionsConnectionResult {
	t.Helper()
	var result actionsConnectionResult
	if err := json.Unmarshal(runLayerCache(t, "integration", "actions", "--config", configPath, "--json"), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func putActionsArchive(t *testing.T, cacheURL, token, key, version string, body []byte) {
	t.Helper()
	reserveBody := fmt.Sprintf(`{"key":%q,"version":%q,"cacheSize":%d}`, key, version, len(body))
	response := actionsRequest(t, http.MethodPost, cacheURL+"_apis/artifactcache/caches", token, []byte(reserveBody), "")
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		message, _ := io.ReadAll(response.Body)
		t.Fatalf("reserve status = %d: %s", response.StatusCode, message)
	}
	var reservation struct {
		CacheID int64 `json:"cacheId"`
	}
	if err := json.NewDecoder(response.Body).Decode(&reservation); err != nil {
		t.Fatal(err)
	}
	uploadURL := fmt.Sprintf("%s_apis/artifactcache/caches/%d", cacheURL, reservation.CacheID)
	response.Body.Close()
	response = actionsRequest(t, http.MethodPatch, uploadURL, token, body, fmt.Sprintf("bytes 0-%d/*", len(body)-1))
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("upload status = %d", response.StatusCode)
	}
	response = actionsRequest(t, http.MethodPost, uploadURL, token, []byte(fmt.Sprintf(`{"size":%d}`, len(body))), "")
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("commit status = %d", response.StatusCode)
	}
}

func getActionsArchive(t *testing.T, cacheURL, token, key, version string) ([]byte, string) {
	t.Helper()
	lookupURL := cacheURL + "_apis/artifactcache/cache?keys=" + url.QueryEscape(key) + "&version=" + url.QueryEscape(version)
	response := actionsRequest(t, http.MethodGet, lookupURL, token, nil, "")
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("lookup status = %d: %s", response.StatusCode, message)
	}
	source := response.Header.Get("X-LayerCache-Source")
	var hit struct {
		ArchiveLocation string `json:"archiveLocation"`
	}
	if err := json.NewDecoder(response.Body).Decode(&hit); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	// This intentionally sends no bearer token. Stock actions/cache v1 fetches
	// archiveLocation as a pre-authorized URL.
	response, err := http.Get(hit.ArchiveLocation)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("archive status = %d: %s", response.StatusCode, body)
	}
	return body, source
}

func actionsRequest(t *testing.T, method, requestURL, token string, body []byte, contentRange string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, requestURL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	if contentRange != "" {
		request.Header.Set("Content-Range", contentRange)
		request.Header.Set("Content-Type", "application/octet-stream")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
