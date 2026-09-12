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

The local host was Linux x86_64. Native Linux arm64 and macOS arm64 remain CI gates rather than claims from these manual trials. BuildKit Team/Public behavior on other target platforms was not tested locally.

All trial daemons were stopped, their ports were verified closed, and exact test builders, images, containers, and networks were removed. Disposable clones were moved to the desktop trash, so they remain recoverable until the trash is emptied. Pre-existing project changes and shared package/Docker caches were left intact.

## Cloud persistence follow-up

Tag publication is now blocked on a dedicated Linux x64 cloud-persistence job. The gate provisions PostgreSQL 16 and a pinned MinIO service, runs the opt-in cloud, measurement, and Public Build PostgreSQL suites under the race detector, and uses a freshly installed binary to save and restore Team Turbo and Actions bytes before and after a process restart. This is automated release-gate coverage, not an additional local manual trial.

On 2026-08-31, the production cloud storage module ran against disposable PostgreSQL 16 and MinIO containers. The checked-in QA recipe and release gate now pin their image manifests by digest. The check passed with the AWS environment credential chain and path-style bucket addressing.

- Thirty-two concurrent writers for one logical identity produced one winner. Identical retries were idempotent and divergent bytes returned a conflict.
- Reads verified the full SHA-256 stream. Replacing the S3 object with same-sized, different bytes failed closed as corrupt.
- A 16-byte project quota evicted the least recently used entry before admitting the next immutable blob. The reported committed usage remained within quota.
- Two project records with the same visible native key could not read each other's metadata or object namespace.
- An expired staged upload and an unreferenced blob were deleted. Active logical records and publication tombstones remained.
- PostgreSQL audit append/read, membership changes, Public Cache ambiguity and revocation, and fenced BuildKit promotion acquire, renew, release, and expiry all completed successfully.
- Cloud Actions accepted out-of-order ranged chunks, committed its generic artifact and protocol index atomically, preserved exact and prefix lookup across a second adapter open, and restored the verified archive. A same-sized corrupt S3 archive was invalidated during lookup and returned a safe miss.
- An installed Team server used the cloud backend for Turbo and Actions over their HTTP protocols. Turbo and Actions returned the expected bytes before and after a clean process restart. The status response named `postgresql+s3` without returning its endpoint or credentials.
- The authenticated membership, audit, and BuildKit promotion acquire, renew, and release routes completed through the installed server. An unauthenticated request produced an append-only denial event, while audit attributes contained no token, key, secret, DSN, or lease credential fields.

## PostgreSQL Public Build follow-up

On 2026-08-31, two installed Public Cache server processes shared one PostgreSQL project and the same MinIO bucket.

- Concurrent identical requests through different servers returned one new build and one reuse of the same build ID.
- One server leased the queued build while the other reported no compatible work. The live lease survived a server restart and renewed through the other process.
- A worker log submitted through one server was read through the other with the configured token and workspace path redactions applied.
- Failure released the build identity for a fresh retry. The retry received a new build ID and cancellation through the other server was immediately visible in both authenticated status responses.
- Both status responses reported the same project-scoped queue counts from PostgreSQL. Neither server created a local `public-builds.db` authority.
- Direct two-coordinator QA also contended twenty duplicate requests, serialized sixteen concurrent logs, selected an exact recipe-compatible build behind an incompatible queue head, recovered an expired lease, rejected stale publication attempts, committed a trusted publication, and restored its outputs and status after reopen.
- Active lease and publication permit records contained only 64-character SHA-256 hashes. A worker that echoed its raw lease credential into a log received `[REDACTED]` in the stored entry, and a publication descriptor containing its active permit was rejected.

The installed QA processes were stopped and their synthetic configuration directory was moved to the system trash. The shared PostgreSQL and MinIO containers remained available for the rest of the release QA run.

## KVM Public Build worker follow-up

On 2026-08-31, the production worker booted a pinned Linux guest under KVM with QEMU 8.2.2 and completed the generic virtio protocol smoke in 4.51 seconds.

