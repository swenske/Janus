#!/usr/bin/env bash
# Proves the LOGS partition (internal/logvol) on a real node: the full
# disk image (image/disk/assemble.sh, LOGS after STATE) under real OVMF,
# SELinux enforcing.
#   1. The first boot formats it and keeps janusd's log, HAProxy's own
#      messages (not its access logs) and the kernel's messages there.
#   2. After a reboot through the API, `system boots` lists two boots and
#      the previous one's logs and kernel messages read back (-boot -1);
#      follow is refused for a previous boot.
#   3. After a power cut (QEMU killed a few seconds after a marker was
#      logged), the boot that was cut is there, marker included.
#   4. A janusd restarted (not the machine) goes on with the same boot.
#   5. Reset (wipe_state) forgets every boot.
#   6. No AVC denial at any point.
#
# Usage: hack/qemu-logvol-test.sh <disk.img> <janusctl-bin>
set -euo pipefail
# shellcheck source=state-read.sh
. "$(dirname "$0")/state-read.sh"

export PATH="$PATH:/usr/sbin:/sbin"

SRC_DISK="${1:?usage: $0 <disk.img> <janusctl-bin>}"
CTL_BIN="${2:?usage: $0 <disk.img> <janusctl-bin>}"
P_HTTP="${QEMU_LOGVOL_HTTP_PORT:-$((18361 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
P_GRPC="${QEMU_LOGVOL_GRPC_PORT:-$((18362 + ${JANUS_TEST_PORT_OFFSET:-0}))}"

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
ALL_LOGS="$WORKDIR/all-boots.log"
fail() {
  echo "LOGS partition test FAILED: $*" >&2
  [ -f "$LOG" ] && { echo "--- console output (last 80 lines) ---" >&2; tail -80 "$LOG" >&2; }
  exit 1
}

DISK="$WORKDIR/disk.img"
cp --sparse=always "$SRC_DISK" "$DISK"
cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/OVMF_VARS.fd"
sgdisk -i 7 "$DISK" | grep -q "^Partition name: 'LOGS'" || fail "the image has no LOGS partition 7"
# Tests before this one may have booted the image itself (a CI job's
# tests share build/rootfs/disk.img): its LOGS partition made blank
# again, as a fresh image has it - this test starts from a first boot.
LOGS_FIRST="$(sgdisk -i 7 "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
LOGS_SECTORS="$(sgdisk -i 7 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
fallocate --punch-hole --offset $((LOGS_FIRST * 512)) --length $((LOGS_SECTORS * 512)) "$DISK"

# start_qemu - boots the disk; the console goes to a new $LOG, the
# previous one is appended to $ALL_LOGS (the AVC check reads them all).
start_qemu() {
  [ -f "$LOG" ] && cat "$LOG" >> "$ALL_LOGS"
  : > "$LOG"
  qemu-system-x86_64 -accel kvm -accel tcg -m 1024M \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$WORKDIR/OVMF_VARS.fd" \
    -drive file="$DISK",format=raw,if=virtio \
    -display none -monitor none \
    -netdev "user,id=net0,hostfwd=tcp::${P_HTTP}-:8080,hostfwd=tcp::${P_GRPC}-:9505" \
    -device virtio-net-pci,netdev=net0 \
    -serial file:"$LOG" </dev/null &
  QEMU_PID=$!
}

# wait_janusd N - until janusd has said "listening on" N times on this
# console.
wait_janusd() {
  local deadline=$((SECONDS + 150))
  until [ "$(grep -ac "listening on" "$LOG" 2>/dev/null || true)" -ge "$1" ]; do
    [ "$SECONDS" -lt "$deadline" ] || fail "janusd never started listening (#$1)"
    kill -0 "$QEMU_PID" 2>/dev/null || fail "QEMU exited"
    sleep 1
  done
  sleep 2
}

start_qemu
wait_janusd 1
grep -aq "logs: kept on /dev/vda7 (512 MiB, 0 earlier boots) - this is boot 1" "$LOG" || fail "janusd didn't open the LOGS partition: $(grep -a 'logs:' "$LOG")"

state_pki "$DISK" "$WORKDIR" || fail "couldn't extract the node's pki/ from STATE"
ctl() { "$CTL_BIN" -endpoint "127.0.0.1:${P_GRPC}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" "$@"; }

# A configuration that makes HAProxy warn (a log format, no log address)
# and log every request to its stdout.
cat > "$WORKDIR/haproxy.cfg" <<'EOF'
global
    log stdout format raw local0
    stats socket /run/janus/haproxy-admin.sock mode 660 level admin
    chroot /var/empty
    uid 1000
    gid 1000

defaults
    mode http
    option httplog
    timeout connect 5s
    timeout client 30s
    timeout server 30s

frontend janus-health
    bind *:8080
    log global
    http-request return status 200 content-type text/plain string "ok\n"

frontend quiet
    bind *:8081
    option httplog
    http-request return status 200
EOF
ctl haproxy apply-config "$WORKDIR/haproxy.cfg" >/dev/null || fail "apply-config refused"
for _ in $(seq 20); do curl -s -o /dev/null "http://127.0.0.1:${P_HTTP}/logvol-access-line"; done
ctl system metrics -port 10071 >/dev/null || fail "MetricsConfigSet failed"
sleep 4 # past logvol.FlushEvery

