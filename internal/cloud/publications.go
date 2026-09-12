package cloud

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/layercache/layercache/internal/publictrust"
)

const DefaultPublicRetention = 90 * 24 * time.Hour

const publicPublicationPinNamespace = "public-publication"

// PublicationRegistry keeps Public Cache trust metadata in PostgreSQL. It does
// not own artifact bytes. A signed publication still has to reference a
// committed Store entry before a gateway can return it.
type PublicationRegistry struct {
	store     *Store
	retention time.Duration
}

func NewPublicationRegistry(store *Store, retention time.Duration) (*PublicationRegistry, error) {
	if store == nil || store.database == nil {
		return nil, errors.New("cloud store is required for Public Cache metadata")
	}
	if retention == 0 {
		retention = DefaultPublicRetention
	}
	if retention < time.Hour {
		return nil, errors.New("Public Cache retention must be at least one hour")
	}
	return &PublicationRegistry{store: store, retention: retention}, nil
}

func (registry *PublicationRegistry) Close() error { return nil }

func (registry *PublicationRegistry) Publish(ctx context.Context, publication publictrust.Publication) error {
	return registry.PublishAs(ctx, "public-build:"+publication.BuildID, publication)
}

func (registry *PublicationRegistry) PublishAs(
	ctx context.Context,
	actor string,
	publication publictrust.Publication,
) error {
	if err := publictrust.ValidatePublication(publication); err != nil {
		return err
	}
	if publication.Project != registry.store.config.Project {
		return publictrust.ErrIdentity
	}
	if err := validateIdentity("Public Cache actor", actor, 512); err != nil {
		return err
	}
	encoded, err := json.Marshal(publication)
	if err != nil {
		return fmt.Errorf("encode Public Cache publication: %w", err)
	}
	now := registry.store.config.Now().UTC()
	expiresAt := now.Add(registry.retention)
	transaction, err := registry.store.database.BeginTx(ctx, nil)
	if err != nil {
		return registry.store.safeError("begin cloud Public Cache publication", err)
	}
	defer transaction.Rollback()
	if _, err := registry.store.lockProject(ctx, transaction); err != nil {
		return err
	}
	cacheIdentity := publication.CacheIdentity()
	publicIdentity := publication.Identity()
	var storedPublicIdentity, digest, state string
	err = transaction.QueryRowContext(ctx, `
		SELECT public_identity, digest, state
		FROM layercache_publications_v1
		WHERE project_id = $1 AND cache_identity = $2
		FOR UPDATE`, publication.Project, cacheIdentity).
		Scan(&storedPublicIdentity, &digest, &state)
	if errors.Is(err, sql.ErrNoRows) {
		if err := registry.pinPublicationEntryTx(ctx, transaction, publication, cacheIdentity, now); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `
			INSERT INTO layercache_publications_v1(
				project_id, cache_identity, public_identity, repository, commit_digest,
				integration, target, recipe_digest, platform, digest, publication_json,
				state, reason, pinned, updated_at, last_accessed_at, expires_at
			) VALUES(
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb,
				'active', '', FALSE, $12, $12, $13
			)`,
			publication.Project, cacheIdentity, publicIdentity, publication.Repository, publication.Commit,
			publication.Integration, publication.Target, publication.RecipeDigest, publication.Platform,
			publication.Digest, string(encoded), now, expiresAt); err != nil {
			return registry.store.safeError("record cloud Public Cache publication", err)
		}
		if err := registry.store.appendAuditTx(ctx, transaction, AuditEvent{
			Project: publication.Project, Actor: actor, Action: "public-cache.publish",
			Resource: "publication:" + publicIdentity, Outcome: "allowed",
			Attributes: map[string]string{"cacheIdentity": cacheIdentity, "digest": publication.Digest}, CreatedAt: now,
		}); err != nil {
			return err
		}
		if err := transaction.Commit(); err != nil {
			return registry.store.safeError("commit cloud Public Cache publication", err)
		}
		return nil
	}
	if err != nil {
		return registry.store.safeError("inspect cloud Public Cache publication", err)
	}
	if state == "revoked" {
		return publictrust.ErrRevoked
	}
	if state == "ambiguous" {
		return publictrust.ErrAmbiguous
	}
	if digest != publication.Digest {
		if _, err := transaction.ExecContext(ctx, `
			UPDATE layercache_publications_v1
			SET state = 'ambiguous', reason = $1, updated_at = $2
			WHERE project_id = $3 AND cache_identity = $4`,
			"trusted builds produced different artifact digests", now, publication.Project, cacheIdentity); err != nil {
			return registry.store.safeError("mark cloud Public Cache publication ambiguous", err)
		}
		if err := registry.unpinPublicationEntryTx(ctx, transaction, cacheIdentity); err != nil {
			return err
		}
		if err := registry.store.appendAuditTx(ctx, transaction, AuditEvent{
			Project: publication.Project, Actor: actor, Action: "public-cache.ambiguous",
			Resource: "cache-publication:" + cacheIdentity, Outcome: "rejected",
			Attributes: map[string]string{"existingDigest": digest, "incomingDigest": publication.Digest}, CreatedAt: now,
		}); err != nil {
			return err
		}
		if err := transaction.Commit(); err != nil {
			return registry.store.safeError("commit ambiguous cloud Public Cache publication", err)
		}
		return publictrust.ErrAmbiguous
	}
	if state == "expired" {
		if err := registry.pinPublicationEntryTx(ctx, transaction, publication, cacheIdentity, now); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `
			UPDATE layercache_publications_v1
			SET public_identity = $1, repository = $2, commit_digest = $3,
				integration = $4, target = $5, recipe_digest = $6, platform = $7,
				publication_json = $8::jsonb, state = 'active', reason = '',
				updated_at = $9, last_accessed_at = $9, expires_at = $10
			WHERE project_id = $11 AND cache_identity = $12`,
			publicIdentity, publication.Repository, publication.Commit, publication.Integration,
			publication.Target, publication.RecipeDigest, publication.Platform, string(encoded), now,
			expiresAt, publication.Project, cacheIdentity); err != nil {
			return registry.store.safeError("replace expired cloud Public Cache publication", err)
		}
	} else {
		if storedPublicIdentity != publicIdentity {
			return publictrust.ErrIdentity
		}
		if err := registry.pinPublicationEntryTx(ctx, transaction, publication, cacheIdentity, now); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, `
			UPDATE layercache_publications_v1
			SET updated_at = $1, last_accessed_at = $1, expires_at = $2
			WHERE project_id = $3 AND cache_identity = $4`,
			now, expiresAt, publication.Project, cacheIdentity); err != nil {
			return registry.store.safeError("refresh cloud Public Cache publication", err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return registry.store.safeError("commit cloud Public Cache publication refresh", err)
	}
	return nil
}

