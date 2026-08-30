# Manual QA

Run these checks against an installed binary and disposable Workspaces. Do not point destructive cleanup commands at an existing repository, Docker builder, image tag, configuration, or cache directory.

The first MVP's recorded real-project and protocol results are in [RESULTS.md](RESULTS.md).

## Automated release checks

From the repository root:

```bash
mise install
mise exec -- go test ./... -count=1
mise exec -- go test -race ./... -count=1
mise exec -- go vet ./...

(
  cd action/cache
  pnpm install --frozen-lockfile
  pnpm check
)
```

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

"$QA_LAYER_CACHE_BIN" run --config "$QA_LAYER_CACHE_CONFIG" -- \
  pnpm exec turbo run build --summarize

"$QA_LAYER_CACHE_BIN" run --config "$QA_LAYER_CACHE_CONFIG" -- \
  pnpm exec turbo run build --summarize
```

The second Turbo summary should report cached tasks, and declared output files must match the first build byte-for-byte. Copy the final `Layer Cache run: run-...` value and inspect its report:

```bash
"$QA_LAYER_CACHE_BIN" report \
  --config "$QA_LAYER_CACHE_CONFIG" \
  --run run-... --json
```

The report should show one final outcome per eligible Turbo artifact, the proven cache source, bytes, timing, and a signed net estimate. A small fixture may legitimately report negative savings.

Repeat the hit from an independent clone at a different absolute path. A linked worktree alone does not prove clone portability.

## Stock `@actions/cache` v1 client

The QA driver imports the exact locked `@actions/cache` package rather than calling Layer Cache internals:

```bash
cd /path/to/layercache
(
  cd action/cache
  pnpm install --frozen-lockfile
)

mkdir -p "$QA_LAYER_CACHE_ROOT/actions-a" "$QA_LAYER_CACHE_ROOT/actions-b"

(
  set -a
  eval "$("$QA_LAYER_CACHE_BIN" integration actions --config "$QA_LAYER_CACHE_CONFIG")"
  set +a
  unset ACTIONS_CACHE_SERVICE_V2

  node qa/stock-actions-cache.mjs \
    save "$QA_LAYER_CACHE_ROOT/actions-a" manual-v1-key
  node qa/stock-actions-cache.mjs \
    restore "$QA_LAYER_CACHE_ROOT/actions-b" manual-v1-key
)
```

The restore must print `restored:manual-v1-key`, and the restored file must match the saved file. The lookup response's archive URL is intentionally used without an Authorization header, matching the stock v1 client; its HMAC signature and expiry provide download authority.

This stock-client smoke test does not exercise Actions v2 or Public Cache restore. Actions v2 is not supported. Verified Public fallback through the running v1 service is covered by `TestGitHubActionsPublicCacheVerifiesWarmsAndHonorsRevocation` below.

## Docker Buildx

This smoke test needs a working Docker daemon and Buildx. It uses a dedicated builder and image name:

```bash
mkdir -p "$QA_LAYER_CACHE_ROOT/docker"
printf 'hello from Layer Cache\n' > "$QA_LAYER_CACHE_ROOT/docker/payload.txt"
printf 'FROM scratch\nCOPY payload.txt /payload.txt\n' > "$QA_LAYER_CACHE_ROOT/docker/Dockerfile"

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

Team Cache registry tests also require a dedicated OCI repository and Docker credentials. Use a unique `--team-export` ID for every build and test `linux/amd64` and `linux/arm64` on native hosts where available.

## Team Cache, Public Cache, and Public Build

The acceptance suite provisions isolated server configurations and verifies fresh-host warming, restart persistence, retry behavior, signed Turbo and Actions Public Cache restore, offline lease behavior, client write rejection, and revocation:

```bash
mise exec -- go test ./acceptance \
  -run 'TestTurboTeamCacheWarmsFreshHost|TestFailedTurboTeamPublicationRetriesAfterRuntimeRestart|TestGitHubActionsV1TeamCacheWarmsFreshHost|TestGitHubActionsPublicCacheVerifiesWarmsAndHonorsRevocation|TestVerifiedPublicCacheWarmsLocalAndRejectsClientWrites|TestPublicBuildControlPlaneCompletesPersistsAndFencesClients' \
  -count=1 -v
```

For a manual Team Cache trial, use a separate Team Cache data directory, configuration, token, and port. Put any non-loopback endpoint behind HTTPS. Verify a fresh client restores from Team Cache, then disconnect Team Cache and verify that the warmed Local Cache still restores the same bytes.

The Public Cache acceptance fixture uses the trusted publisher command. It proves publication verification and revocation, not production Public Build isolation.

The Public Build control-plane scenario requests an allowlisted immutable build, checks request deduplication, leases it to a capability-matched worker identity, sanitizes a log line, publishes a known fixture through the trusted publisher endpoint, and completes the build by publication identity. It then restarts the process and checks persisted state and logs. Separate branches cover cancellation, failure and retry, client and worker credential separation, mutable commit rejection, allowlist enforcement, and raw-byte injection rejection.

That scenario does not execute repository code. There is no production-isolated Public Build worker in this repository, so manual QA must not fill that gap by running an untrusted checkout in a normal process or container.

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

The command deletes the exact configured data directory after validating its ownership marker. It does not remove the surrounding temporary directory, Docker image, or Buildx builder. Remove those separately only after checking their exact names.
