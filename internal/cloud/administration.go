package cloud

import (
	"context"
	"errors"
	"math"
	"strconv"

	"github.com/layercache/layercache/internal/artifact"
)

const administratorPinNamespace = "administrator"

type Quota struct {
	Project          string `json:"project"`
	MaxBytes         int64  `json:"maxBytes"`
	UsedBytes        int64  `json:"usedBytes"`
	MetadataMaxBytes int64  `json:"metadataMaxBytes"`
	MetadataBytes    int64  `json:"metadataBytes"`
}

// Quota reads the authoritative project limit, including changes made by a
// different server. Startup configuration initializes a new project only.
func (store *Store) Quota(ctx context.Context) (Quota, error) {
	result := Quota{Project: store.config.Project}
	err := store.database.QueryRowContext(ctx, `SELECT quota_bytes, used_bytes, metadata_quota_bytes, metadata_bytes
		FROM layercache_projects_v1 WHERE project_id = $1`, store.config.Project).
		Scan(&result.MaxBytes, &result.UsedBytes, &result.MetadataMaxBytes, &result.MetadataBytes)
	return result, store.safeError("read project quota", err)
}

// SetQuota changes the limit and evicts eligible entries in one transaction.
// If pins prevent the requested shrink, both the eviction and limit roll back.
func (store *Store) SetQuota(ctx context.Context, actor string, maxBytes int64) (Quota, error) {
	if err := validateIdentity("quota actor", actor, 512); err != nil {
		return Quota{}, err
	}
	if maxBytes <= 0 || maxBytes == math.MaxInt64 {
		return Quota{}, errors.New("quota must be a positive bounded integer")
	}
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return Quota{}, store.safeError("begin quota change", err)
	}
	defer tx.Rollback()
	usage, err := store.lockProject(ctx, tx)
	if err != nil {
		return Quota{}, err
	}
	old := usage.quota
	usage.quota, usage.metadataQuota = maxBytes, metadataBudget(maxBytes)
	if _, err := tx.ExecContext(ctx, `UPDATE layercache_projects_v1 SET quota_bytes=$2, metadata_quota_bytes=$3, updated_at=$4 WHERE project_id=$1`,
		store.config.Project, maxBytes, usage.metadataQuota, store.config.Now().UTC()); err != nil {
		return Quota{}, store.safeError("set project quota", err)
	}
	if err := store.evictToFit(ctx, tx, &usage, 0, 0, ""); err != nil {
		return Quota{}, err
	}
	if err := store.appendAuditTx(ctx, tx, AuditEvent{
		Project: store.config.Project, Actor: actor, Action: "quota.set", Resource: "project", Outcome: "allowed",
		Attributes: map[string]string{"previousBytes": strconv.FormatInt(old, 10), "maxBytes": strconv.FormatInt(maxBytes, 10)},
		CreatedAt:  store.config.Now().UTC(),
	}); err != nil {
		return Quota{}, err
	}
	if err := tx.Commit(); err != nil {
		return Quota{}, store.safeError("commit project quota", err)
	}
	return Quota{Project: store.config.Project, MaxBytes: maxBytes, UsedBytes: usage.used, MetadataMaxBytes: usage.metadataQuota, MetadataBytes: usage.metadataUsed}, nil
}

func (store *Store) SetAdministratorPin(ctx context.Context, actor string, pin artifact.Pin) error {
	if err := validateIdentity("pin actor", actor, 512); err != nil {
		return err
	}
	return store.pin(ctx, administratorPinNamespace, pin, actor)
}

func (store *Store) RemoveAdministratorPin(ctx context.Context, actor, owner string) error {
	if err := validateIdentity("pin actor", actor, 512); err != nil {
		return err
	}
	return store.unpin(ctx, administratorPinNamespace, owner, actor)
}

// AdministratorPins excludes system pins; an administrator cannot release an
// upload or Public Build publication lease through this API.
func (store *Store) AdministratorPins(ctx context.Context, after string, limit int) ([]artifact.Pin, error) {
	if limit < 1 || limit > 1000 || len(after) > 256 {
		return nil, errors.New("invalid pin page")
	}
	rows, err := store.database.QueryContext(ctx, `SELECT pin.owner, entry.integration, entry.project_id, entry.compatibility,
		entry.native_key, entry.version, entry.ref_scope, entry.digest, entry.size_bytes
		FROM layercache_entry_pins_v1 pin JOIN layercache_entries_v1 entry ON entry.entry_id=pin.entry_id
		WHERE pin.project_id=$1 AND pin.namespace=$2 AND pin.owner > $3 ORDER BY pin.owner LIMIT $4`,
		store.config.Project, administratorPinNamespace, after, limit)
	if err != nil {
		return nil, store.safeError("list administrator pins", err)
	}
	defer rows.Close()
	result := make([]artifact.Pin, 0)
	for rows.Next() {
		var pin artifact.Pin
		if err := rows.Scan(&pin.Owner, &pin.Key.Integration, &pin.Key.Project, &pin.Key.Compatibility, &pin.Key.Native, &pin.Key.Version, &pin.Key.Ref, &pin.Digest, &pin.Size); err != nil {
			return nil, store.safeError("read administrator pin", err)
		}
		result = append(result, pin)
	}
	return result, store.safeError("iterate administrator pins", rows.Err())
}
