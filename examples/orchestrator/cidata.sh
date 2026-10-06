#!/usr/bin/env bash
# The NoCloud volume - an ISO 9660 image labelled cidata, attached to the
# node's virtual machine as a CD-ROM (an ISO image read as a disk isn't
# found: its sectors aren't a CD's).
#
#   user-data.sh ... | cidata.sh OUT.iso
set -euo pipefail
out=$1
seed=$(mktemp -d)
trap 'rm -rf "$seed" "$seed.log"' EXIT
cat > "$seed/user-data"
: > "$seed/meta-data"
if ! xorriso -as mkisofs -V cidata -J -r -o "$out" "$seed" 2>"$seed.log"; then
  cat "$seed.log" >&2
  exit 1
fi
