# Layer Cache working release

Status: ready-for-agent
Labels: ready-for-agent

## Problem Statement

Engineers increasingly run several coding agents at once. Those agents create Git worktrees, independent clones, containers, and disposable VMs. Each new Workspace often downloads the same dependencies and repeats the same builds, tests, Docker stages, and local CI jobs. Model throughput is no longer the only limit. Repeated local compute becomes the bottleneck.

Existing caches do not solve this as one system. Some live inside one checkout. Some belong to one tool or builder. Hosted CI caches have vendor-specific capacity, identity, and retention rules. A cache created on one machine often cannot help another engineer, and a cache created by an open-source build cannot safely become public if arbitrary clients may publish to it.

The engineer also lacks a defensible answer to the basic ROI question. A high hit rate can still save almost no time, and a remote restore can cost more than recomputation. Layer Cache must preserve build correctness, report where reuse came from, and distinguish measured facts from estimated savings.

## Solution

Layer Cache provides a Local Cache that outlives individual Workspaces, a private Team Cache shared by authorized engineers and CI, and a globally readable Public Cache populated only by Layer Cache-controlled Public Builds. Cache lookup is automatic where the native tool permits it. Public Builds remain explicit in the first release, so a cache miss never silently submits code to cloud compute.

The first release supports Turborepo, Docker BuildKit, and GitHub Actions through protocol-specific adapters. It preserves each tool's native identity and restore behavior instead of inventing one universal cache key. The adapters share storage of immutable bytes, authorization policy, provenance, retention, and telemetry where those concepts genuinely match.

The local runtime lives outside each Workspace. A new worktree or clone on the same host can reuse prior results. A disposable VM may connect to the host runtime with an explicit route and short-lived credential, or fall back to Team Cache and eligible Public Cache results. A remote hit warms Local Cache.

Local CI reuses existing `actions/cache` declarations through a Transparent Cache endpoint. GitHub-hosted and GitHub-managed self-hosted workflows replace only the action reference with the Layer Cache action, preserving familiar cache inputs and outputs. Public Cache data is restored only after a Layer Cache-controlled client verifies its provenance and byte digest.

The CLI reports cache hit rate, bytes, cache source, overhead, and signed net estimated build time saved. It reports negative savings when caching added time. It never relabels estimated wall time as measured CPU savings.

The supported client platforms are Linux x64, Linux arm64, and macOS arm64. Public Builds run on Linux x64 and Linux arm64. Native macOS Turborepo and GitHub Actions artifacts use Local Cache and Team Cache only. Docker artifacts follow the requested target platform, which lets a macOS host reuse a Public Cache result for a compatible Linux target.

## User Stories

