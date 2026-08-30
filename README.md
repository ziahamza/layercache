# Layer Cache

Layer Cache reuses build work across short-lived worktrees, clones, VMs, and CI jobs. A local daemon owns a bounded content-addressed cache, speaks native cache protocols, and can fall back to a shared Team Cache or a verified Public Cache.

This repository contains a working local-first MVP for Turborepo, GitHub Actions cache v1, and Docker Buildx/BuildKit. It is intentionally narrower than the full product direction: npm/CDN mirrors, Bazel, Vite, Next.js, Xcode, Windows, multi-tenant cloud storage, and production Public Build execution environments are not implemented here.

## Cache model

| Name | Who can read or request | Who can write or complete | Current implementation |
| --- | --- | --- | --- |
| Local Cache | One machine | Local Builds | Filesystem CAS and SQLite metadata with a configured quota and LRU collection |
| Team Cache | One configured project/team | Clients holding that Team Cache token | A single-project Layer Cache server; Local Cache misses fall back to it and warm locally |
| Public Cache | Anyone | Only the trusted publisher endpoint | Signed, immutable publications verified before a Local Cache exposes their bytes |
| Public Build | Clients holding the Public Cache client token | Workers holding the publisher credential; completion must name an existing matching Public Cache publication | Persistent request, status, log, cancel, lease, complete, and fail control plane; no production executor or trusted output collector |

A Workspace is a checkout and execution environment, including a Git worktree or disposable VM. A Workspace can disappear while Local Cache or Team Cache artifacts remain reusable.

## Build and configure

The pinned development toolchain is Go 1.25.4, Node.js 24.13.0, and pnpm 10.34.5.

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

`--max-size` bounds committed Local Cache artifact bytes. Layer Cache also preserves at least the greatest of 5 GiB, 5% of the cache filesystem, and `--min-free-bytes`; the flag is an optional upward override. Cache admission fails safely when that reserve cannot be kept. Atomic Actions and Public Cache transfers use bounded temporary space: retained upload chunks total at most `--max-size`, and all transient staging combined totals at most twice `--max-size`.

The default compatibility identity is `<goos>-<goarch>-schema1`. Use `--compatibility-id`, for example `linux-amd64-glibc2.39-node@24-schema1`, whenever libc, runtime, or toolchain ABI affects outputs. Layer Cache deliberately does not guess arbitrary toolchains. Identities are lowercase, at most 256 bytes, and may use `-_.:+@` delimiters.

This MVP currently supports preview and explicit `--non-interactive` setup. A guided interactive login/project flow and operating-system credential-manager integration remain future work.

Useful lifecycle commands:

```bash
bin/layercache doctor --json
bin/layercache gc --json
bin/layercache repair --json
bin/layercache stop --json
```

`doctor` is read-only. `repair` recreates missing owned data directories and reconstructs runtime ownership only from the authenticated live daemon identity; it never trusts a PID alone. `gc` requires a running daemon. Uninstall makes the cache choice explicit:

```bash
bin/layercache uninstall --preserve-cache --json
bin/layercache uninstall --delete-cache --yes --json
```

Deletion is limited to the exact configured data directory and requires a matching ownership marker. Broad paths such as a filesystem root or home directory are rejected.

## Turborepo

The daemon implements Turborepo's authenticated v8 artifact API. `layercache run` mints a short-lived Workspace token and injects `TURBO_API`, `TURBO_TOKEN`, and `TURBO_TEAM` for one command:

```bash
bin/layercache run -- pnpm exec turbo run build --summarize
```

The first build stores opaque Turbo artifacts. A later worktree or clone with the same Turbo hash and Layer Cache compatibility identity can restore them. Resolution order is Local Cache, Team Cache, then Public Cache. Team and Public hits are verified and warmed into Local Cache.

Turbo still owns its task fingerprint. Declare task inputs, environment variables, dependencies, and outputs correctly in `turbo.json`; Layer Cache cannot make an incomplete Turbo key safe.

To configure Turbo without `layercache run`, obtain the endpoint values directly:

```bash
bin/layercache integration turbo --json
```

The JSON includes a bearer token. Do not write it to logs or commit it.

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
    token: ${{ secrets.LAYER_CACHE_TOKEN }}
    path: |
      ~/.pnpm-store
      node_modules
    key: ${{ runner.os }}-${{ runner.arch }}-pnpm-${{ hashFiles('pnpm-lock.yaml') }}
    restore-keys: |
      ${{ runner.os }}-${{ runner.arch }}-pnpm-
    public-cache-mode: disabled
