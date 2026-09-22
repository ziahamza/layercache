package config

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/compatibility"
	"github.com/layercache/layercache/internal/publictrust"
	"github.com/layercache/layercache/internal/retention"
)

const (
	DefaultListen      = "127.0.0.1:7437"
	DefaultMaxBytes    = int64(20 * 1024 * 1024 * 1024)
	maximumConfigBytes = int64(1 << 20)
)

type Config struct {
	Version                    int               `json:"version"`
	InstallationID             string            `json:"installationId,omitempty"`
	Role                       string            `json:"role"`
	DataDir                    string            `json:"dataDir"`
	Listen                     string            `json:"listen"`
	MaxBytes                   int64             `json:"maxBytes"`
	EvictionPolicy             retention.Policy  `json:"evictionPolicy,omitempty"`
	MinFreeBytes               int64             `json:"minFreeBytes"` // Upward override for the fixed and percentage filesystem reserve.
	ProjectID                  string            `json:"projectId,omitempty"`
	ProjectRoot                string            `json:"projectRoot,omitempty"`
	TeamURL                    string            `json:"teamUrl,omitempty"`
	TeamToken                  string            `json:"teamToken,omitempty"`
	TeamTokenExpiresAt         time.Time         `json:"teamTokenExpiresAt,omitempty"`
	PublicURL                  string            `json:"publicUrl,omitempty"`
	PublicAccessToken          string            `json:"publicAccessToken,omitempty"`
	PublicAccessTokenExpiresAt time.Time         `json:"publicAccessTokenExpiresAt,omitempty"`
	PublicTrustKey             string            `json:"publicTrustKey,omitempty"`
	PublicTrustSequence        uint64            `json:"publicTrustSequence,omitempty"`
	PublicPrivateKey           string            `json:"publicPrivateKey,omitempty"`
	PublisherToken             string            `json:"publisherToken,omitempty"` // Deprecated migration input. Never authorize both worker and collector with it.
	PublicBuildWorkerToken     string            `json:"publicBuildWorkerToken,omitempty"`
	PublicCollectorToken       string            `json:"publicCollectorToken,omitempty"`
	PublicBuildRepositories    []string          `json:"publicBuildRepositories,omitempty"`
	PublicBuildApprovedRefs    []string          `json:"publicBuildApprovedRefs,omitempty"`
	PublicBuildRecipeDigests   []string          `json:"publicBuildRecipeDigests,omitempty"`
	PublicBuildKernelPath      string            `json:"publicBuildKernelPath,omitempty"`
	PublicBuildKernelSHA256    string            `json:"publicBuildKernelSha256,omitempty"`
	PublicBuildRootFSPath      string            `json:"publicBuildRootfsPath,omitempty"`
	PublicBuildRootFSSHA256    string            `json:"publicBuildRootfsSha256,omitempty"`
	PublicBuildGuestContract   string            `json:"publicBuildGuestContract,omitempty"`
	PublicBuildContractSHA256  string            `json:"publicBuildContractSha256,omitempty"`
	PublicBuildCgroupRoot      string            `json:"publicBuildCgroupRoot,omitempty"`
	PublicBuildWorkRoot        string            `json:"publicBuildWorkRoot,omitempty"`
	PublicBuildMaxScratchBytes int64             `json:"publicBuildMaxScratchBytes,omitempty"`
	PublicBuildSandboxUID      uint32            `json:"publicBuildSandboxUid,omitempty"`
	PublicBuildSandboxGID      uint32            `json:"publicBuildSandboxGid,omitempty"`
	ActionsRepository          string            `json:"actionsRepository"`
	ActionsRef                 string            `json:"actionsRef"`
	ActionsDefaultRef          string            `json:"actionsDefaultRef"`
	ActionsPublicRecipeDigest  string            `json:"actionsPublicRecipeDigest,omitempty"`
	ActionsPublicBuilder       string            `json:"actionsPublicBuilder,omitempty"`
	ActionsArchiveBaseURL      string            `json:"actionsArchiveBaseUrl,omitempty"`
	BuildkitBuilder            string            `json:"buildkitBuilder"`
	BuildkitTeamRepository     string            `json:"buildkitTeamRepository,omitempty"`
	BuildkitPublicRepository   string            `json:"buildkitPublicRepository,omitempty"`
	BuildkitBranch             string            `json:"buildkitBranch,omitempty"`
	BuildkitGCBytes            int64             `json:"buildkitGcBytes,omitempty"`
	GitHubCredentialAccount    string            `json:"githubCredentialAccount,omitempty"`
	GitHubCLIPath              string            `json:"githubCliPath,omitempty"`
	GitHubAPIURL               string            `json:"githubApiUrl,omitempty"`
	GitHubOIDCIssuer           string            `json:"githubOidcIssuer,omitempty"`
	TeamMembers                map[string]string `json:"teamMembers,omitempty"`
	CloudPostgresURL           string            `json:"cloudPostgresUrl,omitempty"`
	CloudS3Endpoint            string            `json:"cloudS3Endpoint,omitempty"`
	CloudS3Bucket              string            `json:"cloudS3Bucket,omitempty"`
	CloudS3Region              string            `json:"cloudS3Region,omitempty"`
	CloudS3AccessKey           string            `json:"cloudS3AccessKey,omitempty"`
	CloudS3SecretKey           string            `json:"cloudS3SecretKey,omitempty"`
	CloudS3UsePathStyle        bool              `json:"cloudS3UsePathStyle,omitempty"`
	CloudStageTTL              time.Duration     `json:"cloudStageTtl,omitempty"`
	CloudBlobGrace             time.Duration     `json:"cloudBlobGrace,omitempty"`
	CacheIdleTTL               time.Duration     `json:"cacheIdleTtl,omitempty"`
	CacheUnreusedTTL           time.Duration     `json:"cacheUnreusedTtl,omitempty"`
	CacheSoftBytes             int64             `json:"cacheSoftBytes,omitempty"`
	RemoteMetadataTimeout      time.Duration     `json:"remoteMetadataTimeout,omitempty"`
	RemoteTransferIdleTimeout  time.Duration     `json:"remoteTransferIdleTimeout,omitempty"`
	BypassAdapters             []string          `json:"bypassAdapters"`
	LocalToken                 string            `json:"localToken"`
	CompatibilityID            string            `json:"compatibilityId"`
	CreatedAt                  time.Time         `json:"createdAt"`
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
		EvictionPolicy:  retention.LRU,
		MinFreeBytes:    5 * 1024 * 1024 * 1024,
		ProjectID:       localProjectID(token),
		LocalToken:      token,
		BypassAdapters:  []string{},
		CompatibilityID: compatibility.Detect(context.Background()),
		CreatedAt:       time.Now().UTC(),
	}
	cfg.ActionsRepository = cfg.ProjectID
	cfg.ActionsRef = "refs/heads/main"
	cfg.ActionsDefaultRef = "refs/heads/main"
	cfg.ActionsPublicBuilder = "layercache-public-builder-v1"
	cfg.BuildkitBuilder = "layercache"
	cfg.BuildkitBranch = "main"
	cfg.BuildkitGCBytes = DefaultMaxBytes / 4
	cfg.CloudStageTTL = 24 * time.Hour
	cfg.CloudBlobGrace = time.Hour
	cfg.RemoteMetadataTimeout = 2 * time.Second
	cfg.RemoteTransferIdleTimeout = 30 * time.Second
	return cfg, nil
}

