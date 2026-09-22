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
target using the shared Expo mode below. The default provider detects that same
identity locally; no compatibility override is needed. Legacy explicit
`compatibility` overrides remain supported, but must equal the planner's output
for shared hits. Never use one value for different toolchains or target ABIs.

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

### Shared Expo development-client keys

Source mode remains the default and does **not** match Expo provider keys.
For iOS Debug simulator clients, use `key-mode: expo` after installing the app's
locked dependencies. Both the action and provider use the **app's installed**
`@expo/fingerprint` (including Expo CLI's pnpm-nested copy), with identical
options. No fingerprint implementation is downloaded at runtime.
The planner also uses the installed Expo environment loader, defaults `NODE_ENV`
and `BABEL_ENV` the same way as `expo run:ios`, and loads the app's `.env` files
silently. Explicit environment overrides must still match across jobs/worktrees.

On the selected Mac toolchain, `pnpm exec layercache-native expo-target` emits
JSON containing `os`, `arch`, the complete `xcodebuild -version` output as `xcode`,
and the simulator SDK build as `sdk`. Record this non-secret target as a project
variable, for example `LAYERCACHE_IOS_TARGET`. Linux can look up this identity
without Xcode. macOS verifies all fields against its actual toolchain before
restore or save; simulator saves cannot run on Linux.

```yaml
- uses: ziahamza/layercache/action/native@main
  id: native
  env:
    NODE_ENV: development
    BABEL_ENV: development
  with:
    team-url: https://layer-cache.ziahamza.com
    key-mode: expo
    expo-root: apps/mobile
    expo-app: mobile # exactly the provider options.app
    expo-target: ${{ vars.LAYERCACHE_IOS_TARGET }}
```

Use these same inputs on the Mac save step, plus `operation: save`, the validated
`.app` path, and **both** Linux lookup outputs as `key` and `compatibility`.
Save recalculates the fingerprint and rejects any mismatch instead of publishing
under a stale lookup key. Acquire save credentials after compilation (each action
invocation exchanges fresh OIDC credentials). `expo-scheme` must match the raw
`--scheme` passed to Expo, not the app's URL scheme. Omit it on both sides when
using Expo's default scheme. Never publish Release output under this mode.

For a local/CI planner outside the action:

```sh
NODE_ENV=development BABEL_ENV=development pnpm exec layercache-native expo-key \
  --root apps/mobile --project github.com/your-team/your-app --app mobile \
  --target '{"os":"darwin","arch":"arm64","xcode":"Xcode 26.0\nBuild version YOUR_BUILD","sdk":"YOUR_SDK_BUILD"}'
```

On macOS `--target` may be omitted to detect the active toolchain. The output is
JSON `{project, compatibility, key}`, not credentials. The action/provider reject
simulator artifacts with the wrong platform or executable architecture and reject
embedded `main.jsbundle` files. These checks are not a build-provenance attestation:
the trusted producer must compile Debug, validate installation/launch on a simulator,
and verify current JavaScript loads through Metro before publication. Pass Expo
`--no-build-cache` for that initial qualification build; the provider honors it for
both restore and upload, so publication can happen only after validation.

Cross-OS equality is **not assumed**. Pin the same dependencies, build-affecting
environment and Expo configuration on Linux, Mac and local worktrees. Expo's own
fingerprint rules determine whether generated native directories are excluded;
tracked native code is not blindly ignored. Qualify the fingerprint before and
after prebuild/CocoaPods, then verify a fresh Linux lookup and a matching Mac
worktree restore. A mismatch safely builds normally, but cannot demonstrate a
skipped Mac runner. Real simulator/Metro qualification remains a project rollout
gate; the transport and identity tests alone do not prove it.

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
it. The default managed cache budget is 5 GiB. `--max-bytes`, action `max-bytes`, or
provider `maxBytes` changes it. Eviction is least-recently-used, with conservative
space reserved before staging. Very large compressible builds may be rejected
by that staging bound. Downloaded and expanded sizes are checked independently.
Expo provider extractions now live in that same cache and share its budget with
archives and download/staging reservations. Repeated hits in one process reuse
one extraction per identity and digest, including across worktrees. Expo has no
release callback, so returned paths and their backing archives stay pinned until the consumer process exits;
the next cache operation reclaims dead owners. Live consumers are never evicted:
insufficient capacity falls back to a normal Expo build. PID reuse conservatively
retains old entries rather than risking deletion of an active app. Expanded
artifacts are validated before their paths are handed to Expo; a newly rejected
extraction releases its reservation without deleting any previously returned path.
Conflicting saves cannot replace an archive backing a live consumer. Expanded
reservations include conservative per-entry allocation overhead. This is managed
payload accounting, not a filesystem quota on arbitrary files or filesystem
metadata; use the deployment's capped filesystem for a physical hard limit.
Explicit CLI/action restore destinations and normal build outputs still belong
to the caller and are not managed cache entries. Old `.expo/layercache` outputs
from previous versions are not automatically deleted because they have no safe
ownership record; remove them only after their Expo consumers stop. Team retention
uses the existing server cap.

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