func (registry *PublicationRegistry) MarkAmbiguous(ctx context.Context, cacheIdentity, incomingDigest string) error {
	if !validDigest(incomingDigest) {
		return errors.New("incoming Public Cache digest is invalid")
	}
	now := registry.store.config.Now().UTC()
	transaction, err := registry.store.database.BeginTx(ctx, nil)
	if err != nil {
		return registry.store.safeError("begin cloud Public Cache conflict", err)
	}
	defer transaction.Rollback()
	if _, err := registry.store.lockProject(ctx, transaction); err != nil {
		return err
	}
	var storedDigest, state string
	err = transaction.QueryRowContext(ctx, `
		SELECT digest, state FROM layercache_publications_v1
		WHERE project_id = $1 AND cache_identity = $2 FOR UPDATE`,
		registry.store.config.Project, cacheIdentity).Scan(&storedDigest, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return publictrust.ErrNotFound
	}
	if err != nil {
		return registry.store.safeError("inspect cloud Public Cache conflict", err)
	}
	if state == "revoked" {
		return publictrust.ErrRevoked
	}
	if state == "ambiguous" {
		return publictrust.ErrAmbiguous
	}
	if storedDigest == incomingDigest {
		return nil
	}
	if _, err := transaction.ExecContext(ctx, `
		UPDATE layercache_publications_v1
		SET state = 'ambiguous', reason = $1, updated_at = $2
		WHERE project_id = $3 AND cache_identity = $4`,
		"trusted builds produced different artifact digests", now,
		registry.store.config.Project, cacheIdentity); err != nil {
		return registry.store.safeError("record cloud Public Cache conflict", err)
	}
	if err := registry.unpinPublicationEntryTx(ctx, transaction, cacheIdentity); err != nil {
		return err
	}
	if err := registry.store.appendAuditTx(ctx, transaction, AuditEvent{
		Project: registry.store.config.Project, Actor: "public-collector",
		Action: "public-cache.ambiguous", Resource: "cache-publication:" + cacheIdentity,
		Outcome: "rejected", Attributes: map[string]string{
			"existingDigest": storedDigest, "incomingDigest": incomingDigest,
		}, CreatedAt: now,
	}); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return registry.store.safeError("commit cloud Public Cache conflict", err)
	}
	return publictrust.ErrAmbiguous
}

