#!/usr/bin/env bash
# Proves a node of the previous published release updates to this tree's
# default image, and back: the node operators run today, its STATE (PKI,
# applied haproxy.cfg) kept - the update that moves default nodes from one
# kernel track or HAProxy branch to the next (variants.mk) when the
# release changes a default.
#
#   1. the release's janus-kvm.qcow2 (checked against the digest GitHub
#      gives for the asset) boots under OVMF, enforcing; a configuration
#      is applied to its HAProxy;
#   2. this tree's default bundle (unsigned: -insecure-skip-signature-
#      check), put on its STATE before the first boot, is installed with
#      the automatic revert: the node comes back on the default kernel
#      track's kernel, says so (Version: the track, the release's
#      default), confirms itself healthy, still serves its configuration
#      and still has its CA (no second first boot);
#   3. Rollback brings the release back.
#
# Usage: hack/qemu-lifecycle-upgrade-from-release-test.sh <release> <bundle-dir> <janusctl-bin>
# <bundle-dir> is image/release/assemble.sh's output for this tree's
# default image (make release-bundle).
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

USAGE="usage: $0 <release> <bundle-dir> <janusctl-bin>"
RELEASE="${1:?$USAGE}"
BUNDLE="${2:?$USAGE}"
CTL="${3:?$USAGE}"
REPO="${GITHUB_REPOSITORY:-swenske/Janus}"
HTTP_TIMEOUT_SECS="${QEMU_UPGRADE_HTTP_TIMEOUT:-90}"
REBOOT_TIMEOUT_SECS="${QEMU_UPGRADE_REBOOT_TIMEOUT:-150}"
HOST_HTTP_PORT="$((18097 + ${JANUS_TEST_PORT_OFFSET:-0}))"
HOST_GRPC_PORT="$((18098 + ${JANUS_TEST_PORT_OFFSET:-0}))"
HOST_APP_PORT="$((18099 + ${JANUS_TEST_PORT_OFFSET:-0}))"
MARKER="JANUS_INIT_BOOT_OK"
FIRST_BOOT_MSG="pki: first boot - generated a new CA"

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
fail() { echo "Upgrade-from-release test FAILED: $*" >&2; exit 1; }

# --- the release's image, as published ---
curl -fsSL -H "Accept: application/vnd.github+json" ${GITHUB_TOKEN:+-H "Authorization: Bearer $GITHUB_TOKEN"} \
  "https://api.github.com/repos/$REPO/releases/tags/$RELEASE" > "$WORKDIR/release.json"
