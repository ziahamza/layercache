# Layer Cache

Layer Cache reuses build work across short-lived worktrees, clones, VMs, and CI jobs. A local daemon owns a bounded content-addressed cache, speaks native cache protocols, and can fall back to a shared Team Cache or a verified Public Cache.

This repository contains working implementations for Turborepo, GitHub Actions cache v1, and Docker Buildx/BuildKit. It includes local persistence, project-scoped PostgreSQL/S3 cloud persistence, signed Public Cache reads, and a Linux KVM/QEMU Public Build worker. An immutable CLI release has not yet been published. npm/CDN mirrors, Bazel, Vite, Next.js, Xcode, Windows, Actions cache v2, and a general adapter SDK remain out of scope.

Release and deployment instructions are in the [verified installer](scripts/README.md), [GitHub runner setup](action/setup/README.md), and [Team Cache deployment guide](docs/deployment.md). See [quota administration](docs/cache-administration.md), [retention policies](docs/retention.md), [trust-key rotation](docs/public-trust.md), and [performance qualification](docs/performance.md) for operations. Native platform execution and an immutable published release remain qualification gates; checked-in workflows alone do not establish a release.

## Cache model

| Name | Who can read or request | Who can write or complete | Current implementation |
| --- | --- | --- | --- |
| Local Cache | One machine | Local Builds | Filesystem CAS and SQLite metadata with a configured quota, default LRU, and opt-in impact-based eviction |
| Team Cache | Authorized members and project capabilities | Project writers and administrators | PostgreSQL logical records and S3-compatible immutable bytes; embedded persistence remains available for local development |
| Public Cache | Verified readers | Only the trusted collector | Signed immutable publications, online revocation, ambiguity handling, and complete digest verification |
| Public Build | Authorized clients | Fenced workers and the trusted host collector | PostgreSQL-backed coordination plus a Linux KVM/QEMU worker with no guest network, read-only source, bounded resources, and host-side publication |

A Workspace is a checkout and execution environment, including a Git worktree or disposable VM. A Workspace can disappear while Local Cache or Team Cache artifacts remain reusable.

## Build and configure

The pinned development toolchain is Go 1.25.13, Node.js 24.13.0, and pnpm 10.34.5.

```bash
mise install
mise exec -- go build -trimpath -o bin/layercache ./cmd/layercache
```

Preview setup without touching the filesystem:

```bash
bin/layercache setup \
  --project github.com/acme/widget \
  --data-dir "$HOME/.cache/layercache" \
  --max-size 21474836480 \
  --preview --json
```

Apply it and start the daemon:

```bash
bin/layercache setup \
  --project github.com/acme/widget \
  --data-dir "$HOME/.cache/layercache" \
  --max-size 21474836480 \
  --min-free-bytes 8589934592 \
  --non-interactive --json

bin/layercache start --json
bin/layercache status --json
```

`status` uses the running daemon when available. When the daemon is stopped, it reads persisted Local Cache metadata in read-only mode so `usageBytes`, `artifacts`, and `entries` continue to describe the cache on disk.

Setup is safe to rerun. Omitted secrets, the installation creation time, and Public Cache signing keys are preserved. Configuration files and the Local Cache ownership marker are written with owner-only permissions.

Pass credentials through the matching `--*-file` option, not as an expanded command-line argument. Secret files must be regular, single-link, owned by the current user, and inaccessible to group or other users. Setup supports protected files for the local and Team tokens, PostgreSQL URL, S3 access and secret keys, Public Build access/worker/collector credentials, and the deprecated publisher credential. Raw value flags remain available for compatibility but can be exposed by process listings and shell history.

`--max-size` bounds committed Local Cache artifact bytes, not total physical disk usage. By default Layer Cache preserves the greater of 5 GiB and 5% of the cache filesystem. A positive `--min-free-bytes` replaces the percentage-based reserve, subject to a 5 GiB absolute floor. Cache admission fails safely when that reserve cannot be kept. Atomic Actions and Public Cache transfers use bounded temporary space: retained upload chunks total at most `--max-size`, and all transient staging combined totals at most twice `--max-size`.

Remote metadata, connection, and TLS setup use `--remote-metadata-timeout` (two seconds by default). Artifact bodies use `--remote-transfer-idle-timeout` (30 seconds by default): a large transfer may run for any total duration while bytes continue moving, but a stalled transfer degrades to a cache miss.

