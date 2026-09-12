#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 amd64|arm64 /var/lib/layercache-turbo-kvm-qa /path/to/pinned/kernel" >&2
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
contract="$script_root/turbo-$architecture-contract.json"
qa_tmp=$(mktemp -d /tmp/layercache-turbo-kvm-rootfs.XXXXXX)
cleanup() {
  sudo rm -rf -- "$qa_tmp"
}
trap cleanup EXIT

node_archive="$qa_tmp/node.tar.xz"
curl --fail --location --proto '=https' --tlsv1.2 \
  --output "$node_archive" "https://nodejs.org/dist/v24.13.0/node-v24.13.0-linux-$node_architecture.tar.xz"
if [[ $(sha256sum "$node_archive" | cut -d ' ' -f 1) != "$node_archive_sha256" ]]; then
  echo "the pinned Node.js archive digest does not match" >&2
  exit 1
fi
tar -xJf "$node_archive" -C "$qa_tmp" --no-same-owner
node_prefix="$qa_tmp/node-v24.13.0-linux-$node_architecture"
node_bin="$node_prefix/bin/node"
npm_root="$node_prefix/lib/node_modules/npm"
if [[ ! -x $node_bin || ! -f $npm_root/bin/npm-cli.js ]]; then
  echo "the pinned Node.js installation is incomplete" >&2
  exit 1
fi
if [[ $($node_bin --version) != v24.13.0 ]]; then
  echo "the Turbo rootfs requires Node.js v24.13.0" >&2
  exit 1
fi
if [[ $($node_bin "$npm_root/bin/npm-cli.js" --version) != 11.6.2 ]]; then
  echo "the Turbo rootfs requires npm 11.6.2" >&2
  exit 1
fi
turbo_archive="$qa_tmp/turbo.tgz"
curl --fail --location --proto '=https' --tlsv1.2 \
  --output "$turbo_archive" "https://registry.npmjs.org/@turbo/$turbo_package/-/$turbo_package-2.10.12.tgz"
if [[ $(sha512sum "$turbo_archive" | cut -d ' ' -f 1) != "$turbo_archive_sha512" ]]; then
  echo "the Turbo package integrity does not match" >&2
  exit 1
fi
mkdir -p "$qa_tmp/turbo-package"
tar -xzf "$turbo_archive" -C "$qa_tmp/turbo-package" --no-same-owner
turbo_bin="$qa_tmp/turbo-package/turbo-linux-$node_architecture/bin/turbo"
if [[ ! -x $turbo_bin ]]; then
  echo "the verified Turbo archive does not contain the expected executable" >&2
  exit 1
fi

stage="$qa_tmp/rootfs-stage"
mkdir -p \
  "$stage/opt/layercache/bin" \
  "$stage/etc/layercache" \
  "$stage/bin" \
  "$stage/sbin" \
  "$stage/usr/bin" \
  "$stage/usr/lib/node_modules" \
  "$stage/lib/$library_triplet" \
  "$stage/$(dirname -- "$dynamic_loader")" \
  "$stage/proc" \
  "$stage/sys" \
  "$stage/dev" \
  "$stage/root" \
  "$stage/tmp" \
  "$stage/source" \
  "$stage/work"

(
  cd "$repo_root"
  CGO_ENABLED=0 GOOS=linux GOARCH="$architecture" go build -trimpath -buildvcs=false \
    -o "$stage/sbin/layercache-public-build-guest" \
    ./internal/publicbuild/sandbox/guestagent
)

install -m 0755 "$turbo_bin" "$stage/opt/layercache/bin/turbo"
install -m 0755 "$node_bin" "$stage/usr/bin/node"
cp -a "$npm_root" "$stage/usr/lib/node_modules/npm"
chmod -R u=rwX,go=rX "$stage/usr/lib/node_modules/npm"
printf '%s\n' \
  '#!/bin/sh' \
  'exec /usr/bin/node /usr/lib/node_modules/npm/bin/npm-cli.js "$@"' \
  >"$stage/usr/bin/npm"
chmod 0755 "$stage/usr/bin/npm"
install -m 0755 /usr/bin/busybox "$stage/bin/busybox"
ln -s busybox "$stage/bin/sh"
install -m 0755 /usr/sbin/mke2fs "$stage/sbin/mke2fs"
for soname in \
  libext2fs.so.2 libcom_err.so.2 libblkid.so.1 libuuid.so.1 libe2p.so.2 \
  libdl.so.2 libstdc++.so.6 libm.so.6 libgcc_s.so.1 libpthread.so.0 libc.so.6; do
  install -m 0755 "$(readlink -f "/lib/$library_triplet/$soname")" \
    "$stage/lib/$library_triplet/$soname"
done
install -m 0755 "$(readlink -f "/$dynamic_loader")" "$stage/$dynamic_loader"
install -m 0644 /etc/mke2fs.conf "$stage/etc/mke2fs.conf"
install -m 0644 "$contract" "$stage/etc/layercache/public-build-contract.json"
printf '%s\n' \
  'root:x:0:0:root:/root:/bin/sh' \
  'nobody:x:65534:65534:nobody:/nonexistent:/bin/sh' \
  >"$stage/etc/passwd"
printf '%s\n' 'root:x:0:' 'nogroup:x:65534:' >"$stage/etc/group"
sudo chown -R root:root "$stage"

truncate -s 805306368 "$qa_tmp/rootfs.raw"
/sbin/mke2fs -q -t ext4 -F -m 0 -O '^has_journal' \
  -d "$stage" "$qa_tmp/rootfs.raw"
/sbin/e2fsck -fn "$qa_tmp/rootfs.raw"

sudo install -d -m 0755 -o root -g root "$asset_root"
sudo install -m 0444 "$kernel" "$asset_root/vmlinuz"
sudo install -m 0444 "$contract" "$asset_root/contract.json"
sudo install -m 0444 "$qa_tmp/rootfs.raw" "$asset_root/rootfs.raw"
record_rootfs_inputs "$stage" "$asset_root"
sudo install -m 0555 "$turbo_bin" "$asset_root/turbo"
sudo install -d -m 0710 -o root -g "$kvm_gid" "$asset_root/work"

sha256sum \
  "$asset_root/vmlinuz" \
  "$asset_root/rootfs.raw" \
  "$asset_root/contract.json" \
  "$asset_root/turbo" \
  "$stage/sbin/layercache-public-build-guest" \
  "$stage/usr/bin/node"
