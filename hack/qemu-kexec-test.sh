#!/usr/bin/env bash
# A reboot through kexec (SystemService.Reboot, RebootMode KEXEC - see
# internal/kexec, internal/api/kexec.go) on a real enforcing node under
# OVMF, against a firmware reboot of the same node:
#
#   1. boot disk.img (slot A), get the admin credentials off STATE;
#   2. `janusctl system reboot` (the firmware): OVMF's own boot manager
#      line (BdsDxe: starting Boot) shows up again, the node comes back;
#   3. `janusctl system reboot -kexec`: the kernel logs "Starting new
#      kernel", OVMF's line does NOT show up again, the node comes back
#      on the same slot with its PKI - the API answers with the same
#      credentials; the time from the call to janusd listening is
#      printed for both, kexec's must be the shorter;
#   4. janusd's own boot timing (internal/boottime) is there after the
#      kexec'd boot too - it's a boot like any other to the kernel;
#   5. a second kexec from the kexec'd kernel: the EFI runtime and the ESP
#      are still there after a jump;
#   6. zero AVC denials across all four boots.
#
# The plain OVMF on q35 with SMM (the machine type Proxmox VE and libvirt
# give a node): it defines no SecureBoot variable, so janusd says so
# ("no UEFI variables, Secure Boot can't be on") and the test image's
# unsigned UKI may be kexec'd, as the firmware boots it - the rule
# internal/api/kexec.go keeps; its refusal with Secure Boot on is
# internal/api's own TestKexecKeepsTheFirmwaresRule. Not the Secure
# Boot-capable OVMF (OVMF_CODE_4M.secboot.fd, whose variable services run
# in SMM): a kernel kexec'd under it crashes at once on real KVM
# (internal/api/kexec.go says what is known) - a limitation, documented,
# not what this test is about.
#
# Usage: hack/qemu-kexec-test.sh <disk.img> <janusctl-bin>
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

SRC_DISK="${1:?usage: $0 <disk.img> <janusctl-bin>}"
CTL_BIN="${2:?usage: $0 <disk.img> <janusctl-bin>}"
HOST_PORT_8080="$((18320 + ${JANUS_TEST_PORT_OFFSET:-0}))"
HOST_GRPC_PORT="$((18321 + ${JANUS_TEST_PORT_OFFSET:-0}))"
MARKER="JANUS_INIT_BOOT_OK"
FIRMWARE_LINE="BdsDxe: starting Boot"
KEXEC_LINE="Starting new kernel"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf)" >&2; exit 1; }

WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

LOG="$WORKDIR/boot.log"
fail() {
  echo "kexec test FAILED: $*" >&2
  [ -f "$LOG" ] && { echo "--- console output ---" >&2; cat "$LOG" >&2; }
  exit 1
}

DISK="$WORKDIR/disk.img"
cp "$SRC_DISK" "$DISK"
cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/OVMF_VARS.fd"
# No -no-reboot: the node reboots inside this same QEMU, twice.
qemu-system-x86_64 -accel kvm -accel tcg \
  -machine q35,smm=on \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$WORKDIR/OVMF_VARS.fd" \
  -drive file="$DISK",format=raw,if=virtio \
  -nographic -display none -m 512M \
  -netdev "user,id=net0,hostfwd=tcp::${HOST_PORT_8080}-:8080,hostfwd=tcp::${HOST_GRPC_PORT}-:9505" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG" \
  &
QEMU_PID=$!

