#!/usr/bin/env bash
# Controller-relay follow-up (2026-09-29, user-requested remote-update
# work, the mode for network topologies where a node can't dial out at
# all): proves the full relay chain end to end - a real
# LifecycleService.UploadReleaseFile call streams a release bundle's 4
# files to a live, already-booted node over mTLS (the same direction
# every other RPC in this project already dials, no new network path
# needed), then a real LifecycleService.Upgrade call, pointed at the
# returned staging directory, installs it into the inactive slot and
# reboots - reusing Upgrade's own already-verified writing/slot-
# switching/health-check logic completely unchanged (see hack/
# qemu-lifecycle-upgrade-test.sh/-url-test.sh for that same proof via
# the other two source modes: local path and node-initiated http(s)://
# fetch).
#
# Mirrors hack/qemu-lifecycle-upgrade-url-test.sh's own structure
# closely (same v2-rootfs-with-a-different-port trick, same "verify via
# the kernel cmdline's root hash, not which HTTP port answers"
# reasoning - see that script's header) but has no HTTP server at all:
# janusctl lifecycle upload-release streams the v2 bundle's files
# straight from the *host's* own local copy, over the exact same mTLS
# connection janusctl already uses for every other call - proving a
# node with literally zero outbound connectivity could still be updated
# this way, since nothing here relies on the guest reaching anything
# other than answering the controller's own inbound gRPC calls.
#
# Usage: hack/qemu-lifecycle-upgrade-relay-test.sh <disk.img> <bzImage> <build-dir> <janusctl-bin>
# Same argument shape as hack/qemu-lifecycle-upgrade-test.sh.
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

DISK="${1:?usage: $0 <disk.img> <bzImage> <build-dir> <janusctl-bin>}"
KERNEL="${2:?usage: $0 <disk.img> <bzImage> <build-dir> <janusctl-bin>}"
BUILD_DIR="${3:?usage: $0 <disk.img> <bzImage> <build-dir> <janusctl-bin>}"
CTL="${4:?usage: $0 <disk.img> <bzImage> <build-dir> <janusctl-bin>}"
HTTP_TIMEOUT_SECS="${QEMU_UPGRADE_HTTP_TIMEOUT:-40}"
REBOOT_TIMEOUT_SECS="${QEMU_UPGRADE_REBOOT_TIMEOUT:-60}"
HOST_PORT_8080="${QEMU_UPGRADE_TEST_PORT:-$((18292 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
HOST_GRPC_PORT="${QEMU_UPGRADE_GRPC_PORT:-$((18294 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
MARKER="JANUS_INIT_BOOT_OK"
FIRST_BOOT_MSG="pki: first boot - generated a new CA"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf) - set \$OVMF_CODE to override" >&2; exit 1; }
[ -f "$OVMF_VARS_TEMPLATE" ] || { echo "OVMF vars template not found at $OVMF_VARS_TEMPLATE - set \$OVMF_VARS_TEMPLATE to override" >&2; exit 1; }

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

# --- build a genuinely different v2 rootfs + release bundle, same as
# the other two upgrade tests' own v2 (:8081 instead of :8080) ---
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
    http-request return status 200 content-type text/plain string "Janus: v2 (relay-uploaded) is up\n"
EOF

V2_ROOTFS="$WORKDIR/rootfs-v2"
mkdir -p "$V2_ROOTFS"
"$SELF_DIR/../rootfs/assemble.sh" "$V2_ROOTFS" "$BUILD_DIR/init" "$BUILD_DIR/janusd" \
  "$BUILD_DIR/haproxy" "$WORKDIR/haproxy-v2.cfg" "$BUILD_DIR/selinux/janus.policy" \
  "$BUILD_DIR/ca-certificates/ca-certificates.crt"

V2_BUNDLE="$WORKDIR/bundle-v2"
"$SELF_DIR/../image/release/assemble.sh" "$V2_BUNDLE" "$KERNEL" "$V2_ROOTFS"

# --- boot slot A (v1, :8080) for real ---
boot_disk() {
  local log="$1"
  local ovmf_vars="$WORKDIR/OVMF_VARS-$(basename "$log").fd"
  cp "$OVMF_VARS_TEMPLATE" "$ovmf_vars"

  qemu-system-x86_64 -accel kvm -accel tcg \
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
    echo "Upgrade relay test FAILED: $label - no 'Kernel command line:' line found in the console log at all" >&2
    echo "--- console output ---" >&2; cat "$log" >&2
    exit 1
  fi
  case "$line" in
    *"verity 1 $data_dev $hash_dev "*) : ;;
    *) echo "Upgrade relay test FAILED: $label - last boot's cmdline doesn't reference $data_dev/$hash_dev: $line" >&2; exit 1 ;;
  esac
  case "$line" in
    *"$want_hash"*) : ;;
    *) echo "Upgrade relay test FAILED: $label - last boot's cmdline doesn't carry root hash $want_hash: $line" >&2; exit 1 ;;
  esac
}

