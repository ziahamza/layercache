# Self-serve cloud

The hosted service lets a GitHub user create a team, invite teammates, add repository projects, inspect cache storage and activity, and connect an existing CLI. Projects reuse the existing Team Cache protocols and isolate artifacts with distinct project identities and signing keys.

## Engineer workflow

1. Open the service origin and continue with GitHub.
2. Create a team, then add a project using `owner/repository`. You must administer that GitHub repository. This prevents registering another repository to obtain its GitHub Actions identity.
3. Copy the project's CLI command. With an existing GitHub CLI sign-in:

   ```sh
   layercache connect --cloud https://cloud.example.com --project PROJECT_ID --github-cli
   ```

   Omit `--github-cli` to use GitHub's device authorization flow. The service's OAuth app must have device flow enabled. The CLI stores that session through its existing protected credential store.
4. Run the `layercache start --config ...` command printed by `connect`. Use the same config for existing `run`, native integration, and `dashboard` commands. Connection alone does not launch a background daemon or modify repository files.
5. Invite a GitHub username as reader, writer, or administrator. The invitation appears when that user signs in; they accept it explicitly. Invitations expire after seven days. No email is sent.

`connect --list` prints accessible teams/projects. Use `--team` and `--project` IDs or unique names to disambiguate. Each connection gets a fresh Local Cache directory and a configuration under the user's configuration directory, namespaced by cloud and project. Existing files are never overwritten. `--config` chooses a new path; use the existing `login` command to refresh an existing connection.

## Roles and identity

| Role | Restore | Publish | Manage team and projects |
| --- | --- | --- | --- |
| Reader | Yes | No | No |
| Writer | Yes | Yes | No |
| Administrator | Yes | Yes | Yes |

Team roles apply to every project in that team. Membership binds to GitHub's stable numeric user ID, including invitations; renaming an account or recycling its username does not transfer access. Cache requests check current membership as well as the signed token's capabilities. Downgrading a writer stops further uploads with their existing token. Removing a member also invalidates outstanding invitations for that member. The last administrator cannot be removed or demoted.

GitHub Actions OIDC remains scoped to the repository configured for the project. Previously issued short-lived signed artifact download URLs retain their existing expiry; removing a member does not recall bytes already downloaded.

## Operator setup

Run one service instance behind HTTPS. The process holds exclusive project runtime locks; PostgreSQL support does not make this an active/active service. Existing operator-managed cache deployments can continue separately.

Create a GitHub OAuth app with homepage `https://YOUR_HOST`, callback `https://YOUR_HOST/auth/callback`, and device flow enabled. The browser uses authorization code flow with state and S256 PKCE. It requests `read:user repo` to verify administrator permissions on private repositories. GitHub documents these flows in [Authorizing OAuth apps](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps). A GitHub App with narrower installation permissions is future work.

Use a dedicated physically bounded storage filesystem, following the existing [aggregate storage budget](aggregate-cache-budget.md). The mounted pool must contain `.layercache-pool` with `layercache-pool-v1`; its filesystem size cannot exceed `storagePool.maxBytes`. Put the service data directory inside that pool. Do not point a new service at an existing cache's data directory.

Prepare an owner-only template with the CLI:

```sh
umask 077
layercache init --config /etc/layercache-cloud/project-template.json \
  --role team --project cloud-template \
  --data-dir /srv/layercache-cloud/template --max-size 1073741824
openssl rand 32 > /etc/layercache-cloud/session-key
```

The template supplies each project's quota and cache options. Project ID, repository, signing secret, data directory, and membership come from the cloud service. Optional existing `cloudPostgresUrl`/S3 template settings enable the established PostgreSQL/object-storage cache backend. Without those settings cache metadata and artifacts live in the bounded filesystem. Team/session metadata uses SQLite unless `postgresUrlFile` supplies a dedicated PostgreSQL database connection. Keep database and object-storage capacity bounded separately when they are external to this filesystem.

Store the OAuth secret in `/etc/layercache-cloud/github-client-secret` without printing it. Config, template, and secret files must have owner-only permissions. Copy [the example config](../deploy/cloud/config.example.json), replace the hostname/client ID/paths and limits, and run:

```sh
layercache serve-cloud --config /etc/layercache-cloud/config.json
```

The example [systemd unit](../deploy/cloud/layercache-cloud.service) listens on loopback port 7440. Configure the HTTPS proxy to preserve the exact public Host. Host mismatch is rejected; forwarded host headers do not override it. Probe `/healthz`, then verify real GitHub sign-in, team/project creation, a second user's invitation, CLI connection, upload/restore, access removal, and restart persistence before declaring the service live. Do not expose the QA fake GitHub provider.

Back up the metadata database, session encryption key, and cache storage together. The session key encrypts GitHub browser tokens at rest; changing it makes existing encrypted tokens unusable. Browser sessions expire after 12 hours. Keep the key stable across restarts and protect database backups as credentials. Sign out removes the server-side session. OAuth state is single-use and expires after 10 minutes.

The service bounds creation to 128 teams total, five created teams per user, ten projects per team, 128 projects total, and 100 pending invitations per team. Capacity conflicts fail explicitly. This first slice has no billing or automated quota purchase flow.

## Delivery boundaries

Included: browser GitHub login, durable teams/projects, invitations and roles, CLI onboarding, Team Cache storage/activity views, Turbo/native/Actions cache HTTP protocols. Reports show recorded cache activity; they do not fabricate savings or build records.

Not included: remote machine enrollment/control, hosted runners, billing, per-project membership overrides, project/team deletion, or a managed BuildKit registry. The first deployment requires an operator-owned OAuth app, hostname, bounded storage, and optional database/object storage credentials. The existing Phone/Parle services are not migrated by this command.

## Validation

`go test ./internal/portal ./internal/cli ./internal/server` covers durable storage, OAuth/CSRF, repository authorization, two-project byte isolation, role changes and access revocation, restart behavior, and protected CLI configuration. The portal store also supports optional disposable PostgreSQL validation; see the QA instructions. `pnpm test:cloud` exercises the real service and browser against a local fake GitHub provider. Chromium, Firefox and WebKit run the same scenario; WebKit is not a claim of testing native Safari hardware.
