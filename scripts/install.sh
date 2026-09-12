#!/usr/bin/env bash
set -euo pipefail
umask 077
export GH_HOST=github.com

usage() {
  printf '%s\n' 'Usage: install.sh --repository OWNER/REPO --version vX.Y.Z [--prefix DIR]'
}
repository=""
version=""
prefix="${HOME}/.local/bin"
while (($#)); do
  case "$1" in
    --repository|--version|--prefix)
      (($# >= 2)) || { usage >&2; exit 2; }
      case "$1" in
        --repository) repository=$2 ;;
        --version) version=$2 ;;
        --prefix) prefix=$2 ;;
      esac
      shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
[[ "$repository" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || { echo 'An explicit GitHub OWNER/REPO is required.' >&2; exit 2; }
[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]] || { echo 'An exact vX.Y.Z release tag is required; latest and branches are refused.' >&2; exit 2; }
[[ -n "$prefix" && "$prefix" != / ]] || { echo 'Choose a binary installation directory.' >&2; exit 2; }
for dependency in gh jq tar shasum; do
  command -v "$dependency" >/dev/null || { echo "Required command missing: $dependency" >&2; exit 1; }
done
case "$(uname -s)/$(uname -m)" in
  Linux/x86_64) platform=linux-amd64 ;;
  Linux/aarch64|Linux/arm64) platform=linux-arm64 ;;
  Darwin/arm64) platform=darwin-arm64 ;;
  *) echo 'Supported platforms: Linux amd64/arm64 and macOS arm64.' >&2; exit 1 ;;
esac
artifact="layercache-$platform"
package="$artifact.tar.gz"
temporary=$(mktemp -d "${TMPDIR:-/tmp}/layercache-install.XXXXXXXX")
staged=""
cleanup() {
  if [[ -n "$staged" ]]; then rm -f -- "$staged"; fi
  rm -rf -- "$temporary"
}
trap cleanup EXIT

# A published immutable release locks both its tag and its assets. Branches,
# mutable releases, and draft assets are not installation sources.
gh api "repos/$repository/releases/tags/$version" >"$temporary/release.json"
jq -e --arg tag "$version" '.tag_name == $tag and .draft == false and .immutable == true' \
  "$temporary/release.json" >/dev/null || { echo 'The release must be published and immutable.' >&2; exit 1; }
gh api "repos/$repository/git/ref/tags/$version" >"$temporary/tag.json"
commit=""
for _ in {1..16}; do
  object_type=$(jq -er '.object.type' "$temporary/tag.json")
  object_sha=$(jq -er '.object.sha' "$temporary/tag.json")
  [[ "$object_sha" =~ ^[a-f0-9]{40}$ ]] || { echo 'Invalid tag object digest.' >&2; exit 1; }
  case "$object_type" in
    commit) commit=$object_sha; break ;;
    tag) gh api "repos/$repository/git/tags/$object_sha" >"$temporary/tag.json" ;;
    *) echo 'The release tag does not resolve to a commit.' >&2; exit 1 ;;
  esac
done
[[ -n "$commit" ]] || { echo 'Release tag nesting limit exceeded.' >&2; exit 1; }
gh release download "$version" --repo "$repository" --dir "$temporary" \
  --pattern "$package" --pattern "$package.sha256"
for subject in "$package" "$package.sha256"; do
  gh attestation verify "$temporary/$subject" --repo "$repository" \
    --signer-workflow "$repository/.github/workflows/release.yml" \
    --source-ref "refs/tags/$version" --source-digest "$commit" \
    --deny-self-hosted-runners >/dev/null
done
expected=$(awk -v name="$package" 'NF == 2 && $2 == name { print $1 }' "$temporary/$package.sha256")
[[ "$expected" =~ ^[a-f0-9]{64}$ ]] || { echo 'Invalid package checksum manifest.' >&2; exit 1; }
actual=$(shasum -a 256 "$temporary/$package")
[[ "${actual%% *}" == "$expected" ]] || { echo 'Package checksum mismatch.' >&2; exit 1; }
[[ "$(tar -tzf "$temporary/$package")" == "$artifact" ]] || { echo 'Unexpected package members.' >&2; exit 1; }
[[ "$(tar -tvzf "$temporary/$package")" == -* ]] || { echo 'Package binary must be a regular file.' >&2; exit 1; }
mkdir -p "$prefix"
[[ ! -L "$prefix/layercache" && ! -d "$prefix/layercache" ]] || { echo 'Refusing a symlink or directory installation target.' >&2; exit 1; }
staged=$(mktemp "$prefix/.layercache-install.XXXXXXXX")
tar -xOzf "$temporary/$package" "$artifact" >"$staged"
chmod 0755 "$staged"
"$staged" help >/dev/null
mv -f -- "$staged" "$prefix/layercache"
staged=""
printf 'Installed Layer Cache %s at %s/layercache\n' "$version" "$prefix"
