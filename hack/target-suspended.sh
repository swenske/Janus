#!/bin/bash
# target-suspended.sh TARGET - exits 0 when variants.mk's SUSPENDED_TARGETS
# lists the image target TARGET (never built, tested nor published
# automatically), 1 when it doesn't, 2 for a name it doesn't know.
# target-suspended.sh alone prints every target as NAME=suspended|active,
# NAME with "_" for "-" - for $GITHUB_OUTPUT (image-build.yml).
#
# The Go side is internal/variants (Targets, SuspendedTargets): its test
# keeps `known` below equal to Targets.
set -euo pipefail
known="vmware baremetal raspberry-pi"
suspended=" $(sed -n 's/^SUSPENDED_TARGETS *:= *//p' "$(dirname "$0")/../variants.mk") "
for t in $suspended; do
  case " $known " in *" $t "*) ;; *) echo "variants.mk: SUSPENDED_TARGETS: \"$t\" isn't an image target ($known)" >&2; exit 2 ;; esac
done
if [ $# -eq 0 ]; then
  for t in $known; do
    case "$suspended" in *" $t "*) s=suspended ;; *) s=active ;; esac
    echo "${t//-/_}=$s"
  done
  exit 0
fi
case " $known " in *" $1 "*) ;; *) echo "$1 isn't an image target ($known)" >&2; exit 2 ;; esac
case "$suspended" in *" $1 "*) exit 0 ;; *) exit 1 ;; esac
