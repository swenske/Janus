#!/usr/bin/env bash
# The A/B slots' size is the disk's, and an update never writes what a
# slot can't hold or what the UKI doesn't pin (#495; security audit
# 2026-09-30 #16). A v2 bundle whose root filesystem is bigger than 64
# MiB (a layer of random bytes) goes to two nodes:
#
#   1. this tree's layout, 128 MiB slots, a node with 384 MiB of memory:
#      - `version` says "Slot size: 128 MiB";
#      - a copy of v2 with one byte changed past its first MiB, over
#        http, is refused part way: the node keeps running, its other
#        slot holds neither release, and Rollback refuses that slot;
#      - the same damaged copy uploaded (into the slot) is refused, saying
#        the slot holds that upload;
#      - v2 uploaded (upload-release: the root filesystem goes straight
#        into the slot, never held whole in memory) and installed: the
#        node reboots into it;
#      - Rollback: slot A checked against its kernel image, accepted,
#        the node back on v1.
#   2. the older layout, 64 MiB slots (JANUS_SLOT_MB=64): `version` says
#      64 MiB; v2 over http is refused before anything is written, its
#      upload too (janusctl sends the file's size); no reboot.
#
# Usage: hack/qemu-lifecycle-slots-test.sh <bzImage> <build-dir> <janusctl-bin>
# <build-dir> is the Makefile's $(BUILD_DIR): rootfs/ (this tree's v1
# rootfs, its state.img) and init/janusd/haproxy/selinux/ca-certificates
# to build v2 from.
set -euo pipefail
# shellcheck source=state-read.sh
. "$(dirname "$0")/state-read.sh"

export PATH="$PATH:/usr/sbin:/sbin"

KERNEL="${1:?usage: $0 <bzImage> <build-dir> <janusctl-bin>}"
BUILD_DIR="${2:?usage: $0 <bzImage> <build-dir> <janusctl-bin>}"
CTL="${3:?usage: $0 <bzImage> <build-dir> <janusctl-bin>}"
HTTP_TIMEOUT_SECS="${QEMU_SLOTS_HTTP_TIMEOUT:-60}"
REBOOT_TIMEOUT_SECS="${QEMU_SLOTS_REBOOT_TIMEOUT:-90}"
MEMORY="${QEMU_SLOTS_MEMORY:-384M}"
HOST_PORT_8080="$((18450 + ${JANUS_TEST_PORT_OFFSET:-0}))"
HOST_GRPC_PORT="$((18451 + ${JANUS_TEST_PORT_OFFSET:-0}))"
BUNDLE_HTTP_PORT="$((18452 + ${JANUS_TEST_PORT_OFFSET:-0}))"
MARKER="JANUS_INIT_BOOT_OK"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf)" >&2; exit 1; }

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
BUILD_DIR="$(cd "$BUILD_DIR" && pwd)"
WORKDIR="$(mktemp -d)"
QEMU_PID=""
HTTP_SERVER_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  [ -n "$HTTP_SERVER_PID" ] && kill "$HTTP_SERVER_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT
fail() { echo "Slots test FAILED: $*" >&2; exit 1; }

# --- v2: this tree's binaries, plus 72 MiB of random bytes that no
# compression shrinks - a root filesystem bigger than a 64 MiB slot ---
mkdir -p "$WORKDIR/padding/usr/share/janus-test"
head -c $((72 << 20)) /dev/urandom > "$WORKDIR/padding/usr/share/janus-test/padding.bin"
tar -C "$WORKDIR/padding" -cf "$WORKDIR/padding.tar" usr
V2_VERSION="v2099.02.02"
cat > "$WORKDIR/image-v2.json" <<EOF
{"schematic": {"customization": {}}, "schematic_id": "a055fbb697e2d0abb0c5911e7702b07040f49eb71befeaf9b90495a905327f47",
 "version": "$V2_VERSION", "arch": "amd64", "haproxy": {"variant": "3.4", "version": "3.4.99", "default": true}}
