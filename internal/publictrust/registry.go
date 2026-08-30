package publictrust

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Registry struct {
	db *sql.DB
}

func OpenRegistry(ctx context.Context, root string) (*Registry, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create Public Cache metadata directory: %w", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "publications.db"))
	if err != nil {
		return nil, fmt.Errorf("open Public Cache metadata: %w", err)
	}
	db.SetMaxOpenConns(4)
	for _, statement := range []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS publications (
			identity TEXT PRIMARY KEY,
			digest TEXT NOT NULL,
			publication_json BLOB NOT NULL,
			state TEXT NOT NULL,
			reason TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize Public Cache metadata: %w", err)
		}
	}
	return &Registry{db: db}, nil
}

func (registry *Registry) Close() error {
	return registry.db.Close()
}

func (registry *Registry) Publish(ctx context.Context, publication Publication) error {
	if err := validatePublication(publication); err != nil {
		return err
	}
	identity := publication.Identity()
	encoded, err := json.Marshal(publication)
	if err != nil {
		return fmt.Errorf("encode Public Cache publication: %w", err)
	}
	tx, err := registry.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Public Cache publication: %w", err)
	}
	defer tx.Rollback()
	var digest, state string
	err = tx.QueryRowContext(ctx, `SELECT digest, state FROM publications WHERE identity = ?`, identity).Scan(&digest, &state)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `INSERT INTO publications(identity, digest, publication_json, state, reason, updated_at)
			VALUES(?, ?, ?, 'active', '', ?)`, identity, publication.Digest, encoded, time.Now().UTC().UnixNano())
		if err != nil {
			return fmt.Errorf("record Public Cache publication: %w", err)
		}
		return tx.Commit()
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
	if digest == publication.Digest {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE publications SET state = 'ambiguous', reason = ?, updated_at = ? WHERE identity = ?`,
		"trusted builds produced different artifact digests", time.Now().UTC().UnixNano(), identity); err != nil {
		return fmt.Errorf("mark Public Cache publication ambiguous: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit ambiguous Public Cache publication: %w", err)
	}
	return ErrAmbiguous
}

func (registry *Registry) Resolve(ctx context.Context, identity string) (Publication, error) {
	var encoded []byte
	var state string
	err := registry.db.QueryRowContext(ctx, `SELECT publication_json, state FROM publications WHERE identity = ?`, identity).Scan(&encoded, &state)
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
	case "active":
	default:
		return Publication{}, fmt.Errorf("unknown Public Cache publication state %q", state)
	}
	var publication Publication
	if err := json.Unmarshal(encoded, &publication); err != nil {
		return Publication{}, fmt.Errorf("decode Public Cache publication: %w", err)
	}
	return publication, nil
}

func (registry *Registry) Revoke(ctx context.Context, identity, reason string, now time.Time) error {
	result, err := registry.db.ExecContext(ctx, `UPDATE publications SET state = 'revoked', reason = ?, updated_at = ? WHERE identity = ?`, reason, now.UTC().UnixNano(), identity)
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