1. As an engineer, I want to install Layer Cache with one CLI flow on Linux x64, Linux arm64, or macOS arm64, so that I do not need to assemble several cache systems.
2. As an engineer, I want onboarding to detect Turborepo, Docker BuildKit, and GitHub Actions usage, so that it suggests only relevant configuration.
3. As an engineer, I want to preview configuration changes before Layer Cache applies them, so that I understand what it will change.
4. As an engineer, I want to choose a Local Cache size limit during setup, so that Layer Cache cannot consume unbounded disk space.
5. As an engineer, I want setup to disclose which artifacts and telemetry may leave my machine, so that connecting Team Cache is an informed choice.
6. As an engineer, I want to use Local Cache without an account or network connection, so that local acceleration works independently.
7. As an engineer, I want to connect a project to Team Cache once, so that later Workspaces inherit the same shared-cache configuration.
8. As an engineer, I want setup to be safe to rerun, so that repair does not create duplicate processes or conflicting settings.
9. As an engineer, I want to see whether Layer Cache is running, which integrations are active, and how much space it uses, so that I can verify setup.
10. As an engineer, I want to bypass Layer Cache for one command, project, or integration, so that I can diagnose suspected cache problems.
11. As an engineer, I want to uninstall Layer Cache and restore configuration it owns, so that trying it does not permanently alter my environment.
12. As an engineer, I want to choose whether uninstall preserves or deletes Local Cache, so that removal does not surprise me with data loss.
13. As an engineer, I want a new Git worktree to reuse earlier Local Build results, so that each coding agent does not repeat the same work.
14. As an engineer, I want an independent clone on the same host to reuse Local Cache, so that reuse does not depend on Git worktree linkage.
15. As an engineer, I want a disposable VM to reach the host Local Cache when I explicitly expose it, so that isolation does not force a cold build.
16. As an engineer, I want a replacement VM to fall back to Team Cache and Public Cache when its former host is unavailable, so that cache lifetime exceeds Workspace lifetime.
17. As an engineer, I want reusable identity to ignore absolute checkout paths, so that equivalent work matches across worktrees, clones, and VMs.
18. As an engineer, I want deleting a Workspace to leave reusable artifacts intact, so that cleanup does not erase prior build value.
19. As an engineer, I want concurrent Workspaces to share results safely, so that several agents cannot corrupt cache data.
20. As an engineer, I want a remote hit to warm Local Cache, so that later runs on the host do not need the network.
21. As an engineer, I want Team Cache checked before Public Cache, so that my team's authorized result wins when both contain a compatible entry.
22. As an engineer, I want to see whether a result came from Local Cache, Team Cache, or Public Cache, so that I can explain a fast build.
23. As an engineer, I want to force recomputation, so that I can validate correctness or refresh an obsolete result.
24. As an engineer, I want ineligible work to run normally, so that Layer Cache remains an optimization rather than a new build system.
25. As an engineer, I want Local Cache to work offline, so that a network outage does not block a Local Build.
26. As an engineer, I want remote lookup or publication failure to leave a successful Local Build successful, so that cache downtime does not become build downtime.
27. As an engineer, I want Layer Cache to report negative savings when restoration was slower, so that regressions are visible.
28. As a team member, I want engineers and CI to share Team Cache results, so that each machine does not repeat completed work.
29. As a team administrator, I want separate read, publish, and administration permissions, so that consumers cannot replace cache entries.
30. As a team administrator, I want CI credentials scoped to one team and project, so that a leaked token cannot cross tenant or project lines.
31. As a team administrator, I want to rotate or revoke credentials without clearing valid artifacts, so that incident response preserves useful data.
32. As a team administrator, I want unauthorized lookup responses to hide whether another team's key exists, so that metadata cannot be enumerated.
33. As a team administrator, I want an audit trail for authentication, publication, deletion, revocation, and permission changes, so that misuse can be investigated.
34. As a team administrator, I want to see Team Cache usage and quota, so that capacity problems appear before publication fails.
35. As an engineer, I want a full Team Cache to preserve reads and my successful build while rejecting new writes, so that quota exhaustion is not an outage.
36. As an engineer, I want Local Cache eviction to follow a documented policy, so that disk pressure is predictable.
37. As an engineer, I want incomplete uploads excluded from lookup, so that a crash cannot expose truncated bytes as a hit.
38. As an engineer, I want concurrent publication of one identity to resolve deterministically, so that races never mix artifact contents.
39. As a Turborepo user, I want normal `turbo` commands and declared outputs to keep working, so that Layer Cache does not replace Turborepo.
40. As a Turborepo user, I want Turborepo's native Workspace cache to remain active, so that its fastest hit path is preserved.
41. As a Turborepo user, I want the host runtime to add a cache shared across independent clones, so that an empty project-local cache can still hit locally.
42. As a Turborepo user, I want a remote miss to check Team Cache before compatible Public Cache, so that private results take precedence.
43. As a Turborepo user, I want Local Builds to publish only to Local Cache and authorized Team Cache, so that my client never writes Public Cache.
44. As a Turborepo user, I want Linux x64, Linux arm64, and macOS arm64 artifacts isolated, so that native outputs never cross incompatible hosts.
45. As a Turborepo user, I want runtime and toolchain facts included in matching, so that equal Turbo hashes do not merge incompatible native results.
46. As a Turborepo user, I want diagnostics when undeclared task inputs make reuse unsafe, so that I understand what Layer Cache cannot repair.
47. As a Turborepo user, I want conflicting trusted Public Builds for one identity to cause a miss, so that nondeterminism is visible.
48. As a Turborepo user, I want producer duration and current restore overhead included in ROI, so that reported savings use available evidence.
49. As a Docker BuildKit user, I want to keep using Buildx and native cache formats, so that Layer Cache does not create a competing Docker cache protocol.
50. As a Docker BuildKit user, I want a named builder's Local Cache to persist outside my Workspace, so that new worktrees reuse Docker layers.
51. As a Docker BuildKit user, I want Team Cache to reuse intermediate stages, so that multi-stage builds benefit across machines.
52. As a Docker BuildKit user, I want Public Cache imports pinned to an approved manifest digest, so that a mutable tag cannot change restored data.
53. As a Docker BuildKit user, I want compatibility based on target platform, so that macOS and Linux hosts may share the same compatible Linux result.
54. As a Docker BuildKit user, I want separate references for each target platform, so that one platform cannot overwrite another platform's cache.
55. As a Docker BuildKit user, I want to request a Public Build on managed Linux compute, so that a large build can leave my machine.
56. As a Docker BuildKit user, I want Layer Cache to push a Public Build result by default, so that a large image is not transferred back unless I request it.
57. As a Docker BuildKit user, I want Team Cache publication to create an immutable build reference before moving a branch reference, so that concurrent builds remain complete.
58. As a Docker BuildKit user, I want a registry outage to fall back to a Local Build, so that Layer Cache remains optional.
59. As a Docker BuildKit user, I want Docker cache data kept out of giant GitHub Actions archives, so that it retains native graph semantics.
60. As a Local CI user, I want existing `actions/cache` declarations to run unchanged, so that local workflow caching is a Transparent Cache.
61. As a Local CI user, I want `path`, `key`, ordered `restore-keys`, cache version, and `cache-hit` behavior preserved, so that local workflow logic matches GitHub.
62. As a Local CI user, I want exact matches, prefix matches, default-branch fallback, and pull-request restrictions preserved, so that local execution does not broaden access.
63. As a Local CI user, I want an exact primary-key hit to suppress a duplicate save, so that stock lifecycle behavior remains intact.
64. As a Local CI user, I want new worktrees and disposable VMs to reuse Local Cache and Team Cache through an injected endpoint, so that local workflow jobs stop rebuilding dependencies.
65. As a GitHub-hosted Actions user, I want to replace only the cache action reference while retaining existing inputs and outputs, so that Team Cache does not require a workflow redesign.
66. As a self-hosted Actions user, I want the Layer Cache action to try Local Cache before Team Cache, so that a persistent runner avoids transfers.
67. As a CI maintainer, I want Team Cache capacity independent of GitHub's repository cache allowance, so that active entries are not evicted by the hosted limit.
68. As a CI maintainer, I want per-entry limits reported separately from aggregate capacity, so that a larger Team Cache is not described as unlimited.
69. As a Local CI user, I want the trusted local runtime to verify Public Cache provenance and bytes before serving them to stock `actions/cache`, so that transparency does not bypass verification.
70. As a GitHub Actions user, I want the Layer Cache action to verify Public Cache provenance and bytes before extraction, so that unsigned public data cannot execute.
71. As a GitHub Actions user, I want a direct stock client connected to a remote compatibility endpoint limited to Team Cache, so that Public Cache requires a Layer Cache-controlled verifier.
72. As a GitHub Actions user, I want Public Cache matching bound to attested source, inputs, cache version, paths, and platform, so that an arbitrary familiar key cannot select public data.
73. As a GitHub Actions user, I want warnings against caching credentials, so that larger shared storage does not encourage unsafe archives.
74. As an engineer, I want to request a Public Build for an allowlisted public GitHub repository at an immutable commit, so that Layer Cache can publish under its own identity.
75. As an engineer, I want to name the integration, target, platform, and supported inputs in a Public Build request, so that its identity is complete.
76. As an engineer, I want an existing compatible Public Cache result to satisfy my request immediately, so that Layer Cache does not rebuild known work.
77. As an engineer, I want to see queued, running, successful, failed, and cancelled Public Build states, so that offloaded work is observable.
78. As an engineer, I want to cancel a queued or running Public Build when safe, so that obsolete work stops consuming capacity.
79. As an engineer, I want sanitized Public Build logs, so that I can diagnose failure without exposing infrastructure credentials.
80. As an engineer, I want Public Builds to run without user secrets, privileged containers, or a host Docker socket, so that public code receives no private authority.
81. As a security reviewer, I want mutually untrusted Public Builds isolated, so that one repository cannot inspect another build.
82. As a security reviewer, I want Public Build network, time, CPU, memory, and storage limits, so that public code cannot use Layer Cache as an unrestricted relay or denial of service.
83. As an engineer, I want only the Public Build publisher identity to create Public Cache metadata, so that clients cannot promote Team Cache bytes.
84. As an engineer, I want public metadata to bind source, recipe, inputs, platform, builder, output digest, cache digest, and duration, so that verification covers origin.
85. As an engineer, I want failed or cancelled Public Builds to publish nothing, so that partial output never becomes globally reusable.
86. As a Layer Cache administrator, I want to revoke a Public Cache publication, so that future online lookups miss even if physical bytes remain.
87. As an engineer, I want conflicting Public Builds for one identity marked ambiguous, so that neither result is served silently.
88. As an engineer, I want `net estimated build time saved` as the headline metric, so that the product reports the outcome I care about.
89. As an engineer, I want hit rate separate from time saved, so that many trivial hits do not look valuable.
90. As an engineer, I want hits and bytes split by Local Cache, Team Cache, and Public Cache when the adapter can prove the source, so that attribution remains honest.
91. As an engineer, I want lookup, transfer, verification, and restore overhead deducted from savings, so that network cost is included.
92. As an engineer, I want task savings and build wall-time savings distinguished, so that parallel tasks are not added as if they ran serially.
93. As an engineer, I want CPU time shown only when measured, so that estimated wall time is not relabeled as CPU-hours.
94. As an engineer, I want backtesting to replay historical runs in timestamp order, so that a later artifact cannot satisfy an earlier build.
95. As an engineer, I want backtesting coverage reported, so that missing fingerprints and timings appear as unknown rather than misses or zero savings.
96. As an engineer, I want sampled cold builds to require explicit consent, so that measurement does not unexpectedly spend compute.
97. As an engineer, I want per-run and historical reports with confidence, so that I can inspect one build and judge value over time.
98. As an engineer, I want corrupted or incompatible artifacts treated as misses, so that correctness wins over hit rate.
99. As an engineer, I want restored outputs to match uncached outputs, so that a hit never changes the build result.
100. As an engineer, I want diagnostics for connectivity, credentials, disk pressure, configuration, misses, and degraded operation, so that failures are actionable without exposing another tenant's metadata.

