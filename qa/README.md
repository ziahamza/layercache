# Manual QA

Run these checks against an installed binary and disposable Workspaces. Do not point destructive cleanup commands at an existing repository, Docker builder, image tag, configuration, or cache directory.

The first MVP's recorded real-project and protocol results are in [RESULTS.md](RESULTS.md).

The September release-sequence follow-up is recorded in that same file. New entry points are [native Public Build images](../docs/public-build-images.md), [calibrated performance](../docs/performance.md), [retention comparisons](../docs/retention.md), and [container deployment](../docs/deployment.md). Checked-in measurements are under `qa/evidence`; they identify the tested binary or builder assets and are not substitutes for native release CI.

## Automated release checks

From the repository root:

```bash
mise install
mise exec -- go test ./... -count=1
mise exec -- go test -race ./... -count=1
mise exec -- go vet ./...
mise exec -- bash qa/lint.sh

(
  cd action/cache
  pnpm install --frozen-lockfile
  pnpm check
)

pnpm --dir qa/fixtures/turbo install --frozen-lockfile
pnpm --dir qa/fixtures/turbo-minimum install --frozen-lockfile
pnpm --dir qa/fixtures/actions-cache-minimum install --frozen-lockfile
```

The shell gate requires actionlint 1.7.12 and ShellCheck 0.10.0. The CI workflows include pinned installation commands. To exercise `action/setup` against a built candidate, set `LAYERCACHE_SETUP_BINARY` to its absolute path and run `node --test action/setup/setup.test.mjs scripts/install.test.mjs`. Without that variable, the installed setup case explicitly skips.

`client-versions.json` defines the minimum and current release clients. Their lockfiles, plus the Buildx release SHA-256 values in that manifest, are release inputs rather than floating network selections.

Tag publication also depends on the dedicated `Cloud persistence QA / Linux x64` job. It starts PostgreSQL 16 and a pinned MinIO service, runs the real opt-in cloud, measurement, and Public Build PostgreSQL suites under the race detector, then installs a fresh binary and proves Team Turbo and Actions bytes survive a clean process restart through PostgreSQL/S3.

The repository must have GitHub release immutability enabled before cutting a tag. The publish job peels annotated tags to their commit, requires that commit to equal the `github.sha` tested by the release run, uploads every asset to a draft, and repeats the tag check immediately before publication. It accepts the release only after GitHub's API reports it immutable and the locked tag still resolves to the tested commit.

Focused installed-product scenarios are also available:

```bash
mise exec -- go test ./acceptance -run 'TestInstalled|TestTurbo|TestGitHubActions|TestBuildx|TestVerifiedPublic|TestPublicBuild|TestRunReport' -count=1 -v
```

## Isolated daemon

Choose an unused loopback port, then create a dedicated installation:

```bash
mise exec -- go build -trimpath -o bin/layercache ./cmd/layercache

export QA_LAYER_CACHE_BIN="$(pwd)/bin/layercache"
export QA_LAYER_CACHE_ROOT="$(mktemp -d)"
export QA_LAYER_CACHE_CONFIG="$QA_LAYER_CACHE_ROOT/config.json"
export QA_LAYER_CACHE_ADDRESS=127.0.0.1:17437

"$QA_LAYER_CACHE_BIN" setup \
  --config "$QA_LAYER_CACHE_CONFIG" \
  --data-dir "$QA_LAYER_CACHE_ROOT/cache" \
  --listen "$QA_LAYER_CACHE_ADDRESS" \
  --project github.com/layercache/manual-qa \
  --actions-repository layercache/manual-qa \
  --buildkit-builder layercache-manual-qa \
  --max-size 1073741824 \
  --non-interactive --json

"$QA_LAYER_CACHE_BIN" start --config "$QA_LAYER_CACHE_CONFIG" --json
"$QA_LAYER_CACHE_BIN" doctor --config "$QA_LAYER_CACHE_CONFIG" --json
```

If the port is already in use, choose another before setup. Keep the generated bearer tokens out of terminal transcripts and CI logs.

## Real Turbo client

Use a disposable project with a pinned Turborepo dependency and a deterministic build task. Run the same build twice:

```bash
cd /path/to/disposable/project

rm -rf .turbo packages/*/.turbo packages/*/dist
"$QA_LAYER_CACHE_BIN" run --config "$QA_LAYER_CACHE_CONFIG" -- \
  pnpm exec turbo run build --remote-only --summarize

rm -rf .turbo packages/*/.turbo packages/*/dist
"$QA_LAYER_CACHE_BIN" run --config "$QA_LAYER_CACHE_CONFIG" -- \
  pnpm exec turbo run build --remote-only --summarize
```

