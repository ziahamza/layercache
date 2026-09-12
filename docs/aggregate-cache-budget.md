# Aggregate cache budget

Implemented for the personal single-host deployment on 2026-09-12.

## Hard ceiling

All active Team Cache PostgreSQL data and WAL, MinIO objects and multipart
uploads, registry layers, gateway staging/indexes, and this engineer's default
Local Cache directory now live on one 48 GiB ext4 filesystem. Its sparse backing
file is bounded at 48 GiB, leaving margin under the approved 50 GiB ceiling for
bounded service logs. A sparse image does not preallocate 48 GiB. Discard returns
freed extents to the host.

The kernel enforces this physical limit across writers and crashes. Missing
mounts fail closed: the gateway and backend startup guards verify the pool marker
and filesystem size. Gateway project paths cannot escape the pool. The gateway
checks both pool and host headroom before writes and while streaming request
bodies. Concurrent allocation is ultimately limited by the filesystem, not by a
periodic directory scan or a process-local counter. Exhaustion rejects writes;
it does not fall back to an unbounded directory.

The 10 GiB reserve remains an admission safety setting for both the pool and
host filesystem. It cannot stop unrelated processes consuming host space.
PostgreSQL/MinIO now have dedicated cache instances. Shared platform services
were not stopped or migrated. Original cache namespaces and duplicate local data
were retired after live verification; database snapshots remain inside the pool.

This is a deployment-level physical guarantee, not a new distributed S3 quota
protocol. Other installations must provision bounded storage to obtain the same
guarantee. Explicit custom cache paths, other engineers' machines, downloaded
workspace build outputs, unrelated Docker build cache, and external VM disks
are not controlled by this pool. Public Builds remain disabled; their scratch
must join a bounded pool before deployment.

## Proactive retention

A ceiling is not a utilization target.

- Team Cache maintenance runs each minute. Unreused blob groups expire after
  24 hours; reused groups expire after seven idle days. A fresh alias, pin, or
  active read lease protects the whole digest group from TTL pruning.
- Each project's committed cache has a 1 GiB soft target, below its existing
  5 GiB quota. Impact eviction ranks observed producer cost, verified reuse,
  age, and unique bytes. Missing timing falls back to LRU. Pins may prevent
  reaching the soft target; they do not bypass the physical ceiling.
- S3 bytes remain physically charged until collection actually removes them,
  including the existing one-hour deletion grace and live-reader protection.
- Registry collection runs every four hours. New manifests get 24 hours of
  probation. Keep the newest three tagged roots per repository unless they
  become idle for seven days; older excess roots are eligible after a day
  without manifest reads. Tags starting with `keep-` protect their root.
  Retained OCI indexes and subjects protect their transitive child manifests.
- An exclusive cross-process registry lease drains gateway requests before
  root retirement and offline mark-and-sweep. New registry requests get a
  retryable 503 during collection. Other cache protocols stay online.
- Native Local Cache prunes seven-day-idle archives during cache operations,
  even below its byte budget. Access refreshes recency.

TTL fields are opt-in in the server configuration:
`cacheIdleTtl`, `cacheUnreusedTtl` are duration nanoseconds;
`cacheSoftBytes` is bytes. Zero disables the respective proactive rule.
Protected workload data should use pins or registry `keep-` tags, not rely on
cache retention for backup.

## Operations and evidence

The pool is mounted through a persistent systemd mount unit at
`/home/hzia/platform/data/layercache-pool`. Default local data at
`~/.cache/layercache` points into its `local` directory. The gateway's authenticated
`/v1/status` includes storagePool capacity, used/available bytes, reserve, and
host free-space health. Container logs are capped at 30 MB per service.

Host preparation: `deploy/pool/prepare.ts`.
Registry selection and collection: `deploy/pool/registry-policy.ts` and
`deploy/pool/prune-registry.ts`. Root-only host migration scripts live in the
config repository; no credentials or cache payloads are checked into Git.

Validation completed:

- Real PostgreSQL/S3 pruning and impact-policy tests passed under the race
  detector, including below-quota expiry, pins, and active reader protection.
- A disposable 64 MiB filesystem rejected concurrent writers at its hard
  ceiling. Deletion, sync and trim reduced allocated backing bytes from
  61,579,264 to 4,272,128. Production was not filled for this test.
- Four projects restored their original 128 MiB artifacts with matching
  digests after migration. Project isolation and signed Actions downloads passed.
- Actual Docker build, push, immutable-digest pull, and file verification passed.
  Registry collection also ran successfully through its systemd service.
- [Parle hosted run](https://github.com/ziahamza/parle-extension/actions/runs/34717881362)
  restored its native artifact after migration and skipped the Mac build.

Remaining refinements: workload-driven tuning, value-aware admission before
displacing a more useful resident, and a distributed capacity coordinator for
deployments without a bounded shared filesystem. This implementation does not
claim optimal hit rate or measured CPU savings from its retention heuristic.