## Implementation Decisions

### Product and platform contract

- The working release includes Turborepo, Docker BuildKit, GitHub Actions, Local Cache, Team Cache, Public Cache, and explicit Public Builds. None of these may remain a non-working placeholder at release.
- Supported engineer machines are Linux x64, Linux arm64, and macOS arm64. The release pipeline produces signed binaries and runs the same acceptance suite on native machines for all three targets.
- Public Builds run on Linux x64 and Linux arm64. Native macOS Turbo and Actions workloads receive Local Cache and Team Cache reuse but cannot consume Linux Public Cache artifacts. Docker on macOS may consume Public Cache entries when its requested target platform matches.
- A cache miss runs the original Local Build. It does not automatically request a Public Build.
- Cache errors fail open by default. A diagnostic strict mode may turn cache errors into command failures, but normal use treats caching as an optimization.
- The product does not promise that every build is faster. It promises correct reuse, visible cache behavior, and honest ROI for cache-eligible work.

### Repository and runtime stack

- Use one monorepo. Go is the primary implementation language for the CLI, per-user local runtime, protocol gateways, cloud control plane, telemetry, and Public Build coordinator. Go fits the required static binaries, concurrent streaming, cross-compilation, and the Docker and OCI ecosystem.
- Use TypeScript only for the near-stock GitHub Action. Bundle the action into one checked-in JavaScript distribution as GitHub Actions requires.
- Ship one `layercache` binary. The same binary runs CLI commands, the per-user background process, integration helpers, maintenance, and local protocol endpoints.
- Use SQLite in WAL mode for local metadata. Write local opaque bytes to a filesystem content-addressed layout outside every Workspace.
- Use PostgreSQL for cloud logical records, authorization state, upload sessions, Public Build state, publication records, audit events, and report metadata.
- Use S3-compatible object storage for Team Cache and Public Cache opaque bytes. Use an OCI Distribution-compatible registry for BuildKit rather than implementing a registry in the first release.
- Start with one modular cloud process for authentication, Turbo and Actions gateways, metadata, reporting, and Public Build requests. Public Build workers are a separate deployable because they run in a different trust domain. Do not split other modules into separately deployed processes until load requires it.
- Expose HTTP and JSON at owned network seams. Keep internal module interfaces in process. Add a network port only where production and tests have real adapters.
- Emit OpenTelemetry-compatible traces and metrics, but persist a product-owned event record so ROI does not depend on one telemetry vendor.
- Do not create a general adapter SDK in this release. The first three adapters will establish which concepts truly deserve an extension interface.

