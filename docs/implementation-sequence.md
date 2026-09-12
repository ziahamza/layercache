# Release implementation sequence

This records the work authorized on 2026-09-12 after the release audit. The installed CLI, authenticated HTTP interfaces, real client tools, and cache persistence APIs are the acceptance boundaries defined by the original specification.

| Work | State | Completion evidence |
| --- | --- | --- |
| Reject wrong-project cache authority | Implemented and tested | Installed CLI tests for unrelated repositories, changed origins, opaque Team IDs, and matching clones |
| Commit and publish the complete implementation | Local implementation complete; publication needs repository destination | Reviewed local commit followed by GitHub native CI |
| Enforce workflow, shell, and generated-file checks | Implemented and tested locally | `qa/lint.sh`, pinned CI tools, action typecheck/tests and reproducible bundles |
| Native Linux amd64/arm64 and macOS arm64 qualification | Pending hosted execution | Native jobs must execute; cross-builds alone do not qualify |
| Linux arm64 Public Build assets | Implemented; native execution needs hardware | Shared architecture-aware recipes, pinned ARM64 inputs, ELF checks, native KVM qualification command; x64 execution passed |
| Immutable prerelease | Pending native gates | Immutable GitHub release, verified checksums and provenance |
| Installer, runner setup, server deployment | Implemented and tested locally | Verified-release installer, installed runner action, Compose Turbo/Actions roundtrip and restart; actual release install and production deployment remain external |
| Team quota and pin administration | Implemented and tested | Administrator-only HTTP/CLI, transactional audit and shrink, pin conflicts/isolation, persistent quota across replicas/restarts |
| Public trust-root distribution and rotation | Rotation implemented; default anchor needs service identity | Signed endpoint-bound rotation with replay/expiry protection; no invented production trust key |
| Calibrated performance gates | Local and Team implemented and tested; Public fixture remains | 44 cold/warm pairs across minimum/current Turbo, exact outputs and source assertions; checked-in sample evidence |
| Impact-aware retention | Opt-in implementation tested | LRU remains default; same-budget persistence/replay cases, pins, aliases, unknown timing, verified reuse; real workload validation and admission comparison remain |
| Module simplification | Bounded cleanup complete | Shared architecture recipes and retention selector; separate Turbo, Local CI, and BuildKit lifecycle implementations without changing their interfaces |

The current host provides native Linux amd64 and KVM. Native arm64 execution and macOS execution need suitable remote machines. Repository owner and visibility are being requested separately; local implementation can continue while that choice is pending.

## Remaining order

1. Choose the GitHub repository owner and visibility, publish the reviewed commit, and run the checked-in native CI matrix. Configure protected main and a trusted native Linux ARM64 KVM runner, then qualify the exact candidate commit.
2. Complete Public Cache performance qualification through trusted Public Builds. The Local/Team fixture deliberately rejects a Public source rather than faking a signed publication.
3. Enable GitHub release immutability and publish a prerelease only after its native, persistence, protocol, and performance gates pass. Exercise the installer against those real release assets.
4. Provision the Team endpoint, TLS, PostgreSQL/S3, OCI registry, backups, membership policy, and Public service identity. Distribute the real public trust key separately from cache responses.
5. Migrate one trusted main-branch workflow per project. Confirm a cold result and a fresh-checkout hit before removing each job's Vercel Turbo settings. Existing project workflows have not been changed by this implementation.
6. Collect representative workload histories and compare LRU with impact at equal quotas. Add admission-value comparison and complete restore-cost attribution, then profile large-cache selection before considering an impact default.

Apple signing/notarization remains an optional distribution step. Windows, Xcode, npm mirrors, and new cache adapters remain outside this release sequence.
