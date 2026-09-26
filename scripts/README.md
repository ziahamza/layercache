# Install a release

Run the installer from an inspected checkout pinned to an immutable commit:

```sh
bash scripts/install.sh --repository ziahamza/layercache --version PUBLISHED_TAG \
  --prefix "$HOME/.local/bin"
```

The repository and version are explicit inputs. Replace `PUBLISHED_TAG` with an
actual immutable release tag; a source checkout cannot satisfy this installer.
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
failures and target preservation. A real release installation remains a release
acceptance check once a repository and immutable release exist.