read -r URL DIGEST < <(python3 -c '
import json, sys
for a in json.load(open(sys.argv[1]))["assets"]:
    if a["name"] == "janus-kvm.qcow2":
        print(a["browser_download_url"], a["digest"].removeprefix("sha256:"))' "$WORKDIR/release.json")
[ -n "${DIGEST:-}" ] || fail "$RELEASE has no janus-kvm.qcow2 with a digest"
curl -fsSL -o "$WORKDIR/janus.qcow2" "$URL"
echo "$DIGEST  $WORKDIR/janus.qcow2" | sha256sum -c --quiet - || fail "janus-kvm.qcow2 isn't what GitHub says it is"
DISK="$WORKDIR/disk.img"
qemu-img convert -O raw "$WORKDIR/janus.qcow2" "$DISK"
echo "$RELEASE's janus-kvm.qcow2, checked against GitHub's digest"

# --- this tree's bundle on its STATE, before the first boot ---
STATE_START="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
STATE_SIZE="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
STATE_IMG="$WORKDIR/state.img"
dd if="$DISK" of="$STATE_IMG" bs=512 skip="$STATE_START" count="$STATE_SIZE" status=none
debugfs -w -R "mkdir upgrade" "$STATE_IMG" >/dev/null 2>&1
for f in rootfs.squashfs rootfs.verity uki-b.efi; do
  debugfs -w -R "write $BUNDLE/$f upgrade/$f" "$STATE_IMG" >/dev/null 2>&1
done
dd if="$STATE_IMG" of="$DISK" bs=512 seek="$STATE_START" conv=notrunc status=none

LOG="$WORKDIR/console.log"
cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/OVMF_VARS.fd"
qemu-system-x86_64 -accel kvm -accel tcg \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$WORKDIR/OVMF_VARS.fd" \
  -drive file="$DISK",format=raw,if=virtio \
  -nographic -display none -m 768M \
  -netdev "user,id=net0,hostfwd=tcp::${HOST_HTTP_PORT}-:8080,hostfwd=tcp::${HOST_GRPC_PORT}-:9505,hostfwd=tcp::${HOST_APP_PORT}-:8081" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG" &
QEMU_PID=$!

# HTTP 200 and at least this many boots (the previous boot answers for a
# moment after an update - CLAUDE.md).
wait_boot() {
  local boots="$1" deadline=$((SECONDS + $2))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if [ "$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_HTTP_PORT}/" || true)" = 200 ] &&
       [ "$(grep -ac "$MARKER" "$LOG" || true)" -ge "$boots" ] &&
       [ "$(grep -ac "listening on" "$LOG" || true)" -ge "$boots" ]; then
      return 0
    fi
    sleep 1
  done
  echo "--- console ---" >&2; cat "$LOG" >&2
  fail "boot $boots didn't come up healthy within $2 s"
}
wait_boot 1 "$HTTP_TIMEOUT_SECS"

dd if="$DISK" of="$STATE_IMG" bs=512 skip="$STATE_START" count="$STATE_SIZE" status=none
for f in ca.crt admin.crt admin.key; do
  debugfs -R "dump pki/$f $WORKDIR/$f" "$STATE_IMG" >/dev/null 2>&1
  [ -s "$WORKDIR/$f" ] || fail "couldn't extract pki/$f from STATE"
done
ctl() { "$CTL" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" "$@"; }

RUNNING="$(ctl version | awk '/^Node:/ {print $2}')"
OLD_KERNEL="$(ctl system info | awk '/^kernel version:/ {print $3}')"
echo "  ok: $RELEASE runs (janusd $RUNNING, kernel $OLD_KERNEL)"

# A configuration of its own, which the update must keep (STATE).
cat > "$WORKDIR/haproxy.cfg" <<'EOF'
global
    stats socket /run/janus/haproxy-admin.sock mode 660 level admin
    chroot /var/empty
    uid 1000
    gid 1000
defaults
    mode http
    timeout connect 5s
    timeout client 30s
    timeout server 30s
frontend janus-health
    bind *:8080
    http-request return status 200 content-type text/plain string "Janus: up\n"
frontend upgraded-app
    bind *:8081
    http-request return status 200 content-type text/plain string "kept across the update\n"
EOF
ctl haproxy apply-config "$WORKDIR/haproxy.cfg" > /dev/null || fail "applying a configuration on $RELEASE"
[ "$(curl -s -m 5 "http://127.0.0.1:${HOST_APP_PORT}/")" = "kept across the update" ] || fail "the applied configuration doesn't serve"

ctl lifecycle upgrade -insecure-skip-signature-check -wait-for-health \
  -sha256 "$(cat "$BUNDLE/rootfs.squashfs.sha256" | awk '{print $1}')" /etc/.state/upgrade | tail -3
wait_boot 2 "$REBOOT_TIMEOUT_SECS"
for _ in $(seq 1 60); do grep -aq "bootcommit: confirmed healthy for slot B" "$LOG" && break; sleep 2; done
grep -aq "bootcommit: confirmed healthy for slot B" "$LOG" || fail "the updated node never confirmed itself healthy"
[ "$(grep -ac "$FIRST_BOOT_MSG" "$LOG")" -eq 1 ] || fail "a second first boot: STATE's PKI didn't survive"
[ "$(curl -s -m 5 "http://127.0.0.1:${HOST_APP_PORT}/")" = "kept across the update" ] || fail "the applied configuration was lost"

VERSION_OUT="$(ctl version)"
echo "$VERSION_OUT"
default_track="$(sed -n 's/^KERNEL_DEFAULT_TRACK *:= *//p' "$ROOT/variants.mk")"
kernel_line="$(grep '^Kernel track: ' <<< "$VERSION_OUT" || true)"
case "$kernel_line" in
  "Kernel track: $default_track (version "*", the release's default)") ;;
  *) fail "the updated node says '$kernel_line', not the default $default_track track" ;;
esac
grep -q "^HAProxy: .*, the release's default)$" <<< "$VERSION_OUT" || fail "the updated node doesn't say its HAProxy is the default branch"
NEW_KERNEL="$(ctl system info | awk '/^kernel version:/ {print $3}')"
grep -q "^Kernel track: $default_track (version $NEW_KERNEL, " <<< "$VERSION_OUT" || fail "running kernel $NEW_KERNEL isn't the $default_track track's"
echo "  ok: updated to the $default_track track's kernel $NEW_KERNEL (was $OLD_KERNEL), STATE kept, confirmed healthy"

ctl lifecycle rollback | tail -2
wait_boot 3 "$REBOOT_TIMEOUT_SECS"
[ "$(ctl system info | awk '/^kernel version:/ {print $3}')" = "$OLD_KERNEL" ] || fail "the rollback didn't bring $RELEASE's kernel back"
[ "$(ctl version | awk '/^Node:/ {print $2}')" = "$RUNNING" ] || fail "the rollback didn't bring $RELEASE back"
echo "  ok: rolled back to $RELEASE (kernel $OLD_KERNEL)"

if grep -aq "avc:.*denied" "$LOG"; then
  grep -a "avc:.*denied" "$LOG" >&2
  fail "AVC denials"
fi
echo "Upgrade-from-release test OK: $RELEASE -> this tree's default image ($default_track kernel $NEW_KERNEL) and back, STATE kept, zero AVC denials"
