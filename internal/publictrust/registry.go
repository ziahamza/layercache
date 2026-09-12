package publictrust

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	_ "modernc.org/sqlite"
)

const DefaultRetention = 90 * 24 * time.Hour

type RegistryOptions struct {
	Retention time.Duration
	Now       func() time.Time
}

type Registry struct {
	db        *sql.DB
	retention time.Duration
	now       func() time.Time
}

type BuildIdentity struct {
	Repository   string
	Commit       string
	Integration  string
	Target       string
	RecipeDigest string
	Platform     string
	Inputs       []DeclaredInput
}

func OpenRegistry(ctx context.Context, root string) (*Registry, error) {
	return OpenRegistryWithOptions(ctx, root, RegistryOptions{})
}

func OpenRegistryWithOptions(ctx context.Context, root string, options RegistryOptions) (*Registry, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create Public Cache metadata directory: %w", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "publications.db"))
	if err != nil {
		return nil, fmt.Errorf("open Public Cache metadata: %w", err)
	}
	db.SetMaxOpenConns(1)
	if options.Retention <= 0 {
		options.Retention = DefaultRetention
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	registry := &Registry{db: db, retention: options.Retention, now: options.Now}
	if err := registry.initialize(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return registry, nil
}

func (registry *Registry) initialize(ctx context.Context) error {
	for _, statement := range []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS publications (
			identity TEXT PRIMARY KEY,
			public_identity TEXT NOT NULL,
			digest TEXT NOT NULL,
			publication_json BLOB NOT NULL,
			state TEXT NOT NULL,
			reason TEXT NOT NULL,
			updated_at INTEGER NOT NULL,
			last_accessed_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL
		)`,
	} {
		if _, err := registry.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize Public Cache metadata: %w", err)
		}
	}
	for name, definition := range map[string]string{
		"public_identity":  `TEXT NOT NULL DEFAULT ''`,
		"last_accessed_at": `INTEGER NOT NULL DEFAULT 0`,
		"expires_at":       `INTEGER NOT NULL DEFAULT 0`,
	} {
		if err := ensureRegistryColumn(ctx, registry.db, name, definition); err != nil {
			return err
		}
	}
	if err := registry.backfillRetention(ctx); err != nil {
		return err
	}
	if _, err := registry.db.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS publications_public_identity ON publications(public_identity)`); err != nil {
		return fmt.Errorf("index Public Cache publication identity: %w", err)
	}
	return nil
}

func ensureRegistryColumn(ctx context.Context, db *sql.DB, name, definition string) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(publications)`)
	if err != nil {
		return fmt.Errorf("inspect Public Cache metadata schema: %w", err)
	}
	found := false
	for rows.Next() {
		var cid int
		var column, columnType string
		var notNull int
		var defaultValue any
		var primaryKey int
		if err := rows.Scan(&cid, &column, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return fmt.Errorf("inspect Public Cache metadata column: %w", err)
		}
		if column == name {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close Public Cache metadata schema: %w", err)
	}
	if found {
		return nil
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE publications ADD COLUMN `+name+` `+definition); err != nil {
		return fmt.Errorf("add Public Cache metadata column %s: %w", name, err)
	}
	return nil
}

func (registry *Registry) backfillRetention(ctx context.Context) error {
	rows, err := registry.db.QueryContext(ctx, `
		SELECT identity, publication_json, updated_at
		FROM publications
		WHERE public_identity = '' OR last_accessed_at = 0 OR expires_at = 0`)
	if err != nil {
		return fmt.Errorf("find legacy Public Cache metadata: %w", err)
	}
	type legacyRecord struct {
		identity       string
		publicIdentity string
		updatedAt      int64
		expiresAt      int64
	}
	var records []legacyRecord
	for rows.Next() {
		var identity string
		var encoded []byte
		var updatedAt int64
		if err := rows.Scan(&identity, &encoded, &updatedAt); err != nil {
			_ = rows.Close()
			return fmt.Errorf("decode legacy Public Cache metadata: %w", err)
		}
		var publication Publication
		if err := json.Unmarshal(encoded, &publication); err != nil {
			_ = rows.Close()
			return fmt.Errorf("decode legacy Public Cache publication: %w", err)
		}
		if updatedAt == 0 {
			updatedAt = registry.now().UTC().UnixNano()
		}
		records = append(records, legacyRecord{
			identity: identity, publicIdentity: publication.Identity(), updatedAt: updatedAt,
			expiresAt: time.Unix(0, updatedAt).UTC().Add(registry.retention).UnixNano(),
		})
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close legacy Public Cache metadata: %w", err)
	}
	for _, record := range records {
		if _, err := registry.db.ExecContext(ctx, `
			UPDATE publications SET public_identity = ?, last_accessed_at = ?, expires_at = ?
			WHERE identity = ?`, record.publicIdentity, record.updatedAt, record.expiresAt, record.identity); err != nil {
			return fmt.Errorf("upgrade legacy Public Cache metadata: %w", err)
		}
	}
	return nil
}