- QEMU had `-nic none`; the guest could not write the read-only source filesystem and wrote only to its bounded work disk.
- The host verified the pinned kernel, raw root filesystem, embedded guest agent, and independent guest-contract digest before boot.
- QEMU entered a dedicated UID/GID, QEMU sandbox restrictions were active, and cgroup CPU, memory, and process limits were applied.
- A secret-shaped guest log was redacted. The collector credential was absent from the guest request, environment, and QEMU arguments.
- The trusted host collected the declared output over virtio-serial, enforced its size and media identity, calculated its SHA-256 digest, and observed a clean guest poweroff and process reap.
- Wrong asset digests and a non-root worker failed preflight. The arm64 worker path cross-compiled and selects `qemu-system-aarch64`.

The repository does not ship a prebuilt guest image. It now includes opt-in Linux x64 assembly recipes and real KVM harnesses under `qa/public-build-kvm`; operators remain responsible for reviewing and maintaining their pinned kernel, root filesystem, contract, and offline dependencies.

The maintained Actions executor then ran twice consecutively from the exact current tree with no guest NIC. Producer durations were 7.579 and 7.516 seconds. Both runs returned the same 158-byte artifact digest, `sha256:8da396eb2322a3b0a1ec0c2d9909ece843e3377b88af250448f9007b466e8dfe`, and the same independently calculated native key, `sha256:7b9ec1669b73e6d45da5d7321e5b3b88ab5b13d3eb2b88d1e6fd2ffc9f24ff56`. The extracted archive contained exactly `public-cache-payload-from-real-kvm` at the declared cache path. The first real run exposed two defects that were fixed before this proof: the guest now moves init into a child cgroup before setting `pids.max`, and the Actions archive canonicalizes tar timestamps. Because that changes output bytes, maintained Actions executor semantics were bumped to `actions-job-v2`.

After the final review bound the kernel, root filesystem, and guest contract into publication provenance, the Actions harness was rebuilt from the final tree and run again. Preflight reported composite builder image digest `sha256:4d3394f1545672b6f63dd5793bb003715494e06c6a24d3ce19c92a463a33110b`, `network: none`, and the exact offline-only dependency contract. The 7.604-second producer run returned the same native key, 158-byte artifact digest, and extracted payload as the prior two executions.

## Turbo Public Build KVM follow-up

On 2026-08-31, the checked-in maintained Turbo harness ran twice successfully from the exact current tree under QEMU 8.2.2 and KVM. A separate live-process inspection captured the dedicated QEMU command with `-nic none` and without `-netdev` or `hostfwd`. Preflight reported the exact `offline-only; dependencies must be vendored in source or pinned in the immutable image` mode, a read-only source block device, and bounded virtio-serial output. The admitted recipe digest was `sha256:b7ec1ceccd08bc82f5300c75e12d65a8c5918df446cf2fdae81040dd3482232b`.

The final two producer durations were exactly 8.366920926 and 8.348363339 seconds, with total harness wall times of 10.16 and 10.09 seconds. Both executions returned native key `25377100f2716363` and the same 256-byte artifact digest, `sha256:a2c8e238d92f915f5ca6d70e19b851afa65cf1e579e8494555e8f61cf731ac22`. The host independently planned the same native key, decoded both zstd archives, found `layercache-real-turbo-2.10.12-kvm\n` at `packages/app/dist/output.txt`, and found the genuine `created deterministic KVM fixture output` task log. A second real Turbo 2.10.12 process restored the collected artifacts as remote cache hits in 5ms and 7ms while a deliberately failing `npm` executable remained untouched.

The checked assets were kernel `sha256:d74be574189057a036866309fd5546724a92390a4f6ba086e93ddab2f6e0e01d`, root filesystem `sha256:dfa8f50499acf7dc64434ca07d0827920afa18436077df3527f018f5a1864d30`, contract `sha256:1b0165aa5eeb1ca65450251ecc3e2fe88a63cbcf8864767adc5b51de80ab9914`, Turbo binary `sha256:cfca1bde77f1216d4dcc8964e567eed9d47be224c848e8001edc9a1a07839dde`, Node binary `sha256:53fb205ae78805130177e24bcb459a69a1518c8d98f8965f31d85aae7ea840fc`, and guest agent `sha256:9bdadbe3f2e978ea82d959da621ccbfd487ce991f1e12f37fca7f370349fe3a2`.