func (registry *PublicationRegistry) Resolve(ctx context.Context, identity string) (publictrust.Publication, error) {
	if !validDigest(identity) {
		return publictrust.Publication{}, publictrust.ErrNotFound
	}
	transaction, err := registry.store.database.BeginTx(ctx, nil)
	if err != nil {
		return publictrust.Publication{}, registry.store.safeError("begin cloud Public Cache resolution", err)
	}
	defer transaction.Rollback()
	if _, err := registry.store.lockProject(ctx, transaction); err != nil {
		return publictrust.Publication{}, err
	}
	var encoded []byte
	var state, cacheIdentity string
	var pinned bool
	var expiresAt time.Time
	err = transaction.QueryRowContext(ctx, `
		SELECT cache_identity, publication_json, state, pinned, expires_at
		FROM layercache_publications_v1
		WHERE project_id = $1 AND cache_identity = $2
		FOR UPDATE`, registry.store.config.Project, identity).
		Scan(&cacheIdentity, &encoded, &state, &pinned, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = transaction.QueryRowContext(ctx, `
			SELECT cache_identity, publication_json, state, pinned, expires_at
			FROM layercache_publications_v1
			WHERE project_id = $1 AND public_identity = $2
			FOR UPDATE`, registry.store.config.Project, identity).
			Scan(&cacheIdentity, &encoded, &state, &pinned, &expiresAt)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return publictrust.Publication{}, publictrust.ErrNotFound
	}
	if err != nil {
		return publictrust.Publication{}, registry.store.safeError("resolve cloud Public Cache publication", err)
	}
	var publication publictrust.Publication
	if err := json.Unmarshal(encoded, &publication); err != nil {
		return publictrust.Publication{}, fmt.Errorf("decode cloud Public Cache publication: %w", err)
	}
	switch state {
	case "revoked":
		return publictrust.Publication{}, publictrust.ErrRevoked
	case "ambiguous":
		return publictrust.Publication{}, publictrust.ErrAmbiguous
	case "expired":
		return publication, publictrust.ErrExpired
	case "active":
	default:
		return publictrust.Publication{}, errors.New("cloud Public Cache publication has invalid state")
	}
	now := registry.store.config.Now().UTC()
	if !pinned && !expiresAt.After(now) {
		if _, err := transaction.ExecContext(ctx, `
			UPDATE layercache_publications_v1
			SET state = 'expired', reason = $1, updated_at = $2
			WHERE project_id = $3 AND cache_identity = $4`,
			"Public Cache publication exceeded idle retention", now,
			registry.store.config.Project, cacheIdentity); err != nil {
			return publictrust.Publication{}, registry.store.safeError("expire cloud Public Cache publication", err)
		}
		if err := registry.unpinPublicationEntryTx(ctx, transaction, cacheIdentity); err != nil {
			return publictrust.Publication{}, err
		}
		if err := transaction.Commit(); err != nil {
			return publictrust.Publication{}, registry.store.safeError("commit cloud Public Cache expiry", err)
		}
		return publication, publictrust.ErrExpired
	}
	if !pinned {
		expiresAt = now.Add(registry.retention)
	}
	if _, err := transaction.ExecContext(ctx, `
		UPDATE layercache_publications_v1
		SET last_accessed_at = $1, expires_at = $2
		WHERE project_id = $3 AND cache_identity = $4`,
		now, expiresAt, registry.store.config.Project, cacheIdentity); err != nil {
		return publictrust.Publication{}, registry.store.safeError("touch cloud Public Cache publication", err)
	}
	if err := transaction.Commit(); err != nil {
		return publictrust.Publication{}, registry.store.safeError("commit cloud Public Cache access", err)
	}
	return publication, nil
}