func (registry *Registry) Close() error {
	return registry.db.Close()
}

func (registry *Registry) Publish(ctx context.Context, publication Publication) error {
	return registry.publishAt(ctx, publication, registry.now().UTC())
}

func (registry *Registry) publishAt(ctx context.Context, publication Publication, now time.Time) error {
	if err := validatePublication(publication); err != nil {
		return err
	}
	cacheIdentity := publication.CacheIdentity()
	publicIdentity := publication.Identity()
	encoded, err := json.Marshal(publication)
	if err != nil {
		return fmt.Errorf("encode Public Cache publication: %w", err)
	}
	tx, err := registry.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Public Cache publication: %w", err)
	}
	defer tx.Rollback()
	var storedPublicIdentity, digest, state string
	err = tx.QueryRowContext(ctx, `SELECT public_identity, digest, state FROM publications WHERE identity = ?`, cacheIdentity).Scan(&storedPublicIdentity, &digest, &state)
	expiresAt := now.Add(registry.retention).UnixNano()
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `INSERT INTO publications(
			identity, public_identity, digest, publication_json, state, reason,
			updated_at, last_accessed_at, expires_at
		) VALUES(?, ?, ?, ?, 'active', '', ?, ?, ?)`,
			cacheIdentity, publicIdentity, publication.Digest, encoded,
			now.UnixNano(), now.UnixNano(), expiresAt)
		if err != nil {
			return fmt.Errorf("record Public Cache publication: %w", err)
		}
		return commitRegistryTransaction(tx, "commit Public Cache publication")
	}
	if err != nil {
		return fmt.Errorf("inspect Public Cache publication: %w", err)
	}
	if state == "revoked" {
		return ErrRevoked
	}
	if state == "ambiguous" {
		return ErrAmbiguous
	}
	if digest != publication.Digest {
		if _, err := tx.ExecContext(ctx, `
			UPDATE publications SET state = 'ambiguous', reason = ?, updated_at = ? WHERE identity = ?`,
			"trusted builds produced different artifact digests", now.UnixNano(), cacheIdentity); err != nil {
			return fmt.Errorf("mark Public Cache publication ambiguous: %w", err)
		}
		if err := commitRegistryTransaction(tx, "commit ambiguous Public Cache publication"); err != nil {
			return err
		}
		return ErrAmbiguous
	}
	if state == "expired" {
		if _, err := tx.ExecContext(ctx, `
			UPDATE publications
			SET public_identity = ?, publication_json = ?, state = 'active', reason = '',
				updated_at = ?, last_accessed_at = ?, expires_at = ?
			WHERE identity = ?`, publicIdentity, encoded, now.UnixNano(), now.UnixNano(), expiresAt, cacheIdentity); err != nil {
			return fmt.Errorf("replace expired Public Cache publication: %w", err)
		}
		return commitRegistryTransaction(tx, "commit replacement Public Cache publication")
	}
	if storedPublicIdentity != publicIdentity {
		return ErrIdentity
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE publications
		SET state = 'active', reason = '', updated_at = ?, last_accessed_at = ?, expires_at = ?
		WHERE identity = ?`, now.UnixNano(), now.UnixNano(), expiresAt, cacheIdentity); err != nil {
		return fmt.Errorf("refresh Public Cache publication: %w", err)
	}
	return commitRegistryTransaction(tx, "commit Public Cache publication refresh")
}

// MarkAmbiguous records a trusted conflicting digest even when the artifact
// layer rejected the later bytes before they could become visible.
func (registry *Registry) MarkAmbiguous(ctx context.Context, cacheIdentity, incomingDigest string) error {
	now := registry.now().UTC()
	result, err := registry.db.ExecContext(ctx, `
		UPDATE publications
		SET state = CASE WHEN digest = ? THEN state ELSE 'ambiguous' END,
			reason = CASE WHEN digest = ? THEN reason ELSE ? END,
			updated_at = ?
		WHERE identity = ? AND state NOT IN ('revoked', 'ambiguous')`,
		incomingDigest, incomingDigest, "trusted builds produced different artifact digests", now.UnixNano(), cacheIdentity)
	if err != nil {
		return fmt.Errorf("record conflicting Public Cache publication: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect conflicting Public Cache publication: %w", err)
	}
	if rows == 0 {
		var state string
		if err := registry.db.QueryRowContext(ctx, `SELECT state FROM publications WHERE identity = ?`, cacheIdentity).Scan(&state); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("inspect conflicting Public Cache identity: %w", err)
		}
		if state == "revoked" {
			return ErrRevoked
		}
		return ErrAmbiguous
	}
	return nil
}

func (registry *Registry) Resolve(ctx context.Context, identity string) (Publication, error) {
	return registry.resolveAt(ctx, identity, registry.now().UTC())
}

// ActivePublications returns the embedded registry entries whose retention
// window is still live without extending that window. The server uses this
// snapshot to rebuild durable artifact pins after a restart.
func (registry *Registry) ActivePublications(ctx context.Context) ([]Publication, error) {
	now := registry.now().UTC().UnixNano()
	rows, err := registry.db.QueryContext(ctx, `
		SELECT publication_json FROM publications
		WHERE state = 'active' AND expires_at > ?
		ORDER BY identity`, now)
	if err != nil {
		return nil, fmt.Errorf("list active Public Cache publications: %w", err)
	}
	defer rows.Close()
	var publications []Publication
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			return nil, fmt.Errorf("read active Public Cache publication: %w", err)
		}
		var publication Publication
		if err := json.Unmarshal(encoded, &publication); err != nil {
			return nil, fmt.Errorf("decode active Public Cache publication: %w", err)
		}
		if err := validatePublication(publication); err != nil {
			return nil, fmt.Errorf("validate active Public Cache publication: %w", err)
		}
		publications = append(publications, publication)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active Public Cache publications: %w", err)
	}
	return publications, nil
}

func (registry *Registry) resolveAt(ctx context.Context, identity string, now time.Time) (Publication, error) {
	tx, err := registry.db.BeginTx(ctx, nil)
	if err != nil {
		return Publication{}, fmt.Errorf("begin Public Cache resolution: %w", err)
	}
	defer tx.Rollback()
	var cacheIdentity string
	var encoded []byte
	var state string
	var expiresAt int64
	err = tx.QueryRowContext(ctx, `
		SELECT identity, publication_json, state, expires_at
		FROM publications WHERE identity = ? OR public_identity = ?`, identity, identity,
	).Scan(&cacheIdentity, &encoded, &state, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Publication{}, ErrNotFound
	}
	if err != nil {
		return Publication{}, fmt.Errorf("resolve Public Cache publication: %w", err)
	}
	switch state {
	case "revoked":
		return Publication{}, ErrRevoked
	case "ambiguous":
		return Publication{}, ErrAmbiguous
	case "expired":
		var publication Publication
		if err := json.Unmarshal(encoded, &publication); err != nil {
			return Publication{}, fmt.Errorf("decode expired Public Cache publication: %w", err)
		}
		return publication, ErrExpired
	case "active":
	default:
		return Publication{}, fmt.Errorf("unknown Public Cache publication state %q", state)
	}
	if expiresAt <= now.UnixNano() {
		var publication Publication
		if err := json.Unmarshal(encoded, &publication); err != nil {
			return Publication{}, fmt.Errorf("decode expiring Public Cache publication: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE publications SET state = 'expired', reason = ?, updated_at = ? WHERE identity = ?`,
			"Public Cache publication exceeded idle retention", now.UnixNano(), cacheIdentity); err != nil {
			return Publication{}, fmt.Errorf("expire Public Cache publication: %w", err)
		}
		if err := commitRegistryTransaction(tx, "commit Public Cache publication expiry"); err != nil {
			return Publication{}, err
		}
		return publication, ErrExpired
	}
	var publication Publication
	if err := json.Unmarshal(encoded, &publication); err != nil {
		return Publication{}, fmt.Errorf("decode Public Cache publication: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE publications SET last_accessed_at = ?, expires_at = ? WHERE identity = ?`,
		now.UnixNano(), now.Add(registry.retention).UnixNano(), cacheIdentity); err != nil {
		return Publication{}, fmt.Errorf("touch Public Cache publication: %w", err)
	}
	if err := commitRegistryTransaction(tx, "commit Public Cache publication access"); err != nil {
		return Publication{}, err
	}
	return publication, nil
}