### Modules and interfaces

- The Workspace Runtime module owns installation state, background-process lifecycle, configuration, platform credential storage, project discovery, short-lived Workspace credentials, integration setup, local quota, repair, and uninstall. Its interface is configure, start, stop, inspect, repair, and uninstall.
- The Cache Coordinator module owns ordered cache resolution for opaque-archive adapters, integrity checks, Local Cache warming, timeout behavior, remote circuit breaking, and durable Team Cache upload jobs. Its interface accepts a read or write intent and returns a final cache outcome. It does not understand protocol-specific key matching.
- Each Protocol Index module owns the identity and matching rules of one protocol. Turbo uses exact task hashes. GitHub Actions uses repository and ref scope, cache version, exact keys, prefix matches, restore keys, and newest-match rules. BuildKit keeps OCI reference, manifest, and graph semantics in the registry. These indexes never flatten into one universal key table.
- The Artifact module owns atomic stream ingestion, SHA-256 calculation, verified streaming reads, immutable bytes, staging cleanup, reference counts, and garbage collection. Its interface commits a stream, opens verified bytes by artifact reference, and releases a logical reference. Filesystem and S3 adapters make this a real seam.
- The Access Policy module owns engineer, CI, Public Build, team, and project principals. Its interface issues scoped capabilities and authorizes one operation without revealing whether an unauthorized key exists.
- The Public Trust module owns signed publication manifests, byte verification, ambiguity, signed offline validity, online revocation, and quarantine. Its interface publishes an attested artifact, resolves a verified publication, and revokes it.
- The Measurements module owns event correlation, causal backtesting, confidence, and ROI calculations. Its interface records an observation and returns a run or period report.
- The Public Build module owns admission, idempotency, queueing, leases, cancellation, sanitized logs, worker dispatch, and trusted publication. Its user-facing interface requests, inspects, and cancels a build. Its worker interface accepts a fully resolved build request and returns immutable output descriptors to a trusted collector.
- The highest product seam is the installed system. A caller runs a real build tool inside a Workspace and observes command exit status, restored output, native hit signals, Layer Cache status, and the machine-readable run report. Tests use this seam wherever practical.

### Core records

- An artifact record contains a SHA-256 digest, byte size, media type, creation time, last access, lifecycle state, and storage location. Staged bytes do not become readable until an atomic commit.
- A logical cache entry contains its integration, cache scope, project identity, compatibility identity, protocol-native identity, artifact or OCI references, producer metadata, creation time, last access, and retention state.
- A Public Cache publication contains the complete public identity, source repository, immutable commit, recipe digest, toolchain and builder identity, target platform, output identities, artifact or OCI digests, sizes, producer duration, signed provenance, expiry, ambiguity state, and revocation state.
- An upload job contains the destination scope, project, logical identity, expected digest and size, retry state, and final outcome. Upload jobs are idempotent.
- A measurement event contains a run and Workspace identifier, integration, hashed logical identity, final hit or miss outcome, source when knowable, bytes, lookup and transfer timing, verification and restore timing, execution timing when observed, producer duration when available, degraded state, and estimator confidence.
- Raw artifact contents, raw cache paths, environment values, secrets, and unhashed user keys do not enter telemetry.

### Local Cache and Workspace lifecycle

- The per-user runtime owns a host-wide Local Cache outside all repositories. It listens on a protected local socket by default.
- A disposable VM may reach the host runtime only through an explicit route and a short-lived project-scoped token. The runtime never binds an unauthenticated endpoint to the LAN.
- Interactive setup asks for a Local Cache budget and suggests 20 GiB. Non-interactive setup defaults to 20 GiB unless the engineer supplies a value. The runtime preserves at least 5 GiB or 5 percent of the volume, whichever is larger, before admitting new bytes.
- The Local Cache evicts complete, unpinned, least-recently-used entries first. Active reads, uploads, and artifacts still referenced by live logical entries cannot be removed.
- Startup removes abandoned staging files only after checking that no live upload owns them. A crash cannot expose partial bytes.
- Tool-native cache remains first. Turborepo checks its Workspace cache before its configured Layer Cache endpoint. BuildKit checks its named builder's internal cache before external imports.
- For Turbo and Actions, the host runtime provides another Local Cache shared across worktrees and independent clones. A verified Team Cache or Public Cache hit warms this host cache before returning or while streaming safely.
- BuildKit Local Cache remains native builder state. Layer Cache configures native garbage collection to fit the remaining local budget and does not delete BuildKit internals itself.
- Removing a Workspace never removes Local Cache. Uninstall restores configuration owned by Layer Cache and asks whether cached bytes should also be deleted.

### Lookup and write policy

- Turbo exact matches resolve in this order: Turbo's Workspace cache, host Local Cache, Team Cache, compatible Public Cache.
- GitHub Actions preserves GitHub's match quality across sources. Current-ref exact matches outrank partial matches, followed by ordered restore keys and default-branch equivalents. When two candidates have equal match quality and ref scope, Local Cache wins over Team Cache and Team Cache wins over Public Cache. A Local partial match must not hide a Team exact match.
- BuildKit receives its persistent local builder plus bounded Team Cache registry imports and digest-pinned Public Cache imports. BuildKit owns graph matching. Layer Cache does not claim exact source attribution for a cached vertex when several importers were available.
- Remote hits warm Local Cache after complete digest and provenance verification. A Public Cache hit never creates Team Cache metadata.
- Local Builds commit to Local Cache first. If Team publication is allowed, the local runtime durably records an upload job and retries it in the background. Hosted CI without a local runtime publishes directly to Team Cache.
- Client writes stop at Local Cache and Team Cache. Only a Public Build publisher may create Public Cache metadata.
- Physical byte equality does not change visibility. A Team Cache artifact becomes public only when an independent Public Build creates a valid publication record.
- Connect and metadata lookup use a short deadline, defaulting to two seconds per remote source. Active transfers use a progress-based idle deadline, defaulting to thirty seconds. These values are configurable.
- Remote timeout, quota rejection, failed upload, unavailable registry, or corrupted bytes does not fail an otherwise successful build. The report records degraded behavior. Strict diagnostic mode may fail instead.
- The first release does not race remote restoration against recomputation. It records slow and negative results so later policy can use evidence.
- Negative build, test, or command results are not cached.