// ActivePublications returns the complete live cloud publication set without
// extending idle retention. The server uses it to remove orphan artifact pins
// and recover any missing pins after a process interruption.
func (registry *PublicationRegistry) ActivePublications(ctx context.Context) ([]publictrust.Publication, error) {
	now := registry.store.config.Now().UTC()
	rows, err := registry.store.database.QueryContext(ctx, `
		SELECT publication_json FROM layercache_publications_v1
		WHERE project_id = $1 AND state = 'active' AND (pinned OR expires_at > $2)
		ORDER BY cache_identity`, registry.store.config.Project, now)
	if err != nil {
		return nil, registry.store.safeError("list active cloud Public Cache publications", err)
	}
	defer rows.Close()
	var publications []publictrust.Publication
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			return nil, registry.store.safeError("read active cloud Public Cache publication", err)
		}
		var publication publictrust.Publication
		if err := json.Unmarshal(encoded, &publication); err != nil {
			return nil, fmt.Errorf("decode active cloud Public Cache publication: %w", err)
		}
		if err := publictrust.ValidatePublication(publication); err != nil {
			return nil, fmt.Errorf("validate active cloud Public Cache publication: %w", err)
		}
		publications = append(publications, publication)
	}
	if err := rows.Err(); err != nil {
		return nil, registry.store.safeError("iterate active cloud Public Cache publications", err)
	}
	return publications, nil
}

func (registry *PublicationRegistry) FindBuild(
	ctx context.Context,
	identity publictrust.BuildIdentity,
) (publictrust.Publication, error) {
	rows, err := registry.store.database.QueryContext(ctx, `
		SELECT cache_identity, publication_json
		FROM layercache_publications_v1
		WHERE project_id = $1 AND repository = $2 AND commit_digest = $3
			AND integration = $4 AND target = $5 AND recipe_digest = $6 AND platform = $7
			AND state = 'active'
		ORDER BY updated_at DESC
		`, registry.store.config.Project, identity.Repository, identity.Commit,
		identity.Integration, identity.Target, identity.RecipeDigest, identity.Platform)
	if err != nil {
		return publictrust.Publication{}, registry.store.safeError("find cloud Public Cache build", err)
	}
	defer rows.Close()
	var matchingCacheIdentity string
	for rows.Next() {
		var cacheIdentity string
		var encoded []byte
		if err := rows.Scan(&cacheIdentity, &encoded); err != nil {
			return publictrust.Publication{}, registry.store.safeError("decode cloud Public Cache build", err)
		}
		var publication publictrust.Publication
		if err := json.Unmarshal(encoded, &publication); err != nil {
			return publictrust.Publication{}, registry.store.safeError("decode cloud Public Cache publication", err)
		}
		if slices.Equal(publication.Inputs, identity.Inputs) {
			matchingCacheIdentity = cacheIdentity
			break
		}
	}
	if err := rows.Err(); err != nil {
		return publictrust.Publication{}, registry.store.safeError("find cloud Public Cache build", err)
	}
	if err := rows.Close(); err != nil {
		return publictrust.Publication{}, registry.store.safeError("close cloud Public Cache build lookup", err)
	}
	if matchingCacheIdentity != "" {
		return registry.Resolve(ctx, matchingCacheIdentity)
	}
	return publictrust.Publication{}, publictrust.ErrNotFound
}

func (registry *PublicationRegistry) Revoke(ctx context.Context, identity, reason string, now time.Time) error {
	return registry.RevokeAs(ctx, "cloud-admin", identity, reason, now)
}

