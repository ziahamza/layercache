# Aggregate cache budget

Status: implementation contract, not an implemented guarantee.

The deployment currently has four independent 5 GiB committed Team Cache quotas.
These are not a 20 GiB physical limit. Temporary writes, metadata, S3 publication
copies, deferred deletion, and registry data must be accounted for separately.
The explicit 10 GiB free-disk reserve is a host safety setting, not cache capacity.

## Required invariant

For each configured storage pool, physical allocated bytes plus reserved future
writes must never exceed its fixed capacity. Every writer must participate,
including native archives, Actions, Turbo, OCI registry uploads, database/index
growth, and LayerCache-managed build scratch. Public Builds are currently disabled.
External checkout outputs and unrelated Docker/VM data are outside that pool and
must be reported separately rather than silently counted as controlled storage.

Do not claim one pool covers multiple independent machines. Each local machine
gets its own limit; hosted cache capacity has its own limit. Reuse across projects
must not bypass project authorization.

## Implementation sequence

1. Inventory physical ownership and backing filesystems. Shared PostgreSQL WAL
   and MinIO housekeeping prevent a precise physical guarantee based solely on
   per-project logical rows. Use dedicated quota-controlled storage for a hard
   physical backstop, or explicitly describe the narrower accounted-byte contract.
2. Add durable, atomic capacity reservations shared by all writers. Reserve the
   peak of uploads, publication copies, decompression, and metadata before writing.
   Unknown-length writes acquire bounded increments before accepting each chunk.
   Pins and active readers count against capacity. Failure to make room rejects
   the operation safely, without dropping valid referenced artifacts.
3. Keep bytes charged through pending deletion and multipart cleanup until the
   backend confirms physical release. Reconcile reservations after crashes before
   reopening writes. Restart, parallel processes, failed deletes, and expired
   uploads must not create uncharged capacity.
4. Add registry graph-aware retention. Delete only eligible manifest/tag roots,
   then collect blobs unreachable from retained roots and active uploads/readers.
   Deduplicated blobs count once. A tag count limit alone is not a byte limit.
5. Reuse impact eviction for known build costs, verified reuse, recency, and
   unique bytes. Fall back to LRU when evidence is missing. Add admission
   comparison so a huge low-value newcomer need not displace useful residents.
   Preserve a small bounded probation area to learn about new artifacts.
6. Add a configurable soft target below the hard ceiling to leave upload and
   verification headroom. Report committed, staging, reserved, pending deletion,
   metadata, registry, and remaining bytes, plus eviction and rejection reasons.

## Acceptance

- Fill a tiny pool under concurrent Actions, native/Turbo, and registry writes.
  Sample allocated storage throughout, not just after periodic GC.
- Assert the ceiling across staging, publication, extraction, cancellation,
  process crashes, restart reconciliation, failed deletes, and active readers.
- Lower the limit below existing usage: block new reservations, reclaim eligible
  objects, and report inability to comply when pins prevent shrinkage. Never claim
  immediate compliance before the bytes are actually released.
- Replay the same workloads against LRU and impact/admission policies. Compare
  net build time saved and hit rate, with timing coverage and unknowns explicit.
- Prove fresh-worktree and CI restores remain correct after pressure-induced GC.

The pool size is a deployment choice still to be confirmed. A proposed starting
budget is 50 GiB, separate from the 10 GiB host reserve. Do not treat that proposal
as deployed configuration.
