#!/usr/bin/env bash
# Real Raspberry Pi 4/5 hardware follow-up (2026-09-29, user-requested
# real-hardware image). Assembles a single, real, SD-card-flashable
# disk image for a genuine Pi4/Pi5 board - the same six-partition GPT
# shape image/disk/assemble.sh already builds for QEMU/Proxmox/KVM/
# VMware (ESP-equivalent, BOOT-A-DATA/HASH, BOOT-B-DATA/HASH, STATE),
# except partition 1 is a combined "Pi native firmware + ESP" partition
# instead of a pure UEFI-only ESP.
#
# Why one combined partition, not a separate small firmware card plus a
# normal ESP: Raspberry Pi 4/5's own boot ROM/EEPROM bootloader is not
# a UEFI implementation at all - it can only read a plain FAT16/32
# filesystem on the *first* partition of whatever it boots from, where
# it expects to find its own bootcode/GPU firmware (start4.elf,
# fixup4.dat, dtbs) plus a `config.txt` telling it to load an
# `armstub` - which is where pftf/RPi4 (Pi4) or NumberOneGit/rpi5-uefi
# (Pi5)'s own RPI_EFI.fd comes in: that's the actual UEFI firmware,
# loaded as the ARM CPU's boot stub by the SoC's own GPU-side
# bootloader, not by anything UEFI-aware. Confirmed empirically (not
# assumed) that Pi4's boot ROM supports GPT with a FAT32 partition 1
# fine, as long as it's genuinely first - a real limitation on Pi3, not
# Pi4/5, per the Raspberry Pi forums' own maintainer replies (see
# CLAUDE.md's own citation trail for the exact threads). Once RPI_EFI.fd
# takes over, it does its own, completely standard UEFI boot-device
# enumeration - which is what then finds \EFI\BOOT\BOOTAA64.EFI (this
# project's own UKI) on that *same* FAT32 partition, exactly the
# UEFI-spec removable-media fallback path image/uki/esp-image.sh's own
# BOOTX64.EFI convention already relies on for x86 - so the one
# partition serves both roles at once, no extra partition needed.
#
# Firmware maturity gap (Pi4 vs Pi5), and why both are still attempted:
# pftf/RPi4 is a well-established, actively-maintained project (the
# official EDK2 Raspberry Pi 4 platform's own CI-built releases,
# confirmed directly against real, current mainline Linux kernel source
# - drivers/mmc/host/sdhci-iproc.c's own ACPI id table matches BCM2847/
# BRCME88C, drivers/net/ethernet/broadcom/genet/bcmgenet.c's own matches
# BCM6E4E - see kernel/configs/janus_rpi4_defconfig's own bullet for the
# full citation) - real confidence this should work. Raspberry Pi 5 has
# no equivalent: no pftf/RPi5 project exists at all, and the best
# available community alternative (NumberOneGit/rpi5-uefi, see
# versions.mk's own comment) is a single early prerelease from a much
# smaller, less-proven effort. Worse, this project's own pinned 6.18.53
# kernel's MAINTAINERS file confirms Pi5's RP1 I/O companion chip has
# no mainline MMC or Ethernet driver *at all* yet (only clock/misc/
# pinctrl) - meaning even a flawless Pi5 UEFI firmware can't get this
# appliance to a working, SD-card-root, networked state today,
# regardless of anything this script does. The Pi5 image is still built
# and worth the user's own hardware test - proving (or disproving)
# genuine UEFI/kernel boot on that specific board is real information
# this project has no other way to get - but it is NOT expected to
# reach a working HAProxy appliance state the way the Pi4 image is.
#
# Usage: image/rpi-uefi/assemble.sh <out-file> <firmware-dir> <Image> <rootfs-dir> <state-image> [active-slot]
# <firmware-dir> is fetch-firmware.sh's own output (the extracted
# RPI_EFI.fd/config.txt/start*.elf/fixup*.dat/*.dtb/overlays/ tree -
# copied onto the SD card's firmware partition wholesale, unmodified).
# <Image> is the arm64 kernel (rpi4-kernel-build's own output - the
# same Image binary for both boards; BCM2711 vs BCM2712 hardware
# differences are firmware/ACPI-table concerns, not something this
# from-scratch kernel needs a separate build for, though Pi5's own
# missing RP1 drivers mean it won't reach a *working* boot regardless -
# see this file's own header above). <rootfs-dir>/<state-image> as
# image/disk/assemble.sh's own args. [active-slot] is "A" or "B",
# defaulting to "A".
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

