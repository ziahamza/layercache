# Native cache rollout, 2026-09-12

Follow-up: the engineer approved a 10 GiB explicit reserve. After deploying that
change, [Parle run 34717114242](https://github.com/ziahamza/parle-extension/actions/runs/34717114242)
restored 2,263,669 bytes from Team Cache in 2,198 ms on Linux and skipped the
Safari macOS + iOS job. This resolves the capacity-blocked warm-cache acceptance
described below. Whole-host capacity enforcement remains separate unfinished work;
see [aggregate cache budget](aggregate-cache-budget.md).

## Implemented

The TypeScript native client serves the Expo provider, local CLI, and
`ziahamza/layercache/action/native@main`. Local archives live outside worktrees,
with a default 5 GiB LRU budget. Team Cache uses existing project-authorized
Turbo transport. Fingerprints and explicit toolchain identities bound reuse;
downloads are digest-verified before extraction or a reported hit.

No runner was provisioned. Projects retain their existing execution providers.
Cross-project binary sharing is deliberately isolated by project identity;
sharing dependencies does not imply sharing whole application binaries.

## Project state

- Parle: PR 45 merged as `bea75ea`. Linux checks for the complete audited
  Safari artifact before scheduling its existing GitHub-hosted Mac job.
  A miss or degraded lookup runs the real build. The first main run published
  2,263,669 bytes. Both cold and repeat workflows passed, but the repeat **did
  not skip macOS**. See the capacity blocker below.
- Booker: `936b642` is on main. Expo local Debug development clients use the
  provider; existing Release CI is unchanged. Workspace typecheck, mobile tests,
  lint, formatting, web export, and main CI passed. The existing native workflow
  was still running when this note was written.
- Phone app: local commit `c26a13c`, no Git remote configured. Provider uses
  `local/phone-app` unless explicitly configured otherwise. Workspace checks,
  web export, and real Expo SDK fingerprint resolution passed. No new native
  binary was compiled, installed, or launched for this rollout.

## Evidence

- [LayerCache hosted CI at 731d7fc](https://github.com/ziahamza/layercache/actions/runs/34716300165):
  all jobs passed, including native cache on Ubuntu/macOS 26, Go on Linux x64,
  Linux arm64 and macOS arm64, bundles, and calibrated cache performance.
- [Parle main cold run and rerun](https://github.com/ziahamza/parle-extension/actions/runs/34715858366)
  and [repeat dispatch](https://github.com/ziahamza/parle-extension/actions/runs/34716057533):
  workflows passed, but live warm reuse remains unqualified.
- [Booker main CI](https://github.com/ziahamza-org/booker/actions/runs/34715632699): passed.
- [Booker existing native workflow](https://github.com/ziahamza-org/booker/actions/runs/34715632673):
  separate from the new local provider; inspect its final status before claiming native acceptance.
- Local native suite: 13/13 with installed Go server, including independent
  producer/consumer Local Caches and wrong-toolchain miss. Normal `pnpm check`
  skips that installed-server test unless its binary is supplied.
- Full Go suite, focused race suites, vet, and Darwin test cross-compilation
  passed. A symlink-parent regression reproduced and fixed macOS configuration
  ownership failures without weakening installation identity checks.

## Capacity blocker and next acceptance

Parle's cloud object was read and verified directly through the cloud client:
2,263,669 bytes intact. HTTP restores failed during local verification staging.
The host had about 51–53 GiB available versus the configured 60 GiB minimum.
Do not lower this safety reserve to make a benchmark pass.

Filtered Docker build-cache pruning reclaimed only about 33 KiB. Although Docker
reported substantial reclaimable cache, the attempted age/ID-limited prunes did
not release meaningful capacity. Containers, volumes, and project data were not
deleted. Further capacity work is required before repeating hosted warm QA.

A regression test now forces the reserve failure and expects HTTP 507 with a
fixed staging-capacity diagnostic, rather than an unexplained HTTP 500. The
cloud artifact remains intact. This server change must be deployed before the
production endpoint reports the improved diagnostic.

Next: restore headroom above 60 GiB, rerun Parle and assert a verified Team Cache
hit plus a skipped Mac job, then qualify an actual Booker Debug simulator build
across CI and a fresh local worktree. Give phone-app a GitHub project identity
before enabling its Team Cache/CI integration.

This is not a complete EAS replacement. Release repacking, OTA hosting, shared
compiler caches, and cross-machine build deduplication remain future work.
Fingerprint-only Expo reuse is restricted to development builds; Release builds
must not reuse stale embedded JavaScript.
