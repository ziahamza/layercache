# Performance release gate

The performance gate runs the installed CLI and a real, pinned Turbo client. It measures 11 cold/warm pairs per source and fails if the warm median exceeds half the cold median. A sample also fails if Layer Cache cannot prove the expected cache source, Turbo does not report a remote hit, or restored bytes differ from the cold output.

Run the gate from the repository root after installing the pinned toolchain and Turbo fixture:

```bash
mise install
pnpm --dir qa/fixtures/turbo install --frozen-lockfile
mise exec -- go build -trimpath -o /tmp/layercache-performance-binary ./cmd/layercache
mise exec -- go run ./qa/performance \
  --binary /tmp/layercache-performance-binary \
  --turbo "$PWD/qa/fixtures/turbo/node_modules/.bin/turbo" \
  --sources local,team \
  --samples 11 \
  --output /tmp/layercache-performance.json
```

Choose unused paths for the binary and evidence. `--output` refuses to overwrite an existing file. Omit it to write JSON to stdout. Progress goes to stderr. To qualify the minimum supported Turbo client, install `qa/fixtures/turbo-minimum` and pass its executable in a separate invocation.

The gate creates its own temporary configurations, repository clones, loopback daemons, and cache directories. It stops those daemons and removes those directories on completion or an interrupt. The JSON evidence file survives cleanup, including completed samples when validation fails. No account, registry, or existing project configuration is used. The Team Cache benchmark uses a separate loopback Team Cache process with two Local Cache processes; it does not characterize production network or S3 latency.

Each pair starts with two independent Git clones of the fixture and no native cache or build output. Turbo runs with `--cache=remote:rw --summarize`, disabling its native Workspace cache. A unique, tracked input value makes each cold task a miss. The second clone has the same input and must restore the exact artifact. Local Cache trials share one daemon. Team Cache trials use a different consumer daemon, and the report must identify Team Cache as the source. A Local Cache hit cannot pass a Team Cache trial.

The task performs deterministic PBKDF2 computation and writes a small output file. Calibration measures the actual computation outside the samples, then fixes the iteration count for all samples. The default target is two seconds per cold task; `--target-duration` accepts 2s through 30s. Every cold sample must record at least one second of task execution. No sleeps or invented producer durations supply the measured saving.

Durations cover the complete `layercache run` invocation, including Turbo startup and final summary reconciliation. Clone creation, daemon setup, calibration, and subsequent report queries are outside that interval. JSON preserves every duration, the output digest, native Turbo summaries, CLI output, and Layer Cache reports, plus the installed binary digest, client versions, platform, and calibration count. The median calculation uses all successful samples without trimming outliers. It retains ratios above one and fails when a restore is slower than the gate allows.

This fixture qualifies acceleration for expensive computation producing small artifacts. It does not establish a cache hit rate for real projects, savings for short tasks, or transfer performance for large archives. Those need separate workload measurements.

Only `local` and `team` are accepted sources. `--sources public` fails explicitly. Public Cache performance qualification still needs an equivalent fixture executed by a trusted Public Build, with signed publication and a verified restore. Publishing this local synthetic task directly would not exercise that trust path. The existing KVM qualification and signed Public Cache acceptance checks remain separate from this timing gate.

The reusable GitHub workflow `.github/workflows/performance.yml` runs this gate for CI and release publication and uploads its JSON evidence. Statistics tests use fixed, hand-calculated examples; the timed workload runs only through the explicit performance command.
