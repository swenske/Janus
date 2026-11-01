#!/usr/bin/env bash
# Proves the Bare-metal Machine tranche's embedded release bundle
# (image/iso/assemble.sh's optional [release-bundle-dir] arg,
# mounted read-only at boot by rootfs/init's mountReleaseBundle) is
# real, not just a file copy that looks right on inspection:
#
#   1. builds a real release bundle (image/release/assemble.sh) and a
#      real ISO with it embedded (image/iso/assemble.sh).
#   2. boots that ISO under real OVMF, as a raw virtio-blk drive - same
#      shape hack/qemu-iso-boot-test.sh already proves boots at all -
#      with a SECOND, blank virtio-blk drive attached as the Install
#      target (vdb).
#   3. pairs it with a fleet of janusctl's own (fleet init, then fleet
#      adopt -code with the pairing code its console shows: this medium
#      is ephemeral by design - see image/iso/assemble.sh's own header -
#      so it trusts nothing at each boot and opens its pairing window).
#   4. calls `janusctl lifecycle install /dev/vdb /etc/janus/release`
#      over real mTLS - the fleet's certificate - against the running
#      ISO instance, the fleet given to the installed disk (-fleet-root,
#      -fleet-bundle) - BUNDLE_DIR is
#      a path on the *server's* (the booted ISO's) own filesystem, not
#      the host running janusctl, which is exactly the point: nothing
#      but what's already on this medium is needed. -sha256 is passed
#      explicitly from the host's own local copy of the bundle (built
#      in step 1) - janusctl's own auto-detection reads BUNDLE_DIR
#      off its OWN (the host's) filesystem, which has no
#      /etc/janus/release at all, so it can't be relied on here (same
#      reasoning hack/qemu-lifecycle-upgrade-test.sh's own -sha256
#      arg already documents for the same host/guest split).
#   5. confirms Install's own final "done" stage actually appears -
#      proof the mounted release_t files were genuinely read and
#      written to /dev/vdb, not just that the RPC returned without
#      immediately erroring.
#
# Usage: hack/qemu-iso-install-test.sh <rootfs-dir> <bzImage> <janusctl-bin>
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

ROOTFS_DIR="${1:?usage: $0 <rootfs-dir> <bzImage> <janusctl-bin>}"
KERNEL="${2:?usage: $0 <rootfs-dir> <bzImage> <janusctl-bin>}"
CTL_REL="${3:?usage: $0 <rootfs-dir> <bzImage> <janusctl-bin>}"
CTL="$(cd "$(dirname "$CTL_REL")" && pwd)/$(basename "$CTL_REL")"

TARGET_DISK_MB="${ISO_INSTALL_TEST_DISK_MB:-512}"
HTTP_TIMEOUT_SECS="${ISO_INSTALL_TEST_HTTP_TIMEOUT:-40}"
HOST_PORT="${ISO_INSTALL_TEST_PORT:-$((18103 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
HOST_GRPC_PORT="${ISO_INSTALL_TEST_GRPC_PORT:-$((18104 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
HOST_PAIR_PORT="${ISO_INSTALL_TEST_PAIR_PORT:-$((18105 + ${JANUS_TEST_PORT_OFFSET:-0}))}"

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

# --- build the bundle + the ISO that embeds it -------------------------
BUNDLE="$WORKDIR/bundle"
"$SELF_DIR/../image/release/assemble.sh" "$BUNDLE" "$KERNEL" "$ROOTFS_DIR"
SHA256="$(cat "$BUNDLE/rootfs.squashfs.sha256")"

ISO="$WORKDIR/janus-bundled.iso"
"$SELF_DIR/../image/iso/assemble.sh" "$ISO" "$KERNEL" "$ROOTFS_DIR" "$BUNDLE"

TARGET_DISK="$WORKDIR/target-disk.img"
truncate -s "${TARGET_DISK_MB}M" "$TARGET_DISK"

OVMF_VARS="$WORKDIR/OVMF_VARS.fd"
cp "$OVMF_VARS_TEMPLATE" "$OVMF_VARS"

LOG="$WORKDIR/iso-install.log"
qemu-system-x86_64 -accel kvm -accel tcg \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$OVMF_VARS" \
  -drive file="$ISO",format=raw,if=virtio \
  -drive file="$TARGET_DISK",format=raw,if=virtio \
  -nographic -no-reboot -display none -m 512M \
  -netdev "user,id=net0,hostfwd=tcp::${HOST_PORT}-:8080,hostfwd=tcp::${HOST_GRPC_PORT}-:9505,hostfwd=tcp::${HOST_PAIR_PORT}-:9506" \
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
if [ "$code" != "200" ]; then
  echo "ISO install test FAILED: no HTTP 200 from HAProxy within ${HTTP_TIMEOUT_SECS}s (last code: '${code}')" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi

# --- pair it: a node that trusts nothing shows a pairing code on its
# console - the only way in, nothing secret printed (no STATE here: a
# new CA and a new code at every boot). janusd starts HAProxy before its
# PKI: wait for its banner, printed once its API listens.
until grep -aq "No shell, no SSH" "$LOG"; do
  [ "$SECONDS" -lt "$deadline" ] || { echo "ISO install test FAILED: janusd never listened" >&2; cat "$LOG" >&2; exit 1; }
  sleep 1
