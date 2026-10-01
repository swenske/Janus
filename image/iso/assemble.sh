#!/usr/bin/env bash
# Assembles a bootable, UEFI-only installer/maintenance-mode ISO -
# hybrid, in the same sense every modern Linux live ISO is: the exact
# same .iso file is also a valid GPT disk, so it can be booted either
# via a real optical/El Torito path (BIOS/UEFI CD-ROM boot) or by
# being dd'd/attached directly to a USB stick or virtio-blk drive,
# addressed via its own GPT partitions (no ISO9660/El Torito
# understanding needed for that second path at all - dm-verity just
# reads raw sectors by LBA, the same as image/disk/assemble.sh's own
# disk). This is the "docs/companion-site-builder-scope.md" tranche 2
# artifact (Bare-metal Machine).
#
# Deliberately EPHEMERAL, not the A/B + STATE shape image/disk/
# assemble.sh produces: this is a throwaway live/maintenance-mode
# environment (matching Talos's own "maintenance mode" ISO concept),
# not a final installed system - no persistent CA/config across boots.
# It boots straight to a running janusd (mTLS, prints its bootstrap
# creds to the console exactly once, same as every other first boot in
# this project) which an operator then drives remotely (e.g.
# `janusctl lifecycle install <target-disk> <bundle-dir>`) to actually
# install Janus onto a real, separate target disk.
#
# [release-bundle-dir], if given (image/release/assemble.sh's own
# output - rootfs.squashfs/rootfs.squashfs.sha256/rootfs.verity/
# uki-a.efi/uki-b.efi), gets embedded directly into the ISO9660
# partition itself (GPT partition 1 - see the layout note below) -
# rootfs/init's mountReleaseBundle mounts it read-only at
# /etc/janus/release at boot, so an operator can call `janusctl
# lifecycle install <target-disk> /etc/janus/release` using only what's
# already on this medium, no network share or second attached volume
# needed. Omitted, this partition just carries a placeholder README -
# still a valid maintenance-mode environment, just without a local
# bundle to install from.
#
# The GPT layout below was arrived at empirically, not from a single
# canonical xorriso recipe - collated from `man xorriso`'s own
# -append_partition/-boot_image efi_path=--interval:... documentation
# (the "any" bootspec's `efi_path=--interval:appended_partition_N:...`
# form referencing an *appended* partition directly, rather than a
# file inside the ISO9660 tree, is what lets this be addressed as a
# real GPT ESP with no `/EFI/BOOT` needed in the ISO9660 filesystem at
# all - xorriso's own warning about that missing directory is
# harmless, not an error). Partition numbers are NOT what get passed
# on the command line (xorriso reserves 1 for the ISO9660 filesystem
# itself) - verified with `sgdisk -p` against a real build: 1=ISO9660
# (unused), 2=ESP, 3=DATA (rootfs.squashfs), 4=HASH (rootfs.verity).
# Booted for real under OVMF as a raw virtio-blk drive (the realistic
# "dd'd to a USB stick" case) - see hack/qemu-iso-boot-test.sh - real
# dm-verity assembly, PKI bootstrap, HTTP 200.
#
# Usage: image/iso/assemble.sh <out.iso> <bzImage> <rootfs-dir> [release-bundle-dir]
# <rootfs-dir> must contain rootfs.squashfs, rootfs.verity and
# rootfs.verity.info/rootfs.roothash (see rootfs/assemble.sh).
#
# Requires ukify (systemd-ukify), mtools/dosfstools (image/uki/), and
# xorriso.
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

OUT="${1:?usage: $0 <out.iso> <bzImage> <rootfs-dir> [release-bundle-dir]}"
KERNEL="${2:?usage: $0 <out.iso> <bzImage> <rootfs-dir> [release-bundle-dir]}"
ROOTFS_DIR="${3:?usage: $0 <out.iso> <bzImage> <rootfs-dir> [release-bundle-dir]}"
BUNDLE_DIR="${4:-}"

SQUASHFS="$ROOTFS_DIR/rootfs.squashfs"
VERITY="$ROOTFS_DIR/rootfs.verity"
SELF_DIR="$(cd "$(dirname "$0")" && pwd)"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

# The medium's root is named by GPT label, like an installed disk's - but
# with labels of its own: while it installs a disk, both are attached,
# and the medium must find its own partitions, not the new disk's
# BOOT-A-*. xorriso can't name the partitions it appends (it calls them
# "AppendedN"), so they're renamed below, once the image is written.
# DATA is partition 3, HASH 4 (see this script's own header comment).
ISO_DATA_LABEL=JANUS-ISO-DATA
ISO_HASH_LABEL=JANUS-ISO-HASH
"$SELF_DIR/../uki/assemble.sh" "$WORKDIR/uki.efi" "$KERNEL" "$ROOTFS_DIR" "PARTLABEL=$ISO_DATA_LABEL" "PARTLABEL=$ISO_HASH_LABEL"
"$SELF_DIR/../uki/esp-image.sh" "$WORKDIR/efi.img" "$WORKDIR/uki.efi" 64

# xorriso needs a non-empty source tree to -map even though nothing in
# it is actually booted from (see header comment) - the volume ID
# alone would leave it empty.
mkdir -p "$WORKDIR/iso-root"
if [ -n "$BUNDLE_DIR" ]; then
  cp "$BUNDLE_DIR"/rootfs.squashfs "$BUNDLE_DIR"/rootfs.squashfs.sha256 \
     "$BUNDLE_DIR"/rootfs.verity "$BUNDLE_DIR"/uki-a.efi "$BUNDLE_DIR"/uki-b.efi \
     "$WORKDIR/iso-root/"
else
  echo "Janus installer/maintenance-mode medium - see docs/provisioning-a-node.md" \
    > "$WORKDIR/iso-root/README.txt"
fi

mkdir -p "$(dirname "$OUT")"
rm -f "$OUT"
xorriso \
  -outdev "$OUT" \
  -blank as_needed \
  -map "$WORKDIR/iso-root" / \
  -volid JANUS \
  -append_partition 2 0xef "$WORKDIR/efi.img" \
  -append_partition 3 0x83 "$SQUASHFS" \
  -append_partition 4 0x83 "$VERITY" \
  -boot_image any partition_offset=16 \
  -boot_image any appended_part_as=gpt \
  -boot_image any efi_path=--interval:appended_partition_2:all:: \
  -boot_image any cat_path=/boot.catalog \
  -commit

# Name the root partitions. sgdisk rewrites the GPT (primary and backup)
# and widens the protective MBR entry to the whole image - the standard
# value; the ISO9660 tree and El Torito boot record aren't touched.
sgdisk -c "3:$ISO_DATA_LABEL" -c "4:$ISO_HASH_LABEL" "$OUT" >/dev/null 2>&1
for n in 3:$ISO_DATA_LABEL 4:$ISO_HASH_LABEL; do
  got="$(sgdisk -i "${n%%:*}" "$OUT" | awk -F"'" '/^Partition name/ {print $2}')"
  [ "$got" = "${n#*:}" ] || { echo "partition ${n%%:*} of $OUT is named '$got', want '${n#*:}'" >&2; exit 1; }
done

echo "Wrote $OUT ($(du -h "$OUT" | cut -f1) hybrid ISO/GPT - bootable via El Torito EFI or as a raw disk)"
