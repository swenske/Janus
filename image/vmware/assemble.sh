#!/usr/bin/env bash
# Wraps image/disk/assemble.sh's real, single-disk GPT image (ESP +
# both A/B slots + STATE - see that script's own header) into VMDK,
# VMware/ESXi's own native disk format. `qemu-img` supports several
# VMDK subformats - `streamOptimized` (compressed, single-file, the
# format VMware's own OVF/OVA tooling and `vmware-vdiskmanager`/
# `ovftool` expect for import) is used here rather than the default
# `monolithicSparse`, since it's the one that imports cleanly into
# both ESXi (via `govc import.vmdk`/datastore upload) and Workstation/
# Fusion without a separate conversion step first.
#
# This produces a bare VMDK, not a full OVA (which additionally needs
# an OVF XML descriptor + a tar wrapper describing CPU/memory/network
# hardware) - see this directory's own README for why that's left as a
# follow-up rather than built here.
#
# Usage: image/vmware/assemble.sh <out.vmdk> <bzImage> <rootfs-dir> <state-image> [active-slot]
# Same inputs as image/disk/assemble.sh - see its own header for what
# <rootfs-dir>/<state-image> need to contain.
#
# Requires everything image/disk/assemble.sh does (sgdisk, ukify,
# mtools/dosfstools), plus qemu-img.
set -euo pipefail

OUT="${1:?usage: $0 <out.vmdk> <bzImage> <rootfs-dir> <state-image> [active-slot]}"
KERNEL="${2:?usage: $0 <out.vmdk> <bzImage> <rootfs-dir> <state-image> [active-slot]}"
ROOTFS_DIR="${3:?usage: $0 <out.vmdk> <bzImage> <rootfs-dir> <state-image> [active-slot]}"
STATE_IMAGE="${4:?usage: $0 <out.vmdk> <bzImage> <rootfs-dir> <state-image> [active-slot]}"
ACTIVE_SLOT="${5:-A}"

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"

RAW="$(mktemp)"
trap 'rm -f "$RAW"' EXIT
"$SELF_DIR/../disk/assemble.sh" "$RAW" "$KERNEL" "$ROOTFS_DIR" "$STATE_IMAGE" "$ACTIVE_SLOT"

mkdir -p "$(dirname "$OUT")"
qemu-img convert -O vmdk -o subformat=streamOptimized "$RAW" "$OUT"

echo "Wrote $OUT ($(du -h "$OUT" | cut -f1) VMDK, from a $(du -h "$RAW" | cut -f1) raw GPT disk)"
