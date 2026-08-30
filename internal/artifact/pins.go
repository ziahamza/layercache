package artifact

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Pin keeps one complete Local Cache entry available while its owner still
// needs the bytes. Namespace lets each subsystem reconcile only its own pins.
type Pin struct {
	Owner  string
	Key    Key
	Digest string
	Size   int64
}

func (store *Store) Pin(ctx context.Context, namespace string, pin Pin) error {
	if err := validatePin(namespace, pin); err != nil {
		return err
	}
	store.writeMu.Lock()
	defer store.writeMu.Unlock()

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Local Cache pin: %w", err)
	}
	defer tx.Rollback()
	var digest string
	var size int64
	err = tx.QueryRowContext(ctx, `SELECT digest, size FROM cache_entries
		WHERE integration = ? AND project = ? AND compatibility = ? AND native_key = ? AND version = ? AND ref_scope = ?`, keyArgs(pin.Key)...).
		Scan(&digest, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read Local Cache pin target: %w", err)
	}
	if digest != pin.Digest || size != pin.Size {
		return ErrConflict
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO cache_entry_pins(
		namespace, owner, integration, project, compatibility, native_key, version, ref_scope, digest, size
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, namespace, pin.Owner,
		pin.Key.Integration, pin.Key.Project, pin.Key.Compatibility, pin.Key.Native, pin.Key.Version, pin.Key.Ref,
		pin.Digest, pin.Size)
	if err != nil {
		return fmt.Errorf("pin Local Cache entry: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("confirm Local Cache pin: %w", err)
	}
	if inserted == 0 {
		var existing Pin
		existing.Owner = pin.Owner
		err := tx.QueryRowContext(ctx, `SELECT integration, project, compatibility, native_key, version, ref_scope, digest, size
			FROM cache_entry_pins WHERE namespace = ? AND owner = ?`, namespace, pin.Owner).
			Scan(&existing.Key.Integration, &existing.Key.Project, &existing.Key.Compatibility, &existing.Key.Native,
				&existing.Key.Version, &existing.Key.Ref, &existing.Digest, &existing.Size)
		if err != nil {
			return fmt.Errorf("read existing Local Cache pin: %w", err)
		}
		if existing != pin {
			return ErrConflict
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Local Cache pin: %w", err)
	}
	return nil
}

// Unpin releases one owner's retention claim. It is safe to repeat after a
// completion whose outcome was already persisted.
func (store *Store) Unpin(ctx context.Context, namespace, owner string) error {
	if err := validatePinIdentity("pin namespace", namespace); err != nil {
		return err
	}
	if err := validatePinIdentity("pin owner", owner); err != nil {
		return err
	}
	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	if _, err := store.db.ExecContext(ctx, `DELETE FROM cache_entry_pins WHERE namespace = ? AND owner = ?`, namespace, owner); err != nil {
		return fmt.Errorf("release Local Cache pin: %w", err)
	}
	return nil
}

// ReconcilePins atomically replaces one subsystem's pins with the complete
// set of owners it recovered from durable work. Pins whose exact entry is no
// longer present are skipped so stale work cannot pin unrelated bytes.
func (store *Store) ReconcilePins(ctx context.Context, namespace string, pins []Pin) (int, error) {
	if err := validatePinIdentity("pin namespace", namespace); err != nil {
		return 0, err
	}
	seen := make(map[string]Pin, len(pins))
	for _, pin := range pins {
		if err := validatePin(namespace, pin); err != nil {
			return 0, err
		}
		if existing, ok := seen[pin.Owner]; ok && existing != pin {
			return 0, ErrConflict
		}
		seen[pin.Owner] = pin
	}

	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin Local Cache pin reconciliation: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM cache_entry_pins WHERE namespace = ?`, namespace); err != nil {
		return 0, fmt.Errorf("clear stale Local Cache pins: %w", err)
	}
	pinned := 0
	for _, pin := range seen {
		var digest string
		var size int64
		err := tx.QueryRowContext(ctx, `SELECT digest, size FROM cache_entries
			WHERE integration = ? AND project = ? AND compatibility = ? AND native_key = ? AND version = ? AND ref_scope = ?`, keyArgs(pin.Key)...).
			Scan(&digest, &size)
		if errors.Is(err, sql.ErrNoRows) || err == nil && (digest != pin.Digest || size != pin.Size) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("read reconciled Local Cache pin target: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO cache_entry_pins(
			namespace, owner, integration, project, compatibility, native_key, version, ref_scope, digest, size
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, namespace, pin.Owner,
			pin.Key.Integration, pin.Key.Project, pin.Key.Compatibility, pin.Key.Native, pin.Key.Version, pin.Key.Ref,
			pin.Digest, pin.Size); err != nil {
			return 0, fmt.Errorf("reconcile Local Cache pin: %w", err)
		}
		pinned++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit Local Cache pin reconciliation: %w", err)
	}
	return pinned, nil
}

func validatePin(namespace string, pin Pin) error {
	if err := validatePinIdentity("pin namespace", namespace); err != nil {
		return err
	}
	if err := validatePinIdentity("pin owner", pin.Owner); err != nil {
		return err
	}
	if err := validateKey(pin.Key); err != nil {
		return err
	}
	if !isLowerHexName(pin.Digest, 64) {
		return errors.New("pin digest must be a lowercase SHA-256 digest")
	}
	if pin.Size < 0 {
		return errors.New("pin size cannot be negative")
	}
	return nil
}

func validatePinIdentity(name, value string) error {
	if value == "" || strings.TrimSpace(value) != value {
		return fmt.Errorf("%s must be non-empty and trimmed", name)
	}
	if len(value) > 256 {
		return fmt.Errorf("%s exceeds 256 bytes", name)
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%s contains control characters", name)
	}
	return nil
}
