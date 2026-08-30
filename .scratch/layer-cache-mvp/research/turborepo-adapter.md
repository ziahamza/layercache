# Turborepo adapter contract

Research snapshot: 2026-08-29. Source links are pinned to Turborepo commit
[`c7661b8`](https://github.com/vercel/turborepo/tree/c7661b808d6ae15f405136fc4347e983b17cbf23).

## Contract decision

Layer Cache should leave Turborepo's Local Cache in place and implement one
Turborepo v8 Remote Cache gateway. Turborepo checks Local Cache first, falls
back to one HTTP cache, and warms Local Cache after a remote hit. The gateway,
not the Turbo client, must therefore resolve Team Cache before Public Cache.
Successful client uploads publish only to Team Cache. Only the Public Build
identity can publish a Public Cache record.

This preserves stock `turbo` behavior and avoids depending on the local archive
layout. The relevant upstream behavior is in the
[cache multiplexer](https://github.com/vercel/turborepo/blob/c7661b808d6ae15f405136fc4347e983b17cbf23/crates/turborepo-cache/src/multiplexer.rs#L128-L218).

## Exact v8 compatibility

Turborepo documents a self-hostable Remote Cache and states that all current
Turbo versions use the v8 endpoints. The official contract is the
[Remote Cache documentation](https://github.com/vercel/turborepo/blob/c7661b808d6ae15f405136fc4347e983b17cbf23/apps/docs/content/docs/core-concepts/remote-caching.mdx#L191-L216)
and [live OpenAPI specification](https://turborepo.com/api/remote-cache-spec).
The current client confirms the actual `/v8` prefix and request behavior in
[`turborepo-api-client`](https://github.com/vercel/turborepo/blob/c7661b808d6ae15f405136fc4347e983b17cbf23/crates/turborepo-api-client/src/lib.rs#L300-L463).

Layer Cache must implement:

| Request | Required behavior |
| --- | --- |
| `GET /v8/artifacts/status` | Authenticate and return `{"status":"enabled"}` for an allowed principal. |
| `HEAD /v8/artifacts/{hash}` | Return `200` and stored metadata headers, or `404` for a miss. No body. |
| `GET /v8/artifacts/{hash}` | Return the exact uploaded bytes as `application/octet-stream`, plus stored headers, or `404`. |
| `PUT /v8/artifacts/{hash}` | Require an authorized Team writer or internal Public Build publisher. Store the body and metadata atomically. Return `200` or `202`; the OpenAPI response contains a `urls` array. |
| `POST /v8/artifacts/events` | Accept the event array and return `200`. It is optional for cache correctness but required for Layer Cache measurement. |
| `POST /v8/artifacts` | Batch metadata query. The OpenAPI specification marks this optional for basic caching and the current client hot path uses individual requests, so it can follow the first working release. |

All endpoints use `Authorization: Bearer <token>`. The self-hosted token format
is deliberately left to the server. Turbo adds `teamId` or `slug` query
parameters, but Layer Cache must treat them as selectors and verify them against
the authenticated token rather than granting access from the query value. The
current client shows the exact
[Bearer handling and team query parameters](https://github.com/vercel/turborepo/blob/c7661b808d6ae15f405136fc4347e983b17cbf23/crates/turborepo-api-client/src/lib.rs#L321-L339)
and [selector construction](https://github.com/vercel/turborepo/blob/c7661b808d6ae15f405136fc4347e983b17cbf23/crates/turborepo-api-client/src/lib.rs#L955-L970).

`PUT` carries `Content-Length`, `x-artifact-duration`, and optionally
`x-artifact-tag`, `x-artifact-sha`, `x-artifact-dirty-hash`, and
`x-artifact-client-ci`. Layer Cache must return the stored duration, tag, SHA,
and dirty hash on `GET`, and the applicable metadata on `HEAD`. The
[upload implementation](https://github.com/vercel/turborepo/blob/c7661b808d6ae15f405136fc4347e983b17cbf23/crates/turborepo-api-client/src/lib.rs#L367-L438)
is the compatibility authority.

Store the body as opaque bytes. The live OpenAPI prose calls it a gzip tarball,
but the current client creates zstd-compressed tar archives and local entries
are named `.tar.zst`. Parsing or recompressing the body would couple Layer Cache
to this documentation mismatch. See the
[archive writer](https://github.com/vercel/turborepo/blob/c7661b808d6ae15f405136fc4347e983b17cbf23/crates/turborepo-cache/src/cache_archive/create.rs#L118-L157)
and [filesystem cache](https://github.com/vercel/turborepo/blob/c7661b808d6ae15f405136fc4347e983b17cbf23/crates/turborepo-cache/src/fs.rs#L87-L105).

## Native Local Cache behavior to preserve

The default directory is `.turbo/cache`. Current Turbo automatically shares the
main worktree cache with linked Git worktrees unless the repository explicitly
sets `cacheDir`. It restores declared task outputs and always caches logs.
[Official caching behavior](https://github.com/vercel/turborepo/blob/c7661b808d6ae15f405136fc4347e983b17cbf23/apps/docs/content/docs/crafting-your-repository/caching.mdx#L109-L210)
documents these rules.

Layer Cache should not replace or import this directory in the first release.
The CLI may configure Turbo's native `cacheMaxAge` and `cacheMaxSize`, both of
which default to disabled, when the user chooses a storage budget.
[Configuration reference](https://github.com/vercel/turborepo/blob/c7661b808d6ae15f405136fc4347e983b17cbf23/apps/docs/content/docs/reference/configuration.mdx#L156-L225)
has their semantics.

## Metadata and defensible ROI

Turbo passes the successful task's measured wall duration, in milliseconds, to
the cache upload as `x-artifact-duration`. A hit exposes that value as
`timeSaved`. This is direct in
[`save_outputs`](https://github.com/vercel/turborepo/blob/c7661b808d6ae15f405136fc4347e983b17cbf23/crates/turborepo-run-cache/src/lib.rs#L654-L715)
and the [Run Summary cache schema](https://github.com/vercel/turborepo/blob/c7661b808d6ae15f405136fc4347e983b17cbf23/crates/turborepo-run-summary/src/task.rs#L33-L66).
`--summarize` writes per-task timing and hash data to `.turbo/runs`.
[Run Summary documentation](https://github.com/vercel/turborepo/blob/c7661b808d6ae15f405136fc4347e983b17cbf23/apps/docs/content/docs/reference/run.mdx#L650-L674)
describes the file.

The events endpoint sends `sessionId`, `LOCAL|REMOTE`, `HIT|MISS`, `hash`, and
`duration`. See the exact
[event schema](https://github.com/vercel/turborepo/blob/c7661b808d6ae15f405136fc4347e983b17cbf23/crates/turborepo-vercel-api/src/lib.rs#L121-L148).
Do not calculate hit rate from raw event count. A single task can emit a Local
Cache miss and then a Remote Cache hit, so that event stream has one miss and
one hit for a task that ultimately hit cache.

For each run, report:

- **Task hit rate:** cache-hit tasks divided by cache-eligible tasks, using the
  final Run Summary result.
- **Gross avoided task time:** sum of `timeSaved` for hits.
- **Net estimated compute time saved:** for each hit, `timeSaved` minus its
  observed lookup/download/restore duration from task execution timestamps,
  clamped at zero, then summed.
- **Net estimated build wall time saved:** simulate the task graph's critical
  path with hit tasks replaced by their stored durations, then subtract the
  observed run duration. Keep the word "estimated" and calibrate it with
  occasional forced-build measurements. Summing task durations is not wall
  time because parallel tasks overlap.
- **Resolved tier and bytes:** Local, Team, or Public, plus uploaded/downloaded
  bytes. Turbo exposes only `LOCAL` or `REMOTE`; the gateway must record whether
  a remote response came from Team Cache or Public Cache.

`timeSaved` is historical wall time from the producing machine, not a guarantee
of CPU time or the counterfactual duration on the current host. Backtests are
defensible only where historical hashes and timings exist or workloads can be
replayed. Forward measurement starts with the instrumented adapter.

## Platform safety

Turbo does not automatically include OS or CPU architecture in normal task
hashes. Its official solution is to generate a platform/architecture file and
add it to task inputs or global dependencies.
[Handling platforms](https://github.com/vercel/turborepo/blob/c7661b808d6ae15f405136fc4347e983b17cbf23/apps/docs/content/docs/guides/handling-platforms.mdx#L13-L85)
documents this requirement.

Layer Cache therefore needs two guards:

1. Bind every authenticated session to a server-side `compatibility_id` and
   include it in Team and Public lookup namespaces. At minimum distinguish
   Linux x64, Linux arm64, and macOS arm64, with Linux libc and relevant runtime
   or toolchain ABI where outputs can contain native code. A Linux Public Build
   must never satisfy a macOS lookup.
2. Require projects to hash output-affecting runtime and toolchain inputs in
   `turbo.json`. The server receives a finished Turbo hash and cannot repair an
   incomplete task definition after the fact.

Turbo hashes use deterministic serialization followed by xxHash64, not a
cryptographic content digest.
[Hash implementation overview](https://github.com/vercel/turborepo/blob/c7661b808d6ae15f405136fc4347e983b17cbf23/crates/turborepo-hash/src/lib.rs#L1-L9)
For Public Cache, also SHA-256 the opaque artifact. If two trusted Public Builds
produce different artifact digests for the same `(compatibility_id, turbo_hash)`,
mark that public key ambiguous and return a miss rather than choosing either
artifact.

## Authorization, integrity, and Public Cache publication

Native Turbo signing is optional HMAC-SHA256. It covers length-framed values for
a version prefix, Turbo hash, team ID, and exact artifact bytes, and travels as
`x-artifact-tag`.
[Signing source](https://github.com/vercel/turborepo/blob/c7661b808d6ae15f405136fc4347e983b17cbf23/crates/turborepo-cache/src/signature_authentication.rs#L9-L125)
Layer Cache can store and echo a Team Cache tag without possessing the team
secret. Signing does not encrypt artifacts, and Turbo's own configuration calls
it an integrity check rather than a security feature.
[Configuration warning](https://github.com/vercel/turborepo/blob/c7661b808d6ae15f405136fc4347e983b17cbf23/apps/docs/content/docs/reference/configuration.mdx#L1221-L1234)

A shared native HMAC secret cannot establish Public Build provenance because a
secret distributed to every public reader also lets every reader forge tags.
It also complicates a unified Team-then-Public gateway: a client with
`remoteCache.signature: true` rejects an unsigned Public Cache fallback.
Therefore the first public path must enforce provenance in Layer Cache itself:

- Client tokens may read Public Cache and read/write their authorized Team
  Cache. A client `PUT` can never select or promote into Public Cache.
- An internal Public Build publisher token is the only principal allowed to
  create public metadata.
- Publication records the source revision, build recipe, builder identity,
  compatibility ID, Turbo hash, artifact SHA-256, size, and build duration.
- The read gateway returns a Public Cache artifact only when that trusted
  publication record exists and compatibility matches.
- Physical blob deduplication is allowed, but a Team Cache blob becomes public
  only after an independent Public Build creates the public record.
- Use TLS and storage checksums. If end-user verification of Public Build origin
  is required, add a Layer Cache asymmetric attestation verified by the local
  CLI or proxy; native Turbo HMAC cannot provide it for a public audience.

For the first working release, keep native HMAC available for Team-only mode.
Do not enable it for the unified Team plus Public path until the local proxy can
verify public attestations and handle the native tag expectation.
