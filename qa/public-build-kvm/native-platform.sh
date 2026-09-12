#!/usr/bin/env bash
# Shared native-only qualification inputs. Versions and digests are reviewed
# together; downloads never resolve a mutable tool version at execution time.
# These variables are outputs read by the sourcing rootfs scripts.
# shellcheck disable=SC2034

require_native_platform() {
  local architecture=$1
  local expected_machine
  case "$architecture" in
    amd64)
      expected_machine=x86_64
      library_triplet=x86_64-linux-gnu
      dynamic_loader=lib64/ld-linux-x86-64.so.2
      node_architecture=x64
      node_archive_sha256=e798599612f4bb71333a3397ab0d095fd62214e115aea45aa858a145fc72d67e
      turbo_package=linux-64
      turbo_archive_sha512=96beca228b6e92f8d9c045e215260078e1f7056ce315505b4b34dbbf47ee185b0dba46321f4fa0d6107979caa7264824614f8a1c4306d5e74e8678e228e375c1
      buildkit_manifest=sha256:040d34121c27906c4ff9ac152a30d52bf2c5d328d3bb748916bb3d2743c02528
      ;;
    arm64)
      expected_machine=aarch64
      library_triplet=aarch64-linux-gnu
      dynamic_loader=lib/ld-linux-aarch64.so.1
      node_architecture=arm64
      node_archive_sha256=aa881151bd0f9f154a0424dd60a72e9ce10672619121658c278a24327ef46831
      turbo_package=linux-arm64
      turbo_archive_sha512=7f4a590d3b6fcc1e52b8dc2e5c168a6d91d408c0ae92073c9cc94712ebcb9a3f7517379cf8c11baf7700bc6368af986d103a999cee8bc6df4dfc63c8ffbee818
      buildkit_manifest=sha256:5a8cd84cb3fcfd082789a08f92bd36f8e745c6231edd78e24a3bf34fd471a823
      ;;
    *)
      echo "unsupported native Public Build architecture: $architecture" >&2
      return 2
      ;;
  esac
  if [[ $(uname -s) != Linux || $(uname -m) != "$expected_machine" ]]; then
    echo "native linux/$architecture qualification requires a Linux $expected_machine host; cross-builds and emulation do not qualify" >&2
    return 2
  fi
  local program_headers
  if [[ ! -x /usr/bin/busybox ]] || ! program_headers=$(readelf -l /usr/bin/busybox) || [[ $program_headers == *INTERP* ]]; then
    echo "install the native busybox-static package before assembling a guest" >&2
    return 2
  fi
}

record_rootfs_inputs() {
  local stage=$1
  local asset_root=$2
  # This captures the distro binaries actually copied into the guest. It is an
  # input inventory, not a claim that host packages or ext4 metadata are hermetic.
  (
    cd "$stage" || exit
    find . -type f -print0 | LC_ALL=C sort -z | xargs -0 sha256sum
  ) | sudo tee "$asset_root/guest-files.sha256" >/dev/null
  {
    uname -sm
    go version
    /sbin/mke2fs -V 2>&1
    dpkg-query -W -f='${Package} ${Version} ${Architecture}\n' \
      libc6 libstdc++6 libgcc-s1 busybox-static e2fsprogs bash dash libtinfo6
  } | sudo tee "$asset_root/build-environment.txt" >/dev/null
  sudo chmod 0444 "$asset_root/guest-files.sha256" "$asset_root/build-environment.txt"
}
