# Layer Cache action

This Node 24 action adapts the familiar `actions/cache` inputs to a Layer Cache v1 endpoint. It preserves exact-hit, restore-key, lookup-only, fail-on-miss, and post-job save behavior.

The action sends Team Cache requests through a compatibility-scoped endpoint. By default it derives `linux-amd64-schema1`, `linux-arm64-schema1`, or `darwin-arm64-schema1` from the Node job process. Set the optional `compatibility` input when libc, runtime, or toolchain ABI can change outputs, for example `linux-amd64-glibc2.39-node@24-schema1`. Identities are lowercase, at most 256 bytes, and may use `-_.:+@` delimiters.

Set `project` and grant the workflow `id-token: write` to use GitHub OIDC. The action requests an ID token with audience `layercache:<project>`, exchanges it at the Team Cache endpoint, and uses the returned 15-minute capability token. The `token` input remains available as a project-scoped secret fallback. Never supply a server administrator token to a workflow.

`public-cache-mode: verified` enables Public Cache restores. Supply `public-trust-key`, `public-recipe-digest`, and `public-builder`; `public-platform` and `public-toolchain` have runner-specific defaults. Before changing the workspace, the action verifies the Ed25519 DSSE signature and the full repository, commit, recipe, target, platform, toolchain, builder, digest, and size identity. It hashes the complete download, rejects unsafe tar paths, link traversal, special files, duplicate members, and set-ID modes, then extracts the validated relative archive without `tar -P`.

Verified lookup allows two seconds by default for connection and response metadata. Set `lookup-timeout-seconds` between 1 and 3600 when the deployment needs another bound. Archive reads retain a separate progress-based idle limit.

OIDC exchange and cache service outages become warnings and cache misses by default, while invalid configuration and unsafe verified archives fail closed. `fail-on-cache-miss: true` also fails on authentication or restore unavailability, preserving the explicit fail-on-miss contract.

Verified Public archives must contain workspace-relative paths. Cache paths outside `GITHUB_WORKSPACE` are rejected in verified mode. GitHub Actions cache v2 is not supported by the Layer Cache endpoint.

## Development

From the repository root, use the pinned Node.js and pnpm versions from `mise.toml`:

```bash
mise install
(
  cd action/cache
  pnpm install --frozen-lockfile
  pnpm check
)
```

All direct dependencies and development dependencies use exact versions. `pnpm-lock.yaml` records integrity hashes for the complete dependency graph. Do not install with a mutable lockfile in CI.

GitHub executes `dist/main/index.js` and `dist/post/index.js`, so both bundles are committed. `pnpm check` snapshots those files, rebuilds them with the locked `@vercel/ncc`, and fails if any byte or file path changes. After an intentional source or dependency update:

```bash
(
  cd action/cache
  pnpm install --frozen-lockfile
  pnpm typecheck
  pnpm test
  pnpm bundle
  pnpm check:bundle
)
```

Review and commit source, lockfile, and bundle changes together. Consumers should reference an immutable repository commit rather than a mutable branch.
