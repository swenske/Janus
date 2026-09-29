#!/usr/bin/env bash
# Builds a FAT32 EFI System Partition (ESP) image containing the given
# Unified Kernel Image at the UEFI-spec-mandated removable-media fallback
# path (\EFI\BOOT\BOOTX64.EFI) - the one firmware auto-boots with no
# NVRAM boot entry configured at all, which is all this project needs
# (no boot menu, matching the "no shell, no unnecessary components"
# design). Built with mtools (mformat/mmd/mcopy) directly against the
# image file - no mount, no loop device, same reasoning as
# rootfs/state-image.sh and image/disk/assemble.sh (loop devices aren't
# available on janus-runner01).
#
# Usage: image/uki/esp-image.sh <out-file> <uki.efi> <size-mb> [boot-filename]
# [boot-filename] (default BOOTX64.EFI, the x86_64 UEFI-spec fallback
# name): the aarch64 equivalent is BOOTAA64.EFI - a genuinely different
# fixed name per architecture, not a build option (UEFI spec section
# 3.5.1.1's own "default boot behavior" table) - firmware looks for its
# own architecture's exact name and nothing else, so this must be
# overridden for every arm64 caller, never left at the x86 default.
set -euo pipefail

# mkfs.vfat (dosfstools) installs to /usr/sbin, same PATH gap already
# hit for veritysetup/mkfs.ext4/debugfs/sgdisk.
export PATH="$PATH:/usr/sbin:/sbin"

OUT="${1:?usage: $0 <out-file> <uki.efi> <size-mb> [boot-filename]}"
UKI="${2:?usage: $0 <out-file> <uki.efi> <size-mb> [boot-filename]}"
SIZE_MB="${3:?usage: $0 <out-file> <uki.efi> <size-mb> [boot-filename]}"
BOOT_FILENAME="${4:-BOOTX64.EFI}"

truncate -s "${SIZE_MB}M" "$OUT"
mkfs.vfat -F 32 "$OUT" >/dev/null
mmd -i "$OUT" ::/EFI
mmd -i "$OUT" ::/EFI/BOOT
mcopy -i "$OUT" "$UKI" "::/EFI/BOOT/$BOOT_FILENAME"

echo "Wrote $OUT (${SIZE_MB}MiB FAT32 ESP, \\EFI\\BOOT\\$BOOT_FILENAME = $UKI)"
