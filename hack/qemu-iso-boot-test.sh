#!/usr/bin/env bash
# Proves image/iso/assemble.sh's hybrid ISO actually boots under real
# UEFI firmware (OVMF) as a plain GPT disk - the realistic "dd'd to a
# USB stick" bare-metal path (see that script's own header comment for
# why this, not a real optical/El Torito boot, is what's tested here:
# proving the GPT/dm-verity side works is what matters, real optical
# media's fixed 2048-byte sector size is a separate, unexercised
# concern this test doesn't claim to cover). Same shape as
# hack/qemu-uefi-ab-boot-test.sh - ONE drive, the .iso itself, no
# -kernel/-append.
#
# Deliberately no STATE drive attached at all (this medium is an
# ephemeral live/maintenance environment, see image/iso/assemble.sh's
# own header) - PKI bootstraps fresh every boot, same as every other
# STATE-less boot test in this project; this test only checks that
# happens once per boot, not that it persists.
#
# Usage: hack/qemu-iso-boot-test.sh <iso-file>
set -euo pipefail

ISO="${1:?usage: $0 <iso-file>}"
HTTP_TIMEOUT_SECS="${QEMU_ISO_HTTP_TIMEOUT:-40}"
HOST_PORT="${QEMU_ISO_TEST_PORT:-$((18100 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
MARKER="JANUS_INIT_BOOT_OK"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf) - set \$OVMF_CODE to override" >&2; exit 1; }
[ -f "$OVMF_VARS_TEMPLATE" ] || { echo "OVMF vars template not found at $OVMF_VARS_TEMPLATE - set \$OVMF_VARS_TEMPLATE to override" >&2; exit 1; }

WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

OVMF_VARS="$WORKDIR/OVMF_VARS.fd"
cp "$OVMF_VARS_TEMPLATE" "$OVMF_VARS"

LOG="$WORKDIR/iso-boot.log"
qemu-system-x86_64 -accel kvm -accel tcg \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$OVMF_VARS" \
  -drive file="$ISO",format=raw,if=virtio \
  -nographic -no-reboot -display none -m 512M \
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
  echo "ISO boot test FAILED: no HTTP 200 from HAProxy within ${HTTP_TIMEOUT_SECS}s (last code: '${code}')" >&2
  echo "--- console output ---" >&2
  cat "$LOG" >&2
  exit 1
fi
if ! grep -q "$MARKER" "$LOG"; then
  echo "ISO boot test FAILED: got HTTP 200 but $MARKER never appeared on the console - investigate" >&2
  echo "--- console output ---" >&2
  cat "$LOG" >&2
  exit 1
fi
echo "ISO boot test OK: real OVMF firmware booted the hybrid ISO as a raw GPT disk, dm-verity verified root, HAProxy answered HTTP 200"
