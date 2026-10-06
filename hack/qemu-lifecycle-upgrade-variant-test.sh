#!/usr/bin/env bash
# Proves a node moves to another kernel track (variants.mk) through a
# real A/B update, and back: the default image's disk (the default
# track's kernel) is updated with a bundle built from the same binaries
# for schematic {"kernel": "<track>"} - its UKI carrying the other
# track's kernel and the schematic's ID in its signed command line, its
# rootfs the image.json that says so.
#
#   1. the update is refused without allow_schematic_change (another
#      schematic), nothing written, no reboot;
#   2. with it, and the automatic revert on (wait_for_health), the node
#      reboots into slot B: the other track's kernel runs, Version
#      reports the track, pinned, and janusd confirms the boot healthy;
#   3. Rollback brings slot A back: the default track again.
#
# Like hack/qemu-lifecycle-upgrade-test.sh, the bundle is put on STATE
# before the first boot (debugfs) and its UKIs are unsigned
# (-insecure-skip-signature-check).
#
# Usage: hack/qemu-lifecycle-upgrade-variant-test.sh <disk.img> <build-dir> <janusctl-bin> <track>
# <build-dir> holds init, janusd, haproxy, shutdown, the SELinux policy,
# the CA bundle and kernel-<track>/bzImage (make kernel-build-<track>).
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

USAGE="usage: $0 <disk.img> <build-dir> <janusctl-bin> <track>"
DISK="${1:?$USAGE}"
BUILD_DIR="${2:?$USAGE}"
CTL="${3:?$USAGE}"
TRACK="${4:?$USAGE}"
KERNEL2="$BUILD_DIR/kernel-$TRACK/bzImage"
[ -s "$KERNEL2" ] || { echo "no $KERNEL2 - make kernel-build-$TRACK" >&2; exit 1; }
HTTP_TIMEOUT_SECS="${QEMU_UPGRADE_HTTP_TIMEOUT:-60}"
REBOOT_TIMEOUT_SECS="${QEMU_UPGRADE_REBOOT_TIMEOUT:-120}"
HOST_HTTP_PORT="$((18095 + ${JANUS_TEST_PORT_OFFSET:-0}))"
HOST_GRPC_PORT="$((18096 + ${JANUS_TEST_PORT_OFFSET:-0}))"
MARKER="JANUS_INIT_BOOT_OK"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf)" >&2; exit 1; }

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT
fail() { echo "Variant upgrade test FAILED: $*" >&2; exit 1; }

# --- the bundle: same binaries, schematic {"kernel": TRACK} ---
printf '{"customization":{"kernel":"%s"}}' "$TRACK" > "$WORKDIR/schematic.json"
SCHEMATIC_ID="$(cd "$ROOT" && go run ./hack/extpack id -schematic "$WORKDIR/schematic.json")"
(cd "$ROOT" && go run ./hack/extpack image-info -schematic "$WORKDIR/schematic.json" -arch amd64 -out "$WORKDIR/image.json")
TRACK_VERSION="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["kernel"]["version"])' "$WORKDIR/image.json")"
mkdir -p "$WORKDIR/rootfs-v2"
JANUS_SHUTDOWN_BIN="$BUILD_DIR/shutdown" JANUS_IMAGE_INFO="$WORKDIR/image.json" \
  "$ROOT/rootfs/assemble.sh" "$WORKDIR/rootfs-v2" "$BUILD_DIR/init" "$BUILD_DIR/janusd" \
  "$BUILD_DIR/haproxy" "$ROOT/rootfs/base/etc/haproxy/haproxy.cfg" "$BUILD_DIR/selinux/janus.policy" \
  "$BUILD_DIR/ca-certificates/ca-certificates.crt"
JANUS_SCHEMATIC="$SCHEMATIC_ID" "$ROOT/image/release/assemble.sh" "$WORKDIR/bundle" "$KERNEL2" "$WORKDIR/rootfs-v2"
SHA256="$(cat "$WORKDIR/bundle/rootfs.squashfs.sha256")"

STATE_START="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
STATE_SIZE="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
STATE_IMG="$WORKDIR/state.img"
dd if="$DISK" of="$STATE_IMG" bs=512 skip="$STATE_START" count="$STATE_SIZE" status=none
debugfs -w -R "mkdir upgrade" "$STATE_IMG" >/dev/null 2>&1
for f in rootfs.squashfs rootfs.verity uki-b.efi; do
  debugfs -w -R "write $WORKDIR/bundle/$f upgrade/$f" "$STATE_IMG" >/dev/null 2>&1
done
dd if="$STATE_IMG" of="$DISK" bs=512 seek="$STATE_START" conv=notrunc status=none

