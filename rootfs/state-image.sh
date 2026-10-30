#!/usr/bin/env bash
# Creates a blank, pre-formatted ext4 image for Janus's persistent
# STATE partition (currently just /etc/janus/pki - the one thing
# that needs to survive a reboot; rootfs/init/main.go's ephemeral tmpfs
# overlay wipes everything else). Formatted here, at build time, not on
# the target - the "no package manager on the node" rule means no
# mkfs.ext4 binary ships in the image, so this has to happen before the
# node ever boots. Requires mkfs.ext4 (e2fsprogs) - doesn't need root.
#
# Usage: rootfs/state-image.sh <out-file> <size-mb>
set -euo pipefail

# Debian installs mkfs.ext4 (e2fsprogs) to /usr/sbin, same as
# veritysetup (cryptsetup-bin) - see rootfs/assemble.sh's own PATH fix
# and CLAUDE.md for why this is needed on janus-runner01's
# non-interactive shell even though the package installs cleanly.
export PATH="$PATH:/usr/sbin:/sbin"

OUT="${1:?usage: $0 <out-file> <size-mb>}"
SIZE_MB="${2:?usage: $0 <out-file> <size-mb>}"

# Its UUID, directory hash seed and times from the commit, not drawn
# when it's made: two builds of a commit, the same image (#433).
. "$(dirname "$0")/../hack/reproducible.sh"
truncate -s "${SIZE_MB}M" "$OUT"
E2FSPROGS_FAKE_TIME="$SOURCE_DATE_EPOCH" mkfs.ext4 -q -F -L janus-state \
  -U "$(uuid_of "state $SOURCE_DATE_EPOCH")" -E hash_seed="$(uuid_of "state hash seed $SOURCE_DATE_EPOCH")" "$OUT"

echo "Wrote $OUT (${SIZE_MB}MiB, blank ext4, label janus-state)"
