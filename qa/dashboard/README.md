# Dashboard browser QA

The TypeScript harness starts real Local Cache, Team Cache, and dashboard
processes with isolated temporary configuration, strictly seeds both caches,
then runs a selected browser against the embedded dashboard. Synthetic responses cover
additional browser projection edge cases. Runtime credentials remain in a
private temporary directory and are never printed.

```sh
mise exec -- pnpm install --frozen-lockfile
mise exec -- pnpm exec playwright install chromium firefox webkit
mise exec -- go build -o /tmp/layercache-dashboard-qa ./cmd/layercache
LAYERCACHE_BIN=/tmp/layercache-dashboard-qa DASHBOARD_BROWSER=chromium mise exec -- pnpm test:dashboard
LAYERCACHE_BIN=/tmp/layercache-dashboard-qa DASHBOARD_BROWSER=firefox mise exec -- pnpm test:dashboard
LAYERCACHE_BIN=/tmp/layercache-dashboard-qa DASHBOARD_BROWSER=webkit mise exec -- pnpm test:dashboard
```

`@playwright/test` is pinned in the root package. `DASHBOARD_BROWSER` accepts
`chromium` (default), `firefox`, or `webkit`. WebKit verifies the browser engine
used by Safari; it does not replace testing the Safari application on macOS/iOS.
Set `CHROMIUM_PATH` only for an explicitly chosen compatible Chromium binary;
Firefox and WebKit always use the versions installed by pinned Playwright. `QA_OUTPUT` optionally selects
the screenshot/results directory; otherwise the harness creates a temporary one.
The command exits nonzero on failed assertions and stops its fixture processes.
Temporary fixture data remains available for inspection; the directory is printed.

Coverage includes six viewport sizes, keyboard dialog operation and accessible
naming, small-text contrast, CLI-link lifecycle, independent tabs, period
selection, failed refresh and recovery, long project identities, maximum project
count, live storage values, zero/negative/partial measurement evidence, report-only
outages, text escaping, and credential-free same-origin request URLs.

For manual inspection, run `fixture.ts` directly with `LAYERCACHE_BIN`; its first
JSON readiness message includes `linkFile`. Open that private file's complete URL
in a browser, or pass its path as `DASHBOARD_LINK_FILE` to `browser.ts`. Stop the
fixture with Ctrl-C or create the `stop` file in its temporary directory.