### Identity and compatibility

- There is no universal cache key. Every lookup includes integration, project identity, compatibility identity, and the protocol-native identity.
- A connected project uses a Team-issued immutable project ID. Before connection, the runtime derives a normalized Git provider, owner, and repository identity. A repository without a usable remote receives a local UUID and remains Local Cache-only until explicitly linked.
- Absolute checkout paths, worktree paths, VM identifiers, and ephemeral container identifiers never define reusable identity.
- Forks do not share Team Cache by default. Public Cache reuse requires an exact trusted public identity, not repository-name similarity.
- Turbo identity includes project, Turbo hash, and host compatibility. Host compatibility includes operating system, CPU architecture, Layer Cache schema version, Linux libc where relevant, and declared output-affecting runtime or toolchain versions.
- Layer Cache cannot repair an incomplete `turbo.json` input declaration. Diagnostics explain this when a project enables cross-Workspace reuse.
- GitHub Actions identity includes project, repository, Git ref scope, user key, cache version, and hidden job compatibility. The hidden compatibility namespace does not change the user-visible matched key or `cache-hit` output.
- Public GitHub Actions matching is exact. Public Cache does not apply broad cross-project prefix or restore-key matching.
- BuildKit compatibility uses target operating system, architecture, and variant. Builder-platform facts join identity only when the Dockerfile consumes them.
- Dirty Local Builds may publish to authorized Team Cache when the native protocol permits it. They never publish to Public Cache.
- If two trusted Public Builds produce different digests for the same public identity, the Public Trust module marks it ambiguous and returns a miss until resolved.

### Team Cache authorization

- Team Cache defines reader, writer, and administrator capabilities. Writers also read. Administrators manage projects, membership, credentials, quota, pins, and audit history.
- Engineer login uses GitHub device authorization for the first release. The local runtime keeps refresh material in the operating-system credential manager and uses short-lived project capabilities for cache traffic.
- GitHub Actions exchanges GitHub OIDC identity for a short-lived project capability when configured. A project-scoped secret is the documented fallback.
- Local CI receives a short-lived Workspace token from the host runtime. A VM route receives a token limited to one project, compatibility identity, and expiry.
- Authorization comes from the token and server-side membership. Turbo selectors, Actions keys, OCI repository names, and user-provided query values never grant access.
- Unauthorized lookup returns the same externally visible result as an allowed miss. The audit log records the denied attempt without disclosing it to the caller.
- Token rotation or revocation changes authority without deleting artifacts.

### Turborepo adapter

- Point an unmodified Turbo client at the local runtime's v8 Remote Cache gateway. Preserve Turbo's own Workspace cache.
- Implement status, exact HEAD and GET, atomic PUT, and event ingestion required by the v8 contract. Persist Turbo bodies as opaque bytes and preserve duration, signature tag, source SHA, dirty hash, and CI metadata.
- Treat Turbo team or slug query values as selectors checked against the authenticated project. They never grant access.
- The local runtime resolves its host Local Cache, then the cloud gateway resolves Team Cache before compatible Public Cache.
- A Turbo PUT commits host Local Cache and may queue Team Cache publication. It can never select Public Cache.
- Preserve native HMAC behavior for Team-only mode. The unified Public Cache path relies on Layer Cache provenance verification because a shared Turbo HMAC cannot prove a public publisher.
- Use final Turbo Run Summary outcomes and producer durations for hit rate and ROI. Do not calculate task hit rate from raw probe events.

### Docker BuildKit adapter

- Compose BuildKit's native interfaces. Do not create a new Docker cache format.
- Configure one persistent named `docker-container` builder per engineer. Its internal state is Local Cache and survives Workspace deletion.
- Use authenticated OCI registry imports and `mode=max` exports for Team Cache. Export each build to an immutable reference, then serialize promotion of a mutable branch reference.
- Use separate references for each target platform. Never let multi-platform writers replace one shared reference.
- Import Public Cache only by a manifest digest approved by the Public Trust module. The client never trusts a mutable public tag.
- Use cache export as best effort so registry failure does not fail the image build.
- Public Builds use an isolated BuildKit trust domain inside the worker. The Public Build module never gives an engineer credentials to a shared general-purpose remote BuildKit daemon.
- Push Public Build output by default. Loading a result back to the engineer's Docker image system is an explicit option because it may erase the offload benefit.
- Report observed BuildKit cached-vertex rate, build duration, requested cache scopes, and remote bytes. Mark causal cache source as unattributed when multiple importers were available.

### GitHub Actions adapter

