#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
actionlint
while IFS= read -r -d '' script; do
  if [[ "$script" == internal/publicbuild/sandbox/testdata/manual-guest-init.sh ]]; then
    # BusyBox interprets the argument in its kernel shebang. ShellCheck needs
    # the shell selected explicitly because it sees /bin/busybox, not /bin/sh.
    sh -n "$script"
    shellcheck --shell=sh --exclude=SC2187 "$script"
  else
    bash -n "$script"
    shellcheck --external-sources "$script"
  fi
done < <(git ls-files -z --cached --others --exclude-standard -- '*.sh')
git diff --check
git diff --cached --check
# CI normally starts from a clean checkout, so also check the checked-out commit.
git show --format= --check HEAD
