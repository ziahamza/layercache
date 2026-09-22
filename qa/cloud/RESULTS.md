# Cloud QA — 2026-09-22

| Check | Result |
| --- | --- |
| Chromium cloud browser journey | 19/19 passed |
| Firefox cloud browser journey | 19/19 passed |
| WebKit cloud browser journey | 19/19 passed |
| SQLite store contract under race detector | Passed |
| PostgreSQL 16 store contract under race detector, three repetitions | Passed |
| Portal OAuth/API/cache integration under race detector | Passed |
| CLI onboarding/operator configuration tests under race detector | Passed |
| TypeScript check, JS tests, generated bundles | Passed |
| Full Go suite (`go test -p 1 ./...`, umask 022) | Passed |
| Go vet and workflow/shell lint | Passed |

Each browser run used the real `serve-cloud` binary, local fake GitHub OAuth with
PKCE, two browser contexts, and real `layercache connect --github-cli` with an
isolated fake `gh` executable. The issued cache token uploaded a real artifact;
the UI displayed 10.0 KiB and one artifact. Invitations, role changes, removal,
last-administrator protection, reload, logout, and expired-session recovery passed.
Screenshots at 1440, 768, 390, and 320 pixels had no horizontal overflow; desktop
and mobile screenshots were visually inspected.

QA fixed an outstanding-invitation revocation hole, a global project-count race,
a concurrent-session-limit race, case-insensitive repository duplicates, a
pool-aware project directory provisioning failure, and CLI device authorization
errors that could include upstream response bodies.

These tests do not establish real GitHub authorization, a public HTTPS rollout,
production storage exhaustion, or native Safari application behavior. The fake
provider is confined to test fixtures. See README for repeatable commands.
