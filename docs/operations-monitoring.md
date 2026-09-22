# Monitoring the deployed cache

`deploy/pool/monitor.ts` probes the bounded single-host deployment without
writing cache entries, changing retention, or restarting services. It checks:

- Authenticated status for each configured project, including cloud maintenance.
- Pool size and admission headroom, with warnings within 2 GiB of either the
  pool or host reserve. It reports missing or malformed capacity as unknown.
- Registry collection failures and successful collection older than 12 hours.
  Exit 75 means a busy lease; it does not renew the last successful collection.
- Public HTTPS health (exact HTTP 200, trusted certificate, no redirects),
  with a warning when the certificate expires within 14 days.
- Last-24-hour project reports: eligible work, hits, observed hit rate and net
  estimated task time saved, including timing coverage. Missing observations
  produce null, not zero savings or a claim of CPU savings. Zero eligible work
  produces a null hit rate. This does not yet aggregate native action summaries
  or prove that every local build used LayerCache.

Requests time out after 10 seconds and response bodies are bounded at 64 KiB.
Only named checks, numeric summaries and fixed issue codes enter output. Tokens,
cache keys, raw server responses and URL query strings are never printed.

## Host installation

Use Node 24 or later. Create a root-owned mode-0600 config outside Git:

```json
{
  "endpoint": "http://127.0.0.1:7437",
  "projects": [
    { "name": "example", "configFile": "/etc/layercache/example/config.json" }
  ],
  "publicChecks": [
    { "name": "cache", "url": "https://cache.example.com/healthz" }
  ],
  "maintenanceUnit": "layercache-registry-retention.service"
}
```

Each referenced project config must be private (no group/other permissions) and
contain the existing `localToken` and `projectId`. Its owner defaults to root;
for a config owned by a container service, explicitly add `"configOwnerUID": 65532`
to that project's entry in the root-owned monitor config. Do not change existing
service file ownership just to monitor it. This is an administrator read probe; it
does not obtain GitHub credentials or mint new client capabilities. Only HTTPS
or loopback HTTP is accepted for authenticated requests. Public checks require
HTTPS URLs with no credentials, query, or fragment.

Preview, then explicitly install:

```sh
node deploy/pool/install-monitor.ts /etc/layercache/monitor.json
sudo /absolute/path/to/node deploy/pool/install-monitor.ts /etc/layercache/monitor.json --install
sudo systemctl start layercache-monitor.service
sudo journalctl -u layercache-monitor.service --since today
sudo cat /var/lib/layercache-monitor/status.json
```

The installer copies one root-owned code snapshot to
`/usr/local/lib/layercache-monitor/monitor.ts`, installs only
`layercache-monitor.service` and `.timer`, and checks every five minutes. Repeat
installation after reviewing an update. It does not alter application services.
The snapshot file is replaced atomically and remains bounded; journal retention
is host-managed. The timer continues after failed probes. Stop/disable that timer
to uninstall monitoring without changing cache data or other services.

## Alert delivery and external availability

Host output explicitly reports `unconfigured-local-journal-only`: no outbound
recipient or webhook has been invented. A failed unit is a local signal, not a
delivered page. Configure a separately approved delivery destination before
claiming on-call alerting. A host-local monitor cannot detect its own host outage.

For independent public checks, set this repository Actions variable:

```text
LAYERCACHE_PUBLIC_HEALTH_URL=https://cache.example.com/healthz
```

The opt-in `Deployed service health` workflow runs every 15 minutes and on manual
dispatch. It has no deployment secrets and fails on unreachable/invalid TLS,
non-200 health, or impending certificate expiry. Leave the variable empty to
disable it in forks or undeployed installations. GitHub Actions failure
notifications depend on the account's notification settings; they are not a
guaranteed paging service, and scheduled jobs may be delayed. Enable and test
those notifications deliberately. This external probe checks public ingress,
not authenticated cache writes or internal maintenance.

The retained Cloudflare tunnel is a separate rollback decision. Do not retire
it based only on health probes; first validate real authenticated app workflows,
callbacks, and streaming and agree that the rollback window has ended.
