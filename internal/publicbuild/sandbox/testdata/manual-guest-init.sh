#!/bin/busybox sh
set -eu

/bin/busybox mount -t proc proc /proc
/bin/busybox mount -t sysfs sysfs /sys
/bin/busybox mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
/bin/busybox mkdir -p /source /work

fail() {
	message=$1
	printf 'manual guest failure: %s\n' "$message" >/dev/console || true
	if [ -n "${control:-}" ]; then
		printf '{"type":"failed","message":"%s"}\n' "$message" >&3 || true
	fi
	/bin/busybox poweroff -f
	while :; do /bin/busybox sleep 1; done
}

/bin/busybox mount -t ext4 -o ro /dev/vdb /source || fail "source mount failed"
[ -f /source/README.txt ] || fail "immutable source file missing"
/bin/busybox touch /source/WRITE-MUST-FAIL >/dev/null 2>&1 && fail "source device is writable"
/bin/busybox mke2fs -F -m 0 /dev/vdc >/dev/null 2>&1 || fail "work format failed"
/bin/busybox mount -t ext4 /dev/vdc /work || fail "work mount failed"
printf '%s' 'writable-output-smoke' >/work/output || fail "work device is not writable"
for interface_path in /sys/class/net/*; do
	interface=${interface_path##*/}
	[ "$interface" = "lo" ] || fail "network interface present"
done

control=""
attempt=0
while [ "$attempt" -lt 200 ]; do
	for name_file in /sys/class/virtio-ports/vport*/name; do
		[ -f "$name_file" ] || continue
		if [ "$(/bin/busybox cat "$name_file")" = "org.layercache.public-build.1" ]; then
			device=${name_file%/name}
			device=${device##*/}
			control=/dev/$device
			break
		fi
	done
	[ -n "$control" ] && [ -c "$control" ] && break
	attempt=$((attempt + 1))
	/bin/busybox sleep 0.05
done
if [ -z "$control" ] || [ ! -c "$control" ]; then
	fail "control port missing"
fi

exec 3<>"$control"
printf '{"type":"hello","protocol":"layercache.public-build/v1"}\n' >&3
IFS= read -r request <&3 || fail "execute request missing"
case "$request" in
	*'"type":"execute"'*'"recipeDigest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"'*) ;;
	*) fail "execute request invalid" ;;
esac
case "$request" in
	*'secret'*|*'token'*|*'docker'*) fail "execute request contains forbidden host data" ;;
esac

printf '{"type":"log","message":"manual guest token=secret-token"}\n' >&3
printf '{"type":"output","nativeKey":"manual-qemu-output","mediaType":"application/vnd.layercache.turbo","sizeBytes":13}\n' >&3
printf '%s' 'manual-output' >&3
printf '{"type":"complete"}\n' >&3
/bin/busybox sync
/bin/busybox poweroff -f
while :; do /bin/busybox sleep 1; done
