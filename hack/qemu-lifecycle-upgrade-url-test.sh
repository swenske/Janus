#!/usr/bin/env bash
# Node-initiated-fetch follow-up (2026-09-29, user-requested remote-
# update work): proves LifecycleService.Upgrade's new http(s):// mode
# (internal/api/lifecycle.go's fetchBundleFile) actually works over a
# real network fetch, not just that local-path mode still does
# (hack/qemu-lifecycle-upgrade-test.sh's own job, unchanged and still
# the primary Upgrade proof - this script exists specifically to add
# coverage for the *other* half of ImageSource.reference: an http(s)://
# base URL, completing what the proto comment always described).
#
# Mirrors hack/qemu-lifecycle-upgrade-test.sh's own structure closely
# (same v2-rootfs-with-a-different-port trick, same "verify via the
# kernel cmdline's root hash, not which HTTP port answers" reasoning -
# see that script's own header for why) but serves the v2 release
# bundle over real HTTP from the host instead of injecting it into
# STATE via debugfs: a plain `python3 -m http.server` bound on the
# host, reachable from the guest at 10.0.2.2 (QEMU usermode/slirp's
# fixed host-gateway address - the same address hack/
# qemu-self-register-test.sh already proved this exact
# guest-connects-back-to-a-host-listener pattern works with).
#
# Usage: hack/qemu-lifecycle-upgrade-url-test.sh <disk.img> <bzImage> <build-dir> <janusctl-bin>
# Same argument shape as hack/qemu-lifecycle-upgrade-test.sh.
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

DISK="${1:?usage: $0 <disk.img> <bzImage> <build-dir> <janusctl-bin>}"
KERNEL="${2:?usage: $0 <disk.img> <bzImage> <build-dir> <janusctl-bin>}"
BUILD_DIR="${3:?usage: $0 <disk.img> <bzImage> <build-dir> <janusctl-bin>}"
CTL="${4:?usage: $0 <disk.img> <bzImage> <build-dir> <janusctl-bin>}"
HTTP_TIMEOUT_SECS="${QEMU_UPGRADE_HTTP_TIMEOUT:-40}"
REBOOT_TIMEOUT_SECS="${QEMU_UPGRADE_REBOOT_TIMEOUT:-60}"
HOST_PORT_8080="${QEMU_UPGRADE_TEST_PORT:-18192}"
HOST_GRPC_PORT="${QEMU_UPGRADE_GRPC_PORT:-18194}"
BUNDLE_HTTP_PORT="${QEMU_UPGRADE_BUNDLE_HTTP_PORT:-18195}"
MARKER="JANUS_INIT_BOOT_OK"
FIRST_BOOT_MSG="pki: first boot - generated a new CA"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf) - set \$OVMF_CODE to override" >&2; exit 1; }
[ -f "$OVMF_VARS_TEMPLATE" ] || { echo "OVMF vars template not found at $OVMF_VARS_TEMPLATE - set \$OVMF_VARS_TEMPLATE to override" >&2; exit 1; }

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
WORKDIR="$(mktemp -d)"
QEMU_PID=""
HTTP_SERVER_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  [ -n "$HTTP_SERVER_PID" ] && kill "$HTTP_SERVER_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

# --- build a genuinely different v2 rootfs + release bundle, same as
# hack/qemu-lifecycle-upgrade-test.sh's own v2 (:8081 instead of :8080,
# so a real, different root hash exists to verify against - see that
# script's header for why the *port* itself is never the real proof
# here, only used to force genuinely different squashfs content) ---
cat > "$WORKDIR/haproxy-v2.cfg" <<'EOF'
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
    bind *:8081
    http-request return status 200 content-type text/plain string "Janus: v2 (url-fetched) is up\n"
EOF

V2_ROOTFS="$WORKDIR/rootfs-v2"
mkdir -p "$V2_ROOTFS"
"$SELF_DIR/../rootfs/assemble.sh" "$V2_ROOTFS" "$BUILD_DIR/init" "$BUILD_DIR/janusd" \
  "$BUILD_DIR/haproxy" "$WORKDIR/haproxy-v2.cfg" "$BUILD_DIR/selinux/janus.policy" \
  "$BUILD_DIR/ca-certificates/ca-certificates.crt"