# count PATTERN: how many console lines match - 0 before the log exists
# (grep -c prints 0 itself on no match, nothing on a missing file).
count() { local n; n="$(grep -ac "$1" "$LOG" 2>/dev/null)" || true; echo "${n:-0}"; }
markers() { count "$MARKER"; }
listening() { count "janusd .* listening on"; }
firmware_boots() { count "$FIRMWARE_LINE"; }
kexecs() { count "$KEXEC_LINE"; }
wait_listening() { # wait_listening N: the Nth boot's janusd listens
  local deadline=$((SECONDS + 90))
  until [ "$(markers)" -ge "$1" ] && [ "$(listening)" -ge "$1" ]; do
    [ "$SECONDS" -lt "$deadline" ] || fail "boot #$1's janusd never listened within 90s"
    sleep 0.2
  done
}
extract_pki() {
  local start size
  start="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
  size="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
  dd if="$DISK" of="$WORKDIR/state.img" bs=512 skip="$start" count="$size" status=none
  for f in ca.crt admin.crt admin.key; do
    rm -f "$WORKDIR/$f"
    debugfs -R "dump pki/$f $WORKDIR/$f" "$WORKDIR/state.img" >/dev/null 2>&1
    [ -s "$WORKDIR/$f" ] || return 1
  done
}
ctl() { "$CTL_BIN" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" "$@"; }
wait_api() {
  local deadline=$((SECONDS + 60)) out
  until out="$(ctl version 2>&1)"; do
    [ "$SECONDS" -lt "$deadline" ] || fail "$1: janusd's API never answered: $out"
    sleep 1
  done
}
now_ms() { date +%s%3N; }

# --- 1. first boot, through the firmware ---
wait_listening 1
until extract_pki; do sleep 1; done
wait_api "first boot"
[ "$(firmware_boots)" -eq 1 ] || fail "the first boot: OVMF's '$FIRMWARE_LINE' seen $(firmware_boots) time(s), want 1"
[ "$(kexecs)" -eq 0 ] || fail "a kexec before any was asked for"
echo "  ok: first boot through the firmware, API up"

# --- 2. a firmware reboot, timed ---
t0=$(now_ms)
ctl system reboot >/dev/null || fail "Reboot (firmware) failed"
wait_listening 2
FIRMWARE_MS=$(( $(now_ms) - t0 ))
[ "$(firmware_boots)" -eq 2 ] || fail "the firmware reboot: OVMF's line seen $(firmware_boots) time(s), want 2"
[ "$(kexecs)" -eq 0 ] || fail "the firmware reboot went through kexec"
wait_api "after the firmware reboot"
echo "  ok: firmware reboot - janusd listening again ${FIRMWARE_MS} ms after the call"

# --- 3. a kexec reboot, timed ---
t0=$(now_ms)
ctl system reboot -kexec >/dev/null || fail "Reboot -kexec failed"
wait_listening 3
KEXEC_MS=$(( $(now_ms) - t0 ))
[ "$(kexecs)" -eq 1 ] || fail "the kexec reboot: '$KEXEC_LINE' seen $(kexecs) time(s), want 1"
[ "$(firmware_boots)" -eq 2 ] || fail "the kexec reboot went through the firmware: OVMF's line seen $(firmware_boots) time(s), want still 2"
grep -aq "kexec: the next reboot jumps into the loaded kernel without the firmware (no UEFI variables, Secure Boot can't be on)" "$LOG" \
  || fail "janusd never said it loaded the kernel for kexec: $(grep -a 'kexec:' "$LOG")"
wait_api "after the kexec reboot"
grep -aq "Kernel command line:.*PARTLABEL=BOOT-A-DATA" "$LOG" || fail "the kexec'd kernel's command line doesn't name slot A"
[ "$(grep -ac "Kernel command line:" "$LOG")" -eq 3 ] || fail "three kernels should have logged their command line, $(grep -ac 'Kernel command line:' "$LOG") did"
echo "  ok: kexec reboot - janusd listening again ${KEXEC_MS} ms after the call, no firmware in between, slot A, same PKI"
[ "$KEXEC_MS" -lt "$FIRMWARE_MS" ] || fail "kexec (${KEXEC_MS} ms) wasn't faster than the firmware (${FIRMWARE_MS} ms)"

# --- 4. the kexec'd boot measured itself ---
[ "$(grep -ac "boot: api listening" "$LOG")" -eq 3 ] || fail "the kexec'd boot has no timing line: $(grep -ac 'boot: api listening' "$LOG") of 3"
echo "  ok: the kexec'd boot's own timing: $(grep -a 'boot: api listening' "$LOG" | tail -1 | sed 's/.*boot: //')"

# --- 5. a second kexec, from the kexec'd kernel: the EFI runtime and
# the ESP survived the first jump, so the chain goes on ---
ctl system reboot -kexec >/dev/null || fail "a second Reboot -kexec, from the kexec'd kernel, failed"
wait_listening 4
[ "$(kexecs)" -eq 2 ] || fail "the second kexec: '$KEXEC_LINE' seen $(kexecs) time(s), want 2"
[ "$(firmware_boots)" -eq 2 ] || fail "the second kexec went through the firmware"
wait_api "after the second kexec"
[ "$(count "kexec: the next reboot jumps into the loaded kernel without the firmware (no UEFI variables")" -eq 2 ] \
  || fail "the Secure Boot state wasn't looked up before both kexecs: $(grep -a 'kexec: the next reboot' "$LOG")"
echo "  ok: a second kexec from the kexec'd kernel - the EFI runtime and the ESP survive a jump"

# --- 6. SELinux ---
if grep -a "avc:.*denied" "$LOG"; then fail "AVC denials"; fi
grep -aq "SELinux:  Initializing" "$LOG" || fail "no SELinux initialization in the console log"
echo "  ok: zero AVC denials across the firmware and kexec boots"

echo "kexec test OK: a reboot through kexec came back on the same slot in ${KEXEC_MS} ms against ${FIRMWARE_MS} ms through OVMF, no firmware in between, the node's PKI and API intact, zero AVC denials"
