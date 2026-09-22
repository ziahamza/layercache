# Layer Cache for Docker BuildKit

Use the direct TypeScript action with GitHub-hosted runners. It authenticates via
GitHub OIDC, logs into only the Layer Cache registry, and supplies standard
BuildKit options. Your release images and GHCR login remain unchanged.

```yaml
permissions:
  contents: read
  id-token: write
steps:
  - uses: actions/checkout@v5
  - uses: docker/setup-buildx-action@v3
  - uses: ziahamza/layercache/action/buildkit@main
    id: cache
    with:
      team-url: https://layer-cache.ziahamza.com
      namespace: gitenv
      scope: machine-runtime
      compatibility: machine-runtime-v1
      ttl-minutes: 60
  - uses: docker/build-push-action@v6
    with:
      context: .
      cache-from: ${{ steps.cache.outputs.cache-from }}
      cache-to: ${{ steps.cache.outputs.cache-to }}
```

`namespace` is the configured project alias, not an arbitrary registry username.
`project` defaults to lowercase `github.com/${github.repository}`. Separate build
targets with `scope`; change `compatibility` when intentional cache isolation is
needed. Its SHA-256 becomes the cache tag. BuildKit still keys actual steps by
their inputs. The server's project-scoped credential permits writes only for
trusted events on its configured default branch. Read-only tokens return an
empty `cache-to` output.

Setup failure warns and returns `enabled=false` with empty cache options. Export
uses `ignore-error=true`, so cache availability does not block image publishing.
For strict qualification, require `enabled=true` and remove that option. Existing
GitHub `type=gha` caches can be additional input/output lines.

Credentials last 1–60 minutes and do not refresh during a build: place setup
immediately before the build and choose a sufficient lifetime. Post cleanup
restores only the owned registry entry; multiple invocations unwind in reverse
order. Existing GHCR credentials and Buildx configuration are preserved. Runners
using a global Docker credential store or a helper for the Layer Cache host
currently fail open without touching that helper. No consumer script or durable
registry secret is required. Registry storage follows the server's pool cap and
retention policy; this action does not bound the runner's local BuildKit disk.