Keep any external execution counter outside the cleared paths. The second Turbo summary should report cached tasks, and declared output files must match the first build byte-for-byte. `--remote-only` plus removal of both root and package-level `.turbo` directories prevents Turborepo's Workspace-local cache from creating a false Layer Cache hit. Copy the final `Layer Cache run: run-...` value and inspect its report:

```bash
"$QA_LAYER_CACHE_BIN" report \
  --config "$QA_LAYER_CACHE_CONFIG" \
  --run run-... --json
```

The report should show one final outcome per eligible Turbo artifact, the proven cache source, bytes, timing, and a signed net estimate. A small fixture may legitimately report negative savings.

Repeat the hit from an independent clone at a different absolute path. A linked worktree alone does not prove clone portability.

The release gate repeats this oracle with the independently locked `fixtures/turbo-minimum` and `fixtures/turbo` projects, currently Turbo 2.9.14 and 2.10.12.

## Stock `@actions/cache` v1 client

The QA driver imports the exact locked `@actions/cache` package rather than calling Layer Cache internals:

```bash
cd /path/to/layercache
(
  cd action/cache
  pnpm install --frozen-lockfile
)

mkdir -p "$QA_LAYER_CACHE_ROOT/actions-a" "$QA_LAYER_CACHE_ROOT/actions-b"

actions_cold_log="$QA_LAYER_CACHE_ROOT/actions-cold.stderr"
"$QA_LAYER_CACHE_BIN" run --config "$QA_LAYER_CACHE_CONFIG" -- \
  node qa/stock-actions-cache.mjs \
  miss-save "$QA_LAYER_CACHE_ROOT/actions-a" manual-v1-key \
  2>"$actions_cold_log"
cat "$actions_cold_log" >&2
actions_cold_run="$(sed -n 's/^Layer Cache run: //p' "$actions_cold_log")"
"$QA_LAYER_CACHE_BIN" report --config "$QA_LAYER_CACHE_CONFIG" \
  --run "$actions_cold_run" --json |
  jq -e '
    .eligible == 1 and .hits == 0 and .misses == 1 and
    .bytes.uploaded > 0 and .netEstimatedBuildTimeSaved.known == 1 and
    ([.outcomes[] | select(
      .integration == "actions" and .result == "miss" and
      (.executionDurationMs | type) == "number" and .bytes.uploaded > 0
    )] | length) == 1
  '

actions_warm_log="$QA_LAYER_CACHE_ROOT/actions-warm.stderr"
"$QA_LAYER_CACHE_BIN" run --config "$QA_LAYER_CACHE_CONFIG" -- \
  node qa/stock-actions-cache.mjs \
  restore "$QA_LAYER_CACHE_ROOT/actions-b" manual-v1-key \
  2>"$actions_warm_log"
cat "$actions_warm_log" >&2
actions_warm_run="$(sed -n 's/^Layer Cache run: //p' "$actions_warm_log")"
"$QA_LAYER_CACHE_BIN" report --config "$QA_LAYER_CACHE_CONFIG" \
  --run "$actions_warm_run" --json |
  jq -e '
    .eligible == 1 and .hits == 1 and .misses == 0 and
    .bytes.downloaded > 0 and .grossAvoidedTaskTime.known == 1 and
    .netEstimatedBuildTimeSaved.known == 1 and
    ([.outcomes[] | select(
      .integration == "actions" and .result == "hit" and
      .source == "localCache" and
      (.producerDurationMs | type) == "number" and .bytes.downloaded > 0
    )] | length) == 1
  '
```

The cold command must print `missed-and-saved:...`; this proves the stock client's actual miss-to-save lifecycle rather than seeding through a write-only shortcut. The restore must print `restored:manual-v1-key`, and the restored file must match the saved file. The two report assertions prove that miss/save correlation retained execution and upload evidence and that the hit retained producer duration, restored bytes, and ROI. The lookup response's archive URL is intentionally used without an Authorization header, matching the stock v1 client; its HMAC signature and expiry provide download authority.

This stock-client smoke test does not exercise Actions v2 or Public Cache restore. Actions v2 is not supported. Verified Public fallback through the running v1 service is covered by `TestGitHubActionsPublicCacheVerifiesWarmsAndHonorsRevocation` below.

The default driver above uses the current action dependency. To qualify the minimum client, install `fixtures/actions-cache-minimum` and pass its absolute `node_modules/@actions/cache/lib/cache.js` path as the driver's fourth argument. The release gate runs both the 5.0.0 minimum and 6.2.0 current client against every native binary.

## Docker Buildx

This smoke test needs a working Docker daemon and Buildx. It uses a dedicated builder and image name:

The release gate downloads the exact Buildx versions and verifies their committed SHA-256 values before execution. The same installed-binary oracle is available for manual qualification:

