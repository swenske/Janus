#!/usr/bin/env bash
# The aarch64 analog of hack/qemu-network-test.sh - boots the same
# init+janusd+haproxy stack under QEMU's generic "virt" machine (real
# virtio-net-pci over a real PCIe root complex) and polls for real HTTP
# 200, the same end-to-end proof x86 gets from qemu-network-test.
#
# Deliberately NOT QEMU's raspi4b machine (that's hack/qemu-raspi4-
# daemon-test.sh's job, console-only) - raspi4b emulates neither PCIe
# nor BCM GENET Ethernet at all, so no test on it could ever give real
# network proof. "virt" is a synthetic board with no Pi-specific
# hardware whatsoever, but its real virtio-net finally closes that gap:
# this proves the janusd/haproxy/kernel-networking pipeline itself
# works on aarch64, decoupled from Pi-specific hardware enablement
# (SD/MMC, USB, BCM GENET), which stays its own, separate, later effort
# gated on real hardware access.
#
# Needed CONFIG_PCI/PCI_HOST_GENERIC/VIRTIO_PCI/VIRTIO_NET/IP_PNP added
# to kernel/configs/janus_rpi4_defconfig beyond what Phase 2's plain
# raspi4b daemon test needed (see that defconfig's own header for the
# two real gating-menu traps found getting there: CONFIG_NETDEVICES and
# CONFIG_BLK_DEV each needed enabling explicitly first, and CONFIG_
# IP_PNP was missing entirely - a real boot showed "Unknown kernel
# command line parameters ip=dhcp" and the PCI virtio-net device
# enumerating fine but never receiving a DHCP lease until fixed).
#
# Usage: hack/qemu-arm64-network-test.sh <Image> <initramfs.cpio.gz>
set -euo pipefail

KERNEL="${1:?usage: $0 <Image> <initramfs.cpio.gz>}"
INITRD="${2:?usage: $0 <Image> <initramfs.cpio.gz>}"
HOST_PORT="${QEMU_ARM64_NET_TEST_PORT:-$((18180 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
TIMEOUT_SECS="${QEMU_ARM64_NET_TEST_TIMEOUT:-30}"

LOG="$(mktemp)"
trap 'rm -f "$LOG"; [ -n "${QEMU_PID:-}" ] && kill "$QEMU_PID" 2>/dev/null || true' EXIT

qemu-system-aarch64 \
  -M virt -cpu cortex-a72 \
  -kernel "$KERNEL" \
  -initrd "$INITRD" \
  -append "console=ttyAMA0 ip=dhcp" \
  -nographic -no-reboot -display none -monitor none -m 256M \
  -netdev "user,id=net0,hostfwd=tcp::${HOST_PORT}-:8080" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG" \
  &
QEMU_PID=$!

deadline=$((SECONDS + TIMEOUT_SECS))
code=""
while [ "$SECONDS" -lt "$deadline" ]; do
  code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_PORT}/" || true)"
  if [ "$code" = "200" ]; then
    echo "arm64 network boot test OK: HTTP 200 from HAProxy inside the VM (QEMU virt, not raspi4b - see this script's own header)"
    exit 0
  fi
  sleep 1
done

echo "arm64 network boot test FAILED: no HTTP 200 within ${TIMEOUT_SECS}s (last code: '${code}')" >&2
echo "--- console output ---" >&2
cat "$LOG" >&2
exit 1