V2_BUNDLE="$WORKDIR/bundle-v2"
"$SELF_DIR/../image/release/assemble.sh" "$V2_BUNDLE" "$KERNEL" "$V2_ROOTFS"

# --- serve the v2 bundle over real HTTP from the host - bound to all
# interfaces so slirp's NAT can actually reach it from the guest side,
# not just 127.0.0.1 ---
( cd "$V2_BUNDLE" && exec python3 -m http.server "$BUNDLE_HTTP_PORT" --bind 0.0.0.0 >"$WORKDIR/http-server.log" 2>&1 ) &
HTTP_SERVER_PID=$!
sleep 1
if ! kill -0 "$HTTP_SERVER_PID" 2>/dev/null; then
  echo "Upgrade URL test FAILED: the local bundle HTTP server never started" >&2
  cat "$WORKDIR/http-server.log" >&2
  exit 1
fi
BUNDLE_URL="http://10.0.2.2:${BUNDLE_HTTP_PORT}"
echo "Serving v2 release bundle at $BUNDLE_URL (host-side python3 http.server)"

# --- boot slot A (v1, :8080) for real ---
boot_disk() {
  local log="$1"
  local ovmf_vars="$WORKDIR/OVMF_VARS-$(basename "$log").fd"
  cp "$OVMF_VARS_TEMPLATE" "$ovmf_vars"

  qemu-system-x86_64 \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$ovmf_vars" \
    -drive file="$DISK",format=raw,if=virtio \
    -nographic -display none -m 512M \
    -netdev "user,id=net0,hostfwd=tcp::${HOST_PORT_8080}-:8080,hostfwd=tcp::${HOST_GRPC_PORT}-:9505" \
    -device virtio-net-pci,netdev=net0 \
    -serial file:"$log" \
    &
  QEMU_PID=$!
}

wait_http() {
  local port="$1" timeout_secs="$2" code=""
  local deadline=$((SECONDS + timeout_secs))
  while [ "$SECONDS" -lt "$deadline" ]; do
    code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${port}/" || true)"
    [ "$code" = "200" ] && return 0
    sleep 1
  done
  return 1
}

wait_http_and_marker() {
  local port="$1" log="$2" want_markers="$3" timeout_secs="$4" code=""
  local deadline=$((SECONDS + timeout_secs))
  while [ "$SECONDS" -lt "$deadline" ]; do
    code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${port}/" || true)"
    if [ "$code" = "200" ] && [ "$(grep -c "$MARKER" "$log" 2>/dev/null || true)" -ge "$want_markers" ]; then
      return 0
    fi
    sleep 1
  done
  return 1
}

V1_HASH="$(cat "$BUILD_DIR/rootfs/rootfs.roothash")"
V2_HASH="$(cat "$V2_BUNDLE/rootfs.roothash")"
assert_last_boot() {
  local log="$1" data_dev="$2" hash_dev="$3" want_hash="$4" label="$5"
  local line
  line="$(grep "^Kernel command line:" "$log" | tail -1)"
  if [ -z "$line" ]; then
    echo "Upgrade URL test FAILED: $label - no 'Kernel command line:' line found in the console log at all" >&2
    echo "--- console output ---" >&2; cat "$log" >&2
    exit 1
  fi
  case "$line" in
    *"verity 1 $data_dev $hash_dev "*) : ;;
    *) echo "Upgrade URL test FAILED: $label - last boot's cmdline doesn't reference $data_dev/$hash_dev: $line" >&2; exit 1 ;;
  esac
  case "$line" in
    *"$want_hash"*) : ;;
    *) echo "Upgrade URL test FAILED: $label - last boot's cmdline doesn't carry root hash $want_hash: $line" >&2; exit 1 ;;
  esac
}

