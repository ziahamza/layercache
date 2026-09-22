# Dashboard QA — 2026-09-22

Scope: the first CLI-connected dashboard slice, including multiple existing
projects, Local Cache and Team Cache visibility, browser sessions and credential
refresh. Hosted provisioning, machine enrollment and cloud runners remain outside
this slice. Three independent subagents reviewed standards/authorization, CLI/spec
behavior, and browser behavior. The lead reproduced findings, implemented fixes,
and ran integration and repository checks. Subagents reviewed the fixes again.

## Reproduced and fixed

| Finding | Fix and regression coverage |
| --- | --- |
| Refreshed project capability discarded on each snapshot | Keep it in memory while persisted scope and credentials remain unchanged; subsequent login, logout, endpoint changes and project replacement invalidate the cached authority. CLI integration regression exercises the actual fake-GitHub exchange flow. |
| Proactive refresh failure hides an unexpired usable capability | Try the unexpired capability against Team Cache; expired capabilities do not receive this fallback. Two expiry cases use a failing credential executable. |
| Logo navigation destroys the browser session | Scroll within the current page; browser regression keeps project cards visible. |
| Pasting the complete CLI link into the same tab cannot reconnect | Consume and erase new session fragments, with in-flight request handling. Browser checks cover reload recovery and reusing the same link during refresh. |
| Long repository identities clip on mobile | Allow the header text column to shrink and wrap; browser regression uses a 39-character owner and 100-character repository name. |
| Unnamed connection dialog and faint small text | Label the dialog, darken muted text, and check keyboard focus and measured contrast. |
| Connection instructions omit endpoint setup | Include setup, login and daemon-start commands while making prior Team Cache provisioning explicit. |
| Report authorization failure loses sign-in guidance | Preserve the report's authentication state independently from available cache status; HTTP and browser regressions verify it. |
| Expired snapshot can return null projects as successful JSON | Return HTTP 504 with retry guidance rather than an invalid successful projection; handler regression covers the response contract. |

## Independent standards review

No documented standards violations or actionable structural smells remained.
Domain terminology follows `CONTEXT.md`. HTTP serving and CLI credential ownership
remain separate. Adversarial tests found no host/session bypass, credential
disclosure, traversal, redirect forwarding, or cross-origin read exposure in the
tested cases. This is evidence for those cases, not a proof of complete security.

## Independent spec review

No outstanding actionable findings remained after the credential fixes. Tests
verify fresh capability reuse, login/logout changes, endpoint changes, expired
versus unexpired fallback, project identity replacement, invalid CLI arguments,
and clean shutdown. Deferred hosted features are documented rather than simulated.

## Verification

- Chromium: **28/28 checks passed**, independently and through the pinned local
  command. Includes six viewport widths, 32 projects, live storage values, session
  recovery, outages, evidence formatting, keyboard focus, contrast, and escaped
  project text. Fixtures run real local and Team daemons; selected response
  injection covers error and measurement edge cases.
- Installed CLI acceptance: two projects behind the real project gateway, distinct
  reader capabilities and persisted reports; wrong-project credentials withheld,
  new configuration picked up, and outage data cleared.
- Dashboard/CLI tests, including new regressions and adversarial HTTP cases, pass
  under the race detector. Typechecks, JavaScript checks/builds, Go vet, workflow
  and shell lint, and whitespace checks pass.
- Full Go suite and full repository race suite pass using the commands below.
  Focused dashboard race checks also pass after the final timeout/error-state edits.

```sh
mise exec -- pnpm check
mise exec -- go build -o /tmp/layercache-dashboard-qa-final ./cmd/layercache
# The existing maintained-job fixture assumes a 022 creation mask.
umask 022
LAYERCACHE_ACCEPTANCE_BINARY=/tmp/layercache-dashboard-qa-final \
  mise exec -- go test -p 1 ./... -count=1
LAYERCACHE_ACCEPTANCE_BINARY=/tmp/layercache-dashboard-qa-final \
  mise exec -- go test -race -p 1 ./... -count=1
LAYERCACHE_BIN=/tmp/layercache-dashboard-qa-final \
  mise exec -- pnpm test:dashboard
```

The initial parallel full-suite run encountered two existing Actions-cache timing
assertions under load; targeted reruns and the serialized full suite passed.
An unrelated maintained-job file-mode assertion failed under this host's `0002`
umask and reproduced on an archived, unchanged `HEAD` (`e1fc9ee`). It passes with
`0022`; no unrelated production code was changed to conceal the failure.

The shipping follow-up also passed **28/28 checks in Firefox and 28/28 in WebKit**
on Linux using fresh binaries and real fixtures. CI now runs all three engines.
Mobile-sized viewports and WebKit are not native Safari on macOS/iOS or physical
mobile hardware. Hosted GitHub execution is tracked in the dashboard PR.
Existing opt-in Go infrastructure tests retain their environment gates; these
commands do not certify a production PostgreSQL/S3 deployment or remote hardware.
No production cache, Phone deployment, or Parle workflow was changed.
