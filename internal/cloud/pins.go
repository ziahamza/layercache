package cloud

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/layercache/layercache/internal/artifact"
)

func (store *Store) Pin(ctx context.Context, namespace string, pin artifact.Pin) error {
	return store.pin(ctx, namespace, pin, "")
}

func (store *Store) pin(ctx context.Context, namespace string, pin artifact.Pin, actor string) error {
	if err := store.validatePin(namespace, pin); err != nil {
		return err
	}
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return store.safeError("begin cloud cache pin", err)
	}
	defer transaction.Rollback()
	if _, err := store.lockProjectShared(ctx, transaction); err != nil {
		return err
	}
	entryID, err := store.pinTarget(ctx, transaction, pin)
	if err != nil {
		return err
	}
	result, err := transaction.ExecContext(ctx, `
		INSERT INTO layercache_entry_pins_v1(project_id, namespace, owner, entry_id, created_at)
		VALUES($1, $2, $3, $4, $5)
		ON CONFLICT(project_id, namespace, owner) DO NOTHING`,
		store.config.Project, namespace, pin.Owner, entryID, store.config.Now().UTC())
	if err != nil {
		return store.safeError("pin cloud cache entry", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return store.safeError("confirm cloud cache pin", err)
	}
	if inserted == 0 {
		var existingEntryID int64
		if err := transaction.QueryRowContext(ctx, `
			SELECT entry_id FROM layercache_entry_pins_v1
			WHERE project_id = $1 AND namespace = $2 AND owner = $3`,
			store.config.Project, namespace, pin.Owner).Scan(&existingEntryID); err != nil {
			return store.safeError("read existing cloud cache pin", err)
		}
		if existingEntryID != entryID {
			return ErrConflict
		}
	}
	if actor != "" {
		if err := store.appendAuditTx(ctx, transaction, AuditEvent{Project: store.config.Project, Actor: actor,
			Action: "cache.pin", Resource: cacheEntryAuditResource(pin.Key), Outcome: "allowed", CreatedAt: store.config.Now().UTC()}); err != nil {
			return err
		}
	}
	if err := transaction.Commit(); err != nil {
		return store.safeError("commit cloud cache pin", err)
	}
	return nil
}

func (store *Store) Unpin(ctx context.Context, namespace, owner string) error {
	return store.unpin(ctx, namespace, owner, "")
}

func (store *Store) unpin(ctx context.Context, namespace, owner, actor string) error {
	if err := validateIdentity("pin namespace", namespace, 256); err != nil {
		return err
	}
	if err := validateIdentity("pin owner", owner, 256); err != nil {
		return err
	}
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return store.safeError("begin cache unpin", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM layercache_entry_pins_v1
		WHERE project_id = $1 AND namespace = $2 AND owner = $3`,
		store.config.Project, namespace, owner); err != nil {
		return store.safeError("release cloud cache pin", err)
	}
	if actor != "" {
		if err := store.appendAuditTx(ctx, tx, AuditEvent{Project: store.config.Project, Actor: actor,
			Action: "cache.unpin", Resource: "administrator-pin", Outcome: "allowed", CreatedAt: store.config.Now().UTC()}); err != nil {
			return err
		}
	}
	return store.safeError("commit cache unpin", tx.Commit())
}

func (store *Store) ReconcilePins(ctx context.Context, namespace string, pins []artifact.Pin) (int, error) {
	if err := validateIdentity("pin namespace", namespace, 256); err != nil {
		return 0, err
	}
	seen := make(map[string]artifact.Pin, len(pins))
	for _, pin := range pins {
		if err := store.validatePin(namespace, pin); err != nil {
			return 0, err
		}
		if existing, found := seen[pin.Owner]; found && existing != pin {
			return 0, ErrConflict
		}
		seen[pin.Owner] = pin
	}
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return 0, store.safeError("begin cloud cache pin reconciliation", err)
	}
	defer transaction.Rollback()
	if _, err := store.lockProject(ctx, transaction); err != nil {
		return 0, err
	}
	if _, err := transaction.ExecContext(ctx, `
		DELETE FROM layercache_entry_pins_v1 WHERE project_id = $1 AND namespace = $2`,
		store.config.Project, namespace); err != nil {
		return 0, store.safeError("clear stale cloud cache pins", err)
	}
	pinned := 0
	for _, pin := range seen {
		entryID, err := store.pinTarget(ctx, transaction, pin)
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if _, err := transaction.ExecContext(ctx, `
			INSERT INTO layercache_entry_pins_v1(project_id, namespace, owner, entry_id, created_at)
			VALUES($1, $2, $3, $4, $5)`,
			store.config.Project, namespace, pin.Owner, entryID, store.config.Now().UTC()); err != nil {
			return 0, store.safeError("reconcile cloud cache pin", err)
		}
		pinned++
	}
	if err := transaction.Commit(); err != nil {
		return 0, store.safeError("commit cloud cache pin reconciliation", err)
	}
	return pinned, nil
}

func (store *Store) pinTarget(ctx context.Context, transaction *sql.Tx, pin artifact.Pin) (int64, error) {
	var entryID int64
	var digest string
	var size int64
	err := transaction.QueryRowContext(ctx, `
		SELECT entry_id, digest, size_bytes
		FROM layercache_entries_v1
		WHERE project_id = $1 AND integration = $2 AND compatibility = $3
			AND native_key = $4 AND version = $5 AND ref_scope = $6`,
		pin.Key.Project, pin.Key.Integration, pin.Key.Compatibility, pin.Key.Native, pin.Key.Version, pin.Key.Ref).
		Scan(&entryID, &digest, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, store.safeError("read cloud cache pin target", err)
	}
	if digest != pin.Digest || size != pin.Size {
		return 0, ErrConflict
	}
	return entryID, nil
}

func (store *Store) validatePin(namespace string, pin artifact.Pin) error {
	if err := validateIdentity("pin namespace", namespace, 256); err != nil {
		return err
	}
	if err := validateIdentity("pin owner", pin.Owner, 256); err != nil {
		return err
	}
	if err := store.validateKey(pin.Key); err != nil {
		return err
	}
	if !validDigest(pin.Digest) {
		return errors.New("pin digest must be a lowercase SHA-256 digest")
	}
	if pin.Size < 0 {
		return fmt.Errorf("pin size cannot be negative")
	}
	return nil
}
