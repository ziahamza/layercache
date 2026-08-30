# Extending `actions/cache` across Layer Cache

Research current to 2026-08-29. Sources are GitHub documentation and pinned first-party source from `actions/cache`, `actions/toolkit`, and the official Actions runner. Local CI claims are pinned to its own source at `a8db594`.

## Decision

Layer Cache can preserve the familiar `path`, `key`, `restore-keys`, and `cache-hit` behavior, but stock `actions/cache` cannot be pointed at Layer Cache from a normal GitHub-hosted or GitHub-managed self-hosted workflow. The stock action has no endpoint or credential input, and the official runner injects GitHub's runtime cache URLs and token after it processes action environment values. [Stock action inputs](https://github.com/actions/cache/blob/3edfce9056124e459a23f683a21433670d47daca/action.yml#L4-L36), [runner environment injection](https://github.com/actions/runner/blob/fb64b9b20d56951bf30c5b7333a128bc25c2d923/src/Runner.Worker/Handlers/NodeScriptActionHandler.cs#L43-L83)

Use two integration paths:

1. Local CI should route unchanged `actions/cache` steps to a Layer Cache v1-compatible endpoint. This needs one small Local CI configuration seam because Local CI currently hardcodes its own endpoint.
2. GitHub-hosted and GitHub-managed self-hosted jobs should replace `uses: actions/cache@...` with a Layer Cache action that retains the stock inputs and outputs but authenticates to Layer Cache explicitly.