done
if grep -aq "PRIVATE KEY" "$LOG"; then
  echo "ISO install test FAILED: a private key on the console" >&2
  exit 1
fi
CODE="$({ grep -ao 'pairing: code [0-9A-Z-]*' "$LOG" || true; } | tail -1 | awk '{print $3}')"
[ -n "$CODE" ] || { echo "ISO install test FAILED: no pairing code on the console" >&2; cat "$LOG" >&2; exit 1; }
ctl() { JANUSCONFIG="$WORKDIR/janusctl.json" "$CTL" "$@"; }
ctl -context iso fleet init -name iso -issuer iso-test -user tester -yes "$WORKDIR/kit.age" >/dev/null 2>&1 \
  || { echo "ISO install test FAILED: fleet init" >&2; exit 1; }
ctl fleet adopt iso-node -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -pairing "127.0.0.1:${HOST_PAIR_PORT}" -code "$CODE" \
  || { echo "ISO install test FAILED: fleet adopt -code" >&2; exit 1; }
ctl fleet export "$WORKDIR/export" >/dev/null || { echo "ISO install test FAILED: fleet export" >&2; exit 1; }

CTL_ARGS=(-n iso-node)

# --- the actual point of this test: Install using only what's already
# on the booted ISO, no bundle reachable from the host at all.
INSTALL_OUT="$(ctl "${CTL_ARGS[@]}" lifecycle install -insecure-skip-signature-check -sha256 "$SHA256" \
  -fleet-root "$WORKDIR/export/root.crt" -fleet-bundle "$WORKDIR/export/bundle.json" /dev/vdb /etc/janus/release 2>&1)" || {
  echo "ISO install test FAILED: janusctl lifecycle install failed:" >&2
  echo "$INSTALL_OUT" >&2
  exit 1
}
echo "$INSTALL_OUT"
echo "$INSTALL_OUT" | grep -q "\[done 100%\]" || {
  echo "ISO install test FAILED: Install's own final 'done' stage never appeared" >&2
  exit 1
}

kill "$QEMU_PID" 2>/dev/null || true
wait "$QEMU_PID" 2>/dev/null || true
QEMU_PID=""

# --- the installed disk boots trusting the fleet: no pairing window, a
# new CA of its own - adopted by the fingerprint its console shows.
cp "$OVMF_VARS_TEMPLATE" "$OVMF_VARS"
LOG="$WORKDIR/installed.log"
qemu-system-x86_64 -accel kvm -accel tcg \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$OVMF_VARS" \
  -drive file="$TARGET_DISK",format=raw,if=virtio \
  -nographic -no-reboot -display none -m 512M \
  -netdev "user,id=net0,hostfwd=tcp::${HOST_PORT}-:8080,hostfwd=tcp::${HOST_GRPC_PORT}-:9505,hostfwd=tcp::${HOST_PAIR_PORT}-:9506" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG" \
  </dev/null &
QEMU_PID=$!
deadline=$((SECONDS + HTTP_TIMEOUT_SECS + 60))
until grep -aq "No shell, no SSH" "$LOG" 2>/dev/null; do
  [ "$SECONDS" -lt "$deadline" ] || { echo "ISO install test FAILED: the installed disk's janusd never listened" >&2; cat "$LOG" >&2; exit 1; }
  sleep 1
done
grep -aq "pki: trusts the fleet of root" "$LOG" || { echo "ISO install test FAILED: the installed disk doesn't trust the fleet Install gave it" >&2; cat "$LOG" >&2; exit 1; }
if grep -aq "pairing: code" "$LOG"; then
  echo "ISO install test FAILED: the installed disk, trusting a fleet, shows a pairing code" >&2
  exit 1
fi
FP="$({ grep -ao "pki: this node's CA: SHA-256 [0-9a-f]*" "$LOG" || true; } | tail -1 | awk '{print $NF}')"
[ "${#FP}" = 64 ] || { echo "ISO install test FAILED: no CA fingerprint on the installed disk's console" >&2; exit 1; }
ctl fleet adopt installed -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca-fingerprint "$FP" \
  || { echo "ISO install test FAILED: fleet adopt -ca-fingerprint the installed disk" >&2; exit 1; }
ctl -n installed version >/dev/null || { echo "ISO install test FAILED: the fleet's certificate doesn't reach the installed disk" >&2; exit 1; }
if grep -a "avc:.*denied" "$WORKDIR/iso-install.log" "$LOG"; then
  echo "ISO install test FAILED: AVC denials" >&2
  exit 1
fi
kill "$QEMU_PID" 2>/dev/null || true
wait "$QEMU_PID" 2>/dev/null || true
QEMU_PID=""

echo "ISO install test OK: a node booted from the embedded-bundle ISO - paired by its code, nothing secret on its console - installed a real disk using only /etc/janus/release, no bundle reachable from the host at all; the installed disk trusts the fleet from its first boot"