// Retire makes one exact unusable publication replaceable without weakening a
// revocation or ambiguity tombstone. The public identity comparison prevents
// a stale repair from retiring a newer publication.
func (registry *PublicationRegistry) Retire(
	ctx context.Context,
	cacheIdentity string,
	publicIdentity string,
	reason string,
	now time.Time,
) error {
	transaction, err := registry.store.database.BeginTx(ctx, nil)
	if err != nil {
		return registry.store.safeError("begin cloud Public Cache publication retirement", err)
	}
	defer transaction.Rollback()
	if _, err := registry.store.lockProject(ctx, transaction); err != nil {
		return err
	}
	var storedPublicIdentity, state string
	err = transaction.QueryRowContext(ctx, `
		SELECT public_identity, state FROM layercache_publications_v1
		WHERE project_id = $1 AND cache_identity = $2 FOR UPDATE`,
		registry.store.config.Project, cacheIdentity,
	).Scan(&storedPublicIdentity, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return publictrust.ErrNotFound
	}
	if err != nil {
		return registry.store.safeError("inspect cloud Public Cache publication retirement", err)
	}
	if storedPublicIdentity != publicIdentity {
		return publictrust.ErrIdentity
	}
	switch state {
	case "expired":
		return nil
	case "revoked":
		return publictrust.ErrRevoked
	case "ambiguous":
		return publictrust.ErrAmbiguous
	case "active":
	default:
		return errors.New("cloud Public Cache publication has invalid state")
	}
	retiredAt := now.UTC()
	if _, err := transaction.ExecContext(ctx, `
		UPDATE layercache_publications_v1
		SET state = 'expired', reason = $1, updated_at = $2
		WHERE project_id = $3 AND cache_identity = $4
			AND public_identity = $5 AND state = 'active'`,
		reason, retiredAt, registry.store.config.Project, cacheIdentity, publicIdentity,
	); err != nil {
		return registry.store.safeError("retire cloud Public Cache publication", err)
	}
	if err := registry.unpinPublicationEntryTx(ctx, transaction, cacheIdentity); err != nil {
		return err
	}
	if err := registry.store.appendAuditTx(ctx, transaction, AuditEvent{
		Project: registry.store.config.Project, Actor: "public-cache-reconciler",
		Action: "public-cache.retire", Resource: "publication:" + publicIdentity,
		Outcome: "allowed", Attributes: map[string]string{"reason": reason}, CreatedAt: retiredAt,
	}); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return registry.store.safeError("commit cloud Public Cache publication retirement", err)
	}
	return nil
}

func (registry *PublicationRegistry) RevokeAs(
	ctx context.Context,
	actor string,
	identity string,
	reason string,
	now time.Time,
) error {
	if err := validateIdentity("Public Cache administrator", actor, 512); err != nil {
		return err
	}
	if !validDigest(identity) {
		return publictrust.ErrNotFound
	}
	if reason == "" || len(reason) > 2048 || strings.ContainsAny(reason, "\x00\r\n") {
		return errors.New("Public Cache revocation reason is invalid")
	}
	transaction, err := registry.store.database.BeginTx(ctx, nil)
	if err != nil {
		return registry.store.safeError("begin cloud Public Cache revocation", err)
	}
	defer transaction.Rollback()
	if _, err := registry.store.lockProject(ctx, transaction); err != nil {
		return err
	}
	var cacheIdentity, publicIdentity string
	err = transaction.QueryRowContext(ctx, `
		SELECT cache_identity, public_identity
		FROM layercache_publications_v1
		WHERE project_id = $1 AND (cache_identity = $2 OR public_identity = $2)
		FOR UPDATE`, registry.store.config.Project, identity).Scan(&cacheIdentity, &publicIdentity)
	if errors.Is(err, sql.ErrNoRows) {
		return publictrust.ErrNotFound
	}
	if err != nil {
		return registry.store.safeError("read cloud Public Cache revocation target", err)
	}
	if _, err := transaction.ExecContext(ctx, `
		UPDATE layercache_publications_v1
		SET state = 'revoked', reason = $1, updated_at = $2
		WHERE project_id = $3 AND cache_identity = $4`,
		reason, now.UTC(), registry.store.config.Project, cacheIdentity); err != nil {
		return registry.store.safeError("revoke cloud Public Cache publication", err)
	}
	if err := registry.unpinPublicationEntryTx(ctx, transaction, cacheIdentity); err != nil {
		return err
	}
	if err := registry.store.appendAuditTx(ctx, transaction, AuditEvent{
		Project: registry.store.config.Project, Actor: actor, Action: "public-cache.revoke",
		Resource: "publication:" + publicIdentity, Outcome: "allowed",
		CreatedAt: now.UTC(),
	}); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return registry.store.safeError("commit cloud Public Cache revocation", err)
	}
	return nil
}