func (registry *Registry) FindBuild(ctx context.Context, identity BuildIdentity) (Publication, error) {
	rows, err := registry.db.QueryContext(ctx, `SELECT publication_json FROM publications WHERE state = 'active' ORDER BY updated_at DESC`)
	if err != nil {
		return Publication{}, fmt.Errorf("find Public Cache build publication: %w", err)
	}
	var match *Publication
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			_ = rows.Close()
			return Publication{}, fmt.Errorf("decode Public Cache build publication: %w", err)
		}
		var publication Publication
		if err := json.Unmarshal(encoded, &publication); err != nil {
			_ = rows.Close()
			return Publication{}, fmt.Errorf("decode Public Cache build publication: %w", err)
		}
		if publication.Repository == identity.Repository && publication.Commit == identity.Commit &&
			publication.Integration == identity.Integration && publication.Target == identity.Target &&
			publication.RecipeDigest == identity.RecipeDigest && publication.Platform == identity.Platform &&
			slices.Equal(publication.Inputs, identity.Inputs) {
			candidate := publication
			match = &candidate
			break
		}
	}
	if err := rows.Close(); err != nil {
		return Publication{}, fmt.Errorf("close Public Cache build publication search: %w", err)
	}
	if match == nil {
		return Publication{}, ErrNotFound
	}
	return registry.Resolve(ctx, match.CacheIdentity())
}

