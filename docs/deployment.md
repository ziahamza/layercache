# Deploying Team Cache

Start with one Team Cache endpoint per project. PostgreSQL owns project records, capabilities, quotas, pins, and audit history. S3 owns immutable artifact bytes. An OCI registry holds BuildKit graphs separately. Team Cache has no hosted endpoint until these services are deployed.

## Build and configure

Build the API image with `docker build -t layercache:tested .`. The Dockerfile pins both base image manifests, builds without CGO, and runs as UID 65532. Record the resulting image digest for deployment. The image supports Team Cache API processes. It does not contain Git, Docker, QEMU, or a Public Build worker.

Create a dedicated configuration directory and state directory owned by the service UID, with mode 0700. Run the installed CLI's `setup --role team --non-interactive` as that user. Set `--data-dir /var/lib/layercache`, `--listen 0.0.0.0:7437`, the canonical project/repository/default ref, PostgreSQL and S3 settings, initial quota, and Team membership. Pass credentials with the protected `--*-file` flags described in the main README. The resulting config must remain mode 0600. For the container image, run setup in the image with those directories mounted at `/etc/layercache` and `/var/lib/layercache`; this preserves the ownership marker's container paths.

`deploy/compose.yml` requires `LAYER_CACHE_IMAGE`, `LAYER_CACHE_CONFIG_DIRECTORY`, and `LAYER_CACHE_DATA_DIRECTORY`. The image value should be the tested immutable digest. The API is published on loopback only. Place a TLS reverse proxy in front of it and set `--actions-archive-base-url` to the external HTTPS origin. Forward requests without rewriting signed archive paths. The API requires Authorization for cache operations. `/healthz` is a process liveness check; use authenticated `status` and `doctor` for dependency health.

Alternatively install `deploy/layercache.service` after creating a dedicated `layercache` system user and provisioning `/etc/layercache/config.json`. The unit assumes the verified release binary is `/usr/local/bin/layercache`. It keeps local state in `/var/lib/layercache` and emits logs to journald. Configure journal retention separately. The unit is for the API, not privileged KVM workers.

On the shared development host, consult its platform documentation before registering a public hostname. A published GitHub runner needs a reachable HTTPS endpoint; a developer-only VPN URL is insufficient unless the runner joins that network.

## GitHub Actions migration

Publish an immutable Layer Cache release before migrating workflows. Install the repository's `action/setup` by immutable commit before Turbo commands, and pass the published release repository/version plus the Team endpoint. Use GitHub OIDC with `id-token: write` so the job gets a project-, repository-, ref-, and integration-scoped credential. `action/cache` obtains its own Actions credential. Never distribute the server's administrator `localToken` to CI.

Migrate one trusted main-branch job first. Compare its declared outputs against a cold run, then run from a fresh checkout and inspect the Layer Cache run report. Keep fork pull requests on a no-secret, no-write path until the endpoint's ref policy is proven for that workflow. Replace Vercel Turbo environment settings only in the migrated jobs. A cold first run is expected because Vercel's existing cache has not been imported.

For `agent-access`, the previous workflow used archived `.turbo` directories rather than native Vercel caching. Prefer the native Turbo integration and remove the redundant `.turbo` archive step after proving reuse. Existing package-manager caching may remain during rollout. npm dependency download caching and Turbo output caching are separate operations.

## Backup and restore

1. Back up PostgreSQL using the provider's snapshots and point-in-time recovery. Include project metadata, memberships, audit, publication tombstones, and Public Build coordination tables. Test restoration into a separate database.
2. Enable S3 versioning or replication and retain objects longer than the database recovery window. A database restored to an earlier point may still reference objects collected later. Back up OCI registry storage separately using its supported method.
3. Protect service configuration and Public Cache signing keys in a secret manager with its own recovery policy. Never place them in this Git repository or ordinary QA output.
4. For a consistent manual recovery point, stop writers and background collection before taking database and object snapshots. Record the database snapshot and object/registry snapshot IDs together.
5. Restore into isolated services with no CI traffic. Verify `status`, authentication, a known Turbo restore, an Actions archive, and an immutable BuildKit pull. Corrupt or missing artifacts must return a miss. Check revocation/tombstone records before enabling Public Cache reads.
6. Resume traffic only after the isolated restore passes. Keep the original snapshot and service configuration available for rollback.

Local Cache is disposable, but backing up its complete stopped state can avoid a cold machine. Do not copy SQLite files piecemeal while a daemon is writing. Team PostgreSQL/S3 records cannot be reconstructed from Local Cache metadata.

## Upgrade and rollback

Save the running binary/image digest and configuration before upgrading. Take a database recovery point, stop API processes, install the candidate, and run the migration/restore checks against an isolated copy first. The schema currently uses additive migrations, but backwards compatibility is not a general downgrade guarantee. When an older binary cannot read the new schema, rollback requires the matching database recovery point and compatible object snapshot. Do not delete the current data merely to make an old binary start.

Watch failed authentication, upload backlog, negative estimated savings, quota pressure, S3 errors, and PostgreSQL latency. A full cache must retain reads and let builds finish locally. Keep log retention bounded independently of cache quotas.
