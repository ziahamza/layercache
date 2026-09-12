package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/retention"
	"github.com/layercache/layercache/internal/server"
)

func TestHTTPConfiguredEvictionPolicyPreservesExpensiveArtifact(t *testing.T) {
	for _, policy := range []retention.Policy{retention.LRU, retention.Impact} {
		t.Run(string(policy), func(t *testing.T) {
			cfg, err := config.Defaults()
			if err != nil {
				t.Fatal(err)
			}
			cfg.DataDir, cfg.MaxBytes, cfg.EvictionPolicy = t.TempDir(), 8, policy
			instance, err := server.New(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer instance.Close()
			endpoint := httptest.NewServer(instance.Handler())
			defer endpoint.Close()
			do := func(method, path, payload, cost string) (int, []byte) {
				t.Helper()
				req := authenticatedRequest(t, cfg.LocalToken, method, endpoint.URL+path, strings.NewReader(payload))
				req.Header.Set("X-Artifact-Duration", cost)
				req.Header.Set("X-LayerCache-Compatibility", cfg.CompatibilityID)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				data, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				return resp.StatusCode, data
			}
			for _, item := range []struct{ key, payload, cost string }{
				{"expensive", "aaaa", "100000"}, {"cheap", "bbbb", "1"}, {"incoming", "cccc", "1"},
			} {
				if status, body := do("PUT", "/v8/artifacts/"+item.key, item.payload, item.cost); status != 200 {
					t.Fatalf("put %s = %d %s", item.key, status, body)
				}
			}
			want := 404
			if policy == retention.Impact {
				want = 200
			}
			if status, body := do("GET", "/v8/artifacts/expensive", "", ""); status != want {
				t.Fatalf("%s expensive artifact = %d %s; want %d", policy, status, body, want)
			}
			status, body := do("GET", "/v1/status", "", "")
			var result struct {
				Policy retention.Policy `json:"evictionPolicy"`
				Usage  int64            `json:"usageBytes"`
			}
			if err := json.Unmarshal(body, &result); err != nil || status != 200 || result.Policy != policy || result.Usage != 8 {
				t.Fatalf("status = %d %s, %v", status, body, err)
			}
		})
	}
}
