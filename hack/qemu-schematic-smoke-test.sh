#!/usr/bin/env bash
# Smoke test of a schematic build (image/schematic/build.sh): boots its
# disk.img under real UEFI firmware (the UKI's own signed cmdline,
# SELinux enforcing), and checks HAProxy answers, the cmdline carries the
# schematic's ID, every extension's services started, and nothing was
# denied. The full proof of extensions is hack/qemu-extensions-test.sh;
# this only checks that one built image is sound before it's published.
#
# Usage: hack/qemu-schematic-smoke-test.sh <disk.img> <schematic.json>
set -euo pipefail

SRC_DISK="${1:?usage: $0 <disk.img> <schematic.json>}"
SCHEMATIC_FILE="${2:?}"
HERE="$(cd "$(dirname "$0")" && pwd)"
PORT="${QEMU_SMOKE_PORT:-18650}"
TIMEOUT="${QEMU_SMOKE_TIMEOUT:-90}"
OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"

WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT
LOG="$WORKDIR/console.log"
fail() {
  echo "Schematic smoke test FAILED: $*" >&2
  [ -f "$LOG" ] && { echo "--- console output ---" >&2; cat "$LOG" >&2; }
  exit 1
}

ID="$(cd "$HERE/.." && go run ./hack/extpack id -schematic "$SCHEMATIC_FILE")"
EXTENSIONS="$(python3 -c 'import json,sys; print(" ".join(json.load(open(sys.argv[1]))["customization"].get("extensions", [])))' "$SCHEMATIC_FILE")"
cp "$SRC_DISK" "$WORKDIR/disk.img"
cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/OVMF_VARS.fd"
qemu-system-x86_64 \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$WORKDIR/OVMF_VARS.fd" \
  -drive file="$WORKDIR/disk.img",format=raw,if=virtio \
  -nographic -display none -m 512M -monitor none -no-reboot \
  -netdev "user,id=net0,hostfwd=tcp::${PORT}-:8080" -device virtio-net-pci,netdev=net0 \
  -device virtio-serial -chardev "socket,path=$WORKDIR/qga.sock,server=on,wait=off,id=qga0" \
  -device virtserialport,chardev=qga0,name=org.qemu.guest_agent.0 \
  -serial file:"$LOG" </dev/null &
QEMU_PID=$!

deadline=$((SECONDS + TIMEOUT))
until [ "$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT}/" || true)" = "200" ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "no HTTP 200 within ${TIMEOUT}s"
  sleep 1
done
echo "  ok: HAProxy answers"
if [ -n "$EXTENSIONS" ]; then
  grep -q "janus.schematic=$ID" "$LOG" || fail "the cmdline doesn't carry janus.schematic=$ID"
  echo "  ok: the signed cmdline carries schematic $ID"
fi
for ext in $EXTENSIONS; do
  deadline=$((SECONDS + 30))
  until grep -q "extensions: started .* ($ext, pid" "$LOG"; do
    [ "$SECONDS" -lt "$deadline" ] || fail "extension $ext never started a service"
    sleep 1
  done
  echo "  ok: $ext started"
done
sleep 5 # let the services run into anything their policy denies
if grep -q "avc:.*denied" "$LOG"; then
  grep "avc:.*denied" "$LOG" >&2
  fail "AVC denials under enforcing"
fi
if grep -Eq "extensions: .* exited" "$LOG"; then
  fail "an extension service exited: $(grep -E 'extensions: .* exited' "$LOG")"
fi
echo "Schematic smoke test OK: schematic $ID boots, serves, runs [${EXTENSIONS:-no extension}], zero AVC denials"