Manual execution exposed two concrete defects before the final proof. Turbo needs loopback for its internal daemon even with no external NIC, so the guest now verifies the loopback interface and raises only its `IFF_UP` flag before execution. The first checked-in harness run completed the guest build but rejected Turbo's canonical trailing-slash directory entries; the validator now accepts only canonical `name/` directories while continuing to reject traversal, non-canonical files, links, special entries, and duplicates. The host restore environment also now replaces the default `PATH` instead of emitting two `PATH` entries.

## BuildKit Public Build registry follow-up

On 2026-08-31, the independent collector test published a complete OCI image-layout fixture to a disposable registry repository and pulled the graph back by the returned root-manifest digest. The pulled root manifest matched byte-for-byte, and its config and layer descriptors were all present.

The checked-in maintained BuildKit harness then composed the current guest agent, a real QEMU/KVM boot, BuildKit 0.32.2 from its pinned image, the trusted collector, and a disposable registry. The guest had no NIC and executed a real Dockerfile `RUN` through runc in 12.296 seconds. The host collected a 2,302,464-byte layout with native key `sha256:230a1d598a2828d8bb0281f3d8262aa9f53a83126fdb7d170df692041c1a9141`, pushed it, pulled immutable OCI manifest `sha256:00c159d3be4419eba476e9197ec5f316f90ffdb556d0cb2bc433e57695ae56b4`, verified all four layers plus the cache config, and found the exact `maintained-buildkit-kvm-e2e` output in the pulled graph.

Manual execution found and fixed two maintained-executor incompatibilities before that pass: current BuildKit rejects `--output type=cacheonly`, and its local OCI exporter leaves an empty `ingest/` directory outside the strict layout. The guest now omits the invalid output and removes only a verified empty real `ingest/` directory after stopping BuildKit. The root filesystem assembly also uses e2fsprogs `mke2fs`; BusyBox's incompatible applet is rejected as a guest-image dependency.

## Integration, telemetry, and verified Action follow-up

On 2026-08-31, an installed binary was exercised from a disposable configuration and repository.

- Turbo, BuildKit, and Local CI integrations previewed without writes, applied idempotently, and serialized concurrent modification with an installation lock.
- Turbo reapplication and uninstall preserved unrelated JSON edits. BuildKit created and selected the exact owned `docker-container` builder, refused an unowned name collision, and restored the previous builder on uninstall. Local CI wrote a mode-0600 expiring capability handoff without printing the token.
- Status and doctor reported integration ownership, upload backlog, credential expiry, telemetry state, cloud health, and Public Build queue state without returning tokens or backend credentials.
- The bundled Node 24 action restored a signed archive transactionally, preserved unrelated workspace files, rejected changed bytes and a `../` archive path before mutation, timed out a stalled transfer on lack of progress, and left no staging directory behind.
- A fake OTLP/HTTP collector received bounded route-template traces and request metrics. A request containing a secret query and Authorization header did not place the target, query, header, cache key, or secret in the exported payload.

## Release-sequence follow-up, 2026-09-12

These checks used the in-progress implementation on native Linux amd64. The KVM and performance trials record their own asset/binary identities. Later quota, retention, and packaging edits mean these receipts must not be described as native qualification of a future release commit.

### Native Public Builds

The new architecture-aware recipes passed the complete x64 KVM command. Actions ran twice at 8.276 and 8.368 seconds and reproduced the previous native key, 158-byte digest, and payload. Turbo ran twice at 9.950 and 10.286 seconds, produced identical artifacts, and restored through genuine Turbo. BuildKit completed a real offline Dockerfile `RUN` in 15.018 seconds, published the OCI graph, pulled it by immutable digest, and checked its output.

