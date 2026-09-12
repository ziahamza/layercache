package config_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/layercache/layercache/internal/config"
	"github.com/layercache/layercache/internal/publictrust"
)

func TestSaveRejectsConfigThatLoadWouldRejectAsOversized(t *testing.T) {
	cfg, err := config.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	cfg.PublicBuildApprovedRefs = []string{"refs/heads/" + strings.Repeat("a", 1<<20)}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path, cfg); err == nil || !strings.Contains(err.Error(), "1048576-byte limit") {
		t.Fatalf("Save() error = %v, want encoded-size rejection", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized config path error = %v, want not exist", err)
	}
}

func TestConfigValidatesPublicSigningKeysBeforeRuntimeStartup(t *testing.T) {
	cfg, err := config.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	cfg.PublicTrustKey = "not-a-key"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "publicTrustKey") {
		t.Fatalf("invalid trust key error = %v", err)
	}

	publicOne, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, privateTwo, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg.PublicTrustKey = publictrust.EncodePublicKey(publicOne)
	cfg.PublicPrivateKey = publictrust.EncodePrivateKey(privateTwo)
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "keypair") {
		t.Fatalf("mismatched keypair error = %v", err)
	}
}

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
				cfg.PublicBuildApprovedRefs = []string{"refs/heads/main"}
				cfg.PublicBuildRecipeDigests = []string{"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}
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
			name: "valid listen and remote endpoint path",
			configure: func(cfg *config.Config) {
				cfg.Listen = "[::1]:7437"
				cfg.TeamURL = "https://team.example.com/layercache"
				cfg.PublicURL = "http://localhost:8080/public"
				cfg.PublicTrustKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
			},
		},
		{
			name: "listen without port",
			configure: func(cfg *config.Config) {
				cfg.Listen = "localhost"
			},
			wantError: true,
		},
		{
			name: "listen with nonnumeric port",
			configure: func(cfg *config.Config) {
				cfg.Listen = "localhost:http"
			},
			wantError: true,
		},
		{
			name: "listen with zero port",
			configure: func(cfg *config.Config) {
				cfg.Listen = "localhost:0"
			},
			wantError: true,
		},
		{
			name: "insecure external Team endpoint",
			configure: func(cfg *config.Config) {
				cfg.TeamURL = "http://team.example.com"
			},
			wantError: true,
		},
		{
			name: "unsupported Team endpoint scheme",
			configure: func(cfg *config.Config) {
				cfg.TeamURL = "ftp://team.example.com"
			},
			wantError: true,
		},
		{
			name: "credentialed Team endpoint",
			configure: func(cfg *config.Config) {
				cfg.TeamURL = "https://secret@team.example.com"
			},
			wantError: true,
		},
		{
			name: "queried Team endpoint",
			configure: func(cfg *config.Config) {
				cfg.TeamURL = "https://team.example.com?token=secret"
			},
			wantError: true,
		},
		{
			name: "insecure external Public endpoint",
			configure: func(cfg *config.Config) {
				cfg.PublicURL = "http://public.example.com"
				cfg.PublicTrustKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
			},
			wantError: true,
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
		{
			name: "complete QEMU Public Build isolation config",
			configure: func(cfg *config.Config) {
				cfg.PublicBuildKernelPath = "/var/lib/layercache/images/vmlinuz"
				cfg.PublicBuildKernelSHA256 = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				cfg.PublicBuildRootFSPath = "/var/lib/layercache/images/rootfs.raw"
				cfg.PublicBuildRootFSSHA256 = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				cfg.PublicBuildGuestContract = "/var/lib/layercache/images/contract.json"
				cfg.PublicBuildContractSHA256 = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
				cfg.PublicBuildCgroupRoot = "/sys/fs/cgroup/layercache-public-build"
				cfg.PublicBuildWorkRoot = "/var/lib/layercache/public-build-worker"
				cfg.PublicBuildMaxScratchBytes = 64 << 30
				cfg.PublicBuildSandboxUID = 70000
				cfg.PublicBuildSandboxGID = 994
			},
		},
		{
			name: "partial QEMU Public Build isolation config",
			configure: func(cfg *config.Config) {
				cfg.PublicBuildKernelPath = "/var/lib/layercache/images/vmlinuz"
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