```bash
export LAYER_CACHE_BIN="$QA_LAYER_CACHE_BIN"
export QA_ROOT="$(mktemp -d)"
export BUILDX_PLATFORM=linux/amd64

DOCKER_CONFIG="$QA_ROOT/docker-config"
VERSION="$(jq -r '.buildx.minimum.version' qa/client-versions.json)"
SHA256="$(jq -r '.buildx.minimum.sha256["linux-amd64"]' qa/client-versions.json)"
qa/install-buildx-client.sh "$VERSION" "$SHA256" "$DOCKER_CONFIG"
export DOCKER_CONFIG
qa/buildx-client-smoke.sh
```

Use the `current` manifest slot to repeat the trial with the current client. On native arm64, select `linux-arm64` and `BUILDX_PLATFORM=linux/arm64`.

For an already installed Buildx client, the equivalent expanded steps are:

```bash
mkdir -p "$QA_LAYER_CACHE_ROOT/docker"
printf 'hello from Layer Cache\n' > "$QA_LAYER_CACHE_ROOT/docker/payload.txt"
printf 'FROM scratch\nCOPY payload.txt /payload.txt\n' > "$QA_LAYER_CACHE_ROOT/docker/Dockerfile"

"$QA_LAYER_CACHE_BIN" integration buildkit \
  --config "$QA_LAYER_CACHE_CONFIG" --apply --json

"$QA_LAYER_CACHE_BIN" buildx plan \
  --config "$QA_LAYER_CACHE_CONFIG" \
  --platform linux/amd64 --load \
  -- --tag layercache-manual-qa:local \
  --file "$QA_LAYER_CACHE_ROOT/docker/Dockerfile" \
  "$QA_LAYER_CACHE_ROOT/docker"

"$QA_LAYER_CACHE_BIN" buildx build \
  --config "$QA_LAYER_CACHE_CONFIG" \
  --platform linux/amd64 --load --json \
  -- --tag layercache-manual-qa:local \
  --file "$QA_LAYER_CACHE_ROOT/docker/Dockerfile" \
  "$QA_LAYER_CACHE_ROOT/docker"

"$QA_LAYER_CACHE_BIN" buildx build \
  --config "$QA_LAYER_CACHE_CONFIG" \
  --platform linux/amd64 --load --json \
  -- --tag layercache-manual-qa:local \
  --file "$QA_LAYER_CACHE_ROOT/docker/Dockerfile" \
  "$QA_LAYER_CACHE_ROOT/docker"
```

The second result should report cached completed vertices. Inspect the loaded image and compare its configuration/filesystem result rather than accepting the cached-vertex count as the sole correctness oracle.

Team Cache registry tests also require a dedicated OCI repository and Docker credentials. Add the dedicated repository to the disposable configuration, preview the derived registry and refs, then delegate login to Docker without putting the password in process arguments or Layer Cache files:

```bash
"$QA_LAYER_CACHE_BIN" setup \
  --config "$QA_LAYER_CACHE_CONFIG" \
  --buildkit-team-repository "$TEAM_CACHE_REPOSITORY" \
  --non-interactive --json

"$QA_LAYER_CACHE_BIN" integration buildkit \
  --config "$QA_LAYER_CACHE_CONFIG" \
  --registry-username "$REGISTRY_USER" \
  --registry-password-stdin --json

printf '%s\n' "$REGISTRY_PASSWORD" | \
  "$QA_LAYER_CACHE_BIN" integration buildkit \
    --config "$QA_LAYER_CACHE_CONFIG" --apply \
    --registry-username "$REGISTRY_USER" \
    --registry-password-stdin --json
```

Disable shell tracing before this command. Docker's configured credential store owns the resulting login, so remove the disposable credential through Docker after QA. Use a unique `--team-export` ID for every build and test `linux/amd64` and `linux/arm64` on native hosts where available.

## Team Cache, Public Cache, and Public Build

### PostgreSQL and S3-compatible persistence

The production Team/Public storage path needs PostgreSQL and an existing S3 bucket. This disposable check uses Postgres 16 and MinIO. Confirm that the names and ports below are unused before starting it.

```bash
docker network create layercache-cloud-qa-net

docker run -d --rm \
  --name layercache-cloud-qa-postgres \
  --network layercache-cloud-qa-net \
  -p 56432:5432 \
  -e POSTGRES_USER=layercache \
  -e POSTGRES_PASSWORD=layercacheqa \
  -e POSTGRES_DB=layercache \
  postgres@sha256:e17e86066e5ef83e0952a9347f5c792b7ece00972e2aa787a6986f471b3dd3d5

docker run -d --rm \
  --name layercache-cloud-qa-minio \
  --network layercache-cloud-qa-net \
  -p 59000:9000 -p 59001:9001 \
  -e MINIO_ROOT_USER=layercacheqa \
  -e MINIO_ROOT_PASSWORD=layercache-secret-qa \
  quay.io/minio/minio@sha256:14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e \
  server /data --console-address :9001

docker exec layercache-cloud-qa-postgres \
  pg_isready -U layercache -d layercache
curl --fail --silent --show-error \
  http://127.0.0.1:59000/minio/health/ready

docker run --rm \
  --network layercache-cloud-qa-net \
  --entrypoint /bin/sh minio/mc@sha256:a7fe349ef4bd8521fb8497f55c6042871b2ae640607cf99d9bede5e9bdf11727 -c \
  'mc alias set qa http://layercache-cloud-qa-minio:9000 layercacheqa layercache-secret-qa >/dev/null && mc mb --ignore-existing qa/layercache-qa'
```