func (registry *Registry) Revoke(ctx context.Context, identity, reason string, now time.Time) error {
	result, err := registry.db.ExecContext(ctx, `
		UPDATE publications SET state = 'revoked', reason = ?, updated_at = ?
		WHERE identity = ? OR public_identity = ?`, reason, now.UTC().UnixNano(), identity, identity)
	if err != nil {
		return fmt.Errorf("revoke Public Cache publication: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect Public Cache revocation: %w", err)
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// Retire marks one exact active publication replaceable after its durable
// build state or artifact bytes have become unavailable. It will not retire a
// newer publication that won a concurrent repair.
func (registry *Registry) Retire(
	ctx context.Context,
	cacheIdentity string,
	publicIdentity string,
	reason string,
	now time.Time,
) error {
	tx, err := registry.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Public Cache publication retirement: %w", err)
	}
	defer tx.Rollback()
	var storedPublicIdentity, state string
	err = tx.QueryRowContext(ctx, `
		SELECT public_identity, state FROM publications WHERE identity = ?`, cacheIdentity,
	).Scan(&storedPublicIdentity, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("inspect Public Cache publication retirement: %w", err)
	}
	if storedPublicIdentity != publicIdentity {
		return ErrIdentity
	}
	switch state {
	case "expired":
		return nil
	case "revoked":
		return ErrRevoked
	case "ambiguous":
		return ErrAmbiguous
	case "active":
	default:
		return fmt.Errorf("unknown Public Cache publication state %q", state)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE publications SET state = 'expired', reason = ?, updated_at = ?
		WHERE identity = ? AND public_identity = ? AND state = 'active'`,
		reason, now.UTC().UnixNano(), cacheIdentity, publicIdentity,
	); err != nil {
		return fmt.Errorf("retire Public Cache publication: %w", err)
	}
	return commitRegistryTransaction(tx, "commit Public Cache publication retirement")
}

func commitRegistryTransaction(tx *sql.Tx, operation string) error {
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return nil
}