The default compatibility identity includes OS and architecture, detected libc or macOS version, available Node major version, the CLI's Go major/minor version, and `schema1`. Detection cannot identify every toolchain that affects outputs. Inspect `setup --preview --json` and use `--compatibility-id`, for example `linux-amd64-glibc2.39-node@24-schema1`, when the build needs an explicit ABI contract. Identities are lowercase, at most 256 bytes, and may use `-_.:+@` delimiters.

`setup --eviction-policy impact` opts into producer-cost-weighted eviction; `lru` remains the default. Stop the daemon before changing policy and restart afterward. The byte cap and pin protection apply to either policy. Use [historical backtests](docs/backtest.md) to compare them on the same workload before changing a shared cache.

Setup supports previews, interactive confirmation, and explicit `--non-interactive` operation. `layercache login` uses GitHub device authorization, keeps refresh material in macOS Keychain or Linux Secret Service, and persists only short-lived project capabilities in the owner-only configuration. If no supported credential manager is available, login fails instead of writing refresh material to disk.

Engineers already signed into GitHub CLI can instead use
`layercache login --github-cli --config /path/to/project-config.json`.
Layer Cache asks `gh auth token --hostname github.com` for the existing identity
and exchanges it for project-scoped capabilities. The GitHub credential stays
with `gh`; it is never copied into Layer Cache configuration. The absolute `gh`
executable path is recorded for automatic refresh while the daemon runs. Run
`gh auth login` first, and rerun Layer Cache login if that executable moves.
Changing the configured Team URL or project clears this authorization choice.
Use separate configurations, data directories and listen ports for different
projects; compatible clones and worktrees of one project can share a daemon.

Useful lifecycle commands:

```bash
bin/layercache doctor --json
bin/layercache gc --json
bin/layercache repair --json
bin/layercache stop --json
```

Open a live dashboard for existing CLI project connections:

```bash
bin/layercache dashboard --config /path/to/project.json
# Repeat --config to view multiple projects.
```

The [CLI-connected dashboard](docs/dashboard.md) shows Local Cache and Team Cache
health, artifact storage, and reuse reports. Open the complete URL printed by the
CLI and keep the process running. [Self-serve cloud teams and projects](docs/self-serve-cloud.md)
are implemented but await public deployment and real GitHub OAuth qualification.
Machine enrollment and cloud runners are subsequent slices.

`doctor` is read-only. `repair` recreates missing owned data directories and reconstructs runtime ownership only from the authenticated live daemon identity; it never trusts a PID alone. `gc` requires a running daemon. Uninstall makes the cache choice explicit:

```bash
bin/layercache uninstall --preserve-cache --json
bin/layercache uninstall --delete-cache --yes --json
```

Deletion is limited to the exact configured data directory and requires a matching ownership marker. Broad paths such as a filesystem root or home directory are rejected.

## Turborepo

For GitHub CI, use the [native Turbo action](action/turbo/README.md). It needs
no CLI release, consumer script, or long-lived cache secret:

```yaml
permissions:
  contents: read
  id-token: write
steps:
  # Check out the project, configure Node/pnpm, and install dependencies first.
  - uses: ziahamza/layercache/action/turbo@main
    with:
      team-url: https://cache.example.com
      compatibility: linux-amd64-node24-schema1
  - run: pnpm turbo run build
```

Use your Team Cache origin and a compatibility identity for your build toolchain.
The server must authorize your repository. `main` intentionally tracks current
code until versioning is introduced. Public action source does not grant access
to anyone else's Team Cache. See the [publication checklist](docs/open-source-preparation.md)
for the remaining source-publication gates.

The [four-project rollout report](docs/rollout-2026-09-12.md) records main-branch
remote-hit evidence, deployment fixes, and remaining qualification limits.

[Native build caching](native/README.md) adds an Expo development-client provider,
a local CLI shared across worktrees, and `action/native` for checking verified
artifacts on Linux before scheduling a project's existing Mac runner. It does
not host runners or replace production signing, repacking, or OTA delivery.

The daemon implements Turborepo's authenticated v8 artifact API. `layercache run` mints a short-lived Workspace token and injects `TURBO_API`, `TURBO_TOKEN`, and `TURBO_TEAM` for one command:

```bash
bin/layercache run -- pnpm exec turbo run build --summarize
```

If the local daemon is unavailable, `layercache run` prints a warning and runs the child without cache environment injection. A cache outage therefore does not replace a correct local build failure or success.