Selected raw results, including composite builder-image and kernel digests, are in [2026-09-12-kvm.json](evidence/2026-09-12-kvm.json). Full assembly logs, tested programs, source hashes, and output remain at `/tmp/layercache-native-review-evidence-20260912` on the QA host. The dedicated registry, cgroups, and guest assets were removed. ARM64 recipes and cross-builds passed local checks; native ARM64 KVM execution is still required. The release workflow now requires a successful ARM64 native job for the exact candidate commit.

### Repeated performance measurements

The installed CPU-heavy fixture ran eleven cold/warm pairs for each source and supported Turbo client. Native Workspace caching was disabled; every warm trial had to prove its Layer Cache source and reproduce the cold output digest.

| Turbo | Source | Cold median | Warm median | Warm/cold |
| --- | --- | --- | --- | --- |
| 2.10.12 | Local Cache | 2,577 ms | 151 ms | 5.8% |
| 2.10.12 | Team Cache | 4,045 ms | 337 ms | 8.3% |
| 2.9.14 | Local Cache | 2,178 ms | 148 ms | 6.8% |
| 2.9.14 | Team Cache | 2,426 ms | 264 ms | 10.9% |

All 44 pairs passed. [2026-09-12-performance.json](evidence/2026-09-12-performance.json) retains every raw duration, output digest, task hash, source assertion, and signed savings estimate. The tested binary digest was `sha256:6984e7d3443dcd520f2626a0f0151f669a327e8a3b1dcba0c06c98b83a669687`. Full CLI logs and native/Layer Cache reports remain at `/tmp/layercache-performance-validation.JzG0lY`.

This demonstrates reuse of expensive deterministic work with small outputs. It does not measure a real-project hit rate, full machine CPU use, or production network/S3 latency. The Team process was separate but loopback-hosted. Public Cache performance qualification is not implemented; the command explicitly rejects that source. Trial processes and Workspaces were cleaned.

### Administration and deployment

Real PostgreSQL 16 and MinIO tests proved administrator-only quota and pin operations, cross-project rejection, pin conflict detection, transactional shrink rollback, same-transaction audit, and quota persistence across reopening with an old configuration. Turbo and Actions accepted a raised limit without server restart. A lower limit evicted unpinned entries and left pinned bytes intact.

Both SQLite and PostgreSQL/S3 passed same-budget LRU-versus-impact cases, pinned aliases, unique-blob accounting, unknown cost fallback, verified-read counter persistence, and failed-upload invariants. The hand-checkable causal replay retained a six-second artifact and predicted 5,999 ms savings under impact versus no hit under LRU. This is a policy regression, not measured production ROI.

The API image ran through Compose as UID 65532 with a read-only root/config, dropped capabilities, and `no-new-privileges`. Its tested image digest was `sha256:a70628e25e30f28808f69e423fb85a06c60aad218a01fffbaf6cacec7c0ed854`. Turbo and Actions bytes survived container recreation. A quota reduced from 1 MiB to 64 bytes remained 64 bytes after restart with 50 bytes in use; an oversized upload returned 507 without losing existing reads. The QA override used host networking only to reach the disposable loopback PostgreSQL/S3 services. The systemd unit passed parsing with its executable path replaced by the temporary tested binary; no system service was installed.

The installed runner setup test passed with detected ABI, an explicit ABI and opaque Team project, OIDC failure fallback to Local Cache, scoped Turbo access, and post-step daemon cleanup. The release installer passed failure and success cases for immutable metadata, tag peeling, provenance, checksums, and archive shape using controlled release fixtures. Actual immutable-release downloads still require publication.

### Combined automated validation

The full `go test ./... -count=1` and `go test -race ./... -count=1` suites passed with the PostgreSQL, S3, measurement PostgreSQL, and Public Build PostgreSQL test environments configured. Installed-product acceptance passed in both runs. `go vet ./...`, Linux amd64/arm64 and macOS arm64 builds, action typecheck, sixteen cache-action tests, four reproducible bundle files, all seven installer/setup tests with the actual built candidate, workflow/shell lint, and `git diff --check` also passed.