EOF
V2_ROOTFS="$WORKDIR/rootfs-v2"
mkdir -p "$V2_ROOTFS"
JANUS_IMAGE_INFO="$WORKDIR/image-v2.json" JANUS_EXTENSIONS="$WORKDIR/padding.tar" \
  "$SELF_DIR/../rootfs/assemble.sh" "$V2_ROOTFS" "$BUILD_DIR/init" "$BUILD_DIR/janusd" \
  "$BUILD_DIR/haproxy" "$SELF_DIR/../rootfs/base/etc/haproxy/haproxy.cfg" "$BUILD_DIR/selinux/janus.policy" \
  "$BUILD_DIR/ca-certificates/ca-certificates.crt" > "$WORKDIR/rootfs-v2.log"
V2_BUNDLE="$WORKDIR/bundle-v2"
"$SELF_DIR/../image/release/assemble.sh" "$V2_BUNDLE" "$KERNEL" "$V2_ROOTFS" > "$WORKDIR/bundle-v2.log"
V2_BYTES="$(stat -c %s "$V2_BUNDLE/rootfs.squashfs")"
[ "$V2_BYTES" -gt $((64 << 20)) ] || fail "v2's root filesystem is $V2_BYTES bytes, not over 64 MiB"
V1_HASH="$(cat "$BUILD_DIR/rootfs/rootfs.roothash")"
V2_HASH="$(cat "$V2_BUNDLE/rootfs.roothash")"
echo "v2: rootfs.squashfs $((V2_BYTES >> 20)) MiB, root hash $V2_HASH"

# Served over http: v2, and v2 with one byte changed 2 MiB in - past the
# first MiB janusd buffers, so part of it reaches the slot.
mkdir -p "$WORKDIR/srv/bad"
ln -s "$V2_BUNDLE" "$WORKDIR/srv/v2"
ln -s "$V2_BUNDLE/uki-a.efi" "$V2_BUNDLE/uki-b.efi" "$V2_BUNDLE/rootfs.verity" "$WORKDIR/srv/bad/"
cp "$V2_BUNDLE/rootfs.squashfs" "$WORKDIR/srv/bad/rootfs.squashfs"
python3 -c 'import sys; f = open(sys.argv[1], "r+b"); f.seek(2 * 1048576 + 100); b = f.read(1); f.seek(-1, 1); f.write(bytes([b[0] ^ 0xff]))' "$WORKDIR/srv/bad/rootfs.squashfs"
( cd "$WORKDIR/srv" && exec python3 -m http.server "$BUNDLE_HTTP_PORT" --bind 0.0.0.0 >"$WORKDIR/http-server.log" 2>&1 ) &
HTTP_SERVER_PID=$!
BUNDLE_URL="http://10.0.2.2:${BUNDLE_HTTP_PORT}"

boot_disk() {
  local disk="$1" log="$2"
  local vars="$WORKDIR/OVMF_VARS-$(basename "$log").fd"
  cp "$OVMF_VARS_TEMPLATE" "$vars"
  qemu-system-x86_64 -accel kvm -accel tcg \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$vars" \
    -drive file="$disk",format=raw,if=virtio \
    -nographic -display none -m "$MEMORY" \
    -netdev "user,id=net0,hostfwd=tcp::${HOST_PORT_8080}-:8080,hostfwd=tcp::${HOST_GRPC_PORT}-:9505" \
    -device virtio-net-pci,netdev=net0 \
    -serial file:"$log" </dev/null &
  QEMU_PID=$!
}
stop_qemu() {
  kill "$QEMU_PID" 2>/dev/null || true
  wait "$QEMU_PID" 2>/dev/null || true
  QEMU_PID=""
}
# HTTP 200 and at least $3 boots: the node answers for a moment after a
# reboot was asked.
wait_boot() {
  local log="$1" want="$2" deadline=$((SECONDS + $3)) code
  while [ "$SECONDS" -lt "$deadline" ]; do
    code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_PORT_8080}/" || true)"
    if [ "$code" = 200 ] && [ "$(grep -ac "$MARKER" "$log" || true)" -ge "$want" ] && grep -aq "listening on" "$log"; then
      return 0
    fi
    sleep 1
  done
  return 1
}
boots() { grep -ac "$MARKER" "$1" || true; }
last_cmdline() { grep -a "^Kernel command line:" "$1" | tail -1; }
assert_boot() {
  local log="$1" slot="$2" hash="$3" what="$4" line
  line="$(last_cmdline "$log")"
  case "$line" in *"verity 1 PARTLABEL=BOOT-$slot-DATA PARTLABEL=BOOT-$slot-HASH "*"$hash"*) ;;
    *) fail "$what: the last boot isn't slot $slot with root hash $hash: $line" ;; esac
}
no_avc() { if grep -aq "avc:  denied" "$1"; then grep -a "avc:  denied" "$1" >&2; fail "AVC denials on $2"; fi; }
# Runs janusctl, wants it refused with every given text in its output,
# and nothing written: no reboot, the boot count unchanged.
refused() {
  local log="$1" what="$2" out rc before
  shift 2
  before="$(boots "$log")"
  set +e
  out="$("$CTL" "${CTL_ARGS[@]}" "${CMD[@]}" 2>&1)"
  rc=$?
  set -e
  echo "$out" | sed 's/^/    /'
  [ "$rc" -ne 0 ] || fail "$what: accepted"
  for want in "$@"; do
    grep -qF -- "$want" <<<"$out" || fail "$what: no \"$want\" in its refusal"
  done
  sleep 5
  [ "$(boots "$log")" = "$before" ] || fail "$what: the node rebooted"
  echo "  ok: $what refused"
}