Run the opt-in cloud behavior check against those real dependencies. The S3 adapter uses the standard AWS credential chain when explicit credentials are absent from Layer Cache configuration.

```bash
export LAYER_CACHE_QA_POSTGRES_URL='postgres://layercache:layercacheqa@127.0.0.1:56432/layercache?sslmode=disable'
export LAYER_CACHE_QA_S3_ENDPOINT='http://127.0.0.1:59000'
export LAYER_CACHE_QA_S3_BUCKET='layercache-qa'
export LAYER_CACHE_QA_S3_REGION='us-east-1'
export AWS_ACCESS_KEY_ID='layercacheqa'
export AWS_SECRET_ACCESS_KEY='layercache-secret-qa'

mise exec -- go test ./internal/cloud \
  -run 'TestCloudStoreAgainstPostgresAndS3|TestConcurrentCloudSchemaMigrationAgainstPostgres|TestConfiguredMembershipReconciliationAgainstPostgres' \
  -count=1 -v

LAYERCACHE_MEASUREMENT_POSTGRES_URL="$LAYER_CACHE_QA_POSTGRES_URL" \
  mise exec -- go test ./internal/measurement \
  -run 'TestConcurrentPostgresRepositoryInitializationAgainstPostgres|TestPostgresRepositoryAgainstPostgres' \
  -count=1 -v

mise exec -- go test ./internal/publicbuild \
  -run TestPostgresCoordinator -count=1 -v
```

This check streams and verifies object SHA-256 values, rejects a conflicting immutable writer, exercises thirty-two concurrent writers, verifies project isolation, forces LRU quota eviction, corrupts and repairs an S3 object, expires staged Turbo and Actions uploads, collects unreferenced blobs after their grace period, reads append-only audit events, changes membership, and contends a durable BuildKit promotion lease across PostgreSQL transactions. It also saves an Actions archive in out-of-order ranges, verifies exact and prefix lookup, reopens a second cloud adapter, restores the same bytes, and invalidates a same-sized corrupt S3 archive as a safe miss. The concurrent migration check opens sixteen independent PostgreSQL pools against one empty schema and verifies first-start migrations serialize cleanly.

Configured Team membership is authoritative at Team startup, including role downgrades and removals. A Public server sharing that PostgreSQL project only adds or updates its configured members, so it cannot delete members owned by the Team configuration. The real PostgreSQL membership case in the automated gate verifies both policies.

The measurement checks first open twelve independent PostgreSQL pools against one empty schema to verify first-start migrations serialize cleanly. They then record immutable outcomes, contend duplicate writers, isolate two projects, reconcile Turbo observations, verify nanosecond period boundaries, reopen the PostgreSQL adapter, and remove its project-scoped fixture rows.

The Public Build check opens two coordinator instances for one project. It contends duplicate requests and worker claims, appends ordered logs from both instances, verifies only credential hashes reach PostgreSQL, preserves a live lease across reopen, recovers an expired lease, fences stale publication attempts, and verifies completed state and status survive reopen.

Run the installed Team service against the same dependencies to verify the HTTP and startup seams. Use a disposable path and synthetic credentials only:

