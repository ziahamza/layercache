package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/layercache/layercache/internal/access"
	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/server"
)

func TestCloudCacheAdministrationHTTPAgainstPostgresAndS3(t *testing.T) {
	if os.Getenv("LAYER_CACHE_QA_POSTGRES_URL") == "" || os.Getenv("LAYER_CACHE_QA_S3_ENDPOINT") == "" {
		t.Skip("set PostgreSQL and S3 QA endpoints")
	}
	cfg, err := config.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Role, cfg.DataDir, cfg.MaxBytes = "team", t.TempDir(), 8
	cfg.ProjectID = "http-quota-" + time.Now().Format("150405.000000000")
	cfg.CloudPostgresURL, cfg.CloudS3Endpoint = os.Getenv("LAYER_CACHE_QA_POSTGRES_URL"), os.Getenv("LAYER_CACHE_QA_S3_ENDPOINT")
	cfg.CloudS3Bucket, cfg.CloudS3Region, cfg.CloudS3UsePathStyle = os.Getenv("LAYER_CACHE_QA_S3_BUCKET"), "us-east-1", true
	cfg.ActionsRepository = "acme/widgets"
	instance, err := server.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	endpoint := httptest.NewServer(instance.Handler())
	defer endpoint.Close()
	writerToken, err := access.MintCapabilityToken(cfg.LocalToken, access.Claims{
		Subject: "writer", Project: cfg.ProjectID, Capabilities: []access.Capability{access.CapabilityWrite}, ExpiresAt: time.Now().Add(time.Hour),
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, token, body string) (int, []byte) {
		t.Helper()
		req := authenticatedRequest(t, token, method, endpoint.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-LayerCache-Compatibility", cfg.CompatibilityID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		contents, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, contents
	}
	for _, path := range []string{"/v1/cache/quota", "/v1/cache/pins"} {
		if status, body := do("GET", path, writerToken, ""); status != 401 {
			t.Fatalf("writer admin read = %d %s", status, body)
		}
	}
	if status, body := do("PUT", "/v1/cache/quota", writerToken, `{"maxBytes":32}`); status != 401 {
		t.Fatalf("writer quota change = %d %s", status, body)
	}
	if status, body := do("PUT", "/v1/cache/quota", cfg.LocalToken, `{"maxBytes":32}`); status != 200 {
		t.Fatalf("admin quota change = %d %s", status, body)
	}
	if status, body := do("PUT", "/v8/artifacts/raised-limit", cfg.LocalToken, "abcdefghijklmnop"); status != 200 {
		t.Fatalf("live Turbo raised limit = %d %s", status, body)
	}
	if status, body := do("POST", "/_apis/artifactcache/caches", cfg.LocalToken, `{"key":"raised-limit","version":"v1","cacheSize":16}`); status != 201 {
		t.Fatalf("live Actions raised limit = %d %s", status, body)
	}
	status, body := do("GET", "/v1/cache/quota", cfg.LocalToken, "")
	var quota struct {
		MaxBytes  int64 `json:"maxBytes"`
		UsedBytes int64 `json:"usedBytes"`
	}
	if err := json.Unmarshal(body, &quota); err != nil || status != 200 || quota.MaxBytes != 32 || quota.UsedBytes != 16 {
		t.Fatalf("quota response = %d %s, %v", status, body, err)
	}
	pin := artifact.Pin{Owner: "keep", Key: artifact.Key{Integration: "turbo", Project: cfg.ProjectID,
		Compatibility: cfg.CompatibilityID, Native: "raised-limit"}, Digest: fmt.Sprintf("%x", sha256.Sum256([]byte("abcdefghijklmnop"))), Size: 16}
	pinJSON, err := json.Marshal(pin)
	if err != nil {
		t.Fatal(err)
	}
	if status, body := do("PUT", "/v1/cache/pins/keep", writerToken, string(pinJSON)); status != 401 {
		t.Fatalf("writer pin = %d %s", status, body)
	}
	if status, body := do("PUT", "/v1/cache/pins/keep", cfg.LocalToken, string(pinJSON)); status != 200 {
		t.Fatalf("admin pin = %d %s", status, body)
	}
	if status, body := do("PUT", "/v1/cache/quota", cfg.LocalToken, `{"maxBytes":8}`); status != 409 {
		t.Fatalf("shrink pinned quota = %d %s", status, body)
	}
	status, body = do("GET", "/v1/cache/quota", cfg.LocalToken, "")
	if err := json.Unmarshal(body, &quota); err != nil || status != 200 || quota.MaxBytes != 32 || quota.UsedBytes != 16 {
		t.Fatalf("failed shrink changed quota = %d %s, %v", status, body, err)
	}
	status, body = do("GET", "/v1/cache/pins?limit=1", cfg.LocalToken, "")
	var page struct {
		Pins []artifact.Pin `json:"pins"`
		Next string         `json:"nextCursor"`
	}
	if err := json.Unmarshal(body, &page); err != nil || status != 200 || len(page.Pins) != 1 || page.Pins[0] != pin || page.Next != "keep" {
		t.Fatalf("pin page = %d %s, %v", status, body, err)
	}
	conflicting := pin
	conflicting.Digest = strings.Repeat("0", 64)
	conflictJSON, _ := json.Marshal(conflicting)
	if status, body := do("PUT", "/v1/cache/pins/keep", cfg.LocalToken, string(conflictJSON)); status != 409 {
		t.Fatalf("conflicting pin = %d %s", status, body)
	}
	foreign := pin
	foreign.Key.Project = "another-project"
	foreignJSON, _ := json.Marshal(foreign)
	if status, body := do("PUT", "/v1/cache/pins/keep", cfg.LocalToken, string(foreignJSON)); status != 404 {
		t.Fatalf("foreign pin = %d %s", status, body)
	}
	if status, body := do("DELETE", "/v1/cache/pins/keep", writerToken, ""); status != 401 {
		t.Fatalf("writer unpin = %d %s", status, body)
	}
	if status, body := do("DELETE", "/v1/cache/pins/keep", cfg.LocalToken, ""); status != 204 {
		t.Fatalf("admin unpin = %d %s", status, body)
	}
	if status, body := do("PUT", "/v1/cache/quota", cfg.LocalToken, `{"maxBytes":8}`); status != 200 {
		t.Fatalf("unpinned shrink = %d %s", status, body)
	}
	if status, body := do("GET", "/v8/artifacts/raised-limit", cfg.LocalToken, ""); status != 404 {
		t.Fatalf("evicted artifact = %d %s", status, body)
	}
}