OUT="${1:?usage: $0 <out-file> <firmware-dir> <Image> <rootfs-dir> <state-image> [active-slot]}"
FIRMWARE_DIR="${2:?usage: $0 <out-file> <firmware-dir> <Image> <rootfs-dir> <state-image> [active-slot]}"
KERNEL="${3:?usage: $0 <out-file> <firmware-dir> <Image> <rootfs-dir> <state-image> [active-slot]}"
ROOTFS_DIR="${4:?usage: $0 <out-file> <firmware-dir> <Image> <rootfs-dir> <state-image> [active-slot]}"
STATE_IMAGE="${5:?usage: $0 <out-file> <firmware-dir> <Image> <rootfs-dir> <state-image> [active-slot]}"
ACTIVE_SLOT="${6:-A}"

case "$ACTIVE_SLOT" in
  A|B) ;;
  *) echo "invalid slot '$ACTIVE_SLOT' - must be A or B" >&2; exit 1 ;;
esac

SQUASHFS="$ROOTFS_DIR/rootfs.squashfs"
VERITY="$ROOTFS_DIR/rootfs.verity"
SELF_DIR="$(cd "$(dirname "$0")" && pwd)"

# The UKIs name their partitions by GPT label (PARTLABEL=BOOT-A-DATA...),
# not /dev/mmcblk0pN: the same image boots from an SD card or a USB
# drive. BOOT_FILENAME is the aarch64 UEFI-spec fallback name - see
# image/uki/esp-image.sh's own doc comment.
BOOT_FILENAME="BOOTAA64.EFI"

# 128MiB for the firmware+ESP partition - pftf/RPi4's own release zip
# is ~7MiB uncompressed, NumberOneGit/rpi5-uefi's ~2.6MiB, this
# project's own UKI ~9-10MiB - comfortable headroom on a real SD card
# without being wasteful. DATA_MB/HASH_MB/STATE_MB match image/disk/
# assemble.sh's own sizes - this project's arm64 rootfs is smaller than
# amd64's (~10MiB squashfs vs the 64MiB budget), same headroom
# reasoning as that file's own comment.
FIRMWARE_MB=128
DATA_MB=64
HASH_MB=4
STATE_MB=128

size_of() { stat -c%s "$1"; }
[ "$(size_of "$SQUASHFS")" -le $((DATA_MB * 1024 * 1024)) ] || { echo "rootfs.squashfs exceeds the ${DATA_MB}MiB BOOT-*-DATA partition size" >&2; exit 1; }
[ "$(size_of "$VERITY")" -le $((HASH_MB * 1024 * 1024)) ] || { echo "rootfs.verity exceeds the ${HASH_MB}MiB BOOT-*-HASH partition size" >&2; exit 1; }
[ "$(size_of "$STATE_IMAGE")" -le $((STATE_MB * 1024 * 1024)) ] || { echo "state.img exceeds the ${STATE_MB}MiB STATE partition size" >&2; exit 1; }

TOTAL_MB=$(( FIRMWARE_MB + DATA_MB + HASH_MB + DATA_MB + HASH_MB + STATE_MB + 2 ))
truncate -s "${TOTAL_MB}M" "$OUT"