boots="$(ctl system boots)" || fail "system boots failed"
echo "$boots"
grep -q "^LOGS partition /dev/vda7" <<<"$boots" || fail "system boots: $boots"
[ "$(grep -cE '^0 ' <<<"$boots")" = 1 ] || fail "this boot isn't listed: $boots"
if ctl system logs -boot -1 janusd 2>"$WORKDIR/err"; then
  fail "a previous boot that doesn't exist was read"
fi
grep -q "holds no boot -1" "$WORKDIR/err" || fail "unexpected error for a missing boot: $(cat "$WORKDIR/err")"

# --- 2. a reboot through the API ---
ctl system reboot >/dev/null || fail "reboot failed"
wait_janusd 2
grep -aq "logs: kept on /dev/vda7 (512 MiB, 1 earlier boots) - this is boot 2" "$LOG" || fail "the second boot isn't boot 2: $(grep -a 'logs:' "$LOG")"
sleep 3
boots="$(ctl system boots)"
echo "$boots"
grep -qE '^-1 ' <<<"$boots" || fail "the previous boot isn't listed: $boots"
prev="$(ctl system logs -boot -1 janusd)" || fail "system logs -boot -1 janusd failed"
grep -q "api: SystemService/MetricsConfigSet: admin" <<<"$prev" || fail "the previous boot's janusd log lacks the MetricsConfigSet call"
grep -q "exporter: metrics served on :10071/metrics" <<<"$prev" || fail "the previous boot's janusd log lacks the exporter's move"
grep -q "api: SystemService/Reboot: admin" <<<"$prev" || fail "the previous boot's janusd log lacks the reboot"
prev_haproxy="$(ctl system logs -boot -1 haproxy)"
grep -q "^\[WARNING\]" <<<"$prev_haproxy" || fail "HAProxy's warnings of the previous boot weren't kept: $prev_haproxy"
if grep -q "logvol-access-line" <<<"$prev_haproxy"; then
  fail "HAProxy's access logs were kept on LOGS"
fi
last2="$(ctl system logs -boot -1 -n 2 janusd)"
[ "$(wc -l <<<"$last2")" = 2 ] || fail "-n 2 gave: $last2"
prev_kernel="$(ctl system dmesg -boot -1)" || fail "system dmesg -boot -1 failed"
grep -q "Linux version" <<<"$prev_kernel" || fail "the previous boot's kernel messages lack 'Linux version'"
grep -qE '^\[ *[0-9]+\.[0-9]{6}\] ' <<<"$prev_kernel" || fail "the previous boot's kernel messages aren't dmesg-formatted"
if ctl system logs -f -boot -1 janusd 2>"$WORKDIR/err"; then
  fail "follow was accepted for a previous boot"
fi
echo "Part 2 OK: a reboot - the previous boot's janusd log, HAProxy's warnings (no access logs) and kernel messages read back"

# --- 4. janusd restarted, not the machine ---
ctl system restart >/dev/null || fail "restart failed"
wait_janusd 3
grep -aq "logs: kept on /dev/vda7 (512 MiB, 2 earlier boots) - this is boot 2" "$LOG" || fail "a restarted janusd took a new boot: $(grep -a 'logs:' "$LOG" | tail -1)"
echo "Part 4 OK: a restarted janusd goes on with the same boot"

# --- 3. a power cut ---
ctl system metrics -port 10072 >/dev/null || fail "MetricsConfigSet failed"
sleep 4 # past logvol.FlushEvery
kill -9 "$QEMU_PID"
wait "$QEMU_PID" 2>/dev/null || true
QEMU_PID=""
start_qemu
wait_janusd 1
grep -aq "logs: kept on /dev/vda7 (512 MiB, 2 earlier boots) - this is boot 3" "$LOG" || fail "after the power cut: $(grep -a 'logs:' "$LOG")"
sleep 3
cut="$(ctl system logs -boot -1 janusd)"
grep -q "exporter: metrics served on :10072/metrics" <<<"$cut" || fail "the boot that was cut lost its last seconds: $(tail -5 <<<"$cut")"
boots="$(ctl system boots)"
echo "$boots"
grep -qE '^-2 ' <<<"$boots" || fail "the first boot is gone: $boots"
echo "Part 3 OK: a power cut - the boot that was cut is there, its last logged line included"

# --- 5. Reset forgets them ---
ctl system reset -wipe-state >/dev/null || fail "reset failed"
wait_janusd 1
deadline=$((SECONDS + 60))
until grep -aq "logs: kept on /dev/vda7 (512 MiB, 0 earlier boots) - this is boot 1" "$LOG"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "after a reset, the LOGS partition still holds boots: $(grep -a 'logs:' "$LOG" | tail -1)"
  sleep 1
done
echo "Part 5 OK: Reset forgot every boot"

cat "$LOG" >> "$ALL_LOGS"
if grep -aq "avc:  denied" "$ALL_LOGS"; then
  fail "AVC denials: $(grep -a 'avc:  denied' "$ALL_LOGS" | head -5)"
fi
echo "LOGS partition test passed: formatted at the first boot, previous boots' logs and kernel messages read back after a reboot and a power cut, a janusd restart keeps the boot, Reset forgets them, no AVC denial"
