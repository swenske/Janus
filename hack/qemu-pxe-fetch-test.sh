#!/usr/bin/env bash
# Bare-metal Machine tranche cont'd: proves the network-delivery half
# of PXE/iPXE boot - a real iPXE instance, running under real OVMF,
# DHCPs over virtio-net and fetches image/uki/assemble.sh's own UKI via
# TFTP (QEMU's slirp built-in TFTP server standing in for a real
# PXE/TFTP server) - the same UKI already proven bootable from local
# media by every other UEFI test in this project
# (hack/qemu-uefi-boot-test.sh).
#
# Deliberately does NOT assert a full boot to HTTP 200 the way every
# other boot test here does - see image/pxe/README.md's own "Known
# limitation" section: executing a Linux kernel EFI stub via a
# *nested* LoadImage/StartImage call (loaded by iPXE, or even by the
# plain UEFI Shell - reproduced with zero iPXE/network involvement at
# all) hangs in this project's own OVMF build, at or immediately after
# ExitBootServices. This is a firmware/kernel-EFI-stub interaction, not
# a bug in Janus's own artifacts - real bare-metal firmware with native
# PXE/HTTP Boot support loads the UKI *directly* (never nested), the
# same firmware boot path every other test here already proves solid,
# and isn't affected.
#
# Usage: hack/qemu-pxe-fetch-test.sh <bzImage> <rootfs-dir>
# Requires the `ipxe` Debian package (ipxe.efi) and OVMF.
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

KERNEL="${1:?usage: $0 <bzImage> <rootfs-dir>}"
ROOTFS_DIR="${2:?usage: $0 <bzImage> <rootfs-dir>}"

IPXE_EFI="${IPXE_EFI:-/usr/lib/ipxe/ipxe.efi}"
[ -f "$IPXE_EFI" ] || { echo "iPXE EFI binary not found at $IPXE_EFI (package: ipxe) - set \$IPXE_EFI to override" >&2; exit 1; }

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf) - set \$OVMF_CODE to override" >&2; exit 1; }
[ -f "$OVMF_VARS_TEMPLATE" ] || { echo "OVMF vars template not found at $OVMF_VARS_TEMPLATE - set \$OVMF_VARS_TEMPLATE to override" >&2; exit 1; }

FETCH_TIMEOUT_SECS="${QEMU_PXE_FETCH_TIMEOUT:-30}"

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

# The same UKI a real PXE/HTTP Boot server would serve directly (no ESP
# involved there at all) - built with plain /dev/vda/vdb device paths,
# matching a PXE-booted node with no ESP taking a drive slot first.
mkdir -p "$WORKDIR/tftproot"
"$SELF_DIR/../image/uki/assemble.sh" "$WORKDIR/tftproot/janus.efi" "$KERNEL" "$ROOTFS_DIR" /dev/vda /dev/vdb
EXPECTED_SIZE="$(stat -c%s "$WORKDIR/tftproot/janus.efi")"

# iPXE chainloaded from a small local ESP - standing in for a real PXE
# ROM/UEFI network-boot option that would fetch ipxe.efi itself, or a
# firmware with iPXE embedded - see image/pxe/README.md for why this
# project's own OVMF build needs this indirection at all.
"$SELF_DIR/../image/uki/esp-image.sh" "$WORKDIR/ipxe-esp.img" "$IPXE_EFI" 64

OVMF_VARS="$WORKDIR/OVMF_VARS.fd"
cp "$OVMF_VARS_TEMPLATE" "$OVMF_VARS"

LOG="$WORKDIR/pxe-fetch.log"
qemu-system-x86_64 -accel kvm -accel tcg \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$OVMF_VARS" \
  -drive file="$WORKDIR/ipxe-esp.img",format=raw,if=virtio \
  -netdev user,id=net0,tftp="$WORKDIR/tftproot",bootfile=/janus.efi \
  -device virtio-net-pci,netdev=net0 \
  -nographic -no-reboot -display none -m 512M \
  -serial file:"$LOG" \
  &
QEMU_PID=$!

deadline=$((SECONDS + FETCH_TIMEOUT_SECS))
fetched=""
while [ "$SECONDS" -lt "$deadline" ]; do
  if grep -q "janus.efi : ${EXPECTED_SIZE} bytes \[EFI\]" "$LOG" 2>/dev/null; then
    fetched=1
    break
  fi
  sleep 1
done

kill "$QEMU_PID" 2>/dev/null || true
wait "$QEMU_PID" 2>/dev/null || true
QEMU_PID=""

if [ -z "$fetched" ]; then
  echo "PXE fetch test FAILED: iPXE never reported fetching janus.efi at the expected size (${EXPECTED_SIZE} bytes) within ${FETCH_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2
  cat "$LOG" >&2
  exit 1
fi

echo "PXE fetch test OK: iPXE DHCP'd over virtio-net and fetched the real Janus UKI via TFTP at the correct size (${EXPECTED_SIZE} bytes) - see image/pxe/README.md for what this does and doesn't prove"