sgdisk --zap-all "$OUT" >/dev/null
sgdisk \
  -n 1:0:+"${FIRMWARE_MB}"M -t 1:ef00 -c 1:FIRMWARE \
  -n 2:0:+"${DATA_MB}"M     -t 2:8300 -c 2:BOOT-A-DATA \
  -n 3:0:+"${HASH_MB}"M     -t 3:8300 -c 3:BOOT-A-HASH \
  -n 4:0:+"${DATA_MB}"M     -t 4:8300 -c 4:BOOT-B-DATA \
  -n 5:0:+"${HASH_MB}"M     -t 5:8300 -c 5:BOOT-B-HASH \
  -n 6:0:0                  -t 6:8300 -c 6:STATE \
  "$OUT" >/dev/null

write_part() {
  local part="$1" src="$2" start_sector
  start_sector="$(sgdisk -i "$part" "$OUT" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
  dd if="$src" of="$OUT" bs=512 seek="$start_sector" conv=notrunc status=none
}

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

# Both slots' UKIs, same reasoning image/disk/activate-slot.sh's own
# header already gives: the running node can never build one itself
# (ukify/sbsign don't exist there), so both are pre-staged now.
UKI_A="$WORKDIR/uki-a.efi"
UKI_B="$WORKDIR/uki-b.efi"
UKIFY_STUB="${UKIFY_STUB:?UKIFY_STUB must be set to the fetched aarch64 sd-stub path - see systemd-stub/Dockerfile}" \
UKI_CONSOLE=ttyAMA0 \
UKI_SELINUX_ENFORCING=0 \
  "$SELF_DIR/../uki/assemble.sh" "$UKI_A" "$KERNEL" "$ROOTFS_DIR" PARTLABEL=BOOT-A-DATA PARTLABEL=BOOT-A-HASH
UKIFY_STUB="${UKIFY_STUB}" \
UKI_CONSOLE=ttyAMA0 \
UKI_SELINUX_ENFORCING=0 \
  "$SELF_DIR/../uki/assemble.sh" "$UKI_B" "$KERNEL" "$ROOTFS_DIR" PARTLABEL=BOOT-B-DATA PARTLABEL=BOOT-B-HASH

ACTIVE_UKI="$UKI_A"
[ "$ACTIVE_SLOT" = "B" ] && ACTIVE_UKI="$UKI_B"

# The firmware+ESP partition: the Pi's own native boot files (as-is,
# unmodified - config.txt's armstub=RPI_EFI.fd line already does
# everything needed, see this file's own header) plus this project's
# UKI at the UEFI removable-media fallback path, plus both slots'
# staged UKIs under \JANUS\ for a future LifecycleService.Rollback -
# same shape image/disk/activate-slot.sh already builds for x86, just
# with the extra firmware files layered in and built here directly
# rather than reusing that script (its own signature is narrowly about
# the UKI/JANUS staging alone, not a general "plus arbitrary extra
# files" mechanism).
FW_IMG="$WORKDIR/firmware.img"
truncate -s "${FIRMWARE_MB}M" "$FW_IMG"
mkfs.vfat -F 32 "$FW_IMG" >/dev/null
mcopy -s -i "$FW_IMG" "$FIRMWARE_DIR"/* ::
mmd -i "$FW_IMG" ::/EFI
mmd -i "$FW_IMG" ::/EFI/BOOT
mmd -i "$FW_IMG" ::/JANUS
mcopy -i "$FW_IMG" "$UKI_A" "::/JANUS/UKI-A.EFI"
mcopy -i "$FW_IMG" "$UKI_B" "::/JANUS/UKI-B.EFI"
mcopy -i "$FW_IMG" "$ACTIVE_UKI" "::/EFI/BOOT/$BOOT_FILENAME"

write_part 1 "$FW_IMG"
write_part 2 "$SQUASHFS"
write_part 3 "$VERITY"
write_part 4 "$SQUASHFS"
write_part 5 "$VERITY"
write_part 6 "$STATE_IMAGE"

echo "Wrote $OUT (${TOTAL_MB}MiB GPT disk: FIRMWARE, BOOT-A-DATA/HASH, BOOT-B-DATA/HASH, STATE; active slot $ACTIVE_SLOT)"
sgdisk -p "$OUT"
