# CLI-connected team dashboard

The CLI dashboard reads existing Local Cache and Team Cache connections through
the CLI. It supports multiple projects and live cache visibility. The separate
[self-serve cloud service](self-serve-cloud.md) implements hosted teams and
projects; its public deployment is a separate launch gate.

Build the CLI normally, then run:

```bash
layercache dashboard --config /path/to/project.json
```

Open the complete loopback URL printed by the command. The CLI stays running
until Ctrl-C. A dashboard launched on a remote machine needs a loopback tunnel
to that machine, preserving its dashboard port. Use `--listen 127.0.0.1:7438`
to choose a stable tunnel port. The dashboard intentionally accepts only its
exact loopback host and port; it cannot be exposed through a generic reverse proxy.

To see several projects, repeat `--config`:

```bash
layercache dashboard \
  --config /path/to/first-project.json \
  --config /path/to/second-project.json
```

Configurations must have the `local` role and distinct project identities. The
limit is 32 projects. These are explicit CLI connections, not a directory of all
projects a GitHub account might be allowed to access. The dashboard doesn't
create or change projects, memberships, endpoints, or quotas.

For each configuration, start its Local Cache with `layercache start --config
/path/to/project.json`. Configure an existing Team Cache endpoint through
`layercache setup --team-url https://cache.example.com --config
/path/to/project.json --non-interactive`, then sign in:

```bash
layercache login --github-cli --config /path/to/project.json
```

A team administrator must already have provisioned the project and granted
access. The dashboard uses the existing GitHub/credential-manager refresh flow
in memory and reloads configuration on refresh, so a subsequent CLI login is
picked up. Refreshed project capabilities stay in memory; rotated GitHub sessions
remain in the protected credential manager. An unexpired capability remains
usable when proactive refresh fails, subject to the Team Cache's authorization.

## Display contract

- Current artifact bytes, configured artifact capacity, artifact count, and
  eviction policy come from each selected runtime's `/v1/status`.
- Last-24-hours or last-seven-days hit outcomes, integration counts, and net
  estimated build time saved come from that runtime's `/v1/reports`.
- Local and Team observations stay separate. They can overlap, so the dashboard
  does not sum them into a team-wide savings number. Team reports are the events
  observed/reconciled at that Team Cache, not a complete history of every
  engineer's local builds. Native artifacts do not yet have a dedicated native
  reporting integration here.
- Missing evidence is unknown. No eligible outcomes displays a dash for hit
  rate; an outage does not display an empty cache. Status can remain visible when
  reporting is unavailable. Failed dashboard refreshes clear the old snapshot.
- Artifact capacity is not total host disk usage or the separate OCI registry
  capacity. Savings are estimates with evidence coverage and confidence, not
  measured CPU hours or money.

The browser receives only a temporary dashboard credential and selected status
and report fields. Runtime/Team credentials, config paths, and internal storage
details stay in the CLI. The link's fragment is removed after page load, the
credential stays in page memory, and a reload requires reopening or pasting the
complete CLI link, including into the same tab. The link grants read access to all projects selected for that process;
keep it private. Stopping the CLI invalidates it. Upstream redirects are not
followed, and the browser cannot supply upstream endpoints or project selectors.

## Next hosted slices

1. Launch and qualify the implemented service with real GitHub OAuth, a bounded
   deployment, an installable CLI, and an independent fresh-runner cache restore.
2. Enroll CLI machines with explicit team/project authorization, revocable machine
   identities, bounded heartbeats and usage reports. Show last-seen and stale
   state, then add approved cache-policy management. A CLI configuration or daemon
   process ID is not a cloud machine identity.
3. Provision private cloud runners with job leases, cancellation, limits,
   credentials, cleanup, and usage accounting. Public Builds retain their distinct
   controlled-publication contract; they are not the private runner scheduler.

The existing cache data path and artifact identity remain reusable across these
slices. The hosted dashboard consumes authorized project projections and checks
browser sessions server-side without exposing cache credentials.

## Development

The Go binary embeds the dashboard. Edit `dashboard/app.ts` and the HTML/CSS under
`internal/dashboard/assets`, then run `pnpm build`. The generated `app.js` is
tracked and CI checks bundle drift. `pnpm typecheck` checks the browser code;
`go test ./internal/dashboard ./internal/cli -run Dashboard` checks the connection
and browser-serving contract.

Run the [browser QA harness](../qa/dashboard/README.md) with `pnpm test:dashboard`
and a built `LAYERCACHE_BIN`. CI installs the pinned Playwright Chromium and
executes the same fixtures and assertions. See the [QA results](../qa/dashboard/RESULTS.md)
for independent review findings, fixes, and verification limits.
