#!/usr/bin/env bash
set -euo pipefail

script_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd -- "$script_root/../.." && pwd)
# shellcheck source=qa/public-build-kvm/native-platform.sh
source "$script_root/native-platform.sh"

if [[ $# -eq 2 && $1 == --check ]]; then
  require_native_platform "$2"
  [[ -c /dev/kvm ]] || { echo '/dev/kvm is required' >&2; exit 1; }
  exit 0
fi
if [[ $# -ne 6 ]]; then
  echo "usage: $0 amd64|arm64 /reviewed/kernel expected-sha256 /delegated/cgroup disposable-registry/repository /new/evidence-directory" >&2
  echo "       $0 --check amd64|arm64" >&2
  exit 2
fi

architecture=$1
kernel=$2
kernel_sha256=${3#sha256:}
cgroup_parent=$4
registry_repository=$5
evidence_directory=$6
require_native_platform "$architecture"
[[ -c /dev/kvm ]] || { echo '/dev/kvm is required' >&2; exit 1; }
[[ $kernel_sha256 =~ ^[0-9a-f]{64}$ ]] || { echo 'expected kernel SHA-256 is required' >&2; exit 2; }
[[ $cgroup_parent == /sys/fs/cgroup/* && -f $cgroup_parent/cgroup.subtree_control ]] || {
  echo 'provide an existing delegated cgroup v2 directory beneath /sys/fs/cgroup' >&2
  exit 2
}
[[ $evidence_directory == /* && ! -e $evidence_directory && ! -L $evidence_directory ]] || {
  echo 'evidence directory must be a new absolute path' >&2
  exit 2
}
sudo -n true
if [[ $(sudo sha256sum "$kernel" | cut -d ' ' -f 1) != "$kernel_sha256" ]]; then
  echo 'reviewed kernel digest does not match' >&2
  exit 1
fi
for controller in cpu memory pids; do
  if [[ " $(<"$cgroup_parent/cgroup.subtree_control") " != *" $controller "* ]]; then
    echo "enable the $controller controller on the delegated parent before qualification" >&2
    exit 1
  fi
done
kvm_gid=$(stat -c %g /dev/kvm)
[[ $kvm_gid -gt 0 ]] || { echo 'KVM must use a dedicated non-root group' >&2; exit 1; }
for command in go docker jq sha256sum; do
  command -v "$command" >/dev/null
done

mkdir -m 0700 "$evidence_directory"
asset_parent=$(sudo mktemp -d /var/lib/layercache-native-kvm-qa.XXXXXXXX)
sudo chmod 0755 "$asset_parent"
run_cgroup=''
cleanup() {
  local status=$?
  if [[ -n $run_cgroup ]] && ! sudo rmdir -- "$run_cgroup"; then
    echo "QA cgroup is still populated or has descendants; preserving assets at $asset_parent" >&2
    exit 1
  fi
  sudo rm -rf -- "$asset_parent"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
run_cgroup=$(sudo mktemp -d "$cgroup_parent/layercache-qa.XXXXXXXX")
sudo sh -c 'printf "%s\n" "+cpu +memory +pids" > "$1/cgroup.subtree_control"' sh "$run_cgroup"

cd "$repo_root"
{
  uname -a
  go version
  git rev-parse HEAD
  git status --porcelain
} >"$evidence_directory/source-state.txt"
# Include new source files, not only the committed tree, in the evidence.
find internal/publicbuild qa/public-build-kvm -type f \( -name '*.go' -o -name '*.json' -o -name '*.sh' \) -print0 |
  LC_ALL=C sort -z | xargs -0 sha256sum >"$evidence_directory/source-files.sha256"
sha256sum go.mod go.sum >>"$evidence_directory/source-files.sha256"

for integration in actions turbo buildkit; do
  asset_root="$asset_parent/$integration"
  bash "$script_root/build-$integration-rootfs.sh" "$architecture" "$asset_root" "$kernel" \
    >"$evidence_directory/$integration-assembly.log" 2>&1
  cp "$asset_root/contract.json" "$evidence_directory/$integration-contract.json"
  cp "$asset_root/guest-files.sha256" "$evidence_directory/$integration-guest-files.sha256"
  cp "$asset_root/build-environment.txt" "$evidence_directory/$integration-build-environment.txt"
  CGO_ENABLED=0 GOOS=linux GOARCH="$architecture" go build -trimpath -buildvcs=false \
    -o "$evidence_directory/$integration-qa" "./qa/public-build-kvm/$integration"
  arguments=(--asset-root "$asset_root" --cgroup-root "$run_cgroup" --sandbox-gid "$kvm_gid")
  repetitions=2
  if [[ $integration == buildkit ]]; then
    arguments+=(--repository "$registry_repository")
    # OCI cache metadata includes per-execution timestamps. Verify every graph
    # descriptor and real RUN payload, without asserting byte determinism.
    repetitions=1
  fi
  for ((run = 1; run <= repetitions; run++)); do
    # Evidence is deliberately written by the caller in its private directory.
    # shellcheck disable=SC2024
    sudo "$evidence_directory/$integration-qa" "${arguments[@]}" \
      >"$evidence_directory/$integration-$run.json" \
      2>"$evidence_directory/$integration-$run.stderr"
    jq -e --arg platform "linux/$architecture" --arg kernel "sha256:$kernel_sha256" '
      .preflight.platform == $platform and
      .preflight.kernelSha256 == $kernel and
      .preflight.network == "none" and
      .preflight.dependencyMode == "offline-only; dependencies must be vendored in source or pinned in the immutable image" and
      (.preflight.builderImageDigest | test("^sha256:[0-9a-f]{64}$")) and
      .outputDigestVerified == true and .nativeKeyVerified == true
    ' "$evidence_directory/$integration-$run.json" >/dev/null
  done
  if [[ $integration == buildkit ]]; then
    jq -e '.runOutputVerified == true and .layers > 0' "$evidence_directory/buildkit-1.json" >/dev/null
  else
    jq -s -e '(.[0].collected.NativeKey | type) == "string" and
      (.[0].collected.Digest | test("^sha256:[0-9a-f]{64}$")) and
      .[0].collected.NativeKey == .[1].collected.NativeKey and
      .[0].collected.Digest == .[1].collected.Digest and
      .[0].archiveMembers == .[1].archiveMembers' \
      "$evidence_directory/$integration-1.json" "$evidence_directory/$integration-2.json" >/dev/null
  fi
done
jq -e '.realTurboRestoreVerified == true' "$evidence_directory/turbo-1.json" >/dev/null
jq -e '.realTurboRestoreVerified == true' "$evidence_directory/turbo-2.json" >/dev/null
printf 'Native linux/%s Actions, Turbo, and BuildKit KVM qualification passed. Evidence: %s\n' "$architecture" "$evidence_directory"
