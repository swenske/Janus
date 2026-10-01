#!/usr/bin/env bash
# Proves LifecycleService.Upgrade's https:// fetch mode works against a
# real public release, verified against the node's own bundled system
# trust store - the path hack/qemu-lifecycle-upgrade-url-test.sh (plain
# http:// from a host-side python server) never touched. That gap hid a
# real bug: rootfs/init's mountEphemeral overmounted /etc with an empty
# tmpfs, hiding /etc/ssl/certs/ca-certificates.crt on every real boot, so
# any https:// Upgrade on a deployed node failed with "certificate
# signed by unknown authority".
#
# Needs real outbound internet from the guest (QEMU usermode/slirp NATs
# through the host). Upgrades a freshly-built disk to the newest
# published GitHub Release (or $JANUS_RELEASE_TAG), then checks the
# rebooted slot B carries exactly the root hash baked into that
# release's own uki-b.efi.
#
# Usage: hack/qemu-lifecycle-upgrade-https-test.sh <disk.img> <build-dir> <janusctl-bin>
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

DISK="${1:?usage: $0 <disk.img> <build-dir> <janusctl-bin>}"
BUILD_DIR="${2:?usage: $0 <disk.img> <build-dir> <janusctl-bin>}"
CTL="${3:?usage: $0 <disk.img> <build-dir> <janusctl-bin>}"
HTTP_TIMEOUT_SECS="${QEMU_UPGRADE_HTTP_TIMEOUT:-40}"
REBOOT_TIMEOUT_SECS="${QEMU_UPGRADE_REBOOT_TIMEOUT:-90}"
HOST_PORT_8080="${QEMU_UPGRADE_TEST_PORT:-18196}"
HOST_GRPC_PORT="${QEMU_UPGRADE_GRPC_PORT:-18197}"
REPO="${JANUS_RELEASE_REPO:-swenske/Janus}"
MARKER="JANUS_INIT_BOOT_OK"
FIRST_BOOT_MSG="pki: first boot - generated a new CA"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf)" >&2; exit 1; }
[ -f "$OVMF_VARS_TEMPLATE" ] || { echo "OVMF vars template not found at $OVMF_VARS_TEMPLATE" >&2; exit 1; }

WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

fail() {
  echo "Upgrade HTTPS test FAILED: $*" >&2
  [ -f "$WORKDIR/boot.log" ] && { echo "--- console output ---" >&2; cat "$WORKDIR/boot.log" >&2; }
  exit 1
}

# --- resolve the release to upgrade to (list endpoint, not /latest:
# /latest skips pre-releases, which the first releases were) ---
if [ -z "${JANUS_RELEASE_TAG:-}" ]; then
  JANUS_RELEASE_TAG="$(curl -fsSL -H 'Accept: application/vnd.github+json' \
    "https://api.github.com/repos/$REPO/releases" |
    python3 -c 'import json,sys; print(next(r["tag_name"] for r in json.load(sys.stdin) if not r["draft"]))')"
fi
RELEASE_URL="https://github.com/$REPO/releases/download/$JANUS_RELEASE_TAG"
echo "Upgrading to $JANUS_RELEASE_TAG ($RELEASE_URL)"

RELEASE_SHA256="$(curl -fsSL "$RELEASE_URL/rootfs.squashfs.sha256" | awk '{print $1}')"
[[ "$RELEASE_SHA256" =~ ^[0-9a-f]{64}$ ]] || fail "couldn't read a sha256 from $RELEASE_URL/rootfs.squashfs.sha256"

curl -fsSL -o "$WORKDIR/uki-b.efi" "$RELEASE_URL/uki-b.efi"
# The UKI's .cmdline section is plain text inside the PE file - grepped
# directly rather than extracted with objcopy, which the self-hosted
# runner doesn't have.
# A release's UKIs name the root by partition label (PARTLABEL=BOOT-B-DATA)
# since the bare-metal tranche, by /dev/vdaN before.
RELEASE_HASH="$(grep -aoE 'verity 1 (/dev/vda4 /dev/vda5|PARTLABEL=BOOT-B-DATA PARTLABEL=BOOT-B-HASH) [0-9 ]+ sha256 [0-9a-f]{64}' "$WORKDIR/uki-b.efi" | head -1 | awk '{print $NF}')"
[ -n "$RELEASE_HASH" ] || fail "couldn't read the root hash out of $JANUS_RELEASE_TAG's uki-b.efi .cmdline section"

V1_HASH="$(cat "$BUILD_DIR/rootfs/rootfs.roothash")"
[ "$V1_HASH" != "$RELEASE_HASH" ] || fail "the local build's root hash equals $JANUS_RELEASE_TAG's - nothing would prove the slot switch"