func localProjectID(seed string) string {
	digest := sha256.Sum256([]byte(seed))
	return fmt.Sprintf("local-%x", digest[:12])
}

func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user config directory: %w", err)
	}
	return filepath.Join(dir, "layercache", "config.json"), nil
}

func Load(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximumConfigBytes+1))
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	if len(data) == 0 || int64(len(data)) > maximumConfigBytes {
		return Config{}, fmt.Errorf("config %s must be between 1 byte and %d bytes", path, maximumConfigBytes)
	}
	var cfg Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("decode config %s: trailing JSON", path)
	}
	cfg.migrate()
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

func Save(path string, cfg Config) error {
	cfg.migrate()
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
	if int64(len(data)) > maximumConfigBytes {
		return fmt.Errorf("encoded config exceeds the %d-byte limit", maximumConfigBytes)
	}
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

func (cfg *Config) migrate() {
	if cfg.BuildkitBuilder == "" {
		cfg.BuildkitBuilder = "layercache"
	}
	if cfg.BuildkitBranch == "" {
		cfg.BuildkitBranch = "main"
	}
	if cfg.BuildkitGCBytes == 0 && cfg.MaxBytes > 0 {
		cfg.BuildkitGCBytes = cfg.MaxBytes / 4
	}
	if cfg.ActionsPublicBuilder == "" {
		cfg.ActionsPublicBuilder = "layercache-public-builder-v1"
	}
	if cfg.CloudStageTTL == 0 {
		cfg.CloudStageTTL = 24 * time.Hour
	}
	if cfg.CloudBlobGrace == 0 {
		cfg.CloudBlobGrace = time.Hour
	}
	if cfg.RemoteMetadataTimeout == 0 {
		cfg.RemoteMetadataTimeout = 2 * time.Second
	}
	if cfg.RemoteTransferIdleTimeout == 0 {
		cfg.RemoteTransferIdleTimeout = 30 * time.Second
	}
	if cfg.PublisherToken == "" {
		return
	}
	if cfg.PublicBuildWorkerToken == "" {
		cfg.PublicBuildWorkerToken = deriveCredential(cfg.PublisherToken, "public-build-worker")
	}
	if cfg.PublicCollectorToken == "" {
		cfg.PublicCollectorToken = deriveCredential(cfg.PublisherToken, "public-collector")
	}
}

func deriveCredential(secret, purpose string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("layercache-credential-v1\x00" + purpose))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
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
	if cfg.CacheIdleTTL < 0 || cfg.CacheUnreusedTTL < 0 || cfg.CacheSoftBytes < 0 || cfg.CacheSoftBytes > cfg.MaxBytes {
		return errors.New("cache retention durations and soft target must be nonnegative; soft target cannot exceed maxBytes")
	}
	if _, err := retention.Normalize(cfg.EvictionPolicy); err != nil {
		return err
	}
	cfg.migrate()
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
	if strings.TrimSpace(cfg.Listen) != cfg.Listen {
		return errors.New("listen must be trimmed")
	}
	_, listenPort, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen must be a host:port address: %w", err)
	}
	port, err := strconv.Atoi(listenPort)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("listen port must be a number from 1 through 65535")
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
	if cfg.RemoteMetadataTimeout <= 0 || cfg.RemoteTransferIdleTimeout <= 0 {
		return errors.New("remote metadata and transfer idle timeouts must be positive")
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
	if cfg.ActionsPublicRecipeDigest != "" && !validSHA256Digest(cfg.ActionsPublicRecipeDigest) {
		return errors.New("actionsPublicRecipeDigest must be a lowercase sha256 digest")
	}
	if strings.TrimSpace(cfg.ActionsPublicBuilder) != cfg.ActionsPublicBuilder || cfg.ActionsPublicBuilder == "" {
		return errors.New("actionsPublicBuilder must be non-empty and trimmed")
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
	if cfg.BuildkitGCBytes <= 0 {
		return errors.New("buildkitGcBytes must be positive")
	}
	if cfg.GitHubAPIURL != "" {
		parsed, err := url.Parse(cfg.GitHubAPIURL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopbackHost(parsed.Hostname()))) {
			return errors.New("githubApiUrl must use HTTPS except on loopback")
		}
		if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return errors.New("githubApiUrl cannot contain credentials, a query, or a fragment")
		}
	}
	if cfg.GitHubOIDCIssuer != "" {
		parsed, err := url.Parse(cfg.GitHubOIDCIssuer)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopbackHost(parsed.Hostname()))) {
			return errors.New("githubOidcIssuer must use HTTPS except on loopback")
		}
		if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return errors.New("githubOidcIssuer cannot contain credentials, a query, or a fragment")
		}
	}
	for login, role := range cfg.TeamMembers {
		if !validGitHubLogin(login) {
			return fmt.Errorf("invalid GitHub team member login %q", login)
		}
		if role != "reader" && role != "writer" && role != "admin" {
			return fmt.Errorf("team member %q role must be reader, writer, or admin", login)
		}
	}
	if err := validateCloudConfig(cfg); err != nil {
		return err
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
	if len(cfg.PublicBuildRepositories) > 0 && (len(cfg.PublicBuildApprovedRefs) == 0 || len(cfg.PublicBuildRecipeDigests) == 0) {
		return errors.New("Public Build repositories require at least one approved ref and maintained recipe digest")
	}
	for _, approvedRef := range cfg.PublicBuildApprovedRefs {
		if strings.TrimSpace(approvedRef) != approvedRef || !(strings.HasPrefix(approvedRef, "refs/heads/") || strings.HasPrefix(approvedRef, "refs/tags/")) || strings.HasSuffix(approvedRef, "/") {
			return fmt.Errorf("invalid Public Build approved ref %q", approvedRef)
		}
	}
	for _, recipe := range cfg.PublicBuildRecipeDigests {
		if !validSHA256Digest(recipe) {
			return fmt.Errorf("invalid Public Build recipe digest %q", recipe)
		}
	}
	workerImageConfigured := cfg.PublicBuildKernelPath != "" || cfg.PublicBuildKernelSHA256 != "" ||
		cfg.PublicBuildRootFSPath != "" || cfg.PublicBuildRootFSSHA256 != "" ||
		cfg.PublicBuildGuestContract != "" || cfg.PublicBuildContractSHA256 != "" ||
		cfg.PublicBuildCgroupRoot != "" || cfg.PublicBuildWorkRoot != "" ||
		cfg.PublicBuildMaxScratchBytes != 0 || cfg.PublicBuildSandboxUID != 0 || cfg.PublicBuildSandboxGID != 0
	if workerImageConfigured {
		if cfg.PublicBuildKernelPath == "" || cfg.PublicBuildRootFSPath == "" || cfg.PublicBuildGuestContract == "" ||
			cfg.PublicBuildCgroupRoot == "" || cfg.PublicBuildWorkRoot == "" ||
			cfg.PublicBuildMaxScratchBytes <= 0 || cfg.PublicBuildSandboxUID == 0 || cfg.PublicBuildSandboxGID == 0 {
			return errors.New("Public Build worker isolation requires kernel, rootfs, guest contract, cgroup and work roots, and non-root sandbox UID/GID")
		}
		for name, digest := range map[string]string{
			"publicBuildKernelSha256":   cfg.PublicBuildKernelSHA256,
			"publicBuildRootfsSha256":   cfg.PublicBuildRootFSSHA256,
			"publicBuildContractSha256": cfg.PublicBuildContractSHA256,
		} {
			if !validSHA256Digest(digest) {
				return fmt.Errorf("%s must be a lowercase sha256 digest", name)
			}
		}
	}
	if cfg.TeamURL == "" && cfg.TeamToken != "" {
		return errors.New("teamUrl is required with teamToken")
	}
	if err := validateRemoteEndpoint("teamUrl", cfg.TeamURL); err != nil {
		return err
	}
	if err := validateRemoteEndpoint("publicUrl", cfg.PublicURL); err != nil {
		return err
	}
	if cfg.PublicURL != "" && cfg.PublicTrustKey == "" {
		return errors.New("publicTrustKey is required with publicUrl")
	}
	var publicKey []byte
	if cfg.PublicTrustKey != "" {
		decoded, err := publictrust.DecodePublicKey(cfg.PublicTrustKey)
		if err != nil {
			return fmt.Errorf("invalid publicTrustKey: %w", err)
		}
		publicKey = decoded
	}
	if cfg.PublicPrivateKey != "" {
		privateKey, err := publictrust.DecodePrivateKey(cfg.PublicPrivateKey)
		if err != nil {
			return fmt.Errorf("invalid publicPrivateKey: %w", err)
		}
		if len(publicKey) > 0 && !bytes.Equal(publicKey, privateKey.Public().(ed25519.PublicKey)) {
			return errors.New("publicTrustKey and publicPrivateKey do not form one Ed25519 keypair")
		}
	}
	if cfg.Role == "public" && (cfg.PublicTrustKey == "" || cfg.PublicPrivateKey == "" || cfg.PublicBuildWorkerToken == "" || cfg.PublicCollectorToken == "") {
		return errors.New("Public Cache role requires signing keys plus separate Public Build worker and collector tokens")
	}
	if cfg.Role == "public" && cfg.PublicBuildWorkerToken == cfg.PublicCollectorToken {
		return errors.New("Public Build worker and collector tokens must be different")
	}
	return nil
}

