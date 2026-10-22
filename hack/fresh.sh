#!/bin/bash
# fresh.sh TARGET STAMP TEXT -- PATH... - exits 0 when TARGET exists, STAMP
# holds exactly TEXT (what the build read from its environment), and no
# file under the PATHs is newer than TARGET: the build that made TARGET
# would make the same bytes again, so make can skip it. Anything else -
# a missing file, a changed input, a changed environment - exits 1.
#
# Why: every `make qemu-*-test` rebuilt the rootfs and the disk image
# (PHONY targets, 6-10 s each) - thirty times in one image-build job -
# although nothing had changed since the step before.
set -euo pipefail
target=$1 stamp=$2 text=$3
shift 3
[ "${1:-}" = "--" ] && shift
[ -e "$target" ] || exit 1
[ -e "$stamp" ] && [ "$(cat "$stamp")" = "$text" ] || exit 1
for p in "$@"; do
  [ -e "$p" ] || exit 1
  if [ -n "$(find "$p" -newer "$target" -print -quit)" ]; then exit 1; fi
done
exit 0