A_LOG="$WORKDIR/boot-a.log"
boot_disk "$A_LOG"
if ! wait_http "$HOST_PORT_8080" "$HTTP_TIMEOUT_SECS"; then
  echo "Upgrade relay test FAILED: slot A (v1, :8080) never answered HTTP 200 within ${HTTP_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2; cat "$A_LOG" >&2
  exit 1
fi
if ! grep -q "$FIRST_BOOT_MSG" "$A_LOG"; then
  echo "Upgrade relay test FAILED: slot A didn't log '$FIRST_BOOT_MSG' - expected a fresh bootstrap" >&2
  exit 1
fi
assert_last_boot "$A_LOG" PARTLABEL=BOOT-A-DATA PARTLABEL=BOOT-A-HASH "$V1_HASH" "initial boot"
echo "Slot A (v1) OK: real UEFI boot, HTTP 200 on :8080, PKI bootstrapped, root hash $V1_HASH confirmed"

# --- extract PKI material straight from disk.img's STATE partition ---
STATE_START_SECTOR="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
STATE_SIZE_SECTORS="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
STATE_IMG="$WORKDIR/state.img"
dd if="$DISK" of="$STATE_IMG" bs=512 skip="$STATE_START_SECTOR" count="$STATE_SIZE_SECTORS" status=none
for f in ca.crt admin.crt admin.key; do
  debugfs -R "dump pki/$f $WORKDIR/$f" "$STATE_IMG" >/dev/null 2>&1
  [ -s "$WORKDIR/$f" ] || { echo "Upgrade relay test FAILED: couldn't extract pki/$f from disk.img's STATE partition after slot A booted" >&2; exit 1; }
done
CTL_ARGS=(-endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key")

# --- relay the v2 bundle to the live node over mTLS - the one step
# that actually differs from hack/qemu-lifecycle-upgrade-test.sh's own
# equivalent local-path call ---
UPLOAD_OUT="$("$CTL" "${CTL_ARGS[@]}" lifecycle upload-release "$V2_BUNDLE")"
echo "$UPLOAD_OUT"
for f in rootfs.squashfs rootfs.verity uki-a.efi uki-b.efi; do
  echo "$UPLOAD_OUT" | grep -q "Uploaded $f" || { echo "Upgrade relay test FAILED: upload-release never reported uploading $f" >&2; exit 1; }
done
STAGING_DIR="$(echo "$UPLOAD_OUT" | grep -o 'staged at [^ ]*' | awk '{print $3}')"
[ -n "$STAGING_DIR" ] || { echo "Upgrade relay test FAILED: couldn't parse the staging directory out of upload-release's own output" >&2; exit 1; }
echo "Relay upload OK: v2 release bundle streamed to the live node's own staging area at $STAGING_DIR"

# --- now call Upgrade against that staging path, exactly like local-
# path mode already works ---
V2_SHA256="$(cat "$V2_BUNDLE/rootfs.squashfs.sha256")"
UPGRADE_OUT="$("$CTL" "${CTL_ARGS[@]}" lifecycle upgrade -insecure-skip-signature-check -sha256 "$V2_SHA256" "$STAGING_DIR")"
echo "$UPGRADE_OUT"
if ! echo "$UPGRADE_OUT" | grep -qi "rebooting"; then
  echo "Upgrade relay test FAILED: janusctl lifecycle upgrade never reached the 'rebooting' stage" >&2
  exit 1
fi

if ! wait_http_and_marker "$HOST_PORT_8080" "$A_LOG" 2 "$REBOOT_TIMEOUT_SECS"; then
  echo "Upgrade relay test FAILED: no healthy, genuinely-rebooted HTTP 200 on :8080 within ${REBOOT_TIMEOUT_SECS}s - did the guest actually reboot into the upgraded slot?" >&2
  echo "--- console output ---" >&2; cat "$A_LOG" >&2
  exit 1
fi
if [ "$(grep -c "$FIRST_BOOT_MSG" "$A_LOG")" -ne 1 ]; then
  echo "Upgrade relay test FAILED: '$FIRST_BOOT_MSG' appeared $(grep -c "$FIRST_BOOT_MSG" "$A_LOG" 2>/dev/null || echo 0) times, want exactly 1 - STATE didn't survive the upgrade-triggered reboot" >&2
  echo "--- console output ---" >&2; cat "$A_LOG" >&2
  exit 1
fi
assert_last_boot "$A_LOG" PARTLABEL=BOOT-B-DATA PARTLABEL=BOOT-B-HASH "$V2_HASH" "post-upgrade boot"
echo "Slot B (v2) OK: real gRPC Upgrade installed the relay-uploaded rootfs (root hash $V2_HASH, BOOT-B-DATA/HASH), switched, and rebooted into it - HTTP healthy, STATE intact"
echo "Upgrade relay test OK: UploadReleaseFile + Upgrade work end to end as a two-step relay, with the node never dialing out anywhere"
