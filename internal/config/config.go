package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/compatibility"
)

const (
	DefaultListen   = "127.0.0.1:7437"
	DefaultMaxBytes = int64(20 * 1024 * 1024 * 1024)
)

type Config struct {
	Version                 int       `json:"version"`
	InstallationID          string    `json:"installationId,omitempty"`
	Role                    string    `json:"role"`
	DataDir                 string    `json:"dataDir"`
	Listen                  string    `json:"listen"`
	MaxBytes                int64     `json:"maxBytes"`
	MinFreeBytes            int64     `json:"minFreeBytes"` // Upward override for the fixed and percentage filesystem reserve.
	ProjectID               string    `json:"projectId,omitempty"`
	ProjectRoot             string    `json:"projectRoot,omitempty"`
	TeamURL                 string    `json:"teamUrl,omitempty"`
	TeamToken               string    `json:"teamToken,omitempty"`
	PublicURL               string    `json:"publicUrl,omitempty"`
	PublicTrustKey          string    `json:"publicTrustKey,omitempty"`
	PublicPrivateKey        string    `json:"publicPrivateKey,omitempty"`
	PublisherToken          string    `json:"publisherToken,omitempty"`
	PublicBuildRepositories []string  `json:"publicBuildRepositories,omitempty"`
	ActionsRepository       string    `json:"actionsRepository"`
	ActionsRef              string    `json:"actionsRef"`
	ActionsDefaultRef       string    `json:"actionsDefaultRef"`
	ActionsArchiveBaseURL   string    `json:"actionsArchiveBaseUrl,omitempty"`
	BuildkitBuilder         string    `json:"buildkitBuilder"`
	BuildkitTeamRepository  string    `json:"buildkitTeamRepository,omitempty"`
	BypassAdapters          []string  `json:"bypassAdapters"`
	LocalToken              string    `json:"localToken"`
	CompatibilityID         string    `json:"compatibilityId"`
	CreatedAt               time.Time `json:"createdAt"`
}

func Defaults() (Config, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return Config{}, fmt.Errorf("find user cache directory: %w", err)
	}
	token, err := NewToken()
	if err != nil {
		return Config{}, err
	}
	installationToken, err := NewToken()
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Version:         1,
		InstallationID:  "install-" + installationToken[:24],
		Role:            "local",
		DataDir:         filepath.Join(cacheDir, "layercache"),
		Listen:          DefaultListen,
		MaxBytes:        DefaultMaxBytes,
		MinFreeBytes:    5 * 1024 * 1024 * 1024,
		ProjectID:       "local-" + token[:12],
		LocalToken:      token,
		BypassAdapters:  []string{},
		CompatibilityID: runtime.GOOS + "-" + runtime.GOARCH + "-schema1",
		CreatedAt:       time.Now().UTC(),
	}
	cfg.ActionsRepository = cfg.ProjectID
	cfg.ActionsRef = "refs/heads/main"
	cfg.ActionsDefaultRef = "refs/heads/main"
	cfg.BuildkitBuilder = "layercache"
	return cfg, nil
}

func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user config directory: %w", err)
	}
	return filepath.Join(dir, "layercache", "config.json"), nil
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

func Save(path string, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return fmt.Errorf("stage config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("protect staged config: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write staged config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync staged config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close staged config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("commit config: %w", err)
	}
	return nil
}

func Exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (cfg Config) Validate() error {
	if cfg.Version != 1 {
		return fmt.Errorf("unsupported config version %d", cfg.Version)
	}
	if cfg.DataDir == "" {
		return errors.New("dataDir is required")
	}
	if cfg.Role != "local" && cfg.Role != "team" && cfg.Role != "public" {
		return fmt.Errorf("role must be local, team, or public, got %q", cfg.Role)
	}
	if cfg.Listen == "" {
		return errors.New("listen is required")
	}
	if cfg.ProjectID == "" {
		return errors.New("projectId is required")
	}
	if cfg.MaxBytes <= 0 {
		return errors.New("maxBytes must be positive")
	}
	if cfg.MinFreeBytes < 0 {
		return errors.New("minFreeBytes cannot be negative")
	}
	if cfg.LocalToken == "" {
		return errors.New("localToken is required")
	}
	if err := compatibility.Validate(cfg.CompatibilityID); err != nil {
		return fmt.Errorf("invalid compatibilityId: %w", err)
	}
	if cfg.ActionsRepository == "" || cfg.ActionsRef == "" || cfg.ActionsDefaultRef == "" {
		return errors.New("Actions repository, ref, and default ref are required")
	}
	if cfg.ActionsArchiveBaseURL != "" {
		parsed, err := url.Parse(cfg.ActionsArchiveBaseURL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return errors.New("actionsArchiveBaseUrl must be an absolute HTTP or HTTPS URL")
		}
		if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return errors.New("actionsArchiveBaseUrl cannot contain credentials, a query, or a fragment")
		}
		if parsed.EscapedPath() != "" && parsed.EscapedPath() != "/" {
			return errors.New("actionsArchiveBaseUrl cannot contain a path prefix")
		}
		if parsed.Scheme == "http" && !loopbackHost(parsed.Hostname()) {
			return errors.New("actionsArchiveBaseUrl must use HTTPS except on loopback")
		}
	}
	if cfg.BuildkitBuilder == "" {
		return errors.New("buildkitBuilder is required")
	}
	seenAdapters := make(map[string]struct{}, len(cfg.BypassAdapters))
	for _, adapter := range cfg.BypassAdapters {
		if adapter != "turbo" && adapter != "actions" && adapter != "buildkit" {
			return fmt.Errorf("unknown bypass adapter %q", adapter)
		}
		if _, exists := seenAdapters[adapter]; exists {
			return fmt.Errorf("duplicate bypass adapter %q", adapter)
		}
		seenAdapters[adapter] = struct{}{}
	}
	for _, repository := range cfg.PublicBuildRepositories {
		if err := validatePublicBuildRepository(repository); err != nil {
			return fmt.Errorf("invalid Public Build repository %q: %w", repository, err)
		}
	}
	if (cfg.TeamURL == "") != (cfg.TeamToken == "") {
		return errors.New("teamUrl and teamToken must be configured together")
	}
	if cfg.PublicURL != "" && cfg.PublicTrustKey == "" {
		return errors.New("publicTrustKey is required with publicUrl")
	}
	if cfg.Role == "public" && (cfg.PublicTrustKey == "" || cfg.PublicPrivateKey == "" || cfg.PublisherToken == "") {
		return errors.New("Public Cache role requires signing keys and publisherToken")
	}
	return nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validatePublicBuildRepository(repository string) error {
	if repository == "" || strings.TrimSpace(repository) != repository {
		return errors.New("repository must be non-empty and trimmed")
	}
	parsed, err := url.Parse(repository)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, "github.com") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return errors.New("repository must be an HTTPS github.com URL")
	}
	path := strings.TrimSuffix(strings.TrimSuffix(parsed.Path, "/"), ".git")
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] == "." || parts[1] == "." ||
		parts[0] == ".." || parts[1] == ".." {
		return errors.New("repository must name one GitHub owner and repository")
	}
	return nil
}

func NewToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate local token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}
