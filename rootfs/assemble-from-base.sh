#!/usr/bin/env bash
# Builds a rootfs from a release's base tree (rootfs-base-<arch>.tar, see
# rootfs/assemble.sh's JANUS_EXPORT_BASE) plus the extensions of a
# schematic - the same binaries as the release, nothing rebuilt. Used by
# the schematic build (image/schematic/build.sh).
#
# Usage: rootfs/assemble-from-base.sh <out-dir> <rootfs-base.tar>
#   JANUS_EXTENSIONS  space-separated extension tars (hack/extpack)
set -euo pipefail

OUT_DIR="${1:?usage: $0 <out-dir> <rootfs-base.tar>}"
BASE="${2:?usage: $0 <out-dir> <rootfs-base.tar>}"

TREE="$(mktemp -d)"
trap 'rm -rf "$TREE"' EXIT
tar -xf "$BASE" -C "$TREE" --no-same-owner
[ -x "$TREE/sbin/init" ] && [ -x "$TREE/sbin/janusd" ] || { echo "$BASE isn't a Janus base tree" >&2; exit 1; }

"$(dirname "$0")/layer-and-squash.sh" "$TREE" "$OUT_DIR"
