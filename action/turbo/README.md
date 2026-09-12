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

This action does not require Public Builds or a published CLI release. Turbo
keeps its ordinary local cache. Full Local Cache management and CLI ROI reports
are provided by `action/setup`.