- Implement the GitHub Actions v1 cache protocol in the local runtime for Transparent Cache use under Local CI.
- Add one configurable Local CI cache endpoint seam. When enabled, Local CI injects the local runtime URL and short-lived token. Existing `actions/cache` workflow declarations remain unchanged.
- Integrate through a small upstreamable Local CI configuration change. Do not copy, bundle, or commercially reuse Local CI code without a separate license that permits it.
- Preserve repository and ref scope, current and default branch lookup, pull-request merge-ref isolation, exact and prefix matching, ordered restore keys, newest partial match, save after successful job, exact-hit save suppression, and `cache-hit` values.
- Publish a TypeScript Layer Cache action for GitHub-hosted and GitHub-managed self-hosted runners. It keeps the stock action's path, key, restore-key, lookup, save, and output behavior while using Layer Cache authentication and verification.
- A direct stock client connected only to a remote compatibility endpoint may use Team Cache but not general Public Cache.
- For Transparent Cache under Local CI, the trusted local runtime may use Public Cache only after it downloads the full archive, verifies signed provenance and byte digest, commits verified bytes locally, and returns a localhost download URL to stock `actions/cache`.
- The Layer Cache action verifies Public Cache provenance and bytes before extraction on GitHub runners.
- Public Actions matching requires an exact attested repository, immutable source and recipe, path set, cache version, job compatibility, and artifact digest. A user key alone is never a public identity.
- The v1 compatibility path may hold more than 10 GiB in aggregate but retains the current client limit on one archive. Report that limit clearly. BuildKit data uses the BuildKit adapter.
- GitHub Actions v2 cache-server compatibility is deferred. The release supports v1 under Local CI and the Layer Cache action on GitHub runners.

### Public Cache trust and revocation

- Public Cache is globally readable only through a Layer Cache-controlled verification path. It accepts no client writes.
- Public Build provenance uses a DSSE envelope containing an in-toto Statement. It binds the repository, immutable commit, recipe digest, toolchain, builder image, target platform, output identity, artifact or OCI digest, size, producer duration, and Public Build identity.
- Clients ship with a pinned Layer Cache trust root. They verify the signed statement, expected public identity, signed validity, and complete byte digest before restore or import.
- Online lookup checks revocation. Offline reuse of a previously verified public artifact is allowed only while its signed verification lease remains valid. The default verification lease is twenty-four hours. After expiry, offline lookup returns a miss.
- Revocation stops future online resolution immediately. Layer Cache cannot recall bytes already restored to a disconnected machine.
- A failed, cancelled, ambiguous, expired, revoked, or incompatible publication returns a miss. It is never served to improve hit rate.
- Public metadata and Team metadata remain separate. Backing storage may deduplicate bytes only after trusted public metadata exists, and neither quota accounting nor response behavior may reveal cross-scope byte existence.

### Team and Public retention

- Team Cache uses quota-driven least-recently-used retention with optional administrator pins. Reads continue when full; new writes fail with a visible warning and do not fail the build.
- Public Cache publications expire after ninety days without access unless the allowlist policy pins them. Revocation tombstones remain after artifact expiry so a revoked identity cannot reappear from stale metadata.
- Unreferenced blobs are deleted after a grace period. Active streams, upload jobs, and live logical references hold leases that prevent deletion.
- Staged multipart uploads expire automatically. Incomplete uploads never appear in lookup or usage totals as committed artifacts.
- Deduplication occurs within one Local Cache, within one Team Cache, and within Public Cache. Cross-team physical deduplication is outside the first release.
- Actions and Turbo use atomic first-writer-wins publication for one immutable identity. Identical retries are idempotent. Conflicting later bytes are recorded and rejected.

### Public Builds

- Accept only authenticated requests for allowlisted public GitHub repositories, immutable commits reachable from an approved branch or release tag, Layer Cache-maintained recipe digests, named targets, and supported Linux platforms.
- The supported first-release recipes are a named Turbo task, a named Docker target, and an allowlisted GitHub workflow job whose Layer Cache action declares public-eligible paths.
- Deduplicate requests by the complete Public Build identity. Return an existing valid Public Cache publication instead of queueing duplicate work.
- Expose queued, running, successful, failed, and cancelled states. Cancellation is best effort once running. After an accepted cancellation, no output may publish even if worker completion races it.
- Run each request in a fresh microVM-equivalent sandbox with an immutable base image, ephemeral disk, no inbound network, restricted dependency egress, no user secrets, no privileged mode, no host Docker socket, and strict CPU, memory, time, process, and storage limits.
- Do not place publication credentials in the sandbox. A trusted collector outside it receives declared outputs, verifies completion, hashes bytes, creates provenance, and publishes only after success.
- Do not share one BuildKit trust domain among mutually untrusted Public Builds.
- Sanitize logs before storing or returning them. Failed and cancelled builds publish no cache metadata.
- The production sandbox provider sits behind the Public Build worker interface. A local fake supports tests, but container-only isolation does not satisfy the production release gate.

### Measurements and ROI

- Record one final outcome per cache-eligible task, step, archive, or BuildKit vertex group. Do not treat every internal lookup probe as a separate hit or miss.
- Report cache hit rate separately from time saved. Report source as Local Cache, Team Cache, Public Cache, or unattributed when the adapter cannot prove it.
- The headline is signed `net estimated build time saved`. It includes miss, lookup, download, verification, restore, and upload overhead. Negative values remain negative.
- Report gross avoided task time separately. Do not sum parallel task durations and call the result build wall time.
- Turbo uses producer duration and the Run Summary task graph. GitHub Actions uses recorded producer and restore duration. BuildKit uses observed vertex hit rate, historical baselines, and sampled empty builders.
- Show CPU time only when the process or runner measured it. Never convert estimated wall time into CPU-hours or money.
- Backtesting replays timestamped historical runs in order. A produced artifact may satisfy only later compatible work. The simulation applies the configured retention and quota policy and reports what fraction of work had usable fingerprints and timing.
- Missing historical identity or timing is unknown. It is not a miss and does not contribute zero savings.
- Sampled forced cold builds require explicit consent and run outside active work. They calibrate estimates and output correctness.
- Provide human-readable per-run and historical reports plus a stable machine-readable JSON report. The JSON report includes final outcome, source, bytes, timing segments, degraded state, estimator method, and confidence.

