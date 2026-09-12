#!/bin/sh
set -eu
# Fail closed if Docker started before the bounded filesystem was mounted.
test "$(cat /pool/.layercache-pool)" = layercache-pool-v1
df -Pk /pool | {
  IFS= read -r _header
  read -r _device size _rest
  case "$size" in ''|*[!0-9]*) exit 1 ;; esac
  test "$size" -gt 0
  test "$size" -le 50331648
}
exec "$@"