```bash
export LAYER_CACHE_CLOUD_ROOT="$(mktemp -d)"
export LAYER_CACHE_CLOUD_BIN="$LAYER_CACHE_CLOUD_ROOT/layercache"
export LAYER_CACHE_CLOUD_CONFIG="$LAYER_CACHE_CLOUD_ROOT/team.json"
export LAYER_CACHE_CLOUD_TOKEN_FILE="$LAYER_CACHE_CLOUD_ROOT/local-token"
export LAYER_CACHE_CLOUD_POSTGRES_FILE="$LAYER_CACHE_CLOUD_ROOT/postgres-url"

(umask 077
  printf '%s\n' 'qa-team-admin-token' >"$LAYER_CACHE_CLOUD_TOKEN_FILE"
  printf '%s\n' "$LAYER_CACHE_QA_POSTGRES_URL" >"$LAYER_CACHE_CLOUD_POSTGRES_FILE"
)

go build -o "$LAYER_CACHE_CLOUD_BIN" ./cmd/layercache

"$LAYER_CACHE_CLOUD_BIN" setup --non-interactive --json \
  --config "$LAYER_CACHE_CLOUD_CONFIG" \
  --data-dir "$LAYER_CACHE_CLOUD_ROOT/team-data" \
  --role team --listen 127.0.0.1:58691 \
  --project github.com/acme/cloud-qa \
  --compatibility-id linux-amd64-node24 \
  --local-token-file "$LAYER_CACHE_CLOUD_TOKEN_FILE" \
  --max-size 10485760 --min-free-bytes 0 \
  --buildkit-gc-bytes 1048576 \
  --actions-repository acme/cloud-qa \
  --actions-ref refs/heads/main \
  --actions-default-ref refs/heads/main \
  --team-member qa-admin=admin \
  --cloud-postgres-url-file "$LAYER_CACHE_CLOUD_POSTGRES_FILE" \
  --cloud-s3-endpoint "$LAYER_CACHE_QA_S3_ENDPOINT" \
  --cloud-s3-bucket "$LAYER_CACHE_QA_S3_BUCKET" \
  --cloud-s3-region "$LAYER_CACHE_QA_S3_REGION" \
  --cloud-s3-path-style

"$LAYER_CACHE_CLOUD_BIN" serve --config "$LAYER_CACHE_CLOUD_CONFIG"
```

In another terminal, write and read Turbo bytes, then exercise Actions ranged upload and restore through the configured compatibility route:

```bash
export LAYER_CACHE_CLOUD_URL=http://127.0.0.1:58691
export LAYER_CACHE_CLOUD_TOKEN="$(tr -d '\r\n' <"$LAYER_CACHE_CLOUD_TOKEN_FILE")"
export LAYER_CACHE_CLOUD_COMPAT=linux-amd64-node24

curl --fail --silent --show-error -X PUT \
  -H "Authorization: Bearer $LAYER_CACHE_CLOUD_TOKEN" \
  -H "x-layercache-compatibility: $LAYER_CACHE_CLOUD_COMPAT" \
  --data-binary 'turbo-cloud-manual' \
  "$LAYER_CACHE_CLOUD_URL/v8/artifacts/manual-turbo-key"

curl --fail --silent --show-error \
  -H "Authorization: Bearer $LAYER_CACHE_CLOUD_TOKEN" \
  -H "x-layercache-compatibility: $LAYER_CACHE_CLOUD_COMPAT" \
  "$LAYER_CACHE_CLOUD_URL/v8/artifacts/manual-turbo-key"

actions_base="$LAYER_CACHE_CLOUD_URL/_layercache/compatibility/$LAYER_CACHE_CLOUD_COMPAT/_apis/artifactcache"
cache_id="$(curl --fail --silent --show-error -X POST \
  -H "Authorization: Bearer $LAYER_CACHE_CLOUD_TOKEN" \
  -H 'Content-Type: application/json' \
  --data '{"key":"manual-actions-key","version":"v1","cacheSize":20}' \
  "$actions_base/caches" | jq -r .cacheId)"

curl --fail --silent --show-error -X PATCH \
  -H "Authorization: Bearer $LAYER_CACHE_CLOUD_TOKEN" \
  -H 'Content-Range: bytes 0-19/*' \
  --data-binary 'actions-cloud-manual' \
  "$actions_base/caches/$cache_id"

curl --fail --silent --show-error -X POST \
  -H "Authorization: Bearer $LAYER_CACHE_CLOUD_TOKEN" \
  -H 'Content-Type: application/json' --data '{"size":20}' \
  "$actions_base/caches/$cache_id"

archive_url="$(curl --fail --silent --show-error \
  -H "Authorization: Bearer $LAYER_CACHE_CLOUD_TOKEN" \
  "$actions_base/cache?keys=manual-actions-key&version=v1" | jq -r .archiveLocation)"
curl --fail --silent --show-error \
  -H "Authorization: Bearer $LAYER_CACHE_CLOUD_TOKEN" "$archive_url"
```

Both reads must reproduce their input exactly. Stop the service with `Ctrl-C`, start the same `serve` command again, and repeat both reads. That restart proves the authoritative Turbo and Actions records are in PostgreSQL/S3 rather than process memory or host Actions SQLite.

The authenticated administration seams can be checked without printing returned lease credentials:

