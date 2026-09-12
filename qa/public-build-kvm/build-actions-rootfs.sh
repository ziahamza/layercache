#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 amd64|arm64 /var/lib/layercache-actions-kvm-qa /path/to/pinned/kernel" >&2
  exit 2
fi

architecture=$1
asset_root=$2
kernel=$3
case "$asset_root" in
  /var/lib/layercache-*-kvm-qa*) ;;
  *)
    echo "asset root must be a dedicated /var/lib/layercache-*-kvm-qa path" >&2
    exit 2
    ;;
esac
script_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=qa/public-build-kvm/native-platform.sh
source "$script_root/native-platform.sh"
require_native_platform "$architecture"
if [[ ! -f $kernel ]]; then
  echo "kernel is not a regular file: $kernel" >&2
  exit 2
fi
if [[ ! -c /dev/kvm ]]; then
  echo "/dev/kvm is not available" >&2
  exit 2
fi
kvm_gid=$(stat -c %g /dev/kvm)
if [[ $kvm_gid -eq 0 ]]; then
  echo "refusing to run the non-root QEMU sandbox with privileged group 0" >&2
  exit 2
fi
if sudo test -e "$asset_root"; then
  echo "refusing to replace existing asset root: $asset_root" >&2
  exit 1
fi

repo_root=$(cd -- "$script_root/../.." && pwd)
contract="$script_root/actions-$architecture-contract.json"
qa_tmp=$(mktemp -d /tmp/layercache-actions-kvm-rootfs.XXXXXX)
cleanup() {
  sudo rm -rf -- "$qa_tmp"
}
trap cleanup EXIT

stage="$qa_tmp/rootfs-stage"
mkdir -p \
  "$stage/opt/layercache/bin" \
  "$stage/etc/layercache" \
  "$stage/bin" \
  "$stage/sbin" \
  "$stage/lib/$library_triplet" \
  "$stage/$(dirname -- "$dynamic_loader")" \
  "$stage/proc" \
  "$stage/sys" \
  "$stage/dev" \
  "$stage/source" \
  "$stage/work"

(
  cd "$repo_root"
  CGO_ENABLED=0 GOOS=linux GOARCH="$architecture" go build -trimpath -buildvcs=false \
    -o "$qa_tmp/layercache-guest-agent" \
    ./internal/publicbuild/sandbox/guestagent
  CGO_ENABLED=0 GOOS=linux GOARCH="$architecture" go build -trimpath -buildvcs=false \
    -o "$qa_tmp/layercache-actions-public-job" \
    ./internal/publicbuild/sandbox/actionsjob/cmd/layercache-actions-public-job
)

install -m 0755 "$qa_tmp/layercache-guest-agent" \
  "$stage/opt/layercache/bin/layercache-guest-agent"
install -m 0755 "$qa_tmp/layercache-actions-public-job" \
  "$stage/opt/layercache/bin/layercache-actions-public-job"
install -m 0755 /bin/dash "$stage/bin/dash"
install -m 0755 /bin/bash "$stage/bin/bash"
install -m 0755 /usr/bin/busybox "$stage/bin/busybox"
install -m 0755 /usr/bin/busybox "$stage/bin/mkdir"
install -m 0755 /usr/sbin/mke2fs "$stage/sbin/mke2fs"
install -m 0755 "$(readlink -f "/$dynamic_loader")" "$stage/$dynamic_loader"
for soname in libc.so.6 libtinfo.so.6 libext2fs.so.2 libcom_err.so.2 libblkid.so.1 libuuid.so.1 libe2p.so.2; do
  install -m 0755 "$(readlink -f "/lib/$library_triplet/$soname")" \
    "$stage/lib/$library_triplet/$soname"
done
install -m 0644 /etc/mke2fs.conf "$stage/etc/mke2fs.conf"
install -m 0644 "$contract" "$stage/etc/layercache/public-build-contract.json"

truncate -s 134217728 "$qa_tmp/rootfs.raw"
/sbin/mke2fs -q -t ext4 -F -m 0 -O '^metadata_csum_seed' \
  -d "$stage" "$qa_tmp/rootfs.raw"

sudo install -d -m 0755 -o root -g root "$asset_root"
sudo install -m 0444 "$kernel" "$asset_root/vmlinuz"
sudo install -m 0444 "$contract" "$asset_root/contract.json"
sudo install -m 0444 "$qa_tmp/rootfs.raw" "$asset_root/rootfs.raw"
record_rootfs_inputs "$stage" "$asset_root"
sudo install -d -m 0710 -o root -g "$kvm_gid" "$asset_root/work"

sha256sum \
  "$asset_root/vmlinuz" \
  "$asset_root/rootfs.raw" \
  "$asset_root/contract.json" \
  "$qa_tmp/layercache-guest-agent" \
  "$qa_tmp/layercache-actions-public-job"
