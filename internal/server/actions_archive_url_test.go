package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/layercache/layercache/internal/config"
)

func TestActionsLookupUsesConfiguredExternalArchiveBaseURL(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		Version: 1, Role: "team", DataDir: t.TempDir(), Listen: "127.0.0.1:7437",
		MaxBytes: 1 << 20, ProjectID: "github.com/acme/widget", LocalToken: "team-token",
		CompatibilityID: "linux-amd64-schema1", ActionsRepository: "acme/widget",
		ActionsRef: "refs/heads/main", ActionsDefaultRef: "refs/heads/main",
		ActionsArchiveBaseURL: "https://cache.example.com",
		BuildkitBuilder:       "layercache-test",
	}
	instance, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	endpoint := httptest.NewServer(instance.Handler())
	t.Cleanup(endpoint.Close)

	reserve := authorizedActionsRequest(t, cfg, http.MethodPost, endpoint.URL+"/_apis/artifactcache/caches",
		strings.NewReader(`{"key":"external-url","version":"v1","cacheSize":7}`))
	reserve.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(reserve)
	if err != nil {
		t.Fatal(err)
	}
	var reservation struct {
		CacheID int64 `json:"cacheId"`
	}
	if err := json.NewDecoder(response.Body).Decode(&reservation); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("reserve status = %d", response.StatusCode)
	}

	uploadURL := fmt.Sprintf("%s/_apis/artifactcache/caches/%d", endpoint.URL, reservation.CacheID)
	upload := authorizedActionsRequest(t, cfg, http.MethodPatch, uploadURL, bytes.NewReader([]byte("archive")))
	upload.Header.Set("Content-Range", "bytes 0-6/*")
	response, err = http.DefaultClient.Do(upload)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("upload status = %d", response.StatusCode)
	}

	commit := authorizedActionsRequest(t, cfg, http.MethodPost, uploadURL, strings.NewReader(`{"size":7}`))
	commit.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(commit)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("commit status = %d", response.StatusCode)
	}

	lookup := authorizedActionsRequest(t, cfg, http.MethodGet,
		endpoint.URL+"/_apis/artifactcache/cache?keys=external-url&version=v1", nil)
	response, err = http.DefaultClient.Do(lookup)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		ArchiveLocation string `json:"archiveLocation"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	wantPrefix := cfg.ActionsArchiveBaseURL + "/_layercache/compatibility/" + cfg.CompatibilityID + "/_apis/"
	if !strings.HasPrefix(result.ArchiveLocation, wantPrefix) {
		t.Fatalf("archiveLocation = %q, want prefix %q", result.ArchiveLocation, wantPrefix)
	}
}

func authorizedActionsRequest(t *testing.T, cfg config.Config, method, target string, body io.Reader) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+cfg.LocalToken)
	if cfg.Role == "team" {
		request.Header.Set("X-LayerCache-Compatibility", cfg.CompatibilityID)
	}
	return request
}
