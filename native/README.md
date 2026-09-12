# Native builds in local and team cache

Layer Cache reuses native build artifacts without owning a runner. Each project
keeps its GitHub Actions, other CI, or local build commands. Run the cache lookup
on Linux before allocating a Mac. A cache miss schedules the project's normal
build. Local engineers use the same client and keys from fresh worktrees.

## Install from main

Until releases are versioned, install the repository's checked-in, built package:

```sh
pnpm add -D '@layercache/native@github:ziahamza/layercache#main'
```

The lockfile pins the resolved commit. Update the dependency explicitly to move
to a newer main. TypeScript is the source; checked-in JavaScript works inside
node_modules without relying on Node's unsupported dependency type stripping.
Node 24 or newer is required. The package has no install/build lifecycle scripts.

## Expo development clients

Set the build cache provider in app configuration:

```ts
buildCacheProvider: {
  plugin: '@layercache/native',
  options: {
    project: 'github.com/your-team/your-app',
    app: 'mobile',
    endpoint: 'https://layer-cache.ziahamza.com',
    maxBytes: 5 * 1024 ** 3,
  },
},
```

By default the provider detects Xcode/SDK build versions and the host architecture.
For Android it detects Java and the connected target's ABIs. Expo's fingerprint
also covers the native source/dependencies/configuration. If detection fails, Expo
builds normally. To align a Linux CI planner with a declared target, set an explicit
`compatibility` option and verify that toolchain on the build runner. Never use
one explicit compatibility value for different toolchains or target ABIs.

Keep short-lived project credentials in `LAYER_CACHE_TOKEN`, never app config.
The provider also accepts paired `TURBO_API`/`TURBO_TOKEN` from Layer Cache setup. Without credentials
it uses local cache only. Team Cache permissions are unchanged. Engineers need
project-authorized credentials; do not distribute the platform administrator token.

Run `pnpm exec expo run:ios` or `run:android` as usual. The provider downloads,
verifies and extracts a matching development client, then gives Expo a local
binary path. Current JS comes from Metro. Expo's normal compilation runs on a
miss or degraded cache operation. iOS physical devices do not use Expo's provider
hook. Release variants are deliberately rejected to prevent stale embedded JS.
This integration does not intercept `eas build` or implement repacking/OTA updates.

With the existing Layer Cache CLI, configure an explicit, matching compatibility identity for
the local daemon and app provider, then run `layercache run -- pnpm exec expo run:ios`.
The provider uses the injected local endpoint and short-lived Workspace token;
the daemon handles authenticated team-cache access. No token-copy script is needed.

## CI before a Mac is allocated

Use `ziahamza/layercache/action/native@main` in a Linux job after checkout.
Grant `contents: read` and `id-token: write`. Inputs:

- `team-url`, `project`, `compatibility`: same endpoint and identities used locally.
- `operation`: `restore`, the default, or `save`.
- `key`: optional complete source key. Without it, the action hashes Git source
  names, executable modes, and current contents. Dirty and untracked source files
  participate; gitignored files do not. Supply output-affecting environment in
  compatibility or an explicit key. Submodules require an explicit key.
- `path`: a new extraction directory for restore, or a file/directory for save.
  Restore without path still downloads and validates the entire archive.

Pass the lookup's `key` output to the save step after build and validation. Start
the Mac job only when `cache-hit != 'true'`. Save only successful, validated
outputs. The server permits shared writes only under its existing trusted-branch
policy. An unavailable or unauthorized cache returns `source=degraded` and never
a hit. Keep downstream artifact publication working on both hit and miss paths.

Pin and check the toolchain on the build runner. A floating `macos-latest` label
does not prove that the binary matches the compatibility declared by Linux.

## Local native artifacts and fresh worktrees

`layercache-native key` calculates the same source key as the CI action. Credentials
are environment-only, not CLI arguments:

```sh
export LAYER_CACHE_URL=https://layer-cache.ziahamza.com
# Obtain LAYER_CACHE_TOKEN through your project's existing Layer Cache login.
layercache-native restore --project github.com/your-team/your-app \
  --compatibility your-exact-toolchain --key your-source-key --path ./restored-native
layercache-native save --project github.com/your-team/your-app \
  --compatibility your-exact-toolchain --key your-source-key --path ./built-app
```

The destination must not already exist. Restore preserves the saved top-level
directory name, executable modes, and safe internal symlinks. CLI results are
JSON. A miss has `hit:false`; errors return a nonzero exit status. The Expo adapter
and GitHub action translate errors into explicit build fallback.

## Storage, trust, and current limits

The shared local archive cache defaults to `~/.cache/layercache/native-v1`, outside
worktrees. `LAYER_CACHE_NATIVE_DIR`, `--cache-dir`, or provider `cacheDir` changes
it. The default archive budget is 5 GiB. `--max-bytes`, action `max-bytes`, or
provider `maxBytes` changes it. Eviction is least-recently-used, with conservative
space reserved before staging. Very large compressible builds may be rejected
by that staging bound. Downloaded and expanded sizes are checked independently.
Extraction outputs belong to the workspace and are not counted in the archive
budget. Expo puts them under `.expo/layercache`; remove that directory when its
build outputs are no longer needed. Team retention uses the existing server cap.

Local operations serialize with a bounded cross-process lease so simultaneous
worktrees cannot race eviction. After a crashed process, check that no native
cache operation remains before removing its `.lock` directory. Cross-machine
build-request deduplication is not implemented yet; projects should retain their
CI concurrency controls. No runner hosting or public build publication is added.

The native client uses namespaced keys over the existing artifact transport and
Turbo-scoped credentials. It does not claim Xcode compiler-cache protocol support.
Project and app identities prevent sharing private app binaries across unrelated
projects. Public dependencies can still use existing package/dependency caches.

Outputs report source, bytes, digest, and elapsed transfer/verification time.
They do not invent a cold-build baseline or report transfer time as compute saved.
Compare skipped Mac jobs with measured cold builds separately.