```bash
curl --fail --silent --show-error -X PUT \
  -H "Authorization: Bearer $LAYER_CACHE_CLOUD_TOKEN" \
  -H 'Content-Type: application/json' --data '{"role":"writer"}' \
  "$LAYER_CACHE_CLOUD_URL/v1/members/github:manual-qa"

lease="$(curl --fail --silent --show-error -X POST \
  -H "Authorization: Bearer $LAYER_CACHE_CLOUD_TOKEN" \
  -H 'Content-Type: application/json' \
  --data '{"reference":"registry.example/acme/cache:main"}' \
  "$LAYER_CACHE_CLOUD_URL/v1/buildkit/promotion-leases/acquire")"

renewed="$(curl --fail --silent --show-error -X POST \
  -H "Authorization: Bearer $LAYER_CACHE_CLOUD_TOKEN" \
  -H 'Content-Type: application/json' \
  --data "$(jq -c '{reference:"registry.example/acme/cache:main",leaseToken:.leaseToken}' <<<"$lease")" \
  "$LAYER_CACHE_CLOUD_URL/v1/buildkit/promotion-leases/renew")"

curl --fail --silent --show-error -X POST \
  -H "Authorization: Bearer $LAYER_CACHE_CLOUD_TOKEN" \
  -H 'Content-Type: application/json' \
  --data "$(jq -c '{reference:"registry.example/acme/cache:main",leaseToken:.leaseToken}' <<<"$renewed")" \
  "$LAYER_CACHE_CLOUD_URL/v1/buildkit/promotion-leases/release"

curl --fail --silent --show-error \
  -H "Authorization: Bearer $LAYER_CACHE_CLOUD_TOKEN" \
  "$LAYER_CACHE_CLOUD_URL/v1/audit?limit=100" \
  | jq '{actions: [.events[].action], attributes: [.events[].attributes]}'
```

The setup JSON must show `[redacted]` for the PostgreSQL URL and bearer token. The audit attributes must contain no bearer, lease, S3, or PostgreSQL credentials.

Inspect the dependencies without printing credentials:

```bash
docker exec layercache-cloud-qa-postgres psql -U layercache -d layercache -c \
  'SELECT project_id, quota_bytes, used_bytes, metadata_bytes FROM layercache_projects_v1 ORDER BY project_id;'

docker run --rm \
  --network layercache-cloud-qa-net \
  --entrypoint /bin/sh minio/mc@sha256:a7fe349ef4bd8521fb8497f55c6042871b2ae640607cf99d9bede5e9bdf11727 -c \
  'mc alias set qa http://layercache-cloud-qa-minio:9000 layercacheqa layercache-secret-qa >/dev/null && mc ls --recursive qa/layercache-qa/projects/'
```

Stop only these named disposable containers, then remove their dedicated network:

```bash
docker stop layercache-cloud-qa-postgres layercache-cloud-qa-minio
docker network rm layercache-cloud-qa-net
unset LAYER_CACHE_QA_POSTGRES_URL LAYER_CACHE_QA_S3_ENDPOINT \
  LAYER_CACHE_QA_S3_BUCKET LAYER_CACHE_QA_S3_REGION \
  AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY \
  LAYER_CACHE_CLOUD_TOKEN LAYER_CACHE_CLOUD_TOKEN_FILE \
  LAYER_CACHE_CLOUD_POSTGRES_FILE
```

The containers use `--rm`, so stopping them deletes their QA-only database and objects.

The acceptance suite provisions isolated server configurations and verifies fresh-host warming, restart persistence, retry behavior, signed Turbo and Actions Public Cache restore, offline lease behavior, client write rejection, and revocation:

```bash
mise exec -- go test ./acceptance \
  -run 'TestTurboTeamCacheWarmsFreshHost|TestFailedTurboTeamPublicationRetriesAfterRuntimeRestart|TestGitHubActionsV1TeamCacheWarmsFreshHost|TestGitHubActionsPublicCacheVerifiesWarmsAndHonorsRevocation|TestVerifiedPublicCacheWarmsLocalAndRejectsClientWrites|TestPublicBuildControlPlaneCompletesPersistsAndFencesClients' \
  -count=1 -v
```

For a manual Team Cache trial, use a separate Team Cache data directory, configuration, token, and port. Put any non-loopback endpoint behind HTTPS. Verify a fresh client restores from Team Cache, then disconnect Team Cache and verify that the warmed Local Cache still restores the same bytes.

The Public Cache acceptance fixture uses the trusted collector seam. It proves publication verification and revocation independently of the guest executor.

The Public Build control-plane scenario requests an allowlisted immutable build, checks request deduplication, leases it to a capability-matched worker identity, sanitizes a log line, publishes a known fixture through the trusted collector endpoint, and completes the build by publication identity. It then restarts the process and checks persisted state and logs. Separate branches cover cancellation, failure and retry, client, worker, and collector credential separation, mutable commit rejection, allowlist enforcement, and raw-byte injection rejection.

That control-plane scenario does not execute repository code. The production worker is a separate Linux KVM/QEMU seam and never falls back to a normal process or container. Supply reviewed, immutable guest assets and a delegated cgroup v2 root, then run its fail-closed preflight before leasing work:

