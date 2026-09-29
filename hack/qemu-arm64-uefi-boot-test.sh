#!/usr/bin/env bash
# Single Board Computer tranche: the aarch64 equivalent of
# hack/qemu-uefi-boot-test.sh - proves image/uki/assemble.sh's arm64
# Unified Kernel Image actually boots under *real* UEFI firmware
# (AAVMF/edk2-aarch64), not QEMU's own -kernel/-append shortcut. No
# -kernel, no -append, no -initrd at all: AAVMF discovers the ESP
# (image/uki/esp-image.sh, built with the aarch64 fallback filename
# BOOTAA64.EFI - see that script's own doc comment for why this can't
# just reuse the x86_64 BOOTX64.EFI name), loads it (systemd-stub with
# the arm64 kernel Image and its exact cmdline embedded, built against
# the fetched linuxaa64.efi.stub - see systemd-stub-arm64/Dockerfile),
# which on arm64 needs no separate "handover protocol" at all (unlike
# x86_64's EFI_HANDOVER_PROTOCOL): the kernel's own Image binary is
# already a directly-runnable UEFI PE/COFF executable once
# CONFIG_EFI_STUB=y is set, so systemd-stub chain-loads straight into
# it. From there it's the same as the amd64 verity boot: dm-verity
# assembles and verifies root from the two virtio-blk drives the
# cmdline was built to expect, real init runs, HAProxy must answer real
# HTTP - proving the full squashfs+dm-verity+UEFI/UKI chain works on
# aarch64 too, not just the initramfs-based boots
# qemu-raspi4-daemon-test/qemu-arm64-network-test already cover.
#
# Booted under QEMU's generic aarch64 "virt" machine (real PCIe root
# complex, real virtio-net-pci/virtio-blk-pci) - not raspi4b, which has
# no PCIe emulation at all - same deliberate choice
# hack/qemu-arm64-network-test.sh already made, see its own header.
#
# The ESP ends up as the *first* virtio-blk drive (vda), same ordering
# gotcha hack/qemu-uefi-boot-test.sh's own comment documents for amd64 -
# root's data/hash devices are /dev/vdb and /dev/vdc, not /dev/vda and
# /dev/vdb.
#
# Usage: hack/qemu-arm64-uefi-boot-test.sh <rootfs-dir> <esp.img>
# <rootfs-dir> must contain rootfs.squashfs and rootfs.verity (see
# rootfs/assemble.sh) - the UKI's own cmdline already has the root hash
# baked in, this script doesn't need rootfs.roothash/verity.info itself.
set -euo pipefail

ROOTFS_DIR="${1:?usage: $0 <rootfs-dir> <esp.img>}"
ESP="${2:?usage: $0 <rootfs-dir> <esp.img>}"
HTTP_TIMEOUT_SECS="${QEMU_UEFI_HTTP_TIMEOUT:-40}"
HOST_PORT="${QEMU_UEFI_TEST_PORT:-18087}"
MARKER="JANUS_INIT_BOOT_OK"

AAVMF_CODE="${AAVMF_CODE:-/usr/share/AAVMF/AAVMF_CODE.fd}"
AAVMF_VARS_TEMPLATE="${AAVMF_VARS_TEMPLATE:-/usr/share/AAVMF/AAVMF_VARS.fd}"
[ -f "$AAVMF_CODE" ] || { echo "AAVMF firmware not found at $AAVMF_CODE (package: qemu-efi-aarch64) - set \$AAVMF_CODE to override" >&2; exit 1; }
[ -f "$AAVMF_VARS_TEMPLATE" ] || { echo "AAVMF vars template not found at $AAVMF_VARS_TEMPLATE - set \$AAVMF_VARS_TEMPLATE to override" >&2; exit 1; }

SQUASHFS="$ROOTFS_DIR/rootfs.squashfs"
VERITY="$ROOTFS_DIR/rootfs.verity"

WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

# AAVMF writes to its "vars" store at runtime (NVRAM emulation) - needs
# a private, writable copy, never the shared template. AAVMF_CODE.fd is
# 64MiB - QEMU's pflash device requires both halves to be the exact
# same size, unlike OVMF's own 4M code/vars split.
AAVMF_VARS="$WORKDIR/AAVMF_VARS.fd"
cp "$AAVMF_VARS_TEMPLATE" "$AAVMF_VARS"

LOG="$WORKDIR/uefi-boot.log"
qemu-system-aarch64 \
  -M virt -cpu cortex-a72 -m 512M \
  -drive if=pflash,format=raw,readonly=on,file="$AAVMF_CODE" \
  -drive if=pflash,format=raw,file="$AAVMF_VARS" \
  -drive file="$ESP",format=raw,if=virtio \
  -drive file="$SQUASHFS",format=raw,if=virtio,readonly=on \
  -drive file="$VERITY",format=raw,if=virtio,readonly=on \
  -nographic -no-reboot -display none \
  -netdev "user,id=net0,hostfwd=tcp::${HOST_PORT}-:8080" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG" \
  &
QEMU_PID=$!

deadline=$((SECONDS + HTTP_TIMEOUT_SECS))
code=""
while [ "$SECONDS" -lt "$deadline" ]; do
  code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_PORT}/" || true)"
  [ "$code" = "200" ] && break
  sleep 1
done

kill "$QEMU_PID" 2>/dev/null || true
wait "$QEMU_PID" 2>/dev/null || true
QEMU_PID=""

if [ "$code" != "200" ]; then
  echo "arm64 UEFI boot test FAILED: no HTTP 200 from HAProxy within ${HTTP_TIMEOUT_SECS}s (last code: '${code}')" >&2
  echo "--- console output ---" >&2
  cat "$LOG" >&2
  exit 1
fi
if ! grep -q "BdsDxe: starting Boot" "$LOG"; then
  echo "arm64 UEFI boot test FAILED: got HTTP 200, but AAVMF's own boot-device log line never appeared - can't confirm this actually went through real UEFI firmware discovery, not some other path" >&2
  echo "--- console output ---" >&2
  cat "$LOG" >&2
  exit 1
fi
if ! grep -q "$MARKER" "$LOG"; then
  echo "arm64 UEFI boot test FAILED: got HTTP 200 but $MARKER never appeared on the console - investigate" >&2
  echo "--- console output ---" >&2
  cat "$LOG" >&2
  exit 1
fi
echo "arm64 UEFI boot test OK: real AAVMF firmware discovered and booted the UKI (no -kernel/-append), dm-verity verified root, HAProxy answered HTTP 200"
