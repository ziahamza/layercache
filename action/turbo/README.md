# Native Turbo action

The action and its tests are TypeScript. The Node 24 action runtime executes
erasable TypeScript directly, with no generated bundle. CI separately runs
strict typechecking because Node's type stripping does not check types.
The older setup action still supplies shared JavaScript OIDC helpers; this
change does not rewrite that existing implementation or the Go service.

This action connects Turbo directly to Team Cache. It requires no CLI download,
consumer script, extra npm dependency, or long-lived CI secret. Its tests belong
in this repository. Use `action/setup` instead when you also want a Layer Cache
local daemon and CLI.

```yaml
permissions:
  contents: read
  id-token: write
steps:
  - uses: actions/checkout@v4
  - uses: ziahamza/layercache/action/turbo@main
    with:
      team-url: https://layer-cache.ziahamza.com
      compatibility: linux-amd64-node24-schema1
  - run: pnpm turbo run build
```

`main` tracks the latest action code, including changes between workflow reruns.
This is the current pre-versioning integration policy. Use a reviewed commit SHA
when reproducibility is required. Repository visibility and action access must
permit the caller to use it. Configure the language tools
and dependencies as usual. The project defaults to the lowercase GitHub repository
identity. Choose compatibility explicitly; only compatible builds should share it.

Credentials default to 30 minutes. Authentication failures fail setup with a
redacted error. Invoke only for trusted workflows; this action does not decide
which repository events you trust. Do not use `pull_request_target` to execute
untrusted checkout contents. No Actions cache environment variables are changed.

Team Cache enforces write authority independently of the workflow step condition.
Turbo writes require the configured default branch and a signed GitHub `push`,
`workflow_dispatch`, or `schedule` event. Other refs/events get read-only
credentials. This prevents PRs from overwriting main's shared Turbo cache.
GitHub's signature, issuer, expiry, repository, and project audience are checked;
workflow filenames are not separately allowlisted. Project members and the cache
administrator remain trusted writers through their own credentials.

This action does not require Public Builds or a published CLI release. Turbo
keeps its ordinary local cache. Full Local Cache management is provided by
`action/setup`.

## Build-time reports

Setup enables `TURBO_RUN_SUMMARY=true`. Its post step submits new `.turbo/runs`
summaries to Team Cache, then prints eligible tasks, hits, misses and estimated
build time saved in the job log and step summary. The server persists those outcomes for period reports. Turbo
tasks satisfied by its own workspace cache are not credited to Layer Cache.
Missing timing stays unknown. These estimates are task/critical-path wall time,
not CPU savings or a measurement of the whole workflow's elapsed time.

Use the action once per job, after checkout and before Turbo commands. For Turbo
invoked below the workspace root, set `working-directory` to that workspace-relative
directory. Multiple Turbo commands at that same root are collected together.
Summaries from before setup are excluded. Commands overriding summary generation
or running at other roots are not reported by this action.

The post step obtains fresh GitHub OIDC credentials, so the initial Turbo token's
lifetime does not limit reporting. The service binds reports to the signed
repository, workflow run/attempt and check/job ID; matrix jobs cannot replace
each other's graphs. The submitted payload contains only task identities, graph,
cache outcome and timing fields, not the summary's environment or command data.
Team/Public source attribution requires server-observed artifact transfers.

Reports are bounded to 32 new summaries, 4,096 total tasks, 8 MiB input, and a
24-hour job window. Files must remain within the workspace and may not be
symlinks. Failed report collection, expired OIDC access or an older server without
the reporting endpoint produces a warning and does not change the build result.
Deploy the server update before expecting reports from `action/turbo@main`.