```bash
bin/layercache public-build worker run \
  --config /var/lib/layercache/public.json \
  --worker-id worker-linux-amd64 \
  --kernel /opt/layercache/guest/vmlinuz \
  --kernel-sha256 sha256:<64-hex> \
  --rootfs /opt/layercache/guest/rootfs.raw \
  --rootfs-sha256 sha256:<64-hex> \
  --guest-contract /opt/layercache/guest/contract.json \
  --guest-contract-sha256 sha256:<64-hex> \
  --cgroup-root /sys/fs/cgroup/layercache-workers \
  --work-root /var/lib/layercache/public-build-worker \
  --sandbox-uid 64000 --sandbox-gid 64000 \
  --max-scratch-bytes 53687091200 \
  --preflight --json
```

For a complete real-boot smoke, set the variables documented by `TestManualQEMUWorker` and run:

```bash
LAYERCACHE_QEMU_MANUAL_QA=1 \
  go test ./internal/publicbuild/sandbox -run TestManualQEMUWorker -count=1 -v
```

The manual fixture checks KVM boot, no NIC, read-only source, bounded virtio-serial collection, log redaction, host-side hashing, and QEMU/cgroup reaping. Its `manual` recipe is a smoke fixture, not a maintained executable recipe, so production worker admission rejects it. Production preflight also requires an exact integration, target, and digest tuple from the reviewed guest contract. Dependencies must be vendored or pinned into that immutable image.

### Maintained Actions, Turbo, and BuildKit KVM recipes

The repository includes opt-in Linux x64 root-filesystem assembly recipes and end-to-end harnesses for the maintained Actions, Turbo, and BuildKit executors. They compile the guest agent from the current checkout, use the checked-in contract, refuse to overwrite an existing asset root, and print every resulting asset digest. The Turbo recipe verifies the exact Turbo package and binary digests and copies the Node 24.13.0 and npm 11.6.2 toolchain selected by `mise`. The BuildKit recipe extracts `buildkitd`, `buildctl`, and runc from its image pinned by digest. All three harnesses require QEMU/KVM, cgroup v2 with the CPU, memory, and pids controllers, e2fsprogs, a static BusyBox, and a reviewed host kernel.

Run the Actions recipe from the repository root with exact disposable paths:

```bash
KERNEL="/boot/vmlinuz-$(uname -r)"
KVM_GID="$(stat -c %g /dev/kvm)"
ACTIONS_ASSETS=/var/lib/layercache-actions-kvm-qa
ACTIONS_CGROUP=/sys/fs/cgroup/layercache-actions-kvm-qa

qa/public-build-kvm/build-actions-amd64-rootfs.sh \
  "$ACTIONS_ASSETS" "$KERNEL"
sudo install -d -m 0700 "$ACTIONS_CGROUP"
sudo sh -c \
  'printf "%s\n" "+cpu +memory +pids" > "$1/cgroup.subtree_control"' \
  sh "$ACTIONS_CGROUP"
go build -trimpath -o /tmp/layercache-actions-kvm-qa \
  ./qa/public-build-kvm/actions
sudo /tmp/layercache-actions-kvm-qa \
  --asset-root "$ACTIONS_ASSETS" \
  --cgroup-root "$ACTIONS_CGROUP" \
  --sandbox-gid "$KVM_GID"
```

The Actions harness asserts the exact sealed source identity, no guest NIC, offline dependency mode, exact native key, host-collected digest, publication descriptor, and extracted cache payload. Run it twice against the same assets; identical inputs must produce the same output digest.

Run the maintained Turbo recipe with a fresh asset root and cgroup:

```bash
mise install
KERNEL="/boot/vmlinuz-$(uname -r)"
KVM_GID="$(stat -c %g /dev/kvm)"
TURBO_ASSETS=/var/lib/layercache-turbo-kvm-qa
TURBO_CGROUP=/sys/fs/cgroup/layercache-turbo-kvm-qa

qa/public-build-kvm/build-turbo-amd64-rootfs.sh \
  "$TURBO_ASSETS" "$KERNEL"
sudo install -d -m 0700 "$TURBO_CGROUP"
sudo sh -c \
  'printf "%s\n" "+cpu +memory +pids" > "$1/cgroup.subtree_control"' \
  sh "$TURBO_CGROUP"
go build -trimpath -o /tmp/layercache-turbo-kvm-qa \
  ./qa/public-build-kvm/turbo
sudo /tmp/layercache-turbo-kvm-qa \
  --asset-root "$TURBO_ASSETS" \
  --cgroup-root "$TURBO_CGROUP" \
  --sandbox-gid "$KVM_GID"
```

The Turbo harness independently plans the sealed fixture with the same pinned Turbo binary, executes the maintained recipe with no guest NIC and exact offline dependency mode, verifies the collected zstd archive and native key, and extracts the expected task output and genuine Turbo log. It then serves those exact bytes to a second real Turbo 2.10.12 process on host loopback. A deliberately failing `npm` executable proves the second process restored the cache instead of executing the task. Run the harness twice against the same assets and compare the native key, artifact digest, size, members, and restored output.

