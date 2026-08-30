# Build the first working Layer Cache system

## Destination

Deliver a working Layer Cache release for Linux x64, Linux arm64, and macOS arm64 that accelerates cache-eligible Turborepo, Docker BuildKit, and GitHub Actions workloads through Local Cache, Team Cache, and Public Cache. New worktrees and disposable VMs reuse prior results instead of beginning cold. Public Cache artifacts come only from Linux Public Builds. The release reports defensible cache-hit and net estimated build-time savings, and either extends `actions/cache` safely across these scopes or records the protocol boundary and ships the strongest compatible shared-cache path.

## Notes

- This map explicitly carries execution through working implementation and validation. It is not planning-only.
- Use the `wayfinder`, `grilling`, `domain-modeling`, `research`, `prototype`, `codebase-design`, and `tdd` skills where their ticket type calls for them.
- Prefer broad parallel research and implementation. Do not narrow the destination merely to fit one agent session.
- The first user is an engineer whose AI-agent throughput is constrained by build compute.
- Use the terms in [`CONTEXT.md`](../../CONTEXT.md): Local Cache, Team Cache, Public Cache, Local Build, Public Build, Workspace, and Transparent Cache.
- The customer-facing headline is net estimated build time saved. Report cache hit rate separately.
- Linux x64, Linux arm64, and macOS arm64 clients are required. Public Builds run on Linux for this effort.
- Turbo artifacts are isolated by host compatibility. Docker artifacts are isolated by target platform.
- Workspaces are disposable. Local Cache must live outside a worktree and must either outlive or be reachable from a disposable VM.
- Existing `actions/cache` declarations should run unchanged when Layer Cache controls the local runner endpoint. GitHub-hosted workflows may replace one action reference to reach the same Team Cache and verified Public Cache path.
- Public Cache never accepts client writes. Team Cache artifacts may share physical blobs with Public Cache only after an independent Public Build establishes trusted public metadata.
- The tracker is Local Markdown. Research branches are unavailable until this empty repository has an initial commit, so research assets live under `research/` and are linked from their tickets.

## Decisions so far

- [Determine how far `actions/cache` can extend across Layer Cache](issues/01-extend-actions-cache.md): redirect a v1-compatible endpoint for transparent Local CI and Workspace reuse, use a near-stock Layer Cache action on GitHub runners, and verify provenance before Public Cache restore.
- [Establish the Turborepo adapter contract](issues/02-turborepo-adapter-contract.md): preserve Turbo's Local Cache and implement its v8 remote protocol as a Team-first, trusted-Public fallback with Layer Cache-owned compatibility, authorization, provenance, and ROI handling.
- [Establish the Docker BuildKit adapter contract](issues/03-buildkit-adapter-contract.md): compose a persistent builder, OCI registry caches, digest-pinned public imports, and the mutual-TLS remote driver rather than inventing a Docker cache protocol.

## Not yet specified

- Production implementation tickets and package boundaries will graduate after the adapter research, prototypes, and implementation-wave decision.
- Public Build scheduling, cancellation, queuing, abuse controls, and operator tooling depend on the request contract and isolation choice.
- Installer, upgrade, migration, and release-channel work depends on the local runtime and repository-shape decisions.
- The first user-facing ROI report or dashboard depends on the telemetry contract and prototype evidence.
- A reusable adapter SDK depends on what the first three protocol adapters actually share.

## Out of scope

- Windows, Intel macOS, Xcode builds, and iOS simulators are outside this map.
- npm registry or CDN mirrors, Bazel, Next.js directory snapshots, Vite caches, and other integrations return in later maps.
- Arbitrary unallowlisted public repositories, user secrets, privileged containers, and direct host Docker-socket access are excluded from the first Public Build path.
- Clients cannot upload or promote artifacts into Public Cache.
- Layer Cache does not guarantee that every build becomes faster. Misses and small tasks may cost more than cache lookup and transfer.
