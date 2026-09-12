package cloud

import (
	"context"
	"fmt"
)

// Serialize first-start DDL across supported replicas sharing one database.
// The transaction-scoped lock is released automatically on commit or rollback.
const schemaMigrationAdvisoryLock int64 = 0x4c61796572436163

var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS layercache_projects_v1 (
		project_id TEXT PRIMARY KEY,
		quota_bytes BIGINT NOT NULL CHECK (quota_bytes > 0),
		metadata_quota_bytes BIGINT NOT NULL CHECK (metadata_quota_bytes > 0),
		used_bytes BIGINT NOT NULL DEFAULT 0 CHECK (used_bytes >= 0),
		metadata_bytes BIGINT NOT NULL DEFAULT 0 CHECK (metadata_bytes >= 0),
		created_at TIMESTAMPTZ NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS layercache_artifacts_v1 (
		project_id TEXT NOT NULL REFERENCES layercache_projects_v1(project_id) ON DELETE RESTRICT,
		digest TEXT NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
		size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
		media_type TEXT NOT NULL,
		object_key TEXT NOT NULL,
		ref_count BIGINT NOT NULL CHECK (ref_count >= 0),
		created_at TIMESTAMPTZ NOT NULL,
		last_accessed_at TIMESTAMPTZ NOT NULL,
		unreferenced_at TIMESTAMPTZ,
		PRIMARY KEY (project_id, digest),
		CHECK ((ref_count = 0) = (unreferenced_at IS NOT NULL))
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS layercache_artifacts_object_v1
		ON layercache_artifacts_v1(project_id, object_key)`,
	`CREATE TABLE IF NOT EXISTS layercache_artifact_access_v1 (
		project_id TEXT NOT NULL,
		digest TEXT NOT NULL,
		read_count BIGINT NOT NULL CHECK (read_count BETWEEN 0 AND 32),
		PRIMARY KEY (project_id, digest),
		FOREIGN KEY (project_id, digest)
			REFERENCES layercache_artifacts_v1(project_id, digest) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS layercache_entries_v1 (
		entry_id BIGSERIAL UNIQUE NOT NULL,
		project_id TEXT NOT NULL REFERENCES layercache_projects_v1(project_id) ON DELETE RESTRICT,
		integration TEXT NOT NULL,
		compatibility TEXT NOT NULL,
		native_key TEXT NOT NULL,
		version TEXT NOT NULL,
		ref_scope TEXT NOT NULL,
		digest TEXT NOT NULL,
		size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
		metadata_json JSONB NOT NULL,
		metadata_bytes BIGINT NOT NULL CHECK (metadata_bytes > 0),
		created_at TIMESTAMPTZ NOT NULL,
		last_accessed_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (project_id, integration, compatibility, native_key, version, ref_scope),
		FOREIGN KEY (project_id, digest)
			REFERENCES layercache_artifacts_v1(project_id, digest) ON DELETE RESTRICT
	)`,
	`CREATE INDEX IF NOT EXISTS layercache_entries_lru_v1
		ON layercache_entries_v1(project_id, last_accessed_at, created_at)`,
	`CREATE INDEX IF NOT EXISTS layercache_actions_lookup_v1
		ON layercache_entries_v1(project_id, integration, compatibility, ref_scope, version, created_at DESC)`,
	`CREATE TABLE IF NOT EXISTS layercache_entry_pins_v1 (
		project_id TEXT NOT NULL,
		namespace TEXT NOT NULL,
		owner TEXT NOT NULL,
		entry_id BIGINT NOT NULL REFERENCES layercache_entries_v1(entry_id) ON DELETE CASCADE,
		created_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (project_id, namespace, owner)
	)`,
	`CREATE INDEX IF NOT EXISTS layercache_entry_pins_entry_v1
		ON layercache_entry_pins_v1(entry_id)`,
	`CREATE TABLE IF NOT EXISTS layercache_uploads_v1 (
		upload_id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL REFERENCES layercache_projects_v1(project_id) ON DELETE RESTRICT,
		object_key TEXT NOT NULL UNIQUE,
		state TEXT NOT NULL CHECK (state IN ('uploading', 'staged')),
		digest TEXT,
		size_bytes BIGINT,
		created_at TIMESTAMPTZ NOT NULL,
		expires_at TIMESTAMPTZ NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS layercache_uploads_expiry_v1
		ON layercache_uploads_v1(expires_at)`,
	`CREATE TABLE IF NOT EXISTS layercache_blob_read_leases_v1 (
		lease_id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		digest TEXT NOT NULL,
		expires_at TIMESTAMPTZ NOT NULL,
		FOREIGN KEY (project_id, digest)
			REFERENCES layercache_artifacts_v1(project_id, digest) ON DELETE CASCADE
	)`,
	`CREATE INDEX IF NOT EXISTS layercache_blob_read_leases_expiry_v1
		ON layercache_blob_read_leases_v1(project_id, digest, expires_at)`,
	`CREATE TABLE IF NOT EXISTS layercache_audit_events_v1 (
		event_id BIGSERIAL PRIMARY KEY,
		project_id TEXT NOT NULL,
		actor TEXT NOT NULL,
		action TEXT NOT NULL,
		resource TEXT NOT NULL,
		outcome TEXT NOT NULL,
		attributes_json JSONB NOT NULL,
		created_at TIMESTAMPTZ NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS layercache_audit_project_v1
		ON layercache_audit_events_v1(project_id, event_id)`,
	`CREATE OR REPLACE FUNCTION layercache_reject_audit_mutation_v1()
	RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN
		RAISE EXCEPTION 'Layer Cache audit events are append-only';
	END;
	$$`,
	`DO $$
	BEGIN
		IF NOT EXISTS (
			SELECT 1 FROM pg_trigger
			WHERE tgname = 'layercache_audit_append_only_v1'
				AND tgrelid = 'layercache_audit_events_v1'::regclass
		) THEN
			CREATE TRIGGER layercache_audit_append_only_v1
			BEFORE UPDATE OR DELETE ON layercache_audit_events_v1
			FOR EACH ROW EXECUTE FUNCTION layercache_reject_audit_mutation_v1();
		END IF;
	END;
	$$`,
	`CREATE TABLE IF NOT EXISTS layercache_promotion_leases_v1 (
		project_id TEXT NOT NULL REFERENCES layercache_projects_v1(project_id) ON DELETE RESTRICT,
		reference TEXT NOT NULL,
		owner TEXT NOT NULL,
		token_hash BYTEA NOT NULL,
		acquired_at TIMESTAMPTZ NOT NULL,
		expires_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (project_id, reference)
	)`,
	`CREATE INDEX IF NOT EXISTS layercache_promotion_lease_expiry_v1
		ON layercache_promotion_leases_v1(expires_at)`,
	`CREATE TABLE IF NOT EXISTS layercache_memberships_v1 (
		project_id TEXT NOT NULL REFERENCES layercache_projects_v1(project_id) ON DELETE RESTRICT,
		subject TEXT NOT NULL,
		role TEXT NOT NULL CHECK (role IN ('reader', 'writer', 'admin')),
		updated_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (project_id, subject)
	)`,
	`CREATE TABLE IF NOT EXISTS layercache_publications_v1 (
		project_id TEXT NOT NULL REFERENCES layercache_projects_v1(project_id) ON DELETE RESTRICT,
		cache_identity TEXT NOT NULL,
		public_identity TEXT NOT NULL,
		repository TEXT NOT NULL,
		commit_digest TEXT NOT NULL,
		integration TEXT NOT NULL,
		target TEXT NOT NULL,
		recipe_digest TEXT NOT NULL,
		platform TEXT NOT NULL,
		digest TEXT NOT NULL,
		publication_json JSONB NOT NULL,
		state TEXT NOT NULL CHECK (state IN ('active', 'ambiguous', 'expired', 'revoked')),
		reason TEXT NOT NULL,
		pinned BOOLEAN NOT NULL DEFAULT FALSE,
		updated_at TIMESTAMPTZ NOT NULL,
		last_accessed_at TIMESTAMPTZ NOT NULL,
		expires_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (project_id, cache_identity),
		UNIQUE (project_id, public_identity)
	)`,
	`CREATE INDEX IF NOT EXISTS layercache_publications_build_v1
		ON layercache_publications_v1(
			project_id, repository, commit_digest, integration, target, recipe_digest, platform, updated_at DESC
		)`,
	`CREATE INDEX IF NOT EXISTS layercache_publications_expiry_v1
		ON layercache_publications_v1(project_id, state, pinned, expires_at)`,
	`CREATE TABLE IF NOT EXISTS layercache_actions_reservations_v1 (
		reservation_id BIGSERIAL PRIMARY KEY,
		project_id TEXT NOT NULL REFERENCES layercache_projects_v1(project_id) ON DELETE RESTRICT,
		repository TEXT NOT NULL,
		compatibility TEXT NOT NULL,
		ref_scope TEXT NOT NULL,
		cache_key TEXT NOT NULL,
		version TEXT NOT NULL,
		expected_size BIGINT,
		maximum_size BIGINT NOT NULL CHECK (maximum_size > 0),
		created_at TIMESTAMPTZ NOT NULL,
		expires_at TIMESTAMPTZ NOT NULL,
		UNIQUE (project_id, repository, compatibility, ref_scope, cache_key, version)
	)`,
	`CREATE INDEX IF NOT EXISTS layercache_actions_reservations_expiry_v1
		ON layercache_actions_reservations_v1(project_id, expires_at)`,
	`CREATE TABLE IF NOT EXISTS layercache_actions_chunks_v1 (
		project_id TEXT NOT NULL,
		reservation_id BIGINT NOT NULL REFERENCES layercache_actions_reservations_v1(reservation_id) ON DELETE CASCADE,
		start_offset BIGINT NOT NULL CHECK (start_offset >= 0),
		end_offset BIGINT NOT NULL CHECK (end_offset >= start_offset),
		object_key TEXT NOT NULL UNIQUE,
		digest TEXT NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
		size_bytes BIGINT NOT NULL CHECK (size_bytes > 0),
		created_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (reservation_id, start_offset)
	)`,
	`CREATE INDEX IF NOT EXISTS layercache_actions_chunks_range_v1
		ON layercache_actions_chunks_v1(reservation_id, start_offset, end_offset)`,
	`CREATE TABLE IF NOT EXISTS layercache_actions_entries_v1 (
		entry_id BIGINT PRIMARY KEY,
		project_id TEXT NOT NULL REFERENCES layercache_projects_v1(project_id) ON DELETE RESTRICT,
		repository TEXT NOT NULL,
		compatibility TEXT NOT NULL,
		ref_scope TEXT NOT NULL,
		cache_key TEXT NOT NULL,
		version TEXT NOT NULL,
		artifact_integration TEXT NOT NULL DEFAULT 'actions' CHECK (artifact_integration = 'actions'),
		artifact_native_key TEXT NOT NULL,
		digest TEXT NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
		size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
		producer_duration_ns BIGINT CHECK (producer_duration_ns >= 0),
		origin TEXT NOT NULL CHECK (origin IN ('localCache', 'teamCache', 'publicCache')),
		public_metadata_json JSONB,
		created_at TIMESTAMPTZ NOT NULL,
		UNIQUE (project_id, repository, compatibility, ref_scope, cache_key, version),
		FOREIGN KEY (
			project_id, artifact_integration, compatibility, artifact_native_key, version, ref_scope
		) REFERENCES layercache_entries_v1(
			project_id, integration, compatibility, native_key, version, ref_scope
		) ON DELETE CASCADE
	)`,
	`ALTER TABLE layercache_actions_entries_v1
		ADD COLUMN IF NOT EXISTS artifact_integration TEXT NOT NULL DEFAULT 'actions'`,
	`ALTER TABLE layercache_actions_entries_v1
		ADD COLUMN IF NOT EXISTS producer_duration_ns BIGINT`,
	`DO $$
	BEGIN
		IF NOT EXISTS (
			SELECT 1 FROM pg_constraint
			WHERE conname = 'layercache_actions_producer_duration_v1'
				AND conrelid = 'layercache_actions_entries_v1'::regclass
		) THEN
			ALTER TABLE layercache_actions_entries_v1
				ADD CONSTRAINT layercache_actions_producer_duration_v1
				CHECK (producer_duration_ns >= 0);
		END IF;
	END;
	$$`,
	`DO $$
	BEGIN
		IF NOT EXISTS (
			SELECT 1 FROM pg_constraint
			WHERE conname = 'layercache_actions_entries_artifact_v1'
				AND conrelid = 'layercache_actions_entries_v1'::regclass
		) THEN
			ALTER TABLE layercache_actions_entries_v1
				ADD CONSTRAINT layercache_actions_entries_artifact_v1
				FOREIGN KEY (
					project_id, artifact_integration, compatibility, artifact_native_key, version, ref_scope
				) REFERENCES layercache_entries_v1(
					project_id, integration, compatibility, native_key, version, ref_scope
				) ON DELETE CASCADE;
		END IF;
	END;
	$$`,
	`CREATE INDEX IF NOT EXISTS layercache_actions_lookup_cloud_v1
		ON layercache_actions_entries_v1(
			project_id, repository, compatibility, ref_scope, version, created_at DESC, entry_id DESC
		)`,
}

func (store *Store) migrate(ctx context.Context) error {
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return store.safeError("begin cloud metadata migration", err)
	}
	defer transaction.Rollback()
	if _, err := transaction.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, schemaMigrationAdvisoryLock); err != nil {
		return store.safeError("lock cloud metadata migration", err)
	}
	for _, statement := range schemaStatements {
		if _, err := transaction.ExecContext(ctx, statement); err != nil {
			return store.safeError("migrate cloud metadata", err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return store.safeError("commit cloud metadata migration", err)
	}
	return nil
}

func metadataBudget(maxBytes int64) int64 {
	budget := maxBytes / 100
	if budget < 16<<20 {
		budget = 16 << 20
	}
	if budget > 512<<20 {
		budget = 512 << 20
	}
	return budget
}

func ensureProject(ctx context.Context, store *Store) error {
	now := store.config.Now().UTC()
	_, err := store.database.ExecContext(ctx, `
		INSERT INTO layercache_projects_v1(
			project_id, quota_bytes, metadata_quota_bytes, used_bytes, metadata_bytes, created_at, updated_at
		) VALUES($1, $2, $3, 0, 0, $4, $4)
		ON CONFLICT(project_id) DO NOTHING`,
		store.config.Project, store.config.MaxBytes, metadataBudget(store.config.MaxBytes), now)
	if err != nil {
		return store.safeError(fmt.Sprintf("initialize cloud project %q", store.config.Project), err)
	}
	return nil
}