# --- boot the local build (slot A) ---
LOG="$WORKDIR/boot.log"
cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/OVMF_VARS.fd"
qemu-system-x86_64 \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$WORKDIR/OVMF_VARS.fd" \
  -drive file="$DISK",format=raw,if=virtio \
  -nographic -display none -m 512M \
  -netdev "user,id=net0,hostfwd=tcp::${HOST_PORT_8080}-:8080,hostfwd=tcp::${HOST_GRPC_PORT}-:9505" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG" \
  &
QEMU_PID=$!

wait_http_and_marker() {
  local want_markers="$1" timeout_secs="$2" code=""
  local deadline=$((SECONDS + timeout_secs))
  while [ "$SECONDS" -lt "$deadline" ]; do
    code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_PORT_8080}/" || true)"
    if [ "$code" = "200" ] && [ "$(grep -c "$MARKER" "$LOG" 2>/dev/null || true)" -ge "$want_markers" ]; then
      return 0
    fi
    sleep 1
  done
  return 1
}

last_cmdline() { grep "^Kernel command line:" "$LOG" | tail -1; }

wait_http_and_marker 1 "$HTTP_TIMEOUT_SECS" || fail "slot A never answered HTTP 200 within ${HTTP_TIMEOUT_SECS}s"
grep -q "$FIRST_BOOT_MSG" "$LOG" || fail "slot A didn't bootstrap a fresh PKI"
case "$(last_cmdline)" in
  *"verity 1 PARTLABEL=BOOT-A-DATA PARTLABEL=BOOT-A-HASH "*"$V1_HASH"*) : ;;
  *) fail "slot A's cmdline doesn't match the local build: $(last_cmdline)" ;;
esac
echo "Slot A (local build) OK: HTTP 200, root hash $V1_HASH"

# --- PKI from STATE (see hack/qemu-lifecycle-rollback-test.sh) ---
STATE_START_SECTOR="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
STATE_SIZE_SECTORS="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
dd if="$DISK" of="$WORKDIR/state.img" bs=512 skip="$STATE_START_SECTOR" count="$STATE_SIZE_SECTORS" status=none
for f in ca.crt admin.crt admin.key; do
  debugfs -R "dump pki/$f $WORKDIR/$f" "$WORKDIR/state.img" >/dev/null 2>&1
  [ -s "$WORKDIR/$f" ] || fail "couldn't extract pki/$f from STATE"
done

# --- the node fetches the real release over https:// itself ---
# No signature opt-out: the newest release (v2026.09.30-3 onwards) is
# signed with the key in image/secureboot/production-cert.pem, so this
# proves the whole chain, the node's own signature check included.
set +e
UPGRADE_OUT="$("$CTL" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" \
  lifecycle upgrade -sha256 "$RELEASE_SHA256" "$RELEASE_URL" 2>&1)"
UPGRADE_RC=$?
set -e
echo "$UPGRADE_OUT"
if echo "$UPGRADE_OUT" | grep -qi "x509\|certificate"; then
  fail "the node couldn't verify GitHub's TLS certificate - is its CA bundle visible at /etc/ssl/certs/ca-certificates.crt after mountEphemeral?"
fi
[ "$UPGRADE_RC" -eq 0 ] || fail "janusctl lifecycle upgrade exited $UPGRADE_RC"
echo "$UPGRADE_OUT" | grep -qi "downloading release bundle at https://" || fail "Upgrade never reported downloading over https://"
echo "$UPGRADE_OUT" | grep -q "uki-[ab].efi: signature verified" || fail "Upgrade never reported verifying the release's UKI signature"
echo "$UPGRADE_OUT" | grep -qi "rebooting" || fail "Upgrade never reached the 'rebooting' stage"

wait_http_and_marker 2 "$REBOOT_TIMEOUT_SECS" || fail "no healthy HTTP 200 after the upgrade reboot within ${REBOOT_TIMEOUT_SECS}s"
case "$(last_cmdline)" in
  *"verity 1 /dev/vda4 /dev/vda5 "*"$RELEASE_HASH"*|*"verity 1 PARTLABEL=BOOT-B-DATA PARTLABEL=BOOT-B-HASH "*"$RELEASE_HASH"*) : ;;
  *) fail "post-upgrade cmdline doesn't carry $JANUS_RELEASE_TAG's slot B root hash $RELEASE_HASH: $(last_cmdline)" ;;
esac
echo "Slot B OK: booted $JANUS_RELEASE_TAG (root hash $RELEASE_HASH, slot B)"
echo "Upgrade HTTPS test OK: the node fetched a real GitHub Release over https://, verified against its own bundled CA trust store, and rebooted into it"