Local Cache and Team Cache can sit behind that compatibility layer. General Public Cache restoration cannot. Stock `actions/cache` downloads and extracts an unsigned archive without checking provenance, so it cannot establish that a globally shared result came from a Layer Cache Public Build. Public reuse needs a Layer Cache-controlled, verified download path, or a deliberately narrow server-side eligibility rule where client-side provenance is not required. GitHub explicitly warns that cache contents are not signed or verified and may lead to code execution after restore. [GitHub cache security guidance](https://docs.github.com/en/actions/reference/workflows-and-actions/dependency-caching#best-practices-for-using-caches-securely)

## Supported stock behavior to retain

### Identity and matching

GitHub identifies a cache by key, cache version, repository, and Git ref scope. A run first searches the current ref, then the default branch. Within each ref it tries the exact primary key, primary-key prefix matches, then ordered `restore-keys`; the newest partial match wins. Pull-request caches use the merge ref and are readable only by reruns of that pull request. Sibling branches and unrelated tags do not share caches. [Matching order](https://docs.github.com/en/actions/reference/workflows-and-actions/dependency-caching#cache-key-matching), [ref restrictions](https://docs.github.com/en/actions/reference/workflows-and-actions/dependency-caching#restrictions-for-accessing-a-cache)

The toolkit limits a key to 512 characters, rejects commas, and accepts at most ten lookup keys including restore keys. [Toolkit validation](https://github.com/actions/toolkit/blob/193fa46c20fde8b0ed54194bc08b841c78c0776d/packages/cache/src/cache.ts#L96-L115), [lookup-key limit](https://github.com/actions/toolkit/blob/193fa46c20fde8b0ed54194bc08b841c78c0776d/packages/cache/src/cache.ts#L338-L350)

Layer Cache should preserve this behavior inside a project and ref scope. The project identity and permitted ref scopes must come from an authenticated Layer Cache token, not from an untrusted key prefix. The Team Cache can share entries among authorized project members while keeping GitHub's default-branch fallback rule.

### Restore and save lifecycle

The combined action restores in its main phase and saves in a post-step only after a successful job. The separate restore and save actions expose the same phases explicitly. An exact primary-key hit suppresses the save. `cache-hit` is `true` for an exact primary-key match, `false` for a restore-key or other partial match, and empty on a miss. [Action lifecycle](https://github.com/actions/cache/blob/3edfce9056124e459a23f683a21433670d47daca/action.yml#L37-L44), [restore result](https://github.com/actions/cache/blob/3edfce9056124e459a23f683a21433670d47daca/src/restoreImpl.ts#L45-L86), [save decision](https://github.com/actions/cache/blob/3edfce9056124e459a23f683a21433670d47daca/src/saveImpl.ts#L35-L74)

Cache saves are effectively first-writer-wins for one identity. The v2 client reserves an entry, uploads it, and finalizes it; a competing reservation or finalization is treated as a benign failure rather than an overwrite. [v2 save flow](https://github.com/actions/toolkit/blob/193fa46c20fde8b0ed54194bc08b841c78c0776d/packages/cache/src/cache.ts#L688-L756)

### Version and platform behavior

The cache version is a SHA-256 hash of the configured paths, compression method, a version salt, and `windows-only` when applicable. It does not generally include operating system or CPU architecture. [Version calculation](https://github.com/actions/toolkit/blob/193fa46c20fde8b0ed54194bc08b841c78c0776d/packages/cache/src/internal/cacheUtils.ts#L136-L159)

Inference: macOS and Linux can produce the same cache version when they use the same paths and compression method. Keys therefore need an explicit Layer Cache compatibility namespace containing operating system, architecture, and any workload-specific ABI inputs. This must happen without changing the user-visible matched key, otherwise stock `cache-hit` behavior changes.

## Storage capacity

GitHub's hosted cache has a 10 GB default total per repository. As of this research, a repository owner can configure up to 10,000 GB, subject to organization limits, billing, and budgets. GitHub evicts least-recently-accessed entries when the configured total is exceeded; the default inactivity retention is seven days. [Repository cache settings](https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/enabling-features-for-your-repository/managing-github-actions-settings-for-a-repository#configuring-cache-settings-for-your-repository), [eviction and rate limits](https://docs.github.com/en/actions/reference/workflows-and-actions/dependency-caching#usage-limits-and-eviction-policy)

That larger GitHub quota remains repository-local storage on GitHub. It does not add Local Cache, Team Cache, cross-repository Public Cache, or a Layer Cache lookup path.

Layer Cache can provide a substantially larger aggregate capacity because the protocol does not set a total backend size. Layer Cache owns admission, quota, retention, eviction, and physical blob deduplication. One constraint remains in the current v1 client: outside GitHub Enterprise Server mode it refuses an individual archive larger than 10 GiB before upload. The v2 client path does not contain that check. [v1 archive-size check](https://github.com/actions/toolkit/blob/193fa46c20fde8b0ed54194bc08b841c78c0776d/packages/cache/src/cache.ts#L539-L575), [v2 save path](https://github.com/actions/toolkit/blob/193fa46c20fde8b0ed54194bc08b841c78c0776d/packages/cache/src/cache.ts#L630-L697)

Implications:

- A v1 compatibility endpoint can hold far more than 10 GB in total as many entries.
- Supporting a single archive over 10 GiB through the near-stock v1 action requires either the toolkit's GHES path or a small client change. Layer Cache should not pretend this limit is gone.
- Docker BuildKit cache data should use the BuildKit adapter, not be packed into one enormous `actions/cache` archive.

## Protocol constraints

### v1 compatibility protocol

The current toolkit retains the v1 client and forces it for GitHub Enterprise Server. It reads `ACTIONS_CACHE_URL`, authenticates with `ACTIONS_RUNTIME_TOKEN`, and uses this flow:

1. `GET /_apis/artifactcache/cache?keys=...&version=...` for lookup.
2. `POST /_apis/artifactcache/caches` with key, version, and archive size to reserve an entry.
3. Parallel `PATCH /_apis/artifactcache/caches/{id}` requests with `Content-Range` to upload bytes.
4. `POST /_apis/artifactcache/caches/{id}` with final size to commit.
5. HTTP download from the returned `archiveLocation`.

[v1 selection and URL](https://github.com/actions/toolkit/blob/193fa46c20fde8b0ed54194bc08b841c78c0776d/packages/cache/src/internal/config.ts#L14-L59), [v1 lookup](https://github.com/actions/toolkit/blob/193fa46c20fde8b0ed54194bc08b841c78c0776d/packages/cache/src/internal/cacheHttpClient.ts#L41-L125), [v1 reserve, upload, and commit](https://github.com/actions/toolkit/blob/193fa46c20fde8b0ed54194bc08b841c78c0776d/packages/cache/src/internal/cacheHttpClient.ts#L184-L377)

This is the right compatibility target for the first Local CI integration. It is small, streams through Layer Cache, and lets the local daemon measure lookup and transfer behavior. GitHub calls it legacy, so Layer Cache should treat it as an adapter with contract tests, not as its internal storage API. [GitHub cache service migration notice](https://github.com/actions/cache/blob/3edfce9056124e459a23f683a21433670d47daca/README.md#L16-L39)

### v2 results protocol

The v2 client reads `ACTIONS_RESULTS_URL` when `ACTIONS_CACHE_SERVICE_V2` is set. It calls three JSON/Twirp methods with the runtime bearer token: `CreateCacheEntry`, `FinalizeCacheEntryUpload`, and `GetCacheEntryDownloadURL`. [v2 client](https://github.com/actions/toolkit/blob/193fa46c20fde8b0ed54194bc08b841c78c0776d/packages/cache/src/internal/shared/cacheTwirpClient.ts#L28-L79), [generated service contract](https://github.com/actions/toolkit/blob/193fa46c20fde8b0ed54194bc08b841c78c0776d/packages/cache/src/generated/results/api/v1/cache.twirp-client.ts#L38-L101)

The control service returns signed upload and download URLs. Upload then uses the Azure Block Blob SDK directly, including block uploads and commit behavior. [v2 request and response fields](https://github.com/actions/toolkit/blob/193fa46c20fde8b0ed54194bc08b841c78c0776d/packages/cache/src/generated/results/api/v1/cache.ts#L140-L218), [Azure upload](https://github.com/actions/toolkit/blob/193fa46c20fde8b0ed54194bc08b841c78c0776d/packages/cache/src/internal/uploadUtils.ts#L122-L175)

Inference: the published client makes v2 technically reproducible, but GitHub does not expose it as a documented third-party cache-server contract. A compatible server also needs Azure-compatible signed uploads or an Azure Blob backend. This adds no MVP value over v1 for Local CI, so v2 should be research-followed and deferred until a real GitHub runner integration requires it.

## Integration by execution environment

### GitHub-hosted runners

Stock `actions/cache` has no backend URL or token input. The official runner sets `ACTIONS_RUNTIME_URL`, `ACTIONS_RUNTIME_TOKEN`, `ACTIONS_CACHE_URL`, `ACTIONS_RESULTS_URL`, and the v2 feature flag for Node actions. This occurs after it copies the action environment, so a workflow-level `env` override is not a supported escape hatch. [Stock inputs](https://github.com/actions/cache/blob/3edfce9056124e459a23f683a21433670d47daca/action.yml#L4-L36), [runner injection order](https://github.com/actions/runner/blob/fb64b9b20d56951bf30c5b7333a128bc25c2d923/src/Runner.Worker/Handlers/NodeScriptActionHandler.cs#L43-L83)

Supported stock choice: keep using GitHub's backend and optionally buy a larger GitHub repository quota.

Layer Cache choice: change the workflow to a Layer Cache action with explicit Layer Cache authentication. The action can retain the stock key, path, restore-key, lookup-only, and hit-output semantics.

### GitHub-managed self-hosted runners

A self-hosted runner registered to GitHub executes the same official runner handler and receives GitHub's service endpoint and token in its job message. Self-hosting the machine does not redirect stock `actions/cache` to a nearby server.

Use the same Layer Cache action as on GitHub-hosted runners. On a persistent self-hosted machine it should speak to the local Layer Cache daemon first; the daemon can serve Local Cache and fetch misses from Team Cache or eligible Public Cache.

### Local CI

Local CI already points both `ACTIONS_CACHE_URL` and `ACTIONS_RESULTS_URL` at its local DTU and injects a mock token. It implements the v1 lookup, reserve, chunk upload, commit, and download routes. [Local CI environment](https://github.com/redwoodjs/local-ci/blob/a8db59493b883d642c07c89be537c7572d3ff965/packages/cli/src/docker/container-config.ts#L82-L120), [Local CI v1 routes](https://github.com/redwoodjs/local-ci/blob/a8db59493b883d642c07c89be537c7572d3ff965/crates/local-ci-runtime/src/dtu/cache.rs#L3-L35)

Its current cache is not a Team Cache security model. Local CI documents that it scopes by key only and does not implement GitHub's ref isolation. Its source also compares lookup keys exactly, so it does not reproduce GitHub's prefix matching for restore keys. [Local CI compatibility note](https://github.com/redwoodjs/local-ci/blob/a8db59493b883d642c07c89be537c7572d3ff965/packages/cli/compatibility.md#github-api-features-dtu-mock), [lookup implementation](https://github.com/redwoodjs/local-ci/blob/a8db59493b883d642c07c89be537c7572d3ff965/crates/local-ci-runtime/src/dtu/cache.rs#L38-L100)

The smallest integration is an upstreamable Local CI option such as `LOCAL_CI_ACTIONS_CACHE_URL`. When set, Local CI should inject the Layer Cache daemon URL and a short-lived scoped token instead of its DTU cache URL. Existing workflow YAML stays unchanged. Layer Cache then implements correct project, ref, exact-key, and restore-prefix semantics.

## Workflow-change and client options

### No workflow change

This is viable only when Layer Cache controls the runner's orchestration environment, initially Local CI. It is not viable on GitHub-hosted or GitHub-managed self-hosted jobs.

### Near-stock Layer Cache action

Recommended for GitHub jobs. Publish `layercache/cache` with the stock action's inputs and outputs. It can reuse the public `@actions/cache` library for local and Team Cache archive creation, extraction, versioning, and hit behavior while supplying Layer Cache endpoint and credentials inside its own process. The library exports `restoreCache` and `saveCache` and selects its service from environment variables. [Public cache library entry points](https://github.com/actions/toolkit/blob/193fa46c20fde8b0ed54194bc08b841c78c0776d/packages/cache/src/cache.ts#L138-L190), [service configuration](https://github.com/actions/toolkit/blob/193fa46c20fde8b0ed54194bc08b841c78c0776d/packages/cache/src/internal/config.ts#L42-L59)

This requires replacing one `uses:` reference. It does not require a separate workflow design.

### Fork of `actions/cache`

A fork could add endpoint, credential, compatibility-namespace, telemetry, and provenance inputs while retaining the full upstream action layout. It would also inherit the compiled JavaScript bundles, Node runtime migrations, archive edge cases, and protocol churn. There is no need to take that maintenance burden for Local Cache and Team Cache when a thin Layer Cache action can call the toolkit library.

Forking becomes justified only if Layer Cache needs to change archive verification or support individual v1 entries over 10 GiB while retaining exact action behavior.

### Stock action plus environment overrides

Do not promise this. It depends on internal runner variables, fails on GitHub-hosted runners because the runner overwrites them, and v2 sends uploads directly to an Azure-compatible signed URL.

## Hiding the cache hierarchy behind one endpoint

The stock client talks to one logical backend. That is enough for Local Cache and Team Cache because the Layer Cache daemon or service can implement the lookup order internally:

1. Local Cache exact and restore-prefix lookup.
2. Team Cache lookup within the authenticated project and permitted ref scopes.
3. Eligible Public Cache lookup only after Layer Cache proves that the request identity matches trusted Public Build metadata.
4. Miss, followed by a write to Local Cache and optionally Team Cache according to policy.

Physical blobs can be content-addressed and deduplicated even though logical entries retain stock key, version, project, ref, and compatibility metadata. A Team Cache blob must not become public merely because its bytes match a later candidate. Public Build metadata is the authority for public eligibility.

## Public Cache safety boundary

GitHub's security model assumes repository and ref isolation. It warns that anyone able to open a pull request may read base-branch caches, that cache entries must not contain secrets, and that restored cache files can execute later. Low-trust triggers receive read-only access to default-branch caches. [Cache security model](https://docs.github.com/en/actions/reference/workflows-and-actions/dependency-caching#cache-access-for-low-trust-workflow-triggers), [secure-use requirements](https://docs.github.com/en/actions/reference/workflows-and-actions/dependency-caching#best-practices-for-using-caches-securely)

The v1 response contains an archive location and matched key. The v2 response contains a signed download URL and matched key. Neither response carries an artifact digest, builder identity, source revision, toolchain identity, platform identity, or signed provenance statement that the stock action verifies before extraction. [v1 cache response use](https://github.com/actions/toolkit/blob/193fa46c20fde8b0ed54194bc08b841c78c0776d/packages/cache/src/internal/cacheHttpClient.ts#L77-L125), [v2 download response](https://github.com/actions/toolkit/blob/193fa46c20fde8b0ed54194bc08b841c78c0776d/packages/cache/src/generated/results/api/v1/cache.ts#L480-L539)

Hard boundary: stock-compatible lookup is not enough for general global or cross-project cache reuse. User-chosen keys are not a trust identity, and TLS or a signed object URL authenticates transport, not how the artifact was built.

For the first release:

- Public Cache remains read-only to clients.
- A Public Build must establish repository, source revision, complete input fingerprint, toolchain, platform, outputs, blob digest, and builder provenance.
- The Layer Cache action or CLI verifies the signed manifest and blob digest before extraction.
- Unchanged stock `actions/cache` steps use Local Cache and Team Cache only. If the server returns any Public Cache data through that path, restrict it to an explicit allowlist whose safety does not depend on client-side provenance verification.

## Resulting implementation contract

Build the GitHub Actions adapter around these boundaries:

- A v1-compatible HTTP adapter for unchanged Local CI workflows.
- A thin near-stock Layer Cache action for GitHub-hosted and self-hosted workflows.
- One internal Layer Cache lookup API that preserves key, version, project, ref, and compatibility scope while searching Local Cache and Team Cache.
- A separate verified Public Cache restore operation, even if it shares physical blobs and user-facing key conventions with the compatibility adapter.
- Backend-owned storage limits and eviction, with explicit reporting of the current v1 10 GiB per-entry client constraint.
- Contract tests copied from observed protocol behavior, not Local CI implementation code.

This preserves the useful `actions/cache` ergonomics, allows much larger Layer Cache storage, and does not smuggle an unsigned global artifact across the public trust boundary.
