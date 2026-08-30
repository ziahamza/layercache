package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/layercache/layercache/internal/config"
)

func TestActionsHandlerUsesConfiguredLocalCacheArtifactLimit(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		Version: 1, Role: "local", DataDir: t.TempDir(), Listen: "127.0.0.1:7437",
		MaxBytes: 6, ProjectID: "github.com/acme/widget", LocalToken: "local-token",
		CompatibilityID: "linux-amd64-schema1", ActionsRepository: "acme/widget",
		ActionsRef: "refs/heads/main", ActionsDefaultRef: "refs/heads/main",
		BuildkitBuilder: "layercache-test",
	}
	instance, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	endpoint := httptest.NewServer(instance.Handler())
	t.Cleanup(endpoint.Close)

	request, err := http.NewRequest(
		http.MethodPost,
		endpoint.URL+"/_apis/artifactcache/caches",
		strings.NewReader(`{"key":"too-large","version":"v1","cacheSize":7}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+cfg.LocalToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversize reserve status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
}
