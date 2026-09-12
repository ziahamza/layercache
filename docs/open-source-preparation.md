# Open-source preparation

The repository is still private. This preparation does not change visibility,
choose a license, publish a CLI release, or enable Public Builds.

## Direct action imports

Consumers use `ziahamza/layercache/action/turbo@main`. There is no actions mirror,
consumer authentication script, or consumer dependency to maintain. The action
and its tests are TypeScript, executed by Node 24 with separate typechecking.
The action imports shared helpers from `action/setup`, so keep both directories
in this repository.

`main` is mutable. Every change to it can affect consumer CI on its next run,
including reruns of older commits. Keep action tests and typechecking green.
Use an immutable commit to roll back a consumer. Add versioned action releases
later without changing the Team Cache protocol.

Private action sharing currently supports consumers under the same GitHub owner.
GitEnv belongs to another owner and must not merge its direct-import change until
this repository is public and its organization permits the action. A skipped
action step is not a reliable way to avoid private-action download restrictions.

## Before publication

- Choose and add an open-source license with the correct copyright holder.
- Approve the public visibility change explicitly. Publishing exposes Git history
  as well as current source. This pass did not inspect secrets or audit history;
  it does not establish that publication is safe.
- Keep runtime configuration, credentials, cache data, and machine-specific
  deployment material outside the source distribution. Ignore rules reduce
  accidental additions but do not remove already tracked files or history.
- Review the outstanding deployment changes separately. They are not included
  merely because the public-action documentation is ready.
- Configure a private vulnerability-reporting channel before accepting reports.
- After publication, run GitEnv CI with the direct action and verify remote hits
  in a fresh job before merging the migration.

Open-source code does not make Team Cache entries public. Project authorization
and scoped GitHub OIDC credentials still apply. Vercel artifacts are not imported
automatically. Public Builds and Docker registry retention remain separate work.
