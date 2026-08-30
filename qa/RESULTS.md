# Manual QA results

These trials were run on 2026-08-30 against the working tree that became the first local-first MVP. Every project trial used disposable independent clones pinned to one commit, an isolated Layer Cache configuration, and uniquely named Docker resources. Original repositories were not modified.

## Real projects

| Project and adapter | Cold result | Fresh Workspace result | Correctness check |
| --- | --- | --- | --- |
| `agent-access`, Turbo, 5 tasks | 4.36 s command wall time | 0.90 s; 5/5 remote hits | All 15 restored JavaScript outputs matched; `node --check` passed |
| `parle`, Turbo, 10 tasks | 12.18 s | 0.58 s; 10/10 hits, then 0.50 s after daemon restart | All 542 declared outputs matched byte-for-byte |
| `gitenv`, Turbo, 1 task | 1.73 s | 0.44 s; 1/1 hit | All 141 outputs matched; restored JavaScript passed `node --check` |
| `booker`, BuildKit production runtime target | 160.22 s | 6.58 s; 12/20 vertices cached | Identical 488,712,786-byte image ID, platform, labels, and offline filesystem probes |

The warm Turbo reports attributed the hits to Local Cache. Their signed net wall-time estimates were +966 ms for `agent-access`, about +4.05 s for `parle`, and +1.20 s for `gitenv`. These estimates are deliberately smaller than gross task time and can be negative for small or transfer-heavy work.

Dependency installation was not claimed as a Layer Cache speedup in these trials. Package managers reused pre-existing local package data, while the measured Layer Cache paths were the declared Turbo outputs or BuildKit graph.

## Protocol and workflow trials

- A stock `@actions/cache` 6.2.0 v1 client saved an archive and restored it into a clean directory.
- Redwood Local CI 0.18.1 ran a real workflow in two distinct fresh runner containers. The first job missed, computed, and saved a 298-byte fixture. The second restored the exact value, reported `cache-hit: "true"`, skipped computation, and suppressed duplicate publication. Local CI itself was not copied, patched, or bundled.
- Two fresh BuildKit builders shared a registry-backed Team Cache. The second builder reported six cached vertices and produced the same image digest and runtime output.
- Installed acceptance scenarios exercised Local Cache lifecycle and quota, Team Cache warming and retry, signed Public Cache verification and revocation, Public Build control-plane persistence, run reports, and causal historical backtesting.

## Scope and cleanup

The local host was Linux x86_64. Native Linux arm64 and macOS arm64 remain CI gates rather than claims from these manual trials. BuildKit Team/Public behavior on other target platforms and a production-isolated Public Build worker were not tested locally.

All trial daemons were stopped, their ports were verified closed, and exact test builders, images, containers, and networks were removed. Disposable clones were moved to the desktop trash, so they remain recoverable until the trash is emptied. Pre-existing project changes and shared package/Docker caches were left intact.