```

The action's `public-cache-mode` remains `disabled`: the Node action does not fetch or verify Public Cache publications itself. Its endpoint may still be a Local Layer Cache runtime with the verified Public fallback described above. In that path, the daemon checks the signed publication and every archive byte before the action receives an ordinary v1 cache hit.

Direct Team Cache requests are namespaced by the action's compatibility identity. Its default is the Node job platform plus `schema1`; set `compatibility` explicitly when output portability also depends on libc, runtime, or toolchain ABI.

Dependencies are exact-pinned in `package.json` and integrity-locked in `pnpm-lock.yaml`. The bundled `dist/main` and `dist/post` files are committed because GitHub Actions executes them directly. CI rebuilds both bundles and fails if the result differs from Git.

## Docker Buildx and BuildKit

Layer Cache creates or reuses a named `docker-container` Buildx builder. The builder's native local cache survives across builds. An optional OCI repository adds platform-scoped Team Cache imports and `mode=max` exports.

```bash
bin/layercache setup \
  --buildkit-builder layercache \
  --buildkit-team-repository ghcr.io/acme/widget-build-cache \
  --non-interactive

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

The target platform is part of the repository path. Exports use unique `build-<id>` tags, OCI media types, an image manifest, and `mode=max`; export failure does not replace a correct Local Build result. Registry login and authorization remain Docker's responsibility.

`--load` supports one target platform. Use `--push` instead for registry output. At most four Team Cache imports are accepted. Automatic mutable-tag promotion and CLI resolution of digest-pinned Public Cache BuildKit artifacts are not included in this release.

The command reports cached and completed BuildKit vertices from the raw JSON progress stream. Those vertex metrics are separate from the Turbo run ROI report.

## Team Cache

A Team Cache server is another Layer Cache process with `--role team`. The working server is scoped to one project and token per configuration and uses local filesystem/SQLite persistence.

```bash
# On the Team Cache host, behind an HTTPS reverse proxy:
bin/layercache setup \
  --config /var/lib/layercache/team.json \
  --data-dir /var/lib/layercache/data \
  --listen 127.0.0.1:7438 \
  --role team \
  --project github.com/acme/widget \
  --actions-archive-base-url https://cache.example.com \
  --local-token "$LAYER_CACHE_TEAM_TOKEN" \
  --non-interactive

bin/layercache serve --config /var/lib/layercache/team.json
```

Connect a developer machine:

```bash
bin/layercache setup \
  --project github.com/acme/widget \
  --team-url https://cache.example.com \
  --team-token "$LAYER_CACHE_TEAM_TOKEN" \
  --non-interactive
```

Turbo and Actions writes are published to Team Cache. A failed Turbo publication is retained in a durable retry queue; its exact Local Cache artifact stays pinned against collection until delivery completes, including across a daemon restart. `status --json` reports pending upload count and bytes. A Team Cache restore warms Local Cache so a later build can work while Team Cache is unavailable.

This is not yet a horizontally scaled, multi-tenant cloud deployment. It has no built-in TLS termination, PostgreSQL metadata service, object-storage backend, organization roles, or token issuance service. Put it behind TLS and network access controls, and run separate configurations for separate trust boundaries.

## Public Cache and Public Builds

Public Cache is globally readable but not client-writable. A local daemon configured with `--public-url` and a pinned Ed25519 key can resolve exact Turbo and Actions identities. It verifies each DSSE/in-toto publication and complete SHA-256 digest before committing bytes to Local Cache. Same-origin redirect rules, expiry, ambiguity, and revocation fail closed. A warmed artifact may be used offline only within its signed lease.

The Public Cache role exposes a separate publisher credential and refuses Turbo client PUTs. `layercache public publish` exists for a trusted publisher pipeline and for acceptance testing; it is not a consumer upload path.

Adding at least one repository allowlist entry enables the Public Build control plane on a Public Cache server:

```bash
bin/layercache setup \
  --config /var/lib/layercache/public.json \
  --data-dir /var/lib/layercache/public-data \
  --listen 127.0.0.1:7439 \
  --role public \
  --project github.com/acme/widget \
  --publisher-token "$LAYER_CACHE_PUBLISHER_TOKEN" \
  --public-build-repository https://github.com/acme/widget \
  --non-interactive

bin/layercache serve --config /var/lib/layercache/public.json
```

The allowlist accepts canonical HTTPS `github.com/<owner>/<repository>` URLs. Repeat `--public-build-repository` to add repositories. Client requests require the Public Cache client token. Worker transitions require its separate publisher credential.

The CLI control commands use the listener and credentials in the named Public role configuration. A request names an immutable commit, a complete recipe digest, one integration, one target, and one Linux platform. Supported integrations are `turbo`, `buildkit`, and `actions`; supported platforms are `linux/amd64` and `linux/arm64`.

