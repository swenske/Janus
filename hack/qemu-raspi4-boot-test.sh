#!/usr/bin/env bash
# Boots a built Janus aarch64 kernel+initramfs under QEMU's raspi4b
# machine (Raspberry Pi 4 Model B emulation) and checks for the init's
# success marker on the (PL011) serial console - the Single Board
# Computer tranche's own Phase-1-equivalent boot-proof test, mirroring
# ../hack/qemu-run.sh's x86 original.
#
# `clk_ignore_unused` is required, not cosmetic: the generic
# `clk_disable_unused` initcall otherwise walks BCM2835's full clock
# tree and hits a real "synchronous external abort" probing a
# clock-gate register QEMU's raspi4b doesn't emulate - see
# kernel/configs/janus_rpi4_defconfig's own header for this and the
# other real QEMU raspi4b emulation gaps found (and fixed) empirically.
#
# `-monitor none </dev/null` is required, not cosmetic either - a real,
# reproducible (100% both ways, not a fluke) gap found running this
# exact script under this repo's own tooling: with no `-monitor`
# override, `-nographic` multiplexes QEMU's monitor onto stdio by
# default; run as a directly-typed interactive command it worked every
# time, but run as this same script executed as a subprocess it failed
# every time with a completely empty serial log (0 bytes - not even the
# kernel's own earliest boot banner), because the script's own stdin
# isn't a real interactive TTY the way a directly-typed command's is.
# Explicitly disabling the monitor and redirecting stdin from /dev/null
# sidesteps the whole interaction rather than depending on it.
#
# Usage: hack/qemu-raspi4-boot-test.sh <Image> <bcm2711-rpi-4-b.dtb> <initramfs.cpio.gz>
set -euo pipefail

KERNEL="${1:?usage: $0 <Image> <bcm2711-rpi-4-b.dtb> <initramfs.cpio.gz>}"
DTB="${2:?usage: $0 <Image> <bcm2711-rpi-4-b.dtb> <initramfs.cpio.gz>}"
INITRD="${3:?usage: $0 <Image> <bcm2711-rpi-4-b.dtb> <initramfs.cpio.gz>}"
TIMEOUT_SECS="${QEMU_BOOT_TIMEOUT:-30}"
MARKER="JANUS_INIT_BOOT_OK"

LOG="$(mktemp)"
trap 'rm -f "$LOG"' EXIT

timeout "${TIMEOUT_SECS}" qemu-system-aarch64 \
  -M raspi4b \
  -kernel "$KERNEL" \
  -dtb "$DTB" \
  -initrd "$INITRD" \
  -append "console=ttyAMA0,115200 earlycon=pl011,0xfe201000 clk_ignore_unused" \
  -nographic -no-reboot -display none -monitor none \
  -serial file:"$LOG" \
  </dev/null >/dev/null 2>&1 || true

if grep -q "$MARKER" "$LOG"; then
  echo "Boot OK: found $MARKER"
  exit 0
fi

echo "Boot FAILED: $MARKER not found in console output within ${TIMEOUT_SECS}s" >&2
echo "--- console output ---" >&2
cat "$LOG" >&2
exit 1
