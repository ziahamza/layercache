package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/compatibility"
	"github.com/layercache/layercache/internal/config"
)

func gatewayTestConfig(t *testing.T, alias string) config.Config {
	t.Helper()
	cfg, err := config.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Role = "team"
	cfg.ProjectID = "github.com/acme/" + alias
	cfg.ActionsRepository = "acme/" + alias
	cfg.DataDir = t.TempDir()
	cfg.MaxBytes = 8 << 20
	cfg.TeamMembers = map[string]string{"alice": "writer", "reader": "reader"}
	return cfg
}

func gatewayFixture(t *testing.T, registry http.Handler) (*ProjectGateway, map[string]config.Config) {
	t.Helper()
	projects := map[string]config.Config{"alpha": gatewayTestConfig(t, "alpha"), "beta": gatewayTestConfig(t, "beta")}
	cfg := ProjectGatewayConfig{Origin: "https://cache.example", Projects: projects}
	if registry != nil {
		backend := httptest.NewServer(registry)
		t.Cleanup(backend.Close)
		cfg.RegistryURL, cfg.RegistryUsername, cfg.RegistryPassword = backend.URL, "backend", "backend-password"
	}
	gateway, err := NewProjectGateway(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gateway.Close() })
	return gateway, projects
}

func gatewayToken(t *testing.T, cfg config.Config, subject, integration string, cap access.Capability) string {
	t.Helper()
	now := time.Now().UTC()
	token, err := access.MintCapabilityToken(cfg.LocalToken, access.Claims{Subject: subject, Project: cfg.ProjectID, Integration: integration, Compatibility: "linux-amd64-schema1", Repository: cfg.ActionsRepository, Ref: "refs/heads/main", DefaultRef: "refs/heads/main", Capabilities: []access.Capability{cap}, ExpiresAt: now.Add(time.Hour)}, now)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func gatewayRequest(gateway *ProjectGateway, method, target, auth, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	r.Header.Set(compatibility.Header, "linux-amd64-schema1")
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	gateway.ServeHTTP(w, r)
	return w
}

func TestProjectGatewayTurboIsolationAndRevocation(t *testing.T) {
	gateway, projects := gatewayFixture(t, nil)
	auth := map[string]string{}
	for alias, cfg := range projects {
		auth[alias] = "Bearer " + gatewayToken(t, cfg, "github:alice", "turbo", access.CapabilityWrite)
		response := gatewayRequest(gateway, "PUT", "/v8/artifacts/same-key", auth[alias], alias, nil)
		if response.Code != 200 {
			t.Fatalf("%s put: %d %s", alias, response.Code, response.Body.String())
		}
	}
	for alias := range projects {
		response := gatewayRequest(gateway, "GET", "/v8/artifacts/same-key", auth[alias], "", nil)
		if response.Code != 200 || response.Body.String() != alias {
			t.Fatalf("%s get: %d %s", alias, response.Code, response.Body.String())
		}
	}
	for _, target := range []string{"/v8/artifacts/same-key?teamId=github.com/acme/beta", "/v8/artifacts/same-key?teamId=github.com/acme/alpha&teamId=github.com/acme/beta"} {
		if response := gatewayRequest(gateway, "GET", target, auth["alpha"], "", nil); response.Code != 404 {
			t.Fatalf("selector bypass: %d", response.Code)
		}
	}
	parts := strings.Split(strings.TrimPrefix(auth["alpha"], "Bearer "), ".")
	decoded, _ := base64.RawURLEncoding.DecodeString(parts[1])
	parts[1] = base64.RawURLEncoding.EncodeToString([]byte(strings.ReplaceAll(string(decoded), "acme/alpha", "acme/beta")))
	if response := gatewayRequest(gateway, "GET", "/v8/artifacts/same-key", "Bearer "+strings.Join(parts, "."), "", nil); response.Code != 404 {
		t.Fatalf("forged project read: %d", response.Code)
	}
	reader := "Bearer " + gatewayToken(t, projects["alpha"], "github:reader", "turbo", access.CapabilityRead)
	if response := gatewayRequest(gateway, "PUT", "/v8/artifacts/reader-write", reader, "bad", nil); response.Code != 401 {
		t.Fatalf("reader wrote: %d", response.Code)
	}
	// Simulate a membership removal without reissuing or expiring the token.
	delete(gateway.projects[projects["alpha"].ProjectID].config.TeamMembers, "alice")
	if response := gatewayRequest(gateway, "GET", "/v8/artifacts/same-key", auth["alpha"], "", nil); response.Code != 404 {
		t.Fatalf("revoked member read: %d", response.Code)
	}
	if response := gatewayRequest(gateway, "GET", "/v8/artifacts/same-key", auth["beta"], "", nil); response.Code != 200 {
		t.Fatalf("other project affected: %d", response.Code)
	}
}

func TestProjectGatewayActionsSignedURLIsolation(t *testing.T) {
	gateway, projects := gatewayFixture(t, nil)
	auth := "Bearer " + gatewayToken(t, projects["alpha"], "github:alice", "actions", access.CapabilityWrite)
	reserve := gatewayRequest(gateway, "POST", "/_apis/artifactcache/caches", auth, `{"key":"same-key","version":"v1","cacheSize":5}`, nil)
	var reservation struct {
		CacheID int64 `json:"cacheId"`
	}
	if reserve.Code != 201 || json.Unmarshal(reserve.Body.Bytes(), &reservation) != nil {
		t.Fatalf("reserve: %d %s", reserve.Code, reserve.Body.String())
	}
	cachePath := fmt.Sprintf("/_apis/artifactcache/caches/%d", reservation.CacheID)
	if response := gatewayRequest(gateway, "PATCH", cachePath, auth, "alpha", map[string]string{"Content-Range": "bytes 0-4/*"}); response.Code != 204 {
		t.Fatalf("upload: %d %s", response.Code, response.Body.String())
	}
	if response := gatewayRequest(gateway, "POST", cachePath, auth, `{"size":5}`, nil); response.Code != 204 {
		t.Fatalf("commit: %d %s", response.Code, response.Body.String())
	}
	lookup := gatewayRequest(gateway, "GET", "/_apis/artifactcache/cache?keys=same-key&version=v1", auth, "", nil)
	var hit struct {
		ArchiveLocation string `json:"archiveLocation"`
	}
	if lookup.Code != 200 || json.Unmarshal(lookup.Body.Bytes(), &hit) != nil {
		t.Fatalf("lookup: %d %s", lookup.Code, lookup.Body.String())
	}
	if !strings.HasPrefix(hit.ArchiveLocation, "https://cache.example/") {
		t.Fatal("archive left shared origin")
	}
	download := gatewayRequest(gateway, "GET", hit.ArchiveLocation, "", "", nil)
	if download.Code != 200 || download.Body.String() != "alpha" {
		t.Fatalf("signed download: %d %s", download.Code, download.Body.String())
	}
	beta := "Bearer " + gatewayToken(t, projects["beta"], "github:alice", "actions", access.CapabilityWrite)
	if response := gatewayRequest(gateway, "GET", "/_apis/artifactcache/cache?keys=same-key&version=v1", beta, "", nil); response.Code != 204 {
		t.Fatalf("cross-project Actions lookup: %d", response.Code)
	}
	parsed, _ := url.Parse(hit.ArchiveLocation)
	query := parsed.Query()
	decoded, _ := base64.RawURLEncoding.DecodeString(query.Get("authority"))
	query.Set("authority", base64.RawURLEncoding.EncodeToString([]byte(strings.ReplaceAll(string(decoded), "acme/alpha", "acme/beta"))))
	parsed.RawQuery = query.Encode()
	if response := gatewayRequest(gateway, "GET", parsed.String(), "", "", nil); response.Code != 404 {
		t.Fatalf("tampered signed URL: %d", response.Code)
	}
}

func TestProjectGatewayRegistryAuthorization(t *testing.T) {
	var calls atomic.Int64
	gateway, projects := gatewayFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		user, password, ok := r.BasicAuth()
		if !ok || user != "backend" || password != "backend-password" {
			http.Error(w, "wrong backend auth", 500)
			return
		}
		if r.Method == "POST" {
			w.Header().Set("Location", "/v2/alpha/cache/blobs/uploads/one")
			w.WriteHeader(202)
			return
		}
		w.WriteHeader(200)
	}))
	basic := func(alias, subject, integration string, cap access.Capability) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(alias+":"+gatewayToken(t, projects[alias], subject, integration, cap)))
	}
	writer := basic("alpha", "github:alice", "buildkit", access.CapabilityWrite)
	reader := basic("alpha", "github:reader", "buildkit", access.CapabilityRead)
	for _, tc := range []struct {
		method, path, auth string
		status             int
	}{
		{"GET", "/v2/", writer, 200},
		{"GET", "/v2/alpha/cache/tags/list", reader, 200},
		{"POST", "/v2/alpha/cache/blobs/uploads/", writer, 202},
		{"PUT", "/v2/alpha/cache/manifests/latest", reader, 401},
		{"GET", "/v2/beta/cache/tags/list", writer, 401},
		{"POST", "/v2/alpha/cache/blobs/uploads/?mount=sha256:a&from=beta/cache", writer, 401},
		{"POST", "/v2/alpha/cache/blobs/uploads/?mount=sha256:a&from=alpha/../beta/cache", writer, 401},
		{"GET", "/v2/_catalog", writer, 401},
		{"GET", "/v2/alpha/cache/tags/list", basic("alpha", "github:alice", "turbo", access.CapabilityWrite), 401},
		{"DELETE", "/v2/alpha/cache/manifests/latest", writer, 401},
	} {
		before := calls.Load()
		response := gatewayRequest(gateway, tc.method, tc.path, tc.auth, "", nil)
		if response.Code != tc.status {
			t.Fatalf("%s %s: got %d want %d", tc.method, tc.path, response.Code, tc.status)
		}
		if tc.status == 401 && calls.Load() != before {
			t.Fatal("unauthorized request reached registry")
		}
	}
}