The first attempt to run the installed setup test started before its candidate binary had finished compiling and failed with a missing-file error. After compilation completed, the test ran without skips and passed. That was a QA command-ordering error, not an installer fallback.

Full Go logs and candidate binaries remain at `/tmp/layercache-sequence-final.J8XmmKr5`. These checks do not claim native Linux ARM64 or macOS execution. The remaining release/deployment work is listed in [the implementation sequence](../docs/implementation-sequence.md).

### Final committed source and calibration follow-up

The committed `f8861c6` Actions KVM program ran twice more, at 10.267 and 9.778 seconds. Both runs reproduced the exact previous 158-byte artifact, native key, and payload. The composite builder-image digest was `sha256:84a58ed19ce3bbb5bdbdd099d38abe4f91ef97bd05ff3d58ed8aedd9e7067e1e`. Source hashes were checked after execution. [The final Actions receipt](evidence/2026-09-12-final-actions-kvm.json) records both runs. Full evidence remains at `/tmp/layercache-final-actions-evidence-20260912`. The temporary assets and cgroup were removed.

The earlier Actions source fingerprint differed only because the program gained the ARM64-specific runner label and a test for it after the original trial. An inverse transform reproduced the exact earlier source hashes. The amd64 workflow itself was unchanged; the extra committed-source execution removes that evidence ambiguity.

The deployment check also uncovered a real CLI credential leak: an unspecified bind address such as `0.0.0.0` could send a local administrator request through `HTTP_PROXY`. A fresh-process regression reproduced it. Local control requests now normalize wildcard addresses to loopback and use a proxy-free client. Explicit remote Team/Public URLs retain proxy support. The full CLI suite passed normally and under race after the fix, followed by installed-candidate acceptance and setup tests. [The deployment receipt](evidence/2026-09-12-deployment.json) identifies the tested API image; that image predates the CLI-only proxy correction.

A performance rerun with the corrected product binary first passed current Turbo but failed minimum Turbo at Team sample nine. All completed restores were correct. The cold task fell below the one-second minimum because wall-time calibration had run under heavier shared-host contention and selected too few PBKDF2 iterations. The failed run is preserved in [the follow-up evidence](evidence/2026-09-12-performance-followup.json); it was not discarded or counted as passing.

Calibration now uses measured user plus system CPU consumption from `process.cpuUsage()`. Tests verify that identical consumed CPU yields the same workload despite different elapsed times, reject invalid measurements, and exercise a real Node probe. The probe's CPU and wall measurements are retained in JSON. Neither the one-second minimum nor the 50% warm/cold threshold changed.

Both maintained clients then passed eleven new cold/warm pairs for each source:

| Turbo | Source | Cold median | Warm median | Warm/cold |
| --- | --- | --- | --- | --- |
| 2.9.14 | Local Cache | 2,521 ms | 96 ms | 3.8% |
| 2.9.14 | Team Cache | 2,576 ms | 115 ms | 4.5% |
| 2.10.12 | Local Cache | 2,491 ms | 82 ms | 3.3% |
| 2.10.12 | Team Cache | 2,487 ms | 103 ms | 4.2% |

All 44 new pairs proved the expected source and identical restored bytes. The tested product binary digest was `sha256:4ea763dee949d8209770869d335680ddb87f2fc9d058415684dd0c7d6f75c26f`. The follow-up JSON retains individual samples, calibration, and failure evidence. The benchmark's normal/race tests and vet passed after the calibration change.

The final installed-product acceptance suite also passed with both the test driver and the actual CLI binary built using `-race`, in 179.704 seconds. Its log is `/tmp/layercache-sequence-final.J8XmmKr5/installed-race.log`. This is separate from running a race-instrumented driver against a normally compiled child process.

The disposable `layercache-sequence-pg` and `layercache-sequence-s3` containers and their synthetic data were removed, as was the initial `layercache-sequence:qa` image. Test recipes can regenerate those datasets. Retained evidence and binaries remain at the paths above; existing projects and shared package/Docker caches were not cleaned.