### CLI behavior

- The CLI provides setup, login, project connection, integration configuration, status, diagnostics, run reports, historical statistics, garbage collection, bypass, repair, Public Build request and status, disable, and uninstall flows.
- Setup detects tools, previews changes, records ownership of each change, and rolls back on interruption. It does not overwrite later user edits blindly.
- Integration setup configures Turbo to use the local v8 endpoint, creates and selects the named BuildKit builder, configures registry credentials and refs, and enables the Local CI endpoint seam.
- Status reports process health, enabled integrations, project and team identity, Local Cache usage and limit, pending uploads, remote reachability, credential expiry, recent degraded behavior, and Public Build state.
- Diagnostics explain misses without disclosing another tenant's key. They distinguish absent, incompatible, unauthorized, expired, revoked, corrupt, ambiguous, and unavailable results where disclosure is safe.
- Bypass may disable all cache access or one integration for one command. Forced recomputation does not delete existing entries unless the engineer requests invalidation.

### Implementation waves

1. Establish the monorepo, domain records, installed-system scenario harness, Go binary packaging, local runtime, filesystem artifact module, SQLite, JSON reports, and Linux x64 CI.
2. Ship the Turbo Local Cache and Team Cache vertical path with cloud metadata, object storage, authorization, upload queue, failure handling, and ROI.
3. In parallel, add the GitHub Actions v1 Transparent Cache path and TypeScript action, the BuildKit named-builder and Team registry path, Linux arm64, and macOS arm64.
4. Add Public Trust, signed publications, manually seeded Public Cache reads, corruption and poisoning tests, ambiguity, expiry, and revocation.
5. Add explicit Public Build request, sandboxed Linux x64 and arm64 workers, trusted collection, and publication for Turbo, Docker, and eligible Actions artifacts.
6. Complete quotas, garbage collection, installers, repair, backtesting, concurrency hardening, outage behavior, performance calibration, and the full native platform acceptance matrix.

## Testing Decisions

### Testing seam

- Test the installed product through the same seam an engineer uses. Install the released CLI and local runtime, configure production-shaped Team Cache and Public Cache dependencies, invoke the real upstream client in a disposable Workspace, then inspect only command results, restored outputs, native client hit signals, documented status, and the JSON run report.
- Use one scenario harness with drivers for real `turbo`, `docker buildx`, Local CI with stock `actions/cache`, the Layer Cache action on an official GitHub runner, and Public Build requests. The driver changes, but the product seam stays the same.
- Do not assert SQLite rows, filesystem layout, Postgres tables, queue internals, goroutine structure, private functions, or internal module calls.
- Narrower tests are justified for protocol byte compatibility, cryptographic verification, deterministic ROI calculations, and faults that cannot be injected safely through the installed seam.
- The repository has no existing test prior art. Use the Turbo v8 contract and client, GitHub Actions cache lifecycle, OCI Distribution behavior, Buildx history and solve output, and the documented Layer Cache interface as prior art.

### Correctness oracles

- Turborepo tests compare the declared output tree, file modes, symlink targets, and logs against a cold build on the same platform. The final Run Summary and Layer Cache report provide hit evidence.
- BuildKit tests run the produced image and compare deterministic image configuration and filesystem digests. Buildx history or the raw solve stream provides cached-vertex evidence.
- GitHub Actions tests compare restored path contents and workflow outputs. They assert exact, partial, and miss `cache-hit` behavior plus the Layer Cache report.
- Public Build tests compare output digests to the same accepted recipe run cold in a controlled environment and verify the signed publication before reuse.
- A restored result that differs from the accepted cold output is always a release-blocking failure, regardless of reported cache hits.

### Platform and Workspace matrix

- Run native acceptance jobs on Linux x64, Linux arm64, and macOS arm64. Emulation does not prove host compatibility.
- For every adapter, test a linked worktree, an independent clone at a different absolute path, a disposable VM with an authenticated host route, and a replacement VM with Team Cache but no host route.
- Turbo Public Cache hits require a Public Build with the same Linux architecture. macOS Turbo Public lookup must miss safely.
- BuildKit tests both `linux/amd64` and `linux/arm64`, including reuse of one target between macOS and Linux hosts.
- GitHub Actions identity follows the job operating system and architecture, not the host that happens to run Local CI.
- Pin a minimum supported client and a current client in release tests for each protocol. Protocol versions, not undocumented internal layouts, define compatibility.

### Required scenarios

- Cold miss: empty every cache, run the workload once, verify canonical output, record a final miss, and publish only to allowed Local Cache and Team Cache.
- Local hit: rerun from each Workspace form, verify no externally visible execution counter increments, and attribute the hit to Local Cache.
- Team hit: clear Local Cache on a second machine, restore from Team Cache, disconnect the network, and prove the warmed Local Cache serves the next run.
- Public hit: empty Local Cache and Team Cache, publish only through Public Build, verify provenance and digest before mutation, restore canonical output, and warm Local Cache.
- Precedence: preload competing Local Cache, Team Cache, and Public Cache candidates and prove the documented match-quality and source rules. For BuildKit, assert candidate configuration and final correctness rather than unsupported causal source claims.
- Compatibility miss: reuse one user-visible key across incompatible operating systems, architectures, runtimes, refs, repositories, and target platforms. Every incompatible lookup must miss.
- Failure fallback: inject DNS failure, connection refusal, metadata timeout, stalled transfer, interrupted download, registry outage, upload rejection, disk full, and corrupt bytes. A correct Local Build still completes in default mode, with visible degraded status and no partial restore.
- Poisoning: attempt cross-tenant reads and writes, selector spoofing, client Public Cache writes, wrong signatures and digests, wrong source or recipe, wrong platform, mutable-tag substitution, unsafe archive paths, and conflicting public outputs. Reject before Workspace mutation and return a safe miss or Local Build.
- Concurrency: run at least thirty-two readers and writers for one identity. Readers may see one complete committed value or a miss, never partial bytes. Identical writes are idempotent. Conflicting immutable writes cannot replace the winner. BuildKit uses unique refs and serialized promotion.
- Garbage collection: run quota eviction during uploads and downloads. Active data survives, the documented victim becomes a miss, shared bytes remain while referenced, and reported usage stays within the documented tolerance.
- Revocation: warm a Public Cache artifact locally, revoke it, and prove online lookup stops immediately. Prove offline use only before signed lease expiry and a miss after expiry.
- Public Build admission: accept an allowlisted immutable request and reject mutable source, an unallowlisted repository, unsupported platform, secrets, privilege, host socket, and excessive resources before execution.
- Public Build lifecycle: verify idempotent duplicate requests, state transitions, safe cancellation, sanitized logs, no publication after failure or accepted cancellation, signed publication after success, and ambiguity after divergent rebuilds.
- Setup and removal: interrupt setup and uninstall at each owned change, then verify rollback, repair, and preservation or deletion of Local Cache according to the engineer's choice.