func validateRemoteEndpoint(name, value string) error {
	if value == "" {
		return nil
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s must be trimmed", name)
	}
	parsed, err := url.Parse(value)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%s must be an absolute HTTP or HTTPS URL", name)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s cannot contain credentials, a query, or a fragment", name)
	}
	if parsed.Scheme == "http" && !loopbackHost(parsed.Hostname()) {
		return fmt.Errorf("%s must use HTTPS except on loopback", name)
	}
	return nil
}

func validGitHubLogin(login string) bool {
	if login == "" || login != strings.ToLower(login) || len(login) > 39 || login[0] == '-' || login[len(login)-1] == '-' {
		return false
	}
	for _, character := range login {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return !strings.Contains(login, "--")
}

func validSHA256Digest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range strings.TrimPrefix(value, "sha256:") {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateCloudConfig(cfg Config) error {
	configured := cfg.CloudPostgresURL != "" || cfg.CloudS3Endpoint != "" || cfg.CloudS3Bucket != "" ||
		cfg.CloudS3Region != "" || cfg.CloudS3AccessKey != "" || cfg.CloudS3SecretKey != ""
	if !configured {
		return nil
	}
	if cfg.Role != "team" && cfg.Role != "public" {
		return errors.New("cloud persistence is available only to team and public roles")
	}
	if cfg.CloudPostgresURL == "" || cfg.CloudS3Endpoint == "" || cfg.CloudS3Bucket == "" || cfg.CloudS3Region == "" {
		return errors.New("cloudPostgresUrl, cloudS3Endpoint, cloudS3Bucket, and cloudS3Region must be configured together")
	}
	postgresURL, err := url.Parse(cfg.CloudPostgresURL)
	if err != nil || postgresURL.Host == "" || (postgresURL.Scheme != "postgres" && postgresURL.Scheme != "postgresql") {
		return errors.New("cloudPostgresUrl must be a PostgreSQL connection URL")
	}
	s3URL, err := url.Parse(cfg.CloudS3Endpoint)
	if err != nil || s3URL.Host == "" || (s3URL.Scheme != "http" && s3URL.Scheme != "https") {
		return errors.New("cloudS3Endpoint must be an absolute HTTP or HTTPS URL")
	}
	if s3URL.User != nil || s3URL.RawQuery != "" || s3URL.Fragment != "" || (s3URL.EscapedPath() != "" && s3URL.EscapedPath() != "/") {
		return errors.New("cloudS3Endpoint cannot contain credentials, a path, query, or fragment")
	}
	if s3URL.Scheme == "http" && !loopbackHost(s3URL.Hostname()) {
		return errors.New("cloudS3Endpoint must use HTTPS except on loopback")
	}
	if !validS3Bucket(cfg.CloudS3Bucket) {
		return errors.New("cloudS3Bucket must be a valid DNS-style S3 bucket name")
	}
	if strings.TrimSpace(cfg.CloudS3Region) != cfg.CloudS3Region || cfg.CloudS3Region == "" || len(cfg.CloudS3Region) > 64 || strings.IndexFunc(cfg.CloudS3Region, func(character rune) bool {
		return !((character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-')
	}) >= 0 {
		return errors.New("cloudS3Region must contain only lowercase letters, digits, and hyphens")
	}
	if (cfg.CloudS3AccessKey == "") != (cfg.CloudS3SecretKey == "") {
		return errors.New("cloud S3 access and secret keys must both be set or both be omitted")
	}
	if cfg.CloudStageTTL <= 0 {
		return errors.New("cloudStageTtl must be positive")
	}
	if cfg.CloudBlobGrace <= 0 {
		return errors.New("cloudBlobGrace must be positive")
	}
	return nil
}

func validS3Bucket(bucket string) bool {
	if len(bucket) < 3 || len(bucket) > 63 || bucket[0] == '-' || bucket[len(bucket)-1] == '-' || strings.Contains(bucket, "..") {
		return false
	}
	for _, character := range bucket {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' && character != '.' {
			return false
		}
	}
	return net.ParseIP(bucket) == nil
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