LOG="$WORKDIR/console.log"
cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/OVMF_VARS.fd"
qemu-system-x86_64 -accel kvm -accel tcg \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$WORKDIR/OVMF_VARS.fd" \
  -drive file="$DISK",format=raw,if=virtio \
  -nographic -display none -m 512M \
  -netdev "user,id=net0,hostfwd=tcp::${HOST_HTTP_PORT}-:8080,hostfwd=tcp::${HOST_GRPC_PORT}-:9505" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG" &
QEMU_PID=$!

# HTTP 200 and at least this many boots (the previous boot answers for a
# moment after an update - CLAUDE.md).
wait_boot() {
  local boots="$1" timeout="$2" deadline=$((SECONDS + $2))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if [ "$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_HTTP_PORT}/" || true)" = 200 ] &&
       [ "$(grep -ac "$MARKER" "$LOG" || true)" -ge "$boots" ] &&
       [ "$(grep -ac "listening on" "$LOG" || true)" -ge "$boots" ]; then
      return 0
    fi
    sleep 1
  done
  echo "--- console ---" >&2; cat "$LOG" >&2
  fail "boot $boots didn't come up healthy within ${timeout}s"
}
wait_boot 1 "$HTTP_TIMEOUT_SECS"

dd if="$DISK" of="$STATE_IMG" bs=512 skip="$STATE_START" count="$STATE_SIZE" status=none
for f in ca.crt admin.crt admin.key; do
  debugfs -R "dump pki/$f $WORKDIR/$f" "$STATE_IMG" >/dev/null 2>&1
  [ -s "$WORKDIR/$f" ] || fail "couldn't extract pki/$f from STATE"
done
ctl() { "$CTL" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" "$@"; }

DEFAULT_LINE="$(ctl version | grep '^Kernel track: ')"
echo "slot A: $DEFAULT_LINE"
case "$DEFAULT_LINE" in *"$TRACK ("*) fail "slot A already runs the $TRACK track" ;; esac

# 1. Another schematic: refused unless allowed, nothing written.
set +e
out="$(ctl lifecycle upgrade -insecure-skip-signature-check -sha256 "$SHA256" /etc/.state/upgrade 2>&1)"
rc=$?
set -e
echo "$out"
[ "$rc" -ne 0 ] && grep -q "built from image schematic ${SCHEMATIC_ID:0:12}" <<< "$out" || fail "an update to another kernel track wasn't refused (exit $rc)"
sleep 5
[ "$(grep -ac "$MARKER" "$LOG")" -eq 1 ] || fail "the node rebooted after refusing the update"
echo "  ok: refused without allow_schematic_change"

# 2. Allowed, with the automatic revert.
ctl lifecycle upgrade -insecure-skip-signature-check -allow-schematic-change -wait-for-health \
  -sha256 "$SHA256" /etc/.state/upgrade | tail -3
wait_boot 2 "$REBOOT_TIMEOUT_SECS"
for _ in $(seq 1 60); do grep -aq "bootcommit: confirmed healthy for slot B" "$LOG" && break; sleep 2; done
grep -aq "bootcommit: confirmed healthy for slot B" "$LOG" || fail "janusd never confirmed slot B healthy"
VERSION_OUT="$(ctl version)"
echo "$VERSION_OUT"
grep -q "^Image schematic: $SCHEMATIC_ID$" <<< "$VERSION_OUT" || fail "slot B doesn't report schematic $SCHEMATIC_ID"
grep -q "^Kernel track: $TRACK (version $TRACK_VERSION, pinned by the schematic)$" <<< "$VERSION_OUT" || fail "slot B doesn't report the $TRACK track, pinned"
running="$(ctl system info | awk '/^kernel version:/ {print $3}')"
[ "$running" = "$TRACK_VERSION" ] || fail "slot B runs kernel $running, not the $TRACK track's $TRACK_VERSION"
echo "  ok: slot B runs the $TRACK track's kernel $running, confirmed healthy"

# 3. Rollback: slot A's default track again.
ctl lifecycle rollback | tail -2
wait_boot 3 "$REBOOT_TIMEOUT_SECS"
[ "$(ctl version | grep '^Kernel track: ')" = "$DEFAULT_LINE" ] || fail "after the rollback: $(ctl version | grep '^Kernel track: ')"
echo "  ok: rolled back to $DEFAULT_LINE"

if grep -aq "avc:.*denied" "$LOG"; then
  grep -a "avc:.*denied" "$LOG" >&2
  fail "AVC denials"
fi
echo "Variant upgrade test OK: a node moved to the $TRACK kernel track through an A/B update (only with allow_schematic_change), confirmed healthy, and rolled back - zero AVC denials"
