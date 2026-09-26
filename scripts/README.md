# Install a release

The first published cache beta is [v0.1.0-beta.1](https://github.com/ziahamza/layercache/releases/tag/v0.1.0-beta.1), built from protected main commit `bd84aee3b6064403e0ce51e6f5dda7599b768c3a`. Public Builds are not qualified by this prerelease; a hosted self-serve cloud requires separate deployment. Run the installer from an inspected checkout pinned to that source commit:

```sh
bash scripts/install.sh --repository ziahamza/layercache --version v0.1.0-beta.1 \
  --prefix "$HOME/.local/bin"
"$HOME/.local/bin/layercache" help
```

The repository and version are explicit inputs. The source checkout supplies only
the installer script; binaries come from the published immutable release.
The installer supports Linux amd64/arm64 and macOS arm64, and requires
`gh`, `jq`, `tar`, and `shasum`. Authenticate `gh` or supply `GH_TOKEN` with read
access to the release and attestations.

Installation requires a published immutable GitHub release. It peels the locked
tag to a commit, downloads the platform archive and checksum, and verifies both
with GitHub's attestation verifier. Verification binds the publishing repository,
`release.yml`, tag, source commit, and GitHub hosted builder. It then checks the
archive checksum and permits exactly one regular binary member. Only a verified
binary that passes `help` replaces the installed executable, using an atomic
rename. Symlink targets are refused. There is no skip-verification option.

The signing policy uses the official
[GitHub attestation verifier](https://cli.github.com/manual/gh_attestation_verify).
An old `gh` without the required verification flags fails rather than weakening
the policy. The Node test uses a local fake GitHub client to exercise policy
failures and target preservation. On 2026-09-26, installation into a fresh
temporary prefix on an existing Linux x64 host passed release attestation and
checksum verification,
`help`, and `setup --preview --json`. The installed binary SHA-256 was
`e8fb1449792978e4df1e3110ff7a1bb35da83a476050cf12797e41ea65015b18`.
The [release workflow](https://github.com/ziahamza/layercache/actions/runs/36222806673)
also passed native Linux arm64 and macOS arm64 qualification. The
[published-binary installer workflow](https://github.com/ziahamza/layercache/actions/runs/36223675118)
then passed on fresh GitHub-hosted Linux x64, Linux arm64, and macOS arm64 runners.
Each job checked the immutable release, exact tag source commit, installer
verification, `help`, and setup preview. A separate post-publication download
verified all six release assets: each platform archive matched its SHA-256 manifest, contained
exactly one member, and both archive and manifest attestations matched the beta
tag and source commit above.
