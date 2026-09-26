# Self-serve cloud browser QA

This suite launches the real `serve-cloud` CLI against isolated configuration and
storage. A local Node HTTP server acts as GitHub for authorization redirects,
PKCE token exchange, stable user identity, and repository administration checks.
The fake provider exists only in this QA fixture; no production authentication
bypass is added.

```sh
mise exec -- pnpm install --frozen-lockfile
mise exec -- pnpm exec playwright install chromium firefox webkit
mise exec -- pnpm build
mise exec -- go build -o /tmp/layercache-cloud-qa ./cmd/layercache
LAYERCACHE_BIN=/tmp/layercache-cloud-qa DASHBOARD_BROWSER=chromium mise exec -- node qa/cloud/run.ts
LAYERCACHE_BIN=/tmp/layercache-cloud-qa DASHBOARD_BROWSER=firefox mise exec -- node qa/cloud/run.ts
LAYERCACHE_BIN=/tmp/layercache-cloud-qa DASHBOARD_BROWSER=webkit mise exec -- node qa/cloud/run.ts
```

`QA_OUTPUT` optionally selects a directory for screenshots and `results.json`.
The fixture's private temporary directory and browser output paths are printed.
Each run uses fresh data and two independent browser contexts. Processes stop
when the suite completes or is interrupted. Test assertions exit nonzero on
failure. `CHROMIUM_PATH` can select an explicit Chromium binary; other engines
use their pinned Playwright installations.

The flow covers real OAuth sign-in, beta creator admission denial at both UI and API, empty-workspace onboarding, team creation,
repository administration denial, project provisioning, an actual cache upload,
an actual `layercache connect --github-cli` run with isolated fake `gh`, CLI instructions, targeted invitations, reader/writer roles, session reload,
report periods, four viewport sizes, last-admin protection, member removal,
logout, and expired-session recovery.

The storage-pool fixture marks a private directory on the host filesystem and
sets its configured ceiling to that filesystem's reported size. This exercises
pool-aware provisioning and cache writes without requiring privileged mounts; it
does **not** prove a physically bounded production volume or disk exhaustion.
WebKit exercises Safari's engine, not the native Safari application on macOS/iOS.

## Two-user Turbo restore

The browser suite checks real CLI connection and a direct artifact upload. The
separate Turbo acceptance run proves that a cache-aware build crosses user and
Workspace boundaries. Install the pinned Turbo client, then run this against a
built CLI or a published installed binary:

```sh
pnpm --dir qa/fixtures/turbo install --frozen-lockfile
LAYERCACHE_BIN=/absolute/path/to/layercache \
TURBO_BIN="$PWD/qa/fixtures/turbo/node_modules/.bin/turbo" \
node qa/cloud/turbo-restore.ts
```

It signs in two emulated GitHub users to an isolated loopback cloud, creates a
team and repository project, invites the second user as a reader, and connects
two separate CLI configurations. The pinned Turbo client builds in Alice's
fresh Git clone, then Bob restores from another clone after Alice's Local Cache
daemon stops. The assertions require one actual task execution, native Turbo
`MISS` then `REMOTE HIT`, positive uploaded/downloaded byte counts, a Team Cache
source in the Layer Cache report, identical output SHA-256, and denial of Bob's
previous capability after removal. Evidence is written to `turbo-restore.json`
in `QA_OUTPUT` or a fresh temporary evidence directory. The CI performance job
runs this against the candidate binary and preserves that JSON.

The GitHub provider and cloud hostname are emulated. This test does not replace
an external HTTPS deployment, GitHub's real OAuth service, or an independent
customer machine on the public network.

## PostgreSQL contract

Portal PostgreSQL CI runs the store contract against a pinned, disposable
PostgreSQL service. Against an isolated local database named
`layercache_portal_test`, run:

```sh
LAYERCACHE_PORTAL_TEST_POSTGRES_URL='postgres://USER:PASSWORD@127.0.0.1:PORT/layercache_portal_test?sslmode=disable' mise exec -- go test -race ./internal/portal -run '^TestStore' -count=3
```

The fixture rejects non-loopback/non-test databases and creates/drops a random
schema per test. Coverage includes durable identity/membership, invitation
replay/revocation, concurrent last-administrator removal, global project capacity,
session limits, and live capability revocation. Three PostgreSQL race runs and
SQLite race tests passed locally on 2026-09-22.
