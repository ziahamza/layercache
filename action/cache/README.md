# Layer Cache action

This Node 24 action adapts the familiar `actions/cache` inputs to a Layer Cache v1 endpoint. It preserves exact-hit, restore-key, lookup-only, fail-on-miss, and post-job save behavior.

The action sends Team Cache requests through a compatibility-scoped endpoint. By default it derives `linux-amd64-schema1`, `linux-arm64-schema1`, or `darwin-arm64-schema1` from the Node job process. Set the optional `compatibility` input when libc, runtime, or toolchain ABI can change outputs, for example `linux-amd64-glibc2.39-node@24-schema1`. Identities are lowercase, at most 256 bytes, and may use `-_.:+@` delimiters.

The action does not resolve Public Cache directly, so its `public-cache-mode` input must remain `disabled`. A Local Layer Cache endpoint may still provide transparent verified Public fallback. In that case, the daemon verifies the signed publication, archive digest, size, identity, provenance, and lease before it returns a normal v1 hit to the action. GitHub Actions cache v2 is not supported by the Layer Cache endpoint.

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
