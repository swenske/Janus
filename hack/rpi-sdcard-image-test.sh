#!/usr/bin/env bash
# Real Raspberry Pi 4/5 hardware follow-up: STRUCTURAL verification
# only of image/rpi-uefi/assemble.sh's output - there is no real Pi4/
# Pi5 hardware anywhere in this project's development or CI
# environment (self-hosted runner included) to actually boot-test
# either image, and QEMU's raspi4b machine cannot substitute here
# either: it has no VideoCore GPU emulation at all, so it can never
# exercise the real firmware chain this image depends on (bootcode
# scanning the FAT partition for config.txt/start4.elf, loading
# RPI_EFI.fd as the armstub) - every existing rpi4 QEMU test in this
# project deliberately bypasses that whole chain via -kernel/-append,
# which is exactly why they can't prove this image boots either. This
# script proves the image was assembled *correctly* - a real GPT
# partition table, a genuinely readable FAT32 firmware partition with
# every expected file present, and real squashfs/dm-verity/ext4
# content at each partition's own offset - which is the most this
# project's own tooling can verify; the actual boot proof can only come
# from the user's own hardware test.
#
# Usage: hack/rpi-sdcard-image-test.sh <disk-img> <firmware-marker-file>
# <firmware-marker-file> is a file expected in the firmware partition's
# root (e.g. RPI_EFI.fd) - confirms the real board firmware was
# actually copied in, not just this project's own UKI/JANUS staging.
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

DISK="${1:?usage: $0 <disk-img> <firmware-marker-file>}"
FIRMWARE_MARKER="${2:?usage: $0 <disk-img> <firmware-marker-file>}"

fail() { echo "rpi-sdcard-image-test FAILED: $1" >&2; exit 1; }

# Part 1: real GPT, six partitions, expected names in order.
PARTS="$(sgdisk -p "$DISK")"
echo "$PARTS"
for name in FIRMWARE BOOT-A-DATA BOOT-A-HASH BOOT-B-DATA BOOT-B-HASH STATE; do
  echo "$PARTS" | grep -q "$name" || fail "partition '$name' missing from the partition table"
done
echo "Part 1 OK: real GPT with all six expected partitions present"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

part_offset_bytes() {
  local part="$1" sector
  sector="$(sgdisk -i "$part" "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
  echo $(( sector * 512 ))
}

# Part 2: the firmware partition is a genuinely readable FAT32
# filesystem containing the real board firmware, this project's own
# UKI at the UEFI fallback path, and both slots staged under \JANUS\.
FW_IMG="$WORKDIR/fw.img"
dd if="$DISK" of="$FW_IMG" bs=512 skip="$(( $(part_offset_bytes 1) / 512 ))" count="$(( $(sgdisk -i 1 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}') ))" status=none
# "-b -/": bare, full-path, recursive listing (one real filename per
# line, e.g. "::/RPI_EFI.fd") - mdir's own default columnar 8.3 display
# splits "RPI_EFI.fd" into two space-padded fields with no dot at all,
# which a first draft of this check matched against literally and
# always failed on, even against a genuinely correct image.
LISTING="$(mdir -i "$FW_IMG" -b -/ 2>&1)"
echo "$LISTING" | grep -qi "/$FIRMWARE_MARKER$" || fail "firmware partition is missing $FIRMWARE_MARKER"
echo "$LISTING" | grep -qi "/config.txt$" || fail "firmware partition is missing config.txt"
echo "$LISTING" | grep -qi "/EFI/BOOT/BOOTAA64.EFI$" || fail "firmware partition is missing EFI/BOOT/BOOTAA64.EFI"
echo "$LISTING" | grep -qi "/JANUS/UKI-A.EFI$" || fail "firmware partition is missing JANUS/UKI-A.EFI"
echo "$LISTING" | grep -qi "/JANUS/UKI-B.EFI$" || fail "firmware partition is missing JANUS/UKI-B.EFI"
echo "Part 2 OK: firmware partition is a real, readable FAT32 filesystem with the board firmware, our UKI, and both staged A/B slots"

# Part 3: BOOT-A/BOOT-B data partitions start with a real squashfs
# superblock ("hsqs"), hash partitions with a real dm-verity
# superblock ("verity\0\0") - proves genuine content was written at
# each partition's own offset, not that the bytes merely fit.
check_magic() {
  local part="$1" want="$2" off got
  off="$(part_offset_bytes "$part")"
  got="$(dd if="$DISK" bs=1 skip="$off" count="${#want}" status=none)"
  [ "$got" = "$want" ] || fail "partition $part does not start with the expected magic ('$want')"
}
check_magic 2 "hsqs"
check_magic 3 $'verity\0\0'
check_magic 4 "hsqs"
check_magic 5 $'verity\0\0'
echo "Part 3 OK: BOOT-A/BOOT-B data partitions carry a real squashfs superblock, hash partitions a real dm-verity superblock"

# Part 4: STATE starts with a real ext4 superblock (magic 0xEF53 at
# byte offset 1080 within the filesystem, i.e. 1024-byte superblock
# offset + 56-byte s_magic field offset).
STATE_OFF="$(part_offset_bytes 6)"
MAGIC="$(dd if="$DISK" bs=1 skip="$(( STATE_OFF + 1080 ))" count=2 status=none | od -A n -t x1 | tr -d ' \n')"
[ "$MAGIC" = "53ef" ] || fail "STATE partition does not carry a real ext4 superblock (got magic $MAGIC, want 53ef)"
echo "Part 4 OK: STATE partition carries a real ext4 superblock"

echo "rpi-sdcard-image-test OK: $DISK is a structurally correct, real Pi SD card image - actual boot behavior can only be confirmed on real Pi4/Pi5 hardware, not by this script or by anything in this project's CI"
