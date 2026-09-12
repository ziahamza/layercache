# Runner setup

This action installs an exact immutable Layer Cache release and starts one Local
Cache for a GitHub job. It exports `TURBO_API`, `TURBO_TEAM`, and `TURBO_TOKEN` for
later steps, including package scripts that invoke Turbo. Pin both actions to an
immutable commit in the repository that publishes Layer Cache.

```yaml
permissions:
  contents: read
  id-token: write
steps:
  - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
  - uses: OWNER/layercache/action/setup@FULL_COMMIT_SHA
    id: layercache
    with:
      repository: OWNER/layercache
      version: v0.1.0
      team-url: https://cache.example.com
      compatibility: linux-amd64-node24-schema1
      max-size: '5368709120'
  - run: pnpm install --frozen-lockfile
  - run: pnpm exec turbo run build
  - uses: OWNER/layercache/action/cache@FULL_COMMIT_SHA
    with:
      endpoint: https://cache.example.com
      project: ${{ steps.layercache.outputs.project }}
      compatibility: ${{ steps.layercache.outputs.compatibility }}
      path: .next/cache
      key: next-${{ runner.os }}-${{ runner.arch }}-${{ hashFiles('pnpm-lock.yaml') }}
```

`OWNER`, the commit, and the release are placeholders until the repository and
first immutable release are published. The installer needs Bash, `gh`, `jq`,
`tar`, and `shasum`, available on the supported GitHub hosted runners. The action
uses Node 24 and supports native Linux amd64/arm64 and macOS arm64. For validation
before publication, set `binary` to the absolute path of a trusted built CLI.
That explicitly bypasses release download and provenance verification.

The Team Cache must authorize the workflow repository. Setup exchanges GitHub
OIDC for a Turbo-only capability. The cache action exchanges its own Actions-only
capability at restore/save time. Neither receives an administrator credential.
The Team Cache server must include the integration selector added with this
setup action. An older server rejects the selector, and setup warns and continues
with Local Cache. `team-token` accepts an explicitly scoped Turbo credential as a
fallback for environments without OIDC. Team authentication failures also warn
and continue with Local Cache.

The default project is `github.com/OWNER/REPO`; `project` also accepts an opaque
Team project identity. The server independently checks the signed GitHub
repository against that project's configured repository. Omitted compatibility
uses the CLI's detected OS, architecture, libc, and toolchain identity. Set up
the job's language toolchain before Layer Cache, or supply an explicit ABI
identity shared by the intended Local and Team builds.

For Local Cache-only Actions traffic, use the setup `endpoint`, `actions-token`,
and `compatibility` outputs with the cache action. Omit its `project` input, which
selects a fresh Team OIDC exchange. Setup leaves GitHub's `ACTIONS_*` environment
intact. This avoids routing unrelated GitHub tooling to an incompatible protocol.

Each invocation creates a protected, random directory under `RUNNER_TEMP` with
its own configuration, cache, and loopback port. Its post action stops the owned
daemon and removes only that directory. Post actions run in reverse order, so
declare setup before cache actions whose post steps save artifacts.

Credentials expire after `ttl-minutes`, which defaults to 60 and accepts 1 to 60.
The `expires-at` output and `LAYER_CACHE_EXPIRES_AT` environment variable make the
deadline explicit. Rerun setup before another long build phase to create fresh
credentials. The new invocation has a fresh Local Cache and can refill from Team
Cache; the previous invocation remains owned by its post cleanup. Expired Team
credentials degrade to Local Cache/build computation. Expired local capabilities
are cache authorization misses; setup does not silently mint a longer lease.

CLI commands should pass `--config "$LAYER_CACHE_CONFIG"`. The environment
variable is a handoff to later steps, not an override of the CLI's ordinary
configuration lookup. To collect full Turbo ROI reconciliation, run
`layercache run --config "$LAYER_CACHE_CONFIG" -- pnpm exec turbo run build --summarize`.

This replaces the Turbo remote endpoint for the job. Vercel's existing artifacts
are not migrated automatically. Keep the old integration in other jobs until a
cold build and a separate warm job demonstrate a Team Cache hit.

Validation:

```sh
node --test action/setup/setup.test.mjs scripts/install.test.mjs
LAYERCACHE_SETUP_BINARY=/absolute/path/to/layercache \
  node --test action/setup/setup.test.mjs
```

The installed test creates a disposable repository, starts the actual daemon,
round-trips Turbo bytes with the exported scoped credential, rejects an admin
request, and runs post cleanup. Release CI also invokes the action itself on
each native platform and checks its exports from a subsequent step.
