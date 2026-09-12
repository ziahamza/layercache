#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 amd64|arm64 /var/lib/layercache-buildkit-kvm-qa /path/to/pinned/kernel" >&2
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
contract="$script_root/buildkit-$architecture-contract.json"
qa_tmp=$(mktemp -d /tmp/layercache-buildkit-kvm-rootfs.XXXXXX)
container=''
cleanup() {
  if [[ -n $container ]]; then
    docker rm --force "$container" >/dev/null 2>&1 || true
  fi
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
  "$stage/run" \
  "$stage/tmp" \
  "$stage/var" \
  "$stage/source" \
  "$stage/work"

container=$(docker create --platform "linux/$architecture" "moby/buildkit@$buildkit_manifest")
docker cp "$container:/usr/bin/buildkitd" "$stage/opt/layercache/bin/buildkitd"
docker cp "$container:/usr/bin/buildctl" "$stage/opt/layercache/bin/buildctl"
docker cp "$container:/usr/bin/buildkit-runc" "$stage/opt/layercache/bin/runc"
docker rm "$container" >/dev/null
container=''
chmod 0755 \
  "$stage/opt/layercache/bin/buildkitd" \
  "$stage/opt/layercache/bin/buildctl" \
  "$stage/opt/layercache/bin/runc"

install -m 0755 /bin/busybox "$stage/bin/busybox"
install -m 0755 /bin/busybox "$stage/bin/sh"
install -m 0755 /usr/sbin/mke2fs "$stage/sbin/mke2fs"
for soname in libext2fs.so.2 libcom_err.so.2 libblkid.so.1 libuuid.so.1 libe2p.so.2 libc.so.6; do
  install -m 0755 "$(readlink -f "/lib/$library_triplet/$soname")" \
    "$stage/lib/$library_triplet/$soname"
done
install -m 0755 "$(readlink -f "/$dynamic_loader")" "$stage/$dynamic_loader"
install -m 0644 /etc/mke2fs.conf "$stage/etc/mke2fs.conf"
install -m 0644 "$contract" "$stage/etc/layercache/public-build-contract.json"
printf '%s\n' 'root:x:0:0:root:/root:/bin/sh' 'nobody:x:65534:65534:nobody:/nonexistent:/bin/sh' > "$stage/etc/passwd"
printf '%s\n' 'root:x:0:' 'nogroup:x:65534:' > "$stage/etc/group"

(
  cd "$repo_root"
  CGO_ENABLED=0 GOOS=linux GOARCH="$architecture" go build -trimpath -buildvcs=false -ldflags='-s -w' \
    -o "$stage/sbin/layercache-public-build-guest" \
    ./internal/publicbuild/sandbox/guestagent
)

truncate -s 536870912 "$qa_tmp/rootfs.raw"
/sbin/mke2fs -q -t ext4 -F -m 0 -O '^has_journal' \
  -d "$stage" "$qa_tmp/rootfs.raw"
/sbin/e2fsck -fn "$qa_tmp/rootfs.raw"

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
  "$stage/sbin/layercache-public-build-guest" \
  "$stage/opt/layercache/bin/buildkitd" \
  "$stage/opt/layercache/bin/buildctl" \
  "$stage/opt/layercache/bin/runc"
