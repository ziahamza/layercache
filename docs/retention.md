# Cache retention

Layer Cache defaults to `lru`. `setup --eviction-policy impact` opts into
retaining artifacts according to observed producer time, verified reuse and
bytes. The configuration field is `evictionPolicy`; servers apply it to Local
Cache and PostgreSQL/S3 persistence when they start. `lru` remains available
for comparison and for workloads with little timing evidence.

Both policies enforce the existing byte quota, metadata budget, staging
limits and minimum local free-space reserve. An upload must pass validation
before it can evict committed artifacts. A pin prevents eviction, so a quota
that cannot fit around retained pins rejects the write. Team Cache removes
metadata visibility transactionally; S3 removes unreferenced objects after
the existing reader-lease and grace rules. Its logical quota is not a limit
on instantaneous S3 billing during that grace period.

## How impact eviction chooses

For blob capacity, `impact` considers each unique immutable blob with all of
its cache-key aliases. It charges the blob's bytes once. If any alias is
pinned, or if a publication currently protects that digest, the whole group
is excluded. The group uses its most recent alias access and the largest
positive producer duration observed on an alias. Extra aliases do not
multiply duration or frequency. A known duration on one alias is evidence
for the same bytes even when another alias lacks timing.

The policy evicts the lowest score:

```text
cost = min(observed producer milliseconds, 24 hours in milliseconds)
reuse = 1 + log2(1 + min(verified reads, 32))
age = max(now - most recent access, 0)
score = cost × reuse × 2^(-age / 7 days) / max(unique blob bytes, 1)
```

This is a gross producer-time heuristic. It does not estimate hit probability,
measure CPU time, or subtract client extraction and network costs that the
cache cannot observe completely. Duration and read-count caps prevent one
large value or an old popular entry from dominating indefinitely. Equal
scores resolve by recency, creation time and stable identity.

If any eligible blob lacks a positive producer-duration sample, that
eviction decision uses LRU across the blob groups. Missing timing does not
mean recomputation is free. Metadata-only pressure continues to evict the
oldest unpinned individual cache entry, which can release alias metadata
without removing a pinned blob. Default `lru` keeps its existing individual
entry ordering.

Read counts persist across server restarts and are shared by aliases of the
same blob. Local Cache increments them after verifying a complete artifact.
Team Cache increments them only when a streamed S3 read reaches verified
EOF. Failed/corrupt reads, partial Team reads and HEAD probes do not increase
the count. Counter failures never break a successful cache restore.

Incoming artifacts still use the existing admission behavior. A new insert
can displace a more valuable resident artifact because this version does not
compare admission value with eviction value. That optimization needs workload
evidence before changing successful publication behavior.

## Compare policies before rollout

Use the same historical input, retention window, source and byte limit for
both replays:

```bash
layercache backtest --input history.json --retention 168h \
  --source localCache --max-bytes 10737418240 --eviction-policy lru --json
layercache backtest --input history.json --retention 168h \
  --source localCache --max-bytes 10737418240 --eviction-policy impact --json
```

Both runs preserve causal ordering. A build's output and producer duration
become available only after it finishes. A later timing sample cannot alter
an earlier eviction. Compare `period.netEstimatedBuildTimeSaved` alongside
hit rate and timing coverage. `impactEvictions` and `lruFallbackEvictions`
show how often the impact replay had enough producer evidence to rank by
value. Missing fingerprints and missing savings evidence remain unknown.

Replay charges `artifactBytes` once per artifact identity and compatibility.
Historical inputs do not contain content digests, so it cannot infer physical
deduplication across distinct artifact identities or reconstruct pins. That
limits comparisons with a live cache whose unrelated keys share one blob.

The hand-checkable regression trace uses an eight-byte cache. It builds a
four-byte artifact costing six seconds, then a four-byte artifact costing
ten milliseconds, then another four-byte artifact. LRU evicts the first
artifact; impact retains it and evicts the ten-millisecond artifact. A final
request for the first artifact saves an estimated 5,999 ms after a 1 ms
restore, versus no hit under LRU. Removing its original timing makes impact
fall back to LRU. This validates policy behavior, not a production ROI claim.

Production tuning still needs representative workload traces, admission
decisions, complete restore-cost attribution, and performance profiling of
candidate selection at large cache sizes. Blob ranking scans the eligible
metadata when the cache is under quota pressure; it is not a constant-time
replacement index.