func TestProjectGatewayRejectsSharedProjectSecrets(t *testing.T) {
	alpha, beta := gatewayTestConfig(t, "alpha"), gatewayTestConfig(t, "beta")
	beta.LocalToken = alpha.LocalToken
	if gateway, err := NewProjectGateway(context.Background(), ProjectGatewayConfig{Origin: "https://cache.example", Projects: map[string]config.Config{"alpha": alpha, "beta": beta}}); err == nil {
		gateway.Close()
		t.Fatal("accepted shared signing secret")
	}
}

func TestProjectGatewayPersistsProjectNamespaces(t *testing.T) {
	projects := map[string]config.Config{"alpha": gatewayTestConfig(t, "alpha"), "beta": gatewayTestConfig(t, "beta")}
	cfg := ProjectGatewayConfig{Origin: "https://cache.example", Projects: projects}
	gateway, err := NewProjectGateway(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	auth := map[string]string{}
	for alias, project := range projects {
		auth[alias] = "Bearer " + gatewayToken(t, project, "github:alice", "turbo", access.CapabilityWrite)
		if response := gatewayRequest(gateway, "PUT", "/v8/artifacts/restart", auth[alias], alias, nil); response.Code != 200 {
			t.Fatal(response.Code)
		}
	}
	if err := gateway.Close(); err != nil {
		t.Fatal(err)
	}
	gateway, err = NewProjectGateway(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	for alias := range projects {
		response := gatewayRequest(gateway, "GET", "/v8/artifacts/restart", auth[alias], "", nil)
		if response.Code != 200 || response.Body.String() != alias {
			t.Fatalf("wrong project after restart: %s %d %q", alias, response.Code, response.Body.String())
		}
	}
}

func TestProjectGatewayRegistryRejectsUnsafeUploadLocations(t *testing.T) {
	for _, location := range []string{"https://other.example/v2/alpha/cache/uploads/one", "//other.example/v2/alpha/cache/uploads/one", "/v2/beta/cache/uploads/one", "/v2/alpha/../beta/uploads/one"} {
		t.Run(location, func(t *testing.T) {
			gateway, projects := gatewayFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Location", location); w.WriteHeader(202) }))
			token := gatewayToken(t, projects["alpha"], "github:alice", "buildkit", access.CapabilityWrite)
			auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("alpha:"+token))
			response := gatewayRequest(gateway, "POST", "/v2/alpha/cache/blobs/uploads/", auth, "", nil)
			if response.Code != 502 || response.Header().Get("Location") != "" {
				t.Fatalf("unsafe location accepted: %d", response.Code)
			}
		})
	}
}