The BuildKit proof also needs a unique disposable OCI repository for which the current Docker configuration has push and pull authority. A loopback registry is sufficient:

```bash
KERNEL="/boot/vmlinuz-$(uname -r)"
KVM_GID="$(stat -c %g /dev/kvm)"
BUILDKIT_ASSETS=/var/lib/layercache-buildkit-kvm-qa
BUILDKIT_CGROUP=/sys/fs/cgroup/layercache-buildkit-kvm-qa
PUBLIC_BUILD_QA_REPOSITORY=127.0.0.1:5000/layercache/disposable-buildkit-qa

qa/public-build-kvm/build-buildkit-amd64-rootfs.sh \
  "$BUILDKIT_ASSETS" "$KERNEL"
sudo install -d -m 0700 "$BUILDKIT_CGROUP"
sudo sh -c \
  'printf "%s\n" "+cpu +memory +pids" > "$1/cgroup.subtree_control"' \
  sh "$BUILDKIT_CGROUP"
go build -trimpath -o /tmp/layercache-buildkit-kvm-qa \
  ./qa/public-build-kvm/buildkit
sudo /tmp/layercache-buildkit-kvm-qa \
  --asset-root "$BUILDKIT_ASSETS" \
  --cgroup-root "$BUILDKIT_CGROUP" \
  --sandbox-gid "$KVM_GID" \
  --repository "$PUBLIC_BUILD_QA_REPOSITORY"
```

That harness executes a real Dockerfile `RUN` through the private in-guest BuildKit daemon, validates the returned OCI layout, pushes the whole graph, pulls it back by immutable manifest digest, checks every config and layer descriptor, and extracts the expected output from the pulled layer. It does not give the guest registry credentials or a network device.

After any run, confirm that the exact delegated cgroup's `cgroup.procs` is empty before removing that cgroup. Inspect and then remove only the exact QA asset root, binary, repository, and container created for the trial. The assembly scripts deliberately refuse to replace an existing asset root.

The maintained BuildKit recipe copies the checkout from its read-only source device into the bounded guest work disk, starts a private offline BuildKit daemon, executes the named Dockerfile target, and returns a complete OCI image-layout cache result. The trusted host validates every descriptor, pushes the complete graph to the configured untagged OCI repository, reads back the exact root manifest, and publishes its signed digest. A successful worker event returns `publicImportSelector`; the same value appears at `publication.publicImportSelector` in status JSON and can be passed unchanged to `layercache buildx build --public-import`.

Exercise the collector-to-registry seam with a unique disposable repository for which Docker already has credentials:

```bash
: "${PUBLIC_BUILD_QA_REPOSITORY:?set this to a disposable untagged OCI repository}"
LAYERCACHE_OCI_REGISTRY_QA="$PUBLIC_BUILD_QA_REPOSITORY" \
  mise exec -- go test ./internal/publicbuild/sandbox \
  -run '^TestManualPublishOCIImageLayout$' -count=1 -v
```

The focused test pushes a complete OCI graph, pulls it by the exact returned root-manifest digest, and verifies the manifest, config, and layers independently of QEMU. The maintained BuildKit harness above composes the real guest executor, trusted collector, and registry into one end-to-end proof. Do not claim native arm64 or macOS execution unless it was run on that platform.

## Project trials

Create independent disposable clones at a fixed commit so source and lockfiles do not change between cold and warm runs:

```bash
git clone --no-hardlinks --no-checkout /path/to/source "$QA_LAYER_CACHE_ROOT/project-a"
git -C "$QA_LAYER_CACHE_ROOT/project-a" checkout --detach <commit>

git clone --no-hardlinks --no-checkout /path/to/source "$QA_LAYER_CACHE_ROOT/project-b"
git -C "$QA_LAYER_CACHE_ROOT/project-b" checkout --detach <commit>
```

Use separate package-manager caches when measuring Layer Cache so an unrelated pnpm/npm cache does not create a false hit. Keep publication, deployment, release, signing, and live-environment commands out of QA. Compare build outputs and execution counters; a reported hit with different output is a release-blocking failure.

## Cleanup

Stop and remove only this QA installation:

```bash
"$QA_LAYER_CACHE_BIN" uninstall \
  --config "$QA_LAYER_CACHE_CONFIG" \
  --delete-cache --yes --json
```

The command deletes the exact configured data directory after validating its ownership marker. An unchanged builder and configuration owned by the applied BuildKit integration are also removed; `--preserve-cache` retains owned cache state. User-created or otherwise unproven builders are never removed. The surrounding temporary directory and Docker image remain for explicit cleanup after checking their exact names.
