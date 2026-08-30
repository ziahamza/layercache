package artifact

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound     = errors.New("cache entry not found")
	ErrConflict     = errors.New("cache entry already contains different bytes")
	ErrCorrupt      = errors.New("artifact digest verification failed")
	ErrQuota        = errors.New("Local Cache quota exceeded")
	ErrMinFreeSpace = minimumFreeSpaceError{}
)

type minimumFreeSpaceError struct{}

func (minimumFreeSpaceError) Error() string {
	return "Local Cache minimum free-space reserve would be violated"
}
func (minimumFreeSpaceError) Unwrap() error { return ErrQuota }

type Key struct {
	Integration   string
	Project       string
	Compatibility string
	Native        string
	Version       string
	Ref           string
}

type Metadata struct {
	DurationMS int64             `json:"durationMs,omitempty"`
	Tag        string            `json:"tag,omitempty"`
	Values     map[string]string `json:"values,omitempty"`
}

type Entry struct {
	Key       Key
	Digest    string
	Size      int64
	Metadata  Metadata
	CreatedAt time.Time
}

type Stats struct {
	UsageBytes int64 `json:"usageBytes"`
	Artifacts  int64 `json:"artifacts"`
	Entries    int64 `json:"entries"`
}

type Store struct {
	db           *sql.DB
	root         string
	maxBytes     int64
	minFreeBytes int64
	writeMu      sync.Mutex
	spaceMu      sync.Mutex
	stagingMu    sync.Mutex
	stagingBytes int64
	uploadBytes  int64
	probeSpace   func(string) (filesystemSpace, error)
	commitTx     func(*sql.Tx) error
}

