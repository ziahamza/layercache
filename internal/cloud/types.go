// Package cloud owns the production Team/Public persistence module. PostgreSQL
// is authoritative for visibility, quota, audit, and leases. S3-compatible
// storage contains immutable bytes only and cannot grant cache visibility.
package cloud

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"time"

	"github.com/layercache/layercache/internal/artifact"
	"github.com/layercache/layercache/internal/retention"
)

var (
	ErrNotFound  = artifact.ErrNotFound
	ErrConflict  = artifact.ErrConflict
	ErrCorrupt   = artifact.ErrCorrupt
	ErrQuota     = artifact.ErrQuota
	ErrLeaseHeld = errors.New("promotion reference already has an active lease")
	ErrLeaseLost = errors.New("promotion lease is no longer active")
)

const (
	DefaultStageTTL  = 24 * time.Hour
	DefaultBlobGrace = time.Hour
	defaultLeaseTTL  = 2 * time.Minute
	readLeaseTTL     = 15 * time.Minute
)

type Config struct {
	PostgresURL    string
	S3Endpoint     string
	S3Bucket       string
	S3Region       string
	S3AccessKey    string
	S3SecretKey    string
	S3PathStyle    bool
	Project        string
	MaxBytes       int64
	EvictionPolicy retention.Policy
	StageTTL       time.Duration
	BlobGrace      time.Duration
	Now            func() time.Time
}

// Store is the cloud persistence interface used by protocol gateways. Put is
// first-writer-wins for the full artifact.Key. Get verifies immutable bytes as
// the caller consumes them. All methods reject keys outside Config.Project.
type Store struct {
	database  *sql.DB
	blobs     blobStore
	config    Config
	namespace string
}

type stagedBlob struct {
	Key       string
	Digest    string
	Size      int64
	MediaType string
}

type blobInfo struct {
	Key          string
	Digest       string
	Size         int64
	LastModified time.Time
}

type blobListResult struct {
	Info blobInfo
	Err  error
}

type blobStore interface {
	Stage(context.Context, string, io.Reader, int64, string) (stagedBlob, error)
	Commit(context.Context, stagedBlob, string) error
	Open(context.Context, string, string, int64) (io.ReadCloser, error)
	Stat(context.Context, string) (blobInfo, error)
	Delete(context.Context, string) error
	List(context.Context, string) <-chan blobListResult
}

type AuditEvent struct {
	ID         int64             `json:"id"`
	Project    string            `json:"project"`
	Actor      string            `json:"actor"`
	Action     string            `json:"action"`
	Resource   string            `json:"resource"`
	Outcome    string            `json:"outcome"`
	Attributes map[string]string `json:"attributes,omitempty"`
	CreatedAt  time.Time         `json:"createdAt"`
}

type AuditQuery struct {
	Project string
	AfterID int64
	Limit   int
}

type PromotionLease struct {
	Project   string    `json:"project"`
	Reference string    `json:"reference"`
	Owner     string    `json:"owner"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type Member struct {
	Project   string    `json:"project"`
	Subject   string    `json:"subject"`
	Role      string    `json:"role"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type MaintenanceResult struct {
	ExpiredUploads int `json:"expiredUploads"`
	DeletedBlobs   int `json:"deletedBlobs"`
	ExpiredLeases  int `json:"expiredLeases"`
}