```bash
bin/layercache public-build request \
  --config /var/lib/layercache/public.json \
  --repository https://github.com/acme/widget \
  --commit 0123456789abcdef0123456789abcdef01234567 \
  --integration turbo \
  --target compile \
  --recipe sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc \
  --platform linux/amd64 \
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
```

The request result supplies the build ID. An identical queued, running, or successful request reuses the existing build. Builds move through `queued`, `running`, and `succeeded`, or end as `failed` or `cancelled`. State, sanitized logs, and successful publication metadata survive a service restart. `public-build cancel --id <build-id>` cancels queued or running work.

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
  --lease-token "$PUBLIC_BUILD_LEASE_TOKEN" --message "building" --json

bin/layercache public-build worker complete \
  --config /var/lib/layercache/public.json \
  --id "$PUBLIC_BUILD_ID" --worker-id worker-linux-amd64 \
  --lease-token "$PUBLIC_BUILD_LEASE_TOKEN" \
  --publication-identity "$PUBLIC_CACHE_PUBLICATION_IDENTITY" --json

bin/layercache public-build worker fail \
  --config /var/lib/layercache/public.json \
  --id "$PUBLIC_BUILD_ID" --worker-id worker-linux-amd64 \
  --lease-token "$PUBLIC_BUILD_LEASE_TOKEN" --reason "build failed" --json
```

The lease result supplies the build ID and token used by later worker commands. Completion cannot upload raw output or accept a caller-provided digest. The worker must first hand output to a trusted publisher pipeline. The server resolves the existing signed Public Cache publication named by `--publication-identity` and checks its build ID, repository, commit, recipe, platform, integration, artifact digest, and size before accepting completion.

Admission rejects repositories outside the allowlist, mutable or malformed commits, partial recipe digests, unsupported integrations or platforms, unsafe target names, and resource requests above 8 CPU cores, 16 GiB memory, 50 GiB disk, or one hour. The request schema has no secret, privileged execution, or host Docker socket fields, and unknown fields are rejected.

This is a working control plane, not a safe hosted build product. The repository does **not** include a production microVM-equivalent executor, restricted dependency egress, or a trusted output collector. The worker commands only move jobs through the protocol. Do not use them to execute untrusted repositories, and do not treat `public publish` as proof that an artifact came from an isolated build.

## Run reports and ROI

`layercache run` prints its run ID after the child command exits. Use that ID to read a report:

```bash
bin/layercache report --run run-...
bin/layercache report --run run-... --json
```

The stable JSON report includes final eligible outcomes, Local/Team/Public source attribution when known, hit rate, bytes, timing segments, degraded state, estimator method, and confidence. Gross avoided task time is reported separately from signed net estimated build time saved; lookup, transfer, verification, restore, and upload overhead can make the net estimate negative. Missing evidence remains unknown rather than becoming a zero.

The running service currently records these final outcomes for Turbo GET/PUT requests made with the scoped Workspace token. Actions and BuildKit expose cache evidence, but they are not yet correlated into the run report. CPU-hours and money are not inferred from estimated wall time.

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

`layercache run` omits the selected Turbo or Actions endpoint. BuildKit bypass removes Team Cache imports/exports and adds `--no-cache`. Bypass forces recomputation but does not delete existing entries.

## Security boundaries

- Local, Team, and publisher tokens are bearer credentials stored in the owner-only configuration file. There is no OS keychain integration yet.
- Workspace tokens are short-lived and tied to a run ID, but the Team Cache token is static until a team administrator rotates it.
- The daemon speaks plain HTTP. Use loopback locally and an authenticated TLS reverse proxy for any remote Team or Public Cache.
- Anyone with a Team Cache token is inside that team's artifact trust boundary. Use separate servers and credentials for unrelated tenants.
- Public Cache signatures prove the configured publisher authorized the bytes; they do not make an unsafe publisher or build environment trustworthy.
- Native cache artifacts are opaque. Turborepo artifacts can include replayable task logs with absolute paths or other build output, so review what a task prints before enabling Team Cache and allow only trusted Public Build publishers.
- Actions archive URLs are short-lived capabilities and may appear in proxy access logs. Treat those logs accordingly.
- BuildKit registry credentials, image signing, and registry authorization are outside Layer Cache.
- Cache reuse is only as correct as the upstream native key. Incomplete Turbo inputs, overly broad Actions restore keys, or mutable Docker inputs can still produce incorrect builds.

## Platforms and CI

The code targets Linux and macOS. Windows and Xcode/iOS caching are not implemented.

The CI workflow runs the full Go test suite, race detector, vet, and a native binary build on GitHub-hosted Linux x64, Linux arm64, and macOS arm64 runners. Each job asserts the runner architecture before testing. Linux x64 also cross-compiles Linux arm64 as an early compile gate. Docker Buildx end-to-end QA still requires a Docker-capable host and registry.

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
