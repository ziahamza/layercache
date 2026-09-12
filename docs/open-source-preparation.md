# Open-source preparation

The owner approved publication on 2026-09-12. The repository is public under
the MIT license. No CLI release or Public Builds deployment was part of this
visibility change.

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

Consumers can now import the action across GitHub owners, subject to their
organization's Actions policy. The earlier private-sharing restriction no longer
requires an actions-only mirror.

## Before publication

- MIT license and explicit public visibility approval are complete. Publication
  exposes Git history as well as current source. This pass did not inspect
  secrets or audit history; it is not a security-audit claim.
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