# ==== 1. 128 MiB slots ====
DISK="$WORKDIR/disk-128.img"
cp --sparse=always "$BUILD_DIR/rootfs/disk.img" "$DISK"
LOG="$WORKDIR/boot-128.log"
boot_disk "$DISK" "$LOG"
wait_boot "$LOG" 1 "$HTTP_TIMEOUT_SECS" || { cat "$LOG" >&2; fail "the 128 MiB node never came up"; }
state_pki "$DISK" "$WORKDIR/pki-128" || fail "couldn't read the 128 MiB node's PKI"
CTL_ARGS=(-endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/pki-128/ca.crt" -cert "$WORKDIR/pki-128/admin.crt" -key "$WORKDIR/pki-128/admin.key")
grep -q "^Slot size: 128 MiB$" <<<"$("$CTL" "${CTL_ARGS[@]}" version)" || fail "version doesn't say 128 MiB slots"
echo "128 MiB node up ($MEMORY of memory), version says so"

CMD=(lifecycle upgrade -insecure-skip-signature-check "$BUNDLE_URL/bad")
refused "$LOG" "v2 with a byte changed 2 MiB in" "rootfs.squashfs: " "block 512 " "no longer holds its previous release" "Rollback won't switch to it"
# The inactive slot's state settles a moment after the refused upload's
# RPC returns; poll rather than read it once (a CI flake, 2026-11-05).
for _ in $(seq 1 20); do
  grep -q "^Other slot: B - .*refused" <<<"$("$CTL" "${CTL_ARGS[@]}" version)" && break
  sleep 0.5
done
grep -q "^Other slot: B - .*refused" <<<"$("$CTL" "${CTL_ARGS[@]}" version)" || fail "version doesn't say slot B holds a refused update: $("$CTL" "${CTL_ARGS[@]}" version | grep '^Other slot')"
CMD=(lifecycle rollback)
refused "$LOG" "Rollback to the half-written slot B" "slot B doesn't hold what its kernel image boots" "update it again"

# The same damaged bundle uploaded: the root filesystem goes into slot B
# first, so the refusal says the slot holds that upload - not that
# nothing was written (#512's finding 2).
BAD_STAGING="$("$CTL" "${CTL_ARGS[@]}" lifecycle upload-release "$WORKDIR/srv/bad" | grep -o 'staged at [^ ]*' | awk '{print $3}' | head -1)"
[ -n "$BAD_STAGING" ] || fail "upload-release of the damaged bundle didn't say where it staged"
CMD=(lifecycle upgrade -insecure-skip-signature-check "$BAD_STAGING")
refused "$LOG" "the damaged bundle uploaded into slot B" "block 512 " "Slot B holds that upload" "Rollback won't switch to it"
if grep -q "nothing was written" <<<"$("$CTL" "${CTL_ARGS[@]}" "${CMD[@]}" 2>&1)"; then fail "the uploaded bundle's refusal says nothing was written"; fi

UPLOAD_OUT="$("$CTL" "${CTL_ARGS[@]}" lifecycle upload-release "$V2_BUNDLE")"
STAGING_DIR="$(grep -o 'staged at [^ ]*' <<<"$UPLOAD_OUT" | awk '{print $3}' | head -1)"
[ -n "$STAGING_DIR" ] || fail "upload-release didn't say where it staged: $UPLOAD_OUT"
UPGRADE_OUT="$("$CTL" "${CTL_ARGS[@]}" lifecycle upgrade -insecure-skip-signature-check -sha256 "$(cut -d' ' -f1 "$V2_BUNDLE/rootfs.squashfs.sha256")" "$STAGING_DIR")"
echo "$UPGRADE_OUT" | sed 's/^/    /'
grep -q "checking the rootfs.squashfs uploaded into slot B" <<<"$UPGRADE_OUT" || fail "the upload wasn't checked in the slot it went into"
grep -q "rootfs.squashfs: Janus $V2_VERSION" <<<"$UPGRADE_OUT" || fail "the update didn't say it installs $V2_VERSION"
wait_boot "$LOG" 2 "$REBOOT_TIMEOUT_SECS" || { tail -50 "$LOG" >&2; fail "no boot into v2"; }
assert_boot "$LOG" B "$V2_HASH" "after the update"
echo "  ok: v2 ($((V2_BYTES >> 20)) MiB) uploaded into slot B, checked there, booted"

"$CTL" "${CTL_ARGS[@]}" lifecycle rollback >/dev/null
wait_boot "$LOG" 3 "$REBOOT_TIMEOUT_SECS" || { tail -50 "$LOG" >&2; fail "no boot back into v1"; }
assert_boot "$LOG" A "$V1_HASH" "after Rollback"
echo "  ok: Rollback checked slot A, booted it"
no_avc "$LOG" "the 128 MiB node"
stop_qemu

# ==== 2. 64 MiB slots ====
DISK="$WORKDIR/disk-64.img"
cp "$BUILD_DIR/rootfs/state.img" "$WORKDIR/state-64.img"
JANUS_SLOT_MB=64 "$SELF_DIR/../image/disk/assemble.sh" "$DISK" "$KERNEL" "$BUILD_DIR/rootfs" "$WORKDIR/state-64.img" > "$WORKDIR/disk-64.log"
LOG="$WORKDIR/boot-64.log"
boot_disk "$DISK" "$LOG"
wait_boot "$LOG" 1 "$HTTP_TIMEOUT_SECS" || { cat "$LOG" >&2; fail "the 64 MiB node never came up"; }
state_pki "$DISK" "$WORKDIR/pki-64" || fail "couldn't read the 64 MiB node's PKI"
CTL_ARGS=(-endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/pki-64/ca.crt" -cert "$WORKDIR/pki-64/admin.crt" -key "$WORKDIR/pki-64/admin.key")
grep -q "^Slot size: 64 MiB$" <<<"$("$CTL" "${CTL_ARGS[@]}" version)" || fail "version doesn't say 64 MiB slots"
echo "64 MiB node up, version says so"

CMD=(lifecycle upgrade -insecure-skip-signature-check "$BUNDLE_URL/v2")
refused "$LOG" "v2 over http on 64 MiB slots" "this release's root filesystem is " "this node's slots are 64 MiB" "recycle it"
CMD=(lifecycle upload-release "$V2_BUNDLE")
refused "$LOG" "v2's upload on 64 MiB slots" "rootfs.squashfs is " "this node's slots are 64 MiB"
OTHER="$("$CTL" "${CTL_ARGS[@]}" version | grep '^Other slot')"
if grep -qE "refused|upload|failed|being written" <<<"$OTHER"; then fail "slot B was touched: $OTHER"; fi
CMD=(lifecycle rollback)
"$CTL" "${CTL_ARGS[@]}" "${CMD[@]}" >/dev/null
wait_boot "$LOG" 2 "$REBOOT_TIMEOUT_SECS" || { tail -50 "$LOG" >&2; fail "Rollback on the untouched 64 MiB node didn't boot"; }
assert_boot "$LOG" B "$V1_HASH" "after Rollback on 64 MiB slots"
echo "  ok: slot B untouched - Rollback checked it and booted it"
no_avc "$LOG" "the 64 MiB node"

echo "Slots test OK: 128 MiB slots take a $((V2_BYTES >> 20)) MiB root filesystem streamed into them with $MEMORY of memory, a bundle changed part way is refused and its slot with it, Rollback checks its slot; 64 MiB slots refuse it before writing anything, zero AVC denials"
