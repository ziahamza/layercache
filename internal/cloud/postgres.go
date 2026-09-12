package cloud

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/layercache/layercache/internal/retention"
)

func Open(ctx context.Context, config Config) (*Store, error) {
	if err := validateConfig(&config); err != nil {
		return nil, err
	}
	database, err := sql.Open("pgx", config.PostgresURL)
	if err != nil {
		return nil, errors.New("open cloud metadata connection")
	}
	database.SetMaxOpenConns(24)
	database.SetMaxIdleConns(6)
	database.SetConnMaxIdleTime(5 * time.Minute)
	database.SetConnMaxLifetime(time.Hour)
	store := &Store{database: database, config: config, namespace: projectNamespace(config.Project)}
	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()
		return nil, store.safeError("connect cloud metadata", err)
	}
	if err := store.migrate(ctx); err != nil {
		_ = database.Close()
		return nil, err
	}
	if err := ensureProject(ctx, store); err != nil {
		_ = database.Close()
		return nil, err
	}
	store.blobs, err = newS3BlobStore(ctx, config, store.namespace)
	if err != nil {
		_ = database.Close()
		return nil, store.safeError("connect cloud object storage", err)
	}
	return store, nil
}

func (store *Store) Close() error {
	if store == nil || store.database == nil {
		return nil
	}
	return store.database.Close()
}

func validateConfig(config *Config) error {
	policy, err := retention.Normalize(config.EvictionPolicy)
	if err != nil {
		return err
	}
	config.EvictionPolicy = policy
	if config.PostgresURL == "" {
		return errors.New("cloud PostgreSQL URL is required")
	}
	postgresURL, err := url.Parse(config.PostgresURL)
	if err != nil || postgresURL.Host == "" || (postgresURL.Scheme != "postgres" && postgresURL.Scheme != "postgresql") {
		return errors.New("cloud PostgreSQL URL is invalid")
	}
	endpoint, err := url.Parse(config.S3Endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
		(endpoint.EscapedPath() != "" && endpoint.EscapedPath() != "/") {
		return errors.New("cloud S3 endpoint is invalid")
	}
	if config.S3Bucket == "" || config.S3Region == "" {
		return errors.New("cloud S3 bucket and region are required")
	}
	if (config.S3AccessKey == "") != (config.S3SecretKey == "") {
		return errors.New("cloud S3 access and secret keys must both be set or both be omitted")
	}
	if err := validateIdentity("project", config.Project, 256); err != nil {
		return err
	}
	if config.MaxBytes <= 0 {
		return errors.New("cloud project quota must be positive")
	}
	if config.IdleTTL < 0 || config.UnreusedTTL < 0 || config.SoftBytes < 0 || config.SoftBytes > config.MaxBytes {
		return errors.New("invalid proactive cache retention configuration")
	}
	if config.StageTTL == 0 {
		config.StageTTL = DefaultStageTTL
	}
	if config.BlobGrace == 0 {
		config.BlobGrace = DefaultBlobGrace
	}
	if config.StageTTL < time.Minute {
		return errors.New("cloud upload expiry must be at least one minute")
	}
	if config.BlobGrace < time.Minute {
		return errors.New("cloud blob grace period must be at least one minute")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return nil
}

func validateIdentity(name, value string, maximum int) error {
	if value == "" || strings.TrimSpace(value) != value {
		return fmt.Errorf("%s must be non-empty and trimmed", name)
	}
	if len(value) > maximum {
		return fmt.Errorf("%s exceeds %d bytes", name, maximum)
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%s contains control characters", name)
	}
	return nil
}

func projectNamespace(project string) string {
	digest := sha256.Sum256([]byte(project))
	return hex.EncodeToString(digest[:16])
}

func (store *Store) safeError(operation string, err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	for _, secret := range []string{
		store.config.PostgresURL, store.config.S3AccessKey, store.config.S3SecretKey,
		os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), os.Getenv("AWS_SESSION_TOKEN"),
	} {
		if secret == "" {
			continue
		}
		message = strings.ReplaceAll(message, secret, "[redacted]")
		message = strings.ReplaceAll(message, url.QueryEscape(secret), "[redacted]")
	}
	if parsed, parseErr := url.Parse(store.config.PostgresURL); parseErr == nil && parsed.User != nil {
		if password, found := parsed.User.Password(); found && password != "" {
			message = strings.ReplaceAll(message, password, "[redacted]")
			message = strings.ReplaceAll(message, url.QueryEscape(password), "[redacted]")
		}
	}
	return fmt.Errorf("%s: %s", operation, message)
}
