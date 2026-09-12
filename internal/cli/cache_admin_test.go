package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/layercache/layercache/internal/config"
)

func TestCacheQuotaCommandUsesAdministratorHTTPContract(t *testing.T) {
	configPath, _ := setupLocalConfig(t)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+cfg.LocalToken || r.URL.Path != "/v1/cache/quota" || r.Method != http.MethodPut {
			t.Errorf("incorrect quota request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"maxBytes":4096,"usedBytes":0}`))
	}))
	defer endpoint.Close()
	cfg.Role, cfg.Listen = "team", strings.TrimPrefix(endpoint.URL, "http://")
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	output, _, err := callCLI("cache", "quota", "--config", configPath, "--max-bytes", "4096", "--json")
	if err != nil || !strings.Contains(output, `"maxBytes":4096`) {
		t.Fatalf("quota command: %s, %v", output, err)
	}
}
