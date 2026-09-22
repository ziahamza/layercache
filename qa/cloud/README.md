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

The flow covers real OAuth sign-in, empty-workspace onboarding, team creation,
repository administration denial, project provisioning, an actual cache upload,
an actual `layercache connect --github-cli` run with isolated fake `gh`, CLI instructions, targeted invitations, reader/writer roles, session reload,
report periods, four viewport sizes, last-admin protection, member removal,
logout, and expired-session recovery.

The storage-pool fixture marks a private directory on the host filesystem and
sets its configured ceiling to that filesystem's reported size. This exercises
pool-aware provisioning and cache writes without requiring privileged mounts; it
does **not** prove a physically bounded production volume or disk exhaustion.
WebKit exercises Safari's engine, not the native Safari application on macOS/iOS.

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