The first build stores opaque Turbo artifacts. A later worktree or clone with the same Turbo hash and Layer Cache compatibility identity can restore them. Resolution order is Local Cache, Team Cache, then Public Cache. Team and Public hits are verified and warmed into Local Cache.

Turbo still owns its task fingerprint. Declare task inputs, environment variables, dependencies, and outputs correctly in `turbo.json`; Layer Cache cannot make an incomplete Turbo key safe.

Preview or apply the repository-local Turbo endpoint without writing a bearer token:

```bash
bin/layercache integration turbo --json
bin/layercache integration turbo --apply --json
```

The integration owns only `.turbo/config.json`'s `apiUrl` field and preserves unrelated edits. Run Turbo through `layercache run` so its short-lived credential stays out of files and command output.

## GitHub Actions cache v1

The daemon implements the GitHub Actions cache v1 reserve, ranged-upload, commit, lookup, and archive-download flow. Exact keys, ordered restore-key prefixes, current/default ref scoping, first-writer-wins behavior, and Team Cache warming are covered.

For a local CI tool that runs an existing workflow, keep the workflow's stock `actions/cache` step unchanged and launch the tool through Layer Cache:

```bash
bin/layercache run -- <your-local-ci-command> [arguments...]
```

`layercache run` injects `ACTIONS_CACHE_URL` and `ACTIONS_RUNTIME_TOKEN` and disables the v2 selection flag for that child process. This is the intended seam for tools such as [Redwood's Local CI](https://github.com/redwoodjs/local-ci), provided the tool forwards those variables to action processes.

The stock action through `layercache run` uses the Local runtime's configured compatibility identity. If Local CI executes a job for a different OS, architecture, libc, runtime, or toolchain ABI than that configuration declares, use a dedicated config with the matching `--compatibility-id`. The Layer Cache action below selects the actual Node job OS and architecture automatically and accepts a more specific `compatibility` override.

The stock v1 client does not attach its bearer token when fetching `archiveLocation`. Layer Cache therefore returns an HMAC-signed archive URL with a five-minute default lifetime; metadata and upload requests still require the bearer token.

When the Local runtime has `--public-url` and `--public-trust-key`, the same Actions v1 endpoint searches Local Cache, optional Team Cache, then verified Public Cache. No workflow change is needed. Public lookup is read-only and exact-coordinate: the server binds repository, ref, key, version, project, and compatibility into the signed identity. It verifies the DSSE signature, repository provenance, full archive digest, size, and signed lease before committing the archive to Local Cache and returning a v1 archive URL.

A warmed Public archive and its signed trust metadata survive a runtime restart. If Public Cache is offline, the Local runtime may reuse those bytes only until the signed lease expires. An authoritative online miss, revocation, or ambiguous publication invalidates the warmed entry. Signature, identity, digest, size, and expiry failures fail closed and are never treated as an offline cache hit.

This release does **not** implement the Actions cache v2 protocol.

### Layer Cache action

[`action/cache`](action/cache) is a Node 24 action that preserves the familiar `path`, `key`, `restore-keys`, `lookup-only`, and `fail-on-cache-miss` inputs while selecting a Layer Cache v1 endpoint. After this repository is published, reference an immutable commit:

```yaml
- name: Restore dependency cache
  uses: layercache/layercache/action/cache@<immutable-commit>
  with:
    endpoint: ${{ secrets.LAYER_CACHE_ENDPOINT }}
    project: github.com/acme/widget
    path: |
      ~/.pnpm-store
      node_modules
    key: ${{ runner.os }}-${{ runner.arch }}-pnpm-${{ hashFiles('pnpm-lock.yaml') }}
    restore-keys: |
      ${{ runner.os }}-${{ runner.arch }}-pnpm-
    public-cache-mode: disabled
```

Grant the job `id-token: write` when using `project`. The action exchanges GitHub OIDC for a short-lived project capability; the `token` input is a project-scoped fallback.

`public-cache-mode: verified` makes the action itself enforce an exact primary-key Public match. It requires `public-trust-key`, `public-recipe-digest`, and `public-builder`, and binds the signed repository, ref, commit, path-derived cache version, compatibility, platform, declared inputs, toolchain, builder, digest, and size. `lookup-timeout-seconds` defaults to two seconds for connection and response metadata; archive transfer has a separate progress-based idle deadline. Signature, identity, traversal, link, type, size, digest, and decompression failures become safe cache misses before workspace mutation. Extraction stages on the workspace filesystem and rolls back ordinary application failures; an incomplete rollback is a hard action failure and preserves recovery data.

OIDC or cache service outages are warnings and cache misses by default. Invalid action inputs and an incomplete transactional rollback are hard failures; an unsafe verified archive fails closed as a cache miss before workspace mutation. When `fail-on-cache-miss: true` is explicit, an authentication outage, restore outage, unsafe archive, or ordinary miss fails the action as requested.

Direct Team Cache requests are namespaced by the action's compatibility identity. Its default is the Node job platform plus `schema1`; set `compatibility` explicitly when output portability also depends on libc, runtime, or toolchain ABI.

Dependencies are exact-pinned in `package.json` and integrity-locked in `pnpm-lock.yaml`. The bundled `dist/main` and `dist/post` files are committed because GitHub Actions executes them directly. CI rebuilds both bundles and fails if the result differs from Git.

## Docker Buildx and BuildKit

Layer Cache creates or reuses a named `docker-container` Buildx builder. The builder's native local cache survives across builds. An optional OCI repository adds platform-scoped Team Cache imports and `mode=max` exports.

```bash
bin/layercache setup \
  --buildkit-builder layercache \
  --buildkit-team-repository ghcr.io/acme/widget-build-cache \
  --non-interactive

# Preview builder, refs, and an optional Docker registry login.
bin/layercache integration buildkit \
  --registry-username "$REGISTRY_USER" \
  --registry-password-stdin --json

# Apply without putting the registry password in arguments or Layer Cache files.
# Shell tracing expands secrets into logs, so disable it before this pipe.
set +x
printf '%s\n' "$REGISTRY_PASSWORD" | \
  bin/layercache integration buildkit --apply \
    --registry-username "$REGISTRY_USER" \
    --registry-password-stdin --json

bin/layercache buildx plan \
  --platform linux/amd64 \
  --team-import main \
  --team-export "$BUILD_ID" \
  --load \
  -- --file Dockerfile .

bin/layercache buildx build \
  --platform linux/amd64 \
  --team-import main \
  --team-export "$BUILD_ID" \
  --load \
  -- --file Dockerfile .
```

The target platform is part of the repository path. Exports use unique `build-<id>` tags, OCI media types, an image manifest, and `mode=max`; export failure does not replace a correct Local Build result. The optional integration login sends the password only over standard input to `docker login`. Docker decides how to persist it. After login, Layer Cache checks Docker's configuration for an external credential helper. If it finds none or cannot inspect the configuration, it warns that Docker may have written reversible auth to `config.json`. Layer Cache neither persists nor prints the password. Do not run the password pipe with shell tracing enabled because `set -x` can print the expanded value. Registry authorization policy and image signing remain external.

`--load` supports one target platform. Use `--push` instead for registry output. At most four Team Cache imports are accepted. Each export uses a unique immutable reference and then promotes the configured mutable branch tag under process, file, and renewable PostgreSQL leases. A lost lease cancels only promotion, not the successful build. `--public-import native-key=publication-identity` resolves signed provenance and imports only the approved OCI manifest digest.

The command reports cached and completed BuildKit vertices from the raw JSON progress stream and persists them as one aggregate BuildKit group in the run report.

## Team Cache

A Team Cache server is another Layer Cache process with `--role team`. A production-shaped configuration uses PostgreSQL for project records, authorization, uploads, audit, measurements, and promotion leases, with S3-compatible storage for immutable bytes. Omitting the cloud settings keeps the embedded development backend.

```bash
# On the Team Cache host, behind an HTTPS reverse proxy:
bin/layercache setup \
  --config /var/lib/layercache/team.json \
  --data-dir /var/lib/layercache/data \
  --listen 127.0.0.1:7438 \
  --role team \
  --project github.com/acme/widget \
  --actions-archive-base-url https://cache.example.com \
  --local-token-file /run/secrets/layercache-admin-token \
  --team-member your-github-login=writer \
  --non-interactive

bin/layercache serve --config /var/lib/layercache/team.json
```

Connect a developer machine:

```bash
bin/layercache setup \
  --project github.com/acme/widget \
  --team-url https://cache.example.com \
  --non-interactive

LAYERCACHE_GITHUB_CLIENT_ID="$LAYER_CACHE_GITHUB_CLIENT_ID" \
  bin/layercache login
```

The server's `--local-token` is an administrator root credential. Keep it only on the Team Cache host and never distribute it as a developer cache token. `layercache login` exchanges an authorized GitHub member identity for a short-lived reader, writer, or administrator capability according to `--team-member`; CI should use the project-scoped OIDC exchange or a separately issued scoped fallback.

Turbo and Actions writes are published to Team Cache. A failed Turbo publication is retained in a durable retry queue; its exact Local Cache artifact stays pinned against collection until delivery completes, including across a daemon restart. `status --json` reports pending upload count and bytes. A Team Cache restore warms Local Cache so a later build can work while Team Cache is unavailable.

Cloud records and object names are project-isolated, and multiple Team/Public processes may share PostgreSQL and S3. GitHub member roles, device-login capability exchange, GitHub Actions OIDC exchange, quota/LRU collection, repair, audit, and durable ranged Actions uploads use the cloud backend. Layer Cache does not terminate TLS; expose remote roles only behind HTTPS and appropriate network controls.

## Public Cache and Public Builds

Public Cache is globally readable but not client-writable. A local daemon configured with `--public-url` and a pinned Ed25519 key can resolve exact Turbo and Actions identities. It verifies each DSSE/in-toto publication and complete SHA-256 digest before committing bytes to Local Cache. Same-origin redirect rules, expiry, ambiguity, and revocation fail closed. A warmed artifact may be used offline only within its signed lease.

The Public Cache role exposes a separate trusted collector credential and refuses Turbo client PUTs. `layercache public publish` exists for that collector pipeline and for acceptance testing; it is not a consumer upload path. A manual collector supplies `--builder-image-digest`; when all three worker asset pins are configured, the command derives the reviewed kernel/rootfs/contract composite automatically.

Adding at least one repository allowlist entry enables the Public Build control plane on a Public Cache server:

```bash
bin/layercache setup \
  --config /var/lib/layercache/public.json \
  --data-dir /var/lib/layercache/public-data \
  --listen 127.0.0.1:7439 \
  --role public \
  --project github.com/acme/widget \
  --public-build-worker-token-file /run/secrets/layercache-worker-token \
  --public-collector-token-file /run/secrets/layercache-collector-token \
  --public-build-repository https://github.com/acme/widget \
  --public-build-approved-ref refs/heads/main \
  --public-build-recipe sha256:9ec61a868d9e6d034014b6f9c36666ac456ebb56c2bbcc26f770bb234bbd0a87 \
  --public-build-recipe sha256:bb99da9d0d88a81fc1b47d3b89fff7cf878bf9b581683578759b4d54a936c3d4 \
  --buildkit-public-repository registry.example.com/acme/widget-public-cache \
  --non-interactive

bin/layercache serve --config /var/lib/layercache/public.json
```

The allowlist accepts canonical HTTPS `github.com/<owner>/<repository>` URLs. Repeat `--public-build-repository` to add repositories. Client, worker, and collector capabilities are separate; raw lease and publication-permit credentials are never stored in PostgreSQL.

The CLI control commands use the listener and credentials in the named Public role configuration. A request names an immutable commit, a complete recipe digest, one integration, one target, one Linux platform, and canonical non-secret inputs. Admission allows at most 32 unique lowercase names, 1,024 bytes per UTF-8 value, and 16 KiB across names and values, then sorts inputs by name. Control characters are rejected. The immutable guest recipe remains responsible for rejecting names or values it does not support. Turbo and Actions requests must declare the complete compatibility identity. Supported execution platforms are `linux/amd64` and `linux/arm64`.

Public Build execution admits maintained Turbo, Actions, and BuildKit recipes. The maintained BuildKit executor starts a private offline BuildKit daemon, builds the named Dockerfile target from the checked-out repository, and exports a complete OCI image-layout cache result. The trusted host validates the layout and every referenced descriptor, pushes the complete graph to the configured untagged OCI repository, reads back the exact root manifest, and signs that manifest digest. It never labels the opaque guest-output hash as an OCI manifest digest.

```bash
bin/layercache public-build request \
  --config /var/lib/layercache/public.json \
  --repository https://github.com/acme/widget \
  --commit 0123456789abcdef0123456789abcdef01234567 \
  --integration turbo \
  --target '@acme/widget#compile' \
  --recipe sha256:9ec61a868d9e6d034014b6f9c36666ac456ebb56c2bbcc26f770bb234bbd0a87 \
  --platform linux/amd64 \
  --input compatibility=linux-amd64-node@24-schema1 \
  --cpu-millis 2000 \
  --memory-bytes 4294967296 \
  --disk-bytes 21474836480 \
  --timeout 30m \
  --json

bin/layercache public-build status \
  --config /var/lib/layercache/public.json \
  --id public-build-1 --json

bin/layercache public-build logs \
  --config /var/lib/layercache/public.json \
  --id public-build-1 --json

# For a reviewed Dockerfile with a named `release` stage:
bin/layercache public-build request \
  --config /var/lib/layercache/public.json \
  --repository https://github.com/acme/widget \
  --commit 0123456789abcdef0123456789abcdef01234567 \
  --integration buildkit \
  --target release \
  --recipe sha256:bb99da9d0d88a81fc1b47d3b89fff7cf878bf9b581683578759b4d54a936c3d4 \
  --platform linux/amd64 \
  --cpu-millis 2000 \
  --memory-bytes 4294967296 \
  --disk-bytes 21474836480 \
  --timeout 30m \
  --json
```

The request result supplies the build ID. An identical queued, running, or successful request reuses the existing build. Builds move through `queued`, `running`, and `succeeded`, or end as `failed` or `cancelled`. State, sanitized logs, and successful publication metadata survive a service restart. `public-build cancel --id <build-id>` cancels queued or running work.

A successful BuildKit worker result returns `publicImportSelector`; `public-build status --json` returns the same value under `publication.publicImportSelector`. Pass that exact `native-key=publication-identity` value to `layercache buildx build --public-import`. Layer Cache verifies the signed publication and imports only the approved registry manifest digest.

The worker protocol has commands for `lease`, `append-log`, `complete`, and `fail`. A lease is bound to the worker ID and its returned lease token:

```bash
bin/layercache public-build worker lease \
  --config /var/lib/layercache/public.json \
  --worker-id worker-linux-amd64 \
  --integration turbo \
  --platform linux/amd64 --json

bin/layercache public-build worker append-log \
  --config /var/lib/layercache/public.json \
  --id "$PUBLIC_BUILD_ID" --worker-id worker-linux-amd64 \
  --lease-token-file /run/secrets/layercache-build-lease --message "building" --json

bin/layercache public-build worker complete \
  --config /var/lib/layercache/public.json \
  --id "$PUBLIC_BUILD_ID" --worker-id worker-linux-amd64 \
  --lease-token-file /run/secrets/layercache-build-lease \
  --publication-identity "$PUBLIC_CACHE_PUBLICATION_IDENTITY" --json

bin/layercache public-build worker fail \
  --config /var/lib/layercache/public.json \
  --id "$PUBLIC_BUILD_ID" --worker-id worker-linux-amd64 \
  --lease-token-file /run/secrets/layercache-build-lease --reason "build failed" --json
```

The lease result supplies the build ID and token used by later worker commands. Completion cannot upload raw output or accept a caller-provided digest. Exact live-lease fencing prevents a cancelled, expired, replaced, or cross-worker lease from publishing. The server resolves the existing signed Public Cache publication named by `--publication-identity` and checks its build ID, repository, commit, recipe, platform, integration, artifact digest, and size before accepting completion.

Admission rejects repositories outside the allowlist, mutable or malformed commits, partial recipe digests, unsupported integrations or platforms, unsafe target names, and resource requests above 8 CPU cores, 16 GiB memory, 50 GiB disk, or one hour. The request schema has no secret, privileged execution, or host Docker socket fields, and unknown fields are rejected.

`public-build worker run` is the production Linux execution seam. It fails closed unless KVM, QEMU, cgroup v2 delegation, a dedicated QEMU UID/GID, an immutable pinned kernel, a raw immutable root filesystem, and a matching maintained guest contract are present. Every build receives a fresh root overlay, read-only source image, bounded writable disk, no NIC, QEMU sandbox restrictions, CPU/memory/pid/storage/time limits, and a virtio-serial output channel to the trusted host collector. The collector credential is never placed in the guest, environment, or QEMU arguments.

Layer Cache does not ship a prebuilt generic guest image or dependency mirror. The opt-in Linux x64 assembly recipes and real KVM QA harnesses under `qa/public-build-kvm` build the current guest agent and reviewed contracts, pin external tool images where applicable, and print the resulting asset digests. They are qualification tools, not a production image supply chain. Operators must still review, pin, and maintain the kernel, root filesystem, guest contract, and recipe dependencies. Guests are offline-only, so dependencies must be vendored in source or baked and pinned in that image. Run `public-build worker run --preflight` before leasing work.

## Disposable VM route

Issuing a VM route creates short-lived, project-scoped Turbo and Actions credentials for an endpoint the VM can already reach. It does not change the daemon listener, open a firewall, or configure TLS. Expose the runtime deliberately, preferably through an authenticated HTTPS reverse proxy, then issue the narrowest useful route:

```bash
bin/layercache vm-route issue \
  --endpoint https://cache-host.example.internal \
  --compatibility linux-amd64-node@24-schema1 \
  --integration all \
  --ttl 15m \
  --output vm-route.json
```

`--output` creates a new regular file with mode `0600` and refuses to replace an existing file or symlink. The JSON contains `TURBO_API`, `TURBO_TEAM`, `TURBO_TOKEN`, `ACTIONS_CACHE_URL`, `ACTIONS_RUNTIME_TOKEN`, and the shared run identity. Import only the variables needed by the VM process and delete the response when the VM is discarded. Treat the file as a credential. Use explicit `--json` only when credential JSON on stdout is intentional; do not rely on ordinary shell redirection to protect it. `--read-only` removes publication authority. Non-loopback cleartext HTTP is rejected unless `--allow-insecure-http` is explicit, and credentials cannot live longer than one hour.

## Run reports and ROI

`layercache run` prints its run ID after the child command exits. Use that ID to read a report:

```bash
bin/layercache report --run run-...
bin/layercache report --run run-... --json
```

The version 2 JSON report includes each outcome's integration, opaque Workspace identity when known, hashed logical identity, final result, source, bytes, timing segments, degraded state, estimator method, and confidence. Run and historical reports include per-integration group, hit, byte, and timing totals. BuildKit keeps one outcome group per successful build with separate completed and cached vertex counts. Gross avoided task time is reported separately from signed net estimated build time saved. Lookup, transfer, verification, restore, and upload overhead can make the net estimate negative. Missing evidence remains unknown rather than becoming a zero.

`layercache run` passes one signed run and Workspace identity to its Turbo, Actions, and nested `layercache buildx build` operations. Turbo gateway observations become final only after Run Summary reconciliation. The Actions v1 endpoint records a restore miss after lookup and a restore hit after the signed archive download completes. When an authenticated run later saves the same primary key and version, the successful commit enriches that existing miss with the observed work interval and upload cost. It also stores the work interval on the entry for future hits. A missing, expired, or evicted correlation stays unknown, and the save never creates a second outcome. Stock-client extraction time remains unknown because the protocol does not expose it. BuildKit records observed build duration, graph identity, cached-vertex ratio, and import or export bytes when the raw progress stream identifies their direction. It marks the source Local only when no remote importer was configured; otherwise a cached source is unattributed. A standalone `layercache buildx build` creates its own report run ID. CPU-hours, causal source when several BuildKit importers exist, and money are not inferred.

A Turbo task satisfied by Turborepo's own Workspace-local cache never reaches the Layer Cache gateway. Its Run Summary task is retained as an unknown, ineligible outcome rather than being credited as a Layer Cache Local hit or time saving. Use `--remote-only` and clear every root and package-level `.turbo` directory when manually proving Layer Cache reuse.

## Bypass and diagnostics

Bypasses are persisted by adapter and shown in `status --json`:

```bash
bin/layercache bypass --adapter turbo --json
bin/layercache bypass --adapter actions --json
bin/layercache bypass --adapter buildkit --json
bin/layercache bypass --adapter all --json

bin/layercache bypass --clear --adapter turbo --json
bin/layercache bypass --clear --json
```

`layercache disable` persistently bypasses every adapter by default, or one named by `--adapter`. `layercache run --bypass turbo -- COMMAND` applies a transient one-command bypass without changing configuration; repeat the flag or use `all`. A run bypass omits the selected Turbo or Actions endpoint. Nested `layercache buildx` commands inherit the transient setting, remove Team Cache imports/exports, and add `--no-cache`. Bypass forces recomputation but does not delete existing entries.

## Security boundaries

- GitHub refresh material is stored only in macOS Keychain or Linux Secret Service. Configuration files contain short-lived project capabilities and service-role credentials supplied by the operator, with owner-only permissions.
- Workspace, CI, Team, Public Build worker, and collector capabilities are distinct and checked server-side. Selectors, keys, and query values never create authority.
- The daemon speaks plain HTTP. Use loopback locally and an authenticated TLS reverse proxy for any remote Team or Public Cache.
- Anyone with a Team Cache token is inside that team's artifact trust boundary. Use separate servers and credentials for unrelated tenants.
- Public Cache signatures prove the configured publisher authorized the bytes; they do not make an unsafe publisher or build environment trustworthy.
- Native cache artifacts are opaque. Turborepo artifacts can include replayable task logs with absolute paths or other build output, so review what a task prints before enabling Team Cache and allow only trusted Public Build publishers.
- Actions archive URLs are short-lived capabilities and may appear in proxy access logs. Treat those logs accordingly.
- Layer Cache can delegate an explicit BuildKit login to Docker without retaining the password. Docker may keep reversible registry auth in `config.json` when no external credential helper is configured. Protect Docker's configuration directory and manage credential-helper lifecycle outside Layer Cache.
- Cache reuse is only as correct as the upstream native key. Incomplete Turbo inputs, overly broad Actions restore keys, or mutable Docker inputs can still produce incorrect builds.

Set standard `OTEL_EXPORTER_OTLP_ENDPOINT` or signal-specific OTLP/HTTP endpoints to export bounded HTTP traces and metrics. Telemetry records static route templates, normalized methods/statuses, role, integration, and timing only. It excludes request targets, queries, headers, bodies, cache keys, paths, environment values, and secrets. Product-owned ROI events remain in PostgreSQL or SQLite independently of OpenTelemetry.

## Platforms and CI

The code targets Linux and macOS. Windows and Xcode/iOS caching are not implemented.

The CI workflow runs the full Go test suite, vet, and a native binary build on GitHub-hosted Linux x64, Linux arm64, and macOS arm64 runners. The race detector runs on both Linux architectures. Each job asserts the runner architecture before testing.

The release support policy is committed in `qa/client-versions.json`. Every native candidate exercises independently locked Turbo 2.9.14 and 2.10.12 fixtures plus stock `@actions/cache` 5.0.0 and 6.2.0 clients. SHA-256-verified Buildx 0.36.1 binaries prove a real local-cache hit and restored image bytes on native Linux x64 and arm64; a separate installed-binary job repeats the oracle with the minimum supported Buildx 0.21.3 client. Team registry, macOS-to-Linux registry reuse, and KVM guest trials remain dedicated manual QA because they require controlled external infrastructure.

Tagged release archives, checksums, and their enclosed binaries receive GitHub's cryptographically signed SLSA provenance attestations before the release is published. Repository release immutability must be enabled. The publish job rejects a tag that no longer resolves to the tested `github.sha`, stages assets in a draft, checks the tag again before publication, and verifies that GitHub locked the published release and tag. After downloading one archive and its checksum from a GitHub release, verify both:

```bash
sha256sum --check layercache-linux-amd64.tar.gz.sha256
gh attestation verify layercache-linux-amd64.tar.gz \
  --repo OWNER/REPO \
  --signer-workflow OWNER/REPO/.github/workflows/release.yml
```

Use the actual repository owner in place of `OWNER/REPO`. The macOS archive has the same GitHub build-provenance attestation, but this release is not Apple Developer ID signed or notarized.

## Development and QA

```bash
mise exec -- go test ./... -count=1
mise exec -- go test -race ./... -count=1
mise exec -- go vet ./...

cd action/cache
pnpm install --frozen-lockfile
pnpm check
```

`pnpm check` typechecks and tests the action, rebuilds its checked-in bundles, and fails on any bundle drift.

See [qa/README.md](qa/README.md) for installed-binary smoke tests with real Turbo, stock `@actions/cache` v1, Buildx, Team Cache, Public Cache, and the Public Build control plane. Run project trials only in disposable independent clones or worktrees, with dedicated cache directories and builder names.

## Self-serve cloud

Connect your CLI to a hosted team project with `layercache connect --cloud URL --project ID --github-cli`. The hosted dashboard provides GitHub sign-in, team invitations and roles, project creation, and cache activity. See [self-serve cloud setup and limits](docs/self-serve-cloud.md) for the engineer workflow and operator deployment.