A_LOG="$WORKDIR/boot-a.log"
boot_disk "$A_LOG"
if ! wait_http "$HOST_PORT_8080" "$HTTP_TIMEOUT_SECS"; then
  echo "Upgrade URL test FAILED: slot A (v1, :8080) never answered HTTP 200 within ${HTTP_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2; cat "$A_LOG" >&2
  exit 1
fi
if ! grep -q "$FIRST_BOOT_MSG" "$A_LOG"; then
  echo "Upgrade URL test FAILED: slot A didn't log '$FIRST_BOOT_MSG' - expected a fresh bootstrap" >&2
  exit 1
fi
assert_last_boot "$A_LOG" /dev/vda2 /dev/vda3 "$V1_HASH" "initial boot"
echo "Slot A (v1) OK: real UEFI boot, HTTP 200 on :8080, PKI bootstrapped, root hash $V1_HASH confirmed"

# --- extract PKI material straight from disk.img's STATE partition ---
STATE_START_SECTOR="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
STATE_SIZE_SECTORS="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
STATE_IMG="$WORKDIR/state.img"
dd if="$DISK" of="$STATE_IMG" bs=512 skip="$STATE_START_SECTOR" count="$STATE_SIZE_SECTORS" status=none
for f in ca.crt admin.crt admin.key; do
  debugfs -R "dump pki/$f $WORKDIR/$f" "$STATE_IMG" >/dev/null 2>&1
  [ -s "$WORKDIR/$f" ] || { echo "Upgrade URL test FAILED: couldn't extract pki/$f from disk.img's STATE partition after slot A booted" >&2; exit 1; }
done

# --- call Upgrade for real, referencing the *URL*, not a local path -
# this is the one line that actually differs from
# hack/qemu-lifecycle-upgrade-test.sh's own equivalent call ---
V2_SHA256="$(cat "$V2_BUNDLE/rootfs.squashfs.sha256")"
UPGRADE_OUT="$("$CTL" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" lifecycle upgrade -insecure-skip-signature-check -sha256 "$V2_SHA256" "$BUNDLE_URL")"
echo "$UPGRADE_OUT"
if ! echo "$UPGRADE_OUT" | grep -qi "downloading"; then
  echo "Upgrade URL test FAILED: janusctl lifecycle upgrade never reported a 'downloading' stage - did fetchBundleFile take the local-path branch instead of the URL one?" >&2
  exit 1
fi
if ! echo "$UPGRADE_OUT" | grep -qi "rebooting"; then
  echo "Upgrade URL test FAILED: janusctl lifecycle upgrade never reached the 'rebooting' stage" >&2
  exit 1
fi

if ! wait_http_and_marker "$HOST_PORT_8080" "$A_LOG" 2 "$REBOOT_TIMEOUT_SECS"; then
  echo "Upgrade URL test FAILED: no healthy, genuinely-rebooted HTTP 200 on :8080 within ${REBOOT_TIMEOUT_SECS}s - did the guest actually reboot into the upgraded slot?" >&2
  echo "--- console output ---" >&2; cat "$A_LOG" >&2
  exit 1
fi
if [ "$(grep -c "$FIRST_BOOT_MSG" "$A_LOG")" -ne 1 ]; then
  echo "Upgrade URL test FAILED: '$FIRST_BOOT_MSG' appeared $(grep -c "$FIRST_BOOT_MSG" "$A_LOG" 2>/dev/null || echo 0) times, want exactly 1 - STATE didn't survive the upgrade-triggered reboot" >&2
  echo "--- console output ---" >&2; cat "$A_LOG" >&2
  exit 1
fi
assert_last_boot "$A_LOG" /dev/vda4 /dev/vda5 "$V2_HASH" "post-upgrade boot"
echo "Slot B (v2) OK: real gRPC Upgrade fetched the release bundle over real HTTP ($BUNDLE_URL) and wrote it (root hash $V2_HASH, /dev/vda4+/dev/vda5), switched, and rebooted into it - HTTP healthy, STATE intact"
echo "Upgrade URL test OK: LifecycleService.Upgrade's http(s):// fetch mode works end to end against a real HTTP server, not just local-path mode"
