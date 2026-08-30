package config_test

import (
	"testing"

	"github.com/layercache/layercache/internal/config"
)

func TestConfigValidatesBypassAdaptersAndPublicBuildRepositories(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*config.Config)
		wantError bool
	}{
		{
			name: "supported bypass and GitHub repository",
			configure: func(cfg *config.Config) {
				cfg.BypassAdapters = []string{"turbo", "actions", "buildkit"}
				cfg.PublicBuildRepositories = []string{"https://github.com/acme/project"}
				cfg.ActionsArchiveBaseURL = "https://cache.example.com"
			},
		},
		{
			name: "insecure external Actions archive base URL",
			configure: func(cfg *config.Config) {
				cfg.ActionsArchiveBaseURL = "http://cache.example.com"
			},
			wantError: true,
		},
		{
			name: "loopback Actions archive base URL",
			configure: func(cfg *config.Config) {
				cfg.ActionsArchiveBaseURL = "http://127.0.0.1:7437"
			},
		},
		{
			name: "credentialed Actions archive base URL",
			configure: func(cfg *config.Config) {
				cfg.ActionsArchiveBaseURL = "https://token@cache.example.com"
			},
			wantError: true,
		},
		{
			name: "Actions archive base URL with a path prefix",
			configure: func(cfg *config.Config) {
				cfg.ActionsArchiveBaseURL = "https://cache.example.com/layercache"
			},
			wantError: true,
		},
		{
			name: "unknown bypass",
			configure: func(cfg *config.Config) {
				cfg.BypassAdapters = []string{"npm"}
			},
			wantError: true,
		},
		{
			name: "negative Local Cache free-space override",
			configure: func(cfg *config.Config) {
				cfg.MinFreeBytes = -1
			},
			wantError: true,
		},
		{
			name: "duplicate bypass",
			configure: func(cfg *config.Config) {
				cfg.BypassAdapters = []string{"turbo", "turbo"}
			},
			wantError: true,
		},
		{
			name: "non GitHub Public Build repository",
			configure: func(cfg *config.Config) {
				cfg.PublicBuildRepositories = []string{"https://gitlab.com/acme/project"}
			},
			wantError: true,
		},
		{
			name: "mutable-looking owner-only repository",
			configure: func(cfg *config.Config) {
				cfg.PublicBuildRepositories = []string{"https://github.com/acme"}
			},
			wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := config.Defaults()
			if err != nil {
				t.Fatal(err)
			}
			test.configure(&cfg)
			err = cfg.Validate()
			if (err != nil) != test.wantError {
				t.Fatalf("Validate() error = %v, want error %v", err, test.wantError)
			}
		})
	}
}
