#!/usr/bin/env bash
# Single Board Computer tranche's own Phase-2-equivalent boot-proof:
# boots the full init+janusd+haproxy arm64 stack (same shape
# hack/qemu-network-test.sh proves on amd64) under QEMU's raspi4b
# machine, and confirms both janusd and haproxy actually start - but
# console-verified only, not the real HTTP check qemu-network-test does.
#
# That's a deliberate, already-made scoping decision, not an oversight:
# QEMU's raspi4b machine type emulates neither PCIe nor BCM GENET
# Ethernet at all (confirmed in QEMU's own documentation, "Missing
# devices") - there is no network path to curl over on this machine
# type, full stop. A real HTTP check needs either genuine Pi hardware or
# accepting the generic aarch64 "virt" machine instead (real virtio-net,
# but exercises none of the Pi-specific firmware/SD/GPIO path) - neither
# decision is made here; this test stays console-only, matching the
# same level of proof Talos Linux itself relies on for SBC support (no
# real-hardware CI there either - see CLAUDE.md's own research notes).
#
# What "started" means here, precisely:
#   - janusd: its own "listening on ... (mTLS required)" log line -
#     proves PKI bootstrap succeeded and the gRPC server actually bound,
#     not just that the process was exec'd.
#   - haproxy: its own real runtime startup line ("Automatically setting
#     global.maxconn to ..." - HAProxy always prints this on a
#     successful start, confirmed against this project's own x86 boot
#     logs; NOT "Available polling systems", which only appears in
#     `haproxy -vv`'s verbose *build-time* diagnostic dump, never in a
#     normal runtime start - a real mistake this script's own first
#     draft made, caught by a real boot that started haproxy
#     successfully and still failed the check) appearing on the
#     console, *and* janusd's own "haproxy: initial start failed"
#     failure line staying absent - cmd/janusd/main.go only logs on
#     failure, never on success, so absence-of-failure plus haproxy's
#     own startup line together are the real proof.
#
# Usage: hack/qemu-raspi4-daemon-test.sh <Image> <bcm2711-rpi-4-b.dtb> <initramfs-full.cpio.gz>
set -euo pipefail

KERNEL="${1:?usage: $0 <Image> <bcm2711-rpi-4-b.dtb> <initramfs-full.cpio.gz>}"
DTB="${2:?usage: $0 <Image> <bcm2711-rpi-4-b.dtb> <initramfs-full.cpio.gz>}"
INITRD="${3:?usage: $0 <Image> <bcm2711-rpi-4-b.dtb> <initramfs-full.cpio.gz>}"
TIMEOUT_SECS="${QEMU_BOOT_TIMEOUT:-30}"

LOG="$(mktemp)"
trap 'rm -f "$LOG"' EXIT

# Same -monitor none </dev/null requirement as qemu-raspi4-boot-test.sh
# - see that script's own header for the real, reproducible gap this
# works around (a subprocess's stdin isn't a real TTY, so -nographic's
# default stdio monitor multiplexing silently eats the entire serial
# log otherwise).
timeout "${TIMEOUT_SECS}" qemu-system-aarch64 \
  -M raspi4b \
  -kernel "$KERNEL" \
  -dtb "$DTB" \
  -initrd "$INITRD" \
  -append "console=ttyAMA0,115200 earlycon=pl011,0xfe201000 clk_ignore_unused" \
  -nographic -no-reboot -display none -monitor none \
  -serial file:"$LOG" \
  </dev/null >/dev/null 2>&1 || true

fail() {
  echo "Daemon test FAILED: $1" >&2
  echo "--- console output ---" >&2
  cat "$LOG" >&2
  exit 1
}

grep -q "JANUS_INIT_BOOT_OK" "$LOG" || fail "init's own boot marker never appeared"
grep -q "listening on .* (mTLS required)" "$LOG" || fail "janusd never logged its own gRPC listener starting"
if grep -q "haproxy: initial start failed" "$LOG"; then
  fail "janusd logged a haproxy start failure"
fi
grep -q "Automatically setting global.maxconn" "$LOG" || fail "haproxy's own runtime startup line never appeared on the console"

echo "Daemon test OK: init, janusd, and haproxy all started for real under QEMU raspi4b (console-verified only - no network path exists on this machine type, see this script's own header)"
