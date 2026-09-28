#!/usr/bin/env bash
# Wraps image/disk/assemble.sh's real, single-disk GPT image (ESP +
# both A/B slots + STATE - see that script's own header) into qcow2,
# for a generic libvirt/KVM deployment (virt-install/virt-manager) -
# as opposed to image/kvm-proxmox/assemble.sh, which targets Proxmox's
# own `qm`-driven import flow specifically. Byte-for-byte the same
# conversion (qemu-img convert -c) - the two exist as separate,
# separately-documented artifacts because the *consuming* tooling
# (virt-install vs. `qm`) and its own conventions differ, not because
# the disk image itself needs to.
#
# Usage: image/kvm/assemble.sh <out.qcow2> <bzImage> <rootfs-dir> <state-image> [active-slot]
# Same inputs as image/disk/assemble.sh - see its own header for what
# <rootfs-dir>/<state-image> need to contain.
#
# Requires everything image/disk/assemble.sh does (sgdisk, ukify,
# mtools/dosfstools), plus qemu-img.
set -euo pipefail

OUT="${1:?usage: $0 <out.qcow2> <bzImage> <rootfs-dir> <state-image> [active-slot]}"
KERNEL="${2:?usage: $0 <out.qcow2> <bzImage> <rootfs-dir> <state-image> [active-slot]}"
ROOTFS_DIR="${3:?usage: $0 <out.qcow2> <bzImage> <rootfs-dir> <state-image> [active-slot]}"
STATE_IMAGE="${4:?usage: $0 <out.qcow2> <bzImage> <rootfs-dir> <state-image> [active-slot]}"
ACTIVE_SLOT="${5:-A}"

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"

RAW="$(mktemp)"
trap 'rm -f "$RAW"' EXIT
"$SELF_DIR/../disk/assemble.sh" "$RAW" "$KERNEL" "$ROOTFS_DIR" "$STATE_IMAGE" "$ACTIVE_SLOT"

mkdir -p "$(dirname "$OUT")"
qemu-img convert -O qcow2 -c "$RAW" "$OUT"

echo "Wrote $OUT ($(du -h "$OUT" | cut -f1) qcow2, from a $(du -h "$RAW" | cut -f1) raw GPT disk)"
