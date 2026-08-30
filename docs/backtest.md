# Historical cache backtests

`layercache backtest` replays historical work in start-time order and estimates how a cache would have behaved. It never lets a run use an artifact before the producing run has finished. The retention window is renewed on each simulated hit.

```sh
layercache backtest \
  --input history.json \
  --retention 168h \
  --source teamCache \
  --json
```

`--source` accepts `localCache`, `teamCache`, `publicCache`, or `unattributed`. It labels simulated hits; it does not change matching. A hit requires the exact `artifactId` and `compatibilityId` pair.

The input is strict JSON with schema version `1`:

```json
{
  "schemaVersion": "1",
  "runs": [
    {
      "runId": "run-1",
      "startedAt": "2026-08-30T10:00:00Z",
      "finishedAt": "2026-08-30T10:00:30Z",
      "work": [
        {
          "workId": "build",
          "dependencies": ["install"],
          "artifactId": "sha256:example",
          "compatibilityId": "linux-amd64-node-24",
          "executionDurationMs": 30000,
          "artifactBytes": 1048576,
          "hitTiming": {
            "lookupMs": 10,
            "downloadMs": 80,
            "verificationMs": 5,
            "restoreMs": 25,
            "uploadMs": 0
          },
          "missTiming": {
            "lookupMs": 10,
            "downloadMs": 0,
            "verificationMs": 5,
            "restoreMs": 0,
            "uploadMs": 90
          }
        }
      ]
    }
  ]
}
```

All timestamps use RFC3339 and all durations in the file use milliseconds. Omit `executionDurationMs` when the producer duration is unknown. The resulting savings estimate then reports incomplete coverage instead of inventing a value. Negative savings are retained when cache overhead exceeds the known producer duration.

For persisted measurements from a running daemon, query a half-open period by run start (`from <= startedAt < to`):

```sh
layercache report \
  --from 2026-08-01T00:00:00Z \
  --to 2026-09-01T00:00:00Z \
  --json
```