func (registry *PublicationRegistry) SetPinned(ctx context.Context, identity string, pinned bool, actor string) error {
	if !validDigest(identity) {
		return publictrust.ErrNotFound
	}
	if err := validateIdentity("Public Cache administrator", actor, 512); err != nil {
		return err
	}
	now := registry.store.config.Now().UTC()
	transaction, err := registry.store.database.BeginTx(ctx, nil)
	if err != nil {
		return registry.store.safeError("begin cloud Public Cache retention change", err)
	}
	defer transaction.Rollback()
	result, err := transaction.ExecContext(ctx, `
		UPDATE layercache_publications_v1 SET pinned = $1, updated_at = $2
		WHERE project_id = $3 AND (cache_identity = $4 OR public_identity = $4)`,
		pinned, now, registry.store.config.Project, identity)
	if err != nil {
		return registry.store.safeError("set cloud Public Cache retention", err)
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		return publictrust.ErrNotFound
	}
	if err := registry.store.appendAuditTx(ctx, transaction, AuditEvent{
		Project: registry.store.config.Project, Actor: actor, Action: "public-cache.pin",
		Resource: "publication:" + identity, Outcome: "allowed",
		Attributes: map[string]string{"pinned": fmt.Sprintf("%t", pinned)}, CreatedAt: now,
	}); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return registry.store.safeError("commit cloud Public Cache retention", err)
	}
	return nil
}

func (registry *PublicationRegistry) pinPublicationEntryTx(
	ctx context.Context,
	transaction *sql.Tx,
	publication publictrust.Publication,
	owner string,
	now time.Time,
) error {
	var entryID int64
	err := transaction.QueryRowContext(ctx, `
		SELECT entry_id FROM layercache_entries_v1
		WHERE project_id = $1 AND integration = $2 AND compatibility = $3
			AND native_key = $4 AND version = '' AND ref_scope = ''
			AND digest = $5 AND size_bytes = $6
		FOR UPDATE`, publication.Project, publication.Integration, publication.Compatibility,
		publication.NativeKey, publication.Digest, publication.Size).Scan(&entryID)
	if errors.Is(err, sql.ErrNoRows) {
		return publictrust.ErrNotFound
	}
	if err != nil {
		return registry.store.safeError("find cloud Public Cache publication artifact", err)
	}
	_, err = transaction.ExecContext(ctx, `
		INSERT INTO layercache_entry_pins_v1(project_id, namespace, owner, entry_id, created_at)
		VALUES($1, $2, $3, $4, $5)
		ON CONFLICT(project_id, namespace, owner) DO UPDATE
		SET entry_id = EXCLUDED.entry_id, created_at = EXCLUDED.created_at`,
		publication.Project, publicPublicationPinNamespace, owner, entryID, now.UTC())
	if err != nil {
		return registry.store.safeError("pin cloud Public Cache publication artifact", err)
	}
	return nil
}

func (registry *PublicationRegistry) unpinPublicationEntryTx(
	ctx context.Context,
	transaction *sql.Tx,
	owner string,
) error {
	_, err := transaction.ExecContext(ctx, `
		DELETE FROM layercache_entry_pins_v1
		WHERE project_id = $1 AND namespace = $2 AND owner = $3`,
		registry.store.config.Project, publicPublicationPinNamespace, owner)
	if err != nil {
		return registry.store.safeError("unpin cloud Public Cache publication artifact", err)
	}
	return nil
}