// Open initializes a Local Cache. minFreeBytes may raise, but never lower,
// the built-in reserve of 5 GiB or five percent of the filesystem volume.
func Open(ctx context.Context, root string, maxBytes, minFreeBytes int64) (*Store, error) {
	if maxBytes <= 0 {
		return nil, errors.New("Local Cache maxBytes must be positive")
	}
	if minFreeBytes < 0 {
		return nil, errors.New("Local Cache minFreeBytes cannot be negative")
	}
	for _, dir := range []string{root, filepath.Join(root, "blobs", "sha256"), filepath.Join(root, "staging")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create artifact directory %s: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "metadata.db"))
	if err != nil {
		return nil, fmt.Errorf("open Local Cache metadata: %w", err)
	}
	db.SetMaxOpenConns(8)
	for _, statement := range []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE IF NOT EXISTS artifacts (
			digest TEXT PRIMARY KEY,
			size INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			last_accessed_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS cache_entries (
			integration TEXT NOT NULL,
			project TEXT NOT NULL,
			compatibility TEXT NOT NULL,
			native_key TEXT NOT NULL,
			version TEXT NOT NULL,
			ref_scope TEXT NOT NULL,
			digest TEXT NOT NULL REFERENCES artifacts(digest),
			size INTEGER NOT NULL,
			metadata_json BLOB NOT NULL,
			created_at INTEGER NOT NULL,
			last_accessed_at INTEGER NOT NULL,
			PRIMARY KEY (integration, project, compatibility, native_key, version, ref_scope)
		)`,
		`CREATE INDEX IF NOT EXISTS cache_entries_lru ON cache_entries(last_accessed_at)`,
		`CREATE TABLE IF NOT EXISTS cache_entry_pins (
			namespace TEXT NOT NULL,
			owner TEXT NOT NULL,
			integration TEXT NOT NULL,
			project TEXT NOT NULL,
			compatibility TEXT NOT NULL,
			native_key TEXT NOT NULL,
			version TEXT NOT NULL,
			ref_scope TEXT NOT NULL,
			digest TEXT NOT NULL,
			size INTEGER NOT NULL,
			PRIMARY KEY (namespace, owner),
			FOREIGN KEY (integration, project, compatibility, native_key, version, ref_scope)
				REFERENCES cache_entries(integration, project, compatibility, native_key, version, ref_scope)
				ON DELETE RESTRICT
		)`,
		`CREATE INDEX IF NOT EXISTS cache_entry_pins_entry ON cache_entry_pins(
			integration, project, compatibility, native_key, version, ref_scope
		)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize Local Cache metadata: %w", err)
		}
	}
	store := &Store{
		db: db, root: root, maxBytes: maxBytes, minFreeBytes: minFreeBytes,
		probeSpace: defaultProbeSpace,
		commitTx:   func(tx *sql.Tx) error { return tx.Commit() },
	}
	if err := store.cleanStaging(); err != nil {
		db.Close()
		return nil, err
	}
	if err := store.reconcileBlobs(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (store *Store) Close() error {
	return store.db.Close()
}

func (store *Store) Put(ctx context.Context, key Key, metadata Metadata, body io.Reader) (Entry, bool, error) {
	return store.put(ctx, key, metadata, body, "")
}

func (store *Store) PutVerified(ctx context.Context, key Key, metadata Metadata, body io.Reader, expectedDigest string) (Entry, bool, error) {
	return store.put(ctx, key, metadata, body, strings.TrimPrefix(expectedDigest, "sha256:"))
}

func (store *Store) put(ctx context.Context, key Key, metadata Metadata, body io.Reader, expectedDigest string) (_ Entry, _ bool, returnErr error) {
	if err := validateKey(key); err != nil {
		return Entry{}, false, err
	}
	if expectedDigest != "" {
		if !isLowerHexName(expectedDigest, sha256.Size*2) {
			return Entry{}, false, ErrCorrupt
		}
		var existingDigest string
		err := store.db.QueryRowContext(ctx, selectEntryDigest, keyArgs(key)...).Scan(&existingDigest)
		if err == nil && existingDigest != expectedDigest {
			return Entry{}, false, ErrConflict
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Entry{}, false, fmt.Errorf("check existing entry: %w", err)
		}
	}
	lease := store.beginArtifactStaging()
	defer lease.Release()
	tmp, err := os.CreateTemp(filepath.Join(store.root, "staging"), "artifact-*")
	if err != nil {
		return Entry{}, false, fmt.Errorf("stage artifact: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(store.spaceCheckedWriter(ctx, tmp, lease), hash), body)
	if err != nil {
		tmp.Close()
		return Entry{}, false, fmt.Errorf("receive artifact: %w", err)
	}
	if err := store.syncStaged(ctx, tmp); err != nil {
		tmp.Close()
		return Entry{}, false, fmt.Errorf("sync staged artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Entry{}, false, fmt.Errorf("close staged artifact: %w", err)
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if expectedDigest != "" && digest != expectedDigest {
		return Entry{}, false, ErrCorrupt
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return Entry{}, false, fmt.Errorf("encode artifact metadata: %w", err)
	}
	now := time.Now().UTC()
	entry := Entry{Key: key, Digest: digest, Size: size, Metadata: metadata, CreatedAt: now}

	store.writeMu.Lock()
	defer store.writeMu.Unlock()

	var existingDigest string
	err = store.db.QueryRowContext(ctx, selectEntryDigest, keyArgs(key)...).Scan(&existingDigest)
	if err == nil {
		if existingDigest != digest {
			return Entry{}, false, ErrConflict
		}
		existing, openErr := store.entry(ctx, key, false)
		if openErr != nil {
			return Entry{}, false, openErr
		}
		if existing.Size != size {
			return Entry{}, false, ErrCorrupt
		}
		if _, err := store.ensureCanonicalBlob(tmpPath, digest, size); err != nil {
			return Entry{}, false, err
		}
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false, fmt.Errorf("check existing entry: %w", err)
	}

	var artifactExists int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE digest = ?`, digest).Scan(&artifactExists); err != nil {
		return Entry{}, false, fmt.Errorf("check artifact: %w", err)
	}
	var newBlob bool
	metadataCommitted := false
	defer func() {
		if !newBlob || metadataCommitted {
			return
		}
		if err := store.removeBlobIfUnreferenced(digest); err != nil {
			returnErr = errors.Join(returnErr, err)
		}
	}()
	if artifactExists == 0 {
		if size > store.maxBytes {
			return Entry{}, false, ErrQuota
		}
		usage, err := store.Usage(ctx)
		if err != nil {
			return Entry{}, false, err
		}
		if usage+size > store.maxBytes {
			if err := store.evictTo(ctx, store.maxBytes-size); err != nil {
				return Entry{}, false, err
			}
		}
		newBlob, err = store.ensureCanonicalBlob(tmpPath, digest, size)
		if err != nil {
			return Entry{}, false, err
		}
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return Entry{}, false, fmt.Errorf("begin artifact commit: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO artifacts(digest, size, created_at, last_accessed_at) VALUES(?, ?, ?, ?)`,
		digest, size, now.UnixNano(), now.UnixNano(),
	); err != nil {
		return Entry{}, false, fmt.Errorf("record artifact: %w", err)
	}
	result, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO cache_entries(
			integration, project, compatibility, native_key, version, ref_scope,
			digest, size, metadata_json, created_at, last_accessed_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		key.Integration, key.Project, key.Compatibility, key.Native, key.Version, key.Ref,
		digest, size, metadataJSON, now.UnixNano(), now.UnixNano(),
	)
	if err != nil {
		return Entry{}, false, fmt.Errorf("record cache entry: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Entry{}, false, fmt.Errorf("inspect cache commit: %w", err)
	}
	if rows != 1 {
		var winner string
		if err := tx.QueryRowContext(ctx, selectEntryDigest, keyArgs(key)...).Scan(&winner); err != nil {
			return Entry{}, false, fmt.Errorf("read cache winner: %w", err)
		}
		if winner != digest {
			return Entry{}, false, ErrConflict
		}
	}
	if err := store.commitTx(tx); err != nil {
		return Entry{}, false, fmt.Errorf("commit cache entry: %w", err)
	}
	metadataCommitted = true
	return entry, rows == 1, nil
}

func (store *Store) Get(ctx context.Context, key Key) (Entry, *os.File, error) {
	entry, err := store.entry(ctx, key, true)
	if err != nil {
		return Entry{}, nil, err
	}
	file, err := os.Open(store.blobPath(entry.Digest))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Entry{}, nil, ErrCorrupt
		}
		return Entry{}, nil, fmt.Errorf("open artifact: %w", err)
	}
	if err := verifyFile(file, entry.Digest, entry.Size); err != nil {
		file.Close()
		return Entry{}, nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return Entry{}, nil, fmt.Errorf("rewind artifact: %w", err)
	}
	return entry, file, nil
}

func (store *Store) Head(ctx context.Context, key Key) (Entry, error) {
	return store.entry(ctx, key, true)
}

func (store *Store) Usage(ctx context.Context) (int64, error) {
	var usage int64
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM artifacts`).Scan(&usage); err != nil {
		return 0, fmt.Errorf("read Local Cache usage: %w", err)
	}
	return usage, nil
}

func (store *Store) Stats(ctx context.Context) (Stats, error) {
	var stats Stats
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0), COUNT(*) FROM artifacts`).Scan(&stats.UsageBytes, &stats.Artifacts); err != nil {
		return Stats{}, fmt.Errorf("read artifact statistics: %w", err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cache_entries`).Scan(&stats.Entries); err != nil {
		return Stats{}, fmt.Errorf("read cache entry statistics: %w", err)
	}
	return stats, nil
}

func (store *Store) Delete(ctx context.Context, key Key) error {
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin cache entry deletion: %w", err)
	}
	defer tx.Rollback()
	var digest string
	if err := tx.QueryRowContext(ctx, selectEntryDigest, keyArgs(key)...).Scan(&digest); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("read cache entry for deletion: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM cache_entries
		WHERE integration = ? AND project = ? AND compatibility = ? AND native_key = ? AND version = ? AND ref_scope = ?`, keyArgs(key)...); err != nil {
		return fmt.Errorf("delete cache entry: %w", err)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM artifacts WHERE digest = ?
		AND NOT EXISTS (SELECT 1 FROM cache_entries WHERE digest = ?)`, digest, digest)
	if err != nil {
		return fmt.Errorf("release unreferenced artifact: %w", err)
	}
	removedArtifact, _ := result.RowsAffected()
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit cache entry deletion: %w", err)
	}
	if removedArtifact == 1 {
		if err := os.Remove(store.blobPath(digest)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("delete unreferenced artifact bytes: %w", err)
		}
	}
	return nil
}

func (store *Store) GC(ctx context.Context, targetBytes int64) error {
	if targetBytes < 0 {
		return errors.New("garbage collection target cannot be negative")
	}
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	return store.evictTo(ctx, targetBytes)
}

func (store *Store) evictTo(ctx context.Context, targetBytes int64) error {
	for {
		usage, err := store.Usage(ctx)
		if err != nil {
			return err
		}
		if usage <= targetBytes {
			return nil
		}
		removed, err := store.evictOldest(ctx, nil)
		if err != nil {
			return err
		}
		if !removed {
			return ErrQuota
		}
	}
}

func (store *Store) evictOldest(ctx context.Context, protected *Key) (bool, error) {
	var key Key
	var digest string
	query := `SELECT entry.integration, entry.project, entry.compatibility, entry.native_key, entry.version, entry.ref_scope, entry.digest
		FROM cache_entries AS entry
		WHERE NOT EXISTS (
			SELECT 1 FROM cache_entry_pins AS pin
			WHERE pin.integration = entry.integration
				AND pin.project = entry.project
				AND pin.compatibility = entry.compatibility
				AND pin.native_key = entry.native_key
				AND pin.version = entry.version
				AND pin.ref_scope = entry.ref_scope
		)`
	var args []any
	if protected != nil {
		query += ` AND NOT (entry.integration = ? AND entry.project = ? AND entry.compatibility = ? AND entry.native_key = ? AND entry.version = ? AND entry.ref_scope = ?)`
		args = keyArgs(*protected)
	}
	query += ` ORDER BY entry.last_accessed_at ASC, entry.created_at ASC LIMIT 1`
	err := store.db.QueryRowContext(ctx, query, args...).
		Scan(&key.Integration, &key.Project, &key.Compatibility, &key.Native, &key.Version, &key.Ref, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("select Local Cache eviction victim: %w", err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM cache_entries
			WHERE integration = ? AND project = ? AND compatibility = ? AND native_key = ? AND version = ? AND ref_scope = ?`, keyArgs(key)...); err != nil {
		tx.Rollback()
		return false, fmt.Errorf("evict Local Cache entry: %w", err)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM artifacts WHERE digest = ?
			AND NOT EXISTS (SELECT 1 FROM cache_entries WHERE digest = ?)`, digest, digest)
	if err != nil {
		tx.Rollback()
		return false, fmt.Errorf("release evicted Local Cache artifact: %w", err)
	}
	removedArtifact, _ := result.RowsAffected()
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if removedArtifact == 1 {
		if err := os.Remove(store.blobPath(digest)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("delete evicted Local Cache bytes: %w", err)
		}
	}
	return true, nil
}

func (store *Store) entry(ctx context.Context, key Key, touch bool) (Entry, error) {
	var entry Entry
	entry.Key = key
	var metadataJSON []byte
	var createdAt int64
	err := store.db.QueryRowContext(ctx, `SELECT digest, size, metadata_json, created_at
		FROM cache_entries
		WHERE integration = ? AND project = ? AND compatibility = ? AND native_key = ? AND version = ? AND ref_scope = ?`, keyArgs(key)...).
		Scan(&entry.Digest, &entry.Size, &metadataJSON, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, fmt.Errorf("read cache entry: %w", err)
	}
	if err := json.Unmarshal(metadataJSON, &entry.Metadata); err != nil {
		return Entry{}, fmt.Errorf("decode cache metadata: %w", err)
	}
	entry.CreatedAt = time.Unix(0, createdAt).UTC()
	if touch {
		now := time.Now().UTC().UnixNano()
		_, _ = store.db.ExecContext(ctx, `UPDATE cache_entries SET last_accessed_at = ?
			WHERE integration = ? AND project = ? AND compatibility = ? AND native_key = ? AND version = ? AND ref_scope = ?`, append([]any{now}, keyArgs(key)...)...)
		_, _ = store.db.ExecContext(ctx, `UPDATE artifacts SET last_accessed_at = ? WHERE digest = ?`, now, entry.Digest)
	}
	return entry, nil
}

func (store *Store) ensureCanonicalBlob(stagedPath, digest string, size int64) (bool, error) {
	target := store.blobPath(digest)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return false, fmt.Errorf("create blob directory: %w", err)
	}
	info, err := os.Lstat(target)
	if err == nil && info.Mode().IsRegular() {
		file, openErr := os.Open(target)
		if openErr != nil {
			return false, fmt.Errorf("open canonical artifact for verification: %w", openErr)
		}
		verifyErr := verifyFile(file, digest, size)
		closeErr := file.Close()
		if verifyErr == nil {
			if closeErr != nil {
				return false, fmt.Errorf("close canonical artifact after verification: %w", closeErr)
			}
			return false, nil
		}
		if !errors.Is(verifyErr, ErrCorrupt) {
			return false, verifyErr
		}
	} else if err == nil && info.IsDir() {
		return false, errors.New("canonical artifact path is a directory")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("inspect canonical artifact: %w", err)
	}
	if err := os.Chmod(stagedPath, 0o600); err != nil {
		return false, fmt.Errorf("protect staged artifact bytes: %w", err)
	}
	if err := os.Rename(stagedPath, target); err != nil {
		return false, fmt.Errorf("commit artifact bytes: %w", err)
	}
	return true, nil
}

func (store *Store) blobPath(digest string) string {
	return filepath.Join(store.root, "blobs", "sha256", digest[:2], digest[2:])
}

func (store *Store) removeBlobIfUnreferenced(digest string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var referenced int
	if err := store.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM artifacts WHERE digest = ?)`, digest).Scan(&referenced); err != nil {
		return fmt.Errorf("check blob reference after failed metadata commit: %w", err)
	}
	if referenced != 0 {
		return nil
	}
	if err := os.Remove(store.blobPath(digest)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove blob after failed metadata commit: %w", err)
	}
	return nil
}

func (store *Store) cleanStaging() error {
	entries, err := os.ReadDir(filepath.Join(store.root, "staging"))
	if err != nil {
		return fmt.Errorf("read staging directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(store.root, "staging", entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove abandoned staged artifact: %w", err)
		}
	}
	return nil
}

func (store *Store) reconcileBlobs(ctx context.Context) error {
	referenced := make(map[string]struct{})
	rows, err := store.db.QueryContext(ctx, `SELECT digest FROM artifacts`)
	if err != nil {
		return fmt.Errorf("read referenced artifact digests: %w", err)
	}
	for rows.Next() {
		var digest string
		if err := rows.Scan(&digest); err != nil {
			rows.Close()
			return fmt.Errorf("scan referenced artifact digest: %w", err)
		}
		referenced[digest] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close referenced artifact digests: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate referenced artifact digests: %w", err)
	}

	root := filepath.Join(store.root, "blobs", "sha256")
	shards, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read blob directory: %w", err)
	}
	for _, shard := range shards {
		if !shard.IsDir() || !isLowerHexName(shard.Name(), 2) {
			continue
		}
		shardPath := filepath.Join(root, shard.Name())
		blobs, err := os.ReadDir(shardPath)
		if err != nil {
			return fmt.Errorf("read blob shard %s: %w", shard.Name(), err)
		}
		for _, blob := range blobs {
			if !isLowerHexName(blob.Name(), sha256.Size*2-2) {
				continue
			}
			info, err := blob.Info()
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return fmt.Errorf("inspect blob %s/%s: %w", shard.Name(), blob.Name(), err)
			}
			if !info.Mode().IsRegular() {
				continue
			}
			digest := shard.Name() + blob.Name()
			if _, ok := referenced[digest]; ok {
				continue
			}
			if err := os.Remove(filepath.Join(shardPath, blob.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove orphan blob %s: %w", digest, err)
			}
		}
	}
	return nil
}

func isLowerHexName(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validateKey(key Key) error {
	if key.Integration == "" || key.Project == "" || key.Compatibility == "" || key.Native == "" {
		return errors.New("integration, project, compatibility, and native key are required")
	}
	return nil
}

func keyArgs(key Key) []any {
	return []any{key.Integration, key.Project, key.Compatibility, key.Native, key.Version, key.Ref}
}

func verifyFile(file *os.File, digest string, size int64) error {
	hash := sha256.New()
	written, err := io.Copy(hash, file)
	if err != nil {
		return fmt.Errorf("verify artifact: %w", err)
	}
	if written != size || hex.EncodeToString(hash.Sum(nil)) != digest {
		return ErrCorrupt
	}
	return nil
}

const selectEntryDigest = `SELECT digest FROM cache_entries
	WHERE integration = ? AND project = ? AND compatibility = ? AND native_key = ? AND version = ? AND ref_scope = ?`