### Protocol contract coverage

- Turbo covers authenticated status, HEAD, GET, PUT, exact opaque bytes, metadata headers, atomic publication, event acceptance, Team-only HMAC, selectors that cannot grant access, and final Run Summary correlation.
- GitHub Actions covers key validation, cache version, current and default ref order, exact and prefix matches, ordered restore keys, newest partial result, pull-request isolation, save after success, exact-hit save suppression, first-writer-wins, ranged v1 upload and commit, unchanged Local CI YAML, and one-reference replacement on an official GitHub runner.
- BuildKit covers persistent builder state, `mode=max` intermediate reuse, multiple imports, target-platform refs, digest-pinned Public Cache, immutable publication followed by mutable promotion, load and push outcomes, cache import and export failure, and final image behavior even when BuildKit reports cached vertices.

### ROI and performance gates

- Use a deterministic fixture with parallel tasks so a test fails if the implementation adds task durations as though they ran serially.
- Include a historical fixture where an artifact appears only in a later run, so backtesting fails if it leaks future knowledge into earlier runs.
- Assert hit rate from final cache-eligible work, not raw probe counts. Failed, cancelled, duplicate, and ineligible work does not count as saved.
- Assert signed net estimated build time saved, gross avoided task time, overhead segments, bytes, source when knowable, method, confidence, and historical coverage.
- A tiny fixture must be allowed to report negative savings. Missing evidence must report unavailable or low confidence.
- After fill, all eligible Turbo tasks and exact Actions keys in deterministic fixtures must hit. Every cacheable BuildKit vertex should hit except unavoidable source and export work.
- For representative fixtures whose cold compute dominates transfer, each supported cache source must produce positive median net wall-time savings over repeated runs. The median warm run must take no more than half of the median cold run under the controlled benchmark network.
- Correctness, isolation, and provenance gates take precedence over speed and hit-rate targets.

## Out of Scope

- Windows, Intel macOS, native Xcode builds, iOS simulators, and native macOS Public Builds.
- npm registry mirrors, package CDNs, Bazel, Next.js directory snapshots, Vite caches, and adapters beyond Turborepo, Docker BuildKit, and GitHub Actions.
- A universal cache key, universal archive format, or generic arbitrary-command cache.
- Replacing Turborepo, BuildKit, Docker, GitHub Actions, or Local CI as build and execution systems.
- Copying or bundling Local CI as part of Layer Cache without a separate commercial license.
- Transparent redirection of stock `actions/cache` on GitHub-hosted or GitHub-managed self-hosted runners. Those environments require the Layer Cache action.
- A third-party-compatible GitHub Actions v2 cache server in the first release.
- General Public Cache restore directly through an unverified stock remote `actions/cache` client.
- Client publication or promotion into Public Cache.
- Arbitrary unallowlisted repositories, mutable source references, user-provided build recipes, user secrets, privileged builds, inbound networking, host Docker sockets, or a shared multi-tenant BuildKit daemon in Public Builds.
- Automatic Public Build submission on a cache miss.
- Team-owned remote execution. Team Cache shares artifacts; Public Builds are the only managed compute in this release.
- Cross-project Public Cache matching by a user-chosen Actions key alone.
- Unsafe cross-platform reuse. Compatibility must be explicit, and an incompatible result is a miss.
- Cross-team physical blob deduplication in the first release.
- Multi-region active-active control plane, pricing, billing, enterprise SSO, a browser dashboard, and formal uptime guarantees.
- Guaranteed acceleration for misses, tiny tasks, transfer-heavy artifacts, or every build.
- Claims of measured compute or monetary savings when only wall-time estimates exist.

## Further Notes

- This specification consolidates the [Wayfinder map](map.md), the [Turborepo adapter research](research/turborepo-adapter.md), the [BuildKit adapter research](research/buildkit-adapter.md), and the [`actions/cache` extension research](research/actions-cache-extension.md).
- The specification is the implementation authority for the working release. Open Wayfinder tickets remain useful context and may become execution tickets, but they must not reopen a decision that this specification makes unless new evidence invalidates it.
- The external testing seam is intentionally high. A passing internal cache lookup is not enough. A real tool must restore the right output in a new Workspace and report the right source and ROI.
- Public provenance proves origin and inputs. It does not prove that cached executable content is harmless. Verify before restore, bind identity tightly, and preserve isolation.
- Research reflects upstream behavior checked on 2026-08-29. Protocol contract tests protect Layer Cache from future upstream changes.
- The repository contains no implementation or prior tests yet. The first implementation wave creates the product modules and the acceptance harness together.
