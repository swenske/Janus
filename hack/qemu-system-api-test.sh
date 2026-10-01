#!/usr/bin/env bash
# Proves the SystemService/HAProxyService/NetworkService methods beyond
# the lifecycle ones on a real node: the full disk image under real OVMF
# with SELinux enforcing (baked into the UKI), every call made over mTLS
# with janusctl. Read-only methods first, then the disruptive ones in
# order - janusd restart (HAProxy must keep answering throughout), a
# reboot (PKI survives), a reset (PKI regenerated, old certificates
# refused), and a shutdown (QEMU exits). Fails on any AVC denial.
#
# Usage: hack/qemu-system-api-test.sh <disk.img> <janusctl-bin>
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

DISK="${1:?usage: $0 <disk.img> <janusctl-bin>}"
CTL_BIN="${2:?usage: $0 <disk.img> <janusctl-bin>}"
BOOT_TIMEOUT_SECS="${QEMU_API_BOOT_TIMEOUT:-60}"
HOST_PORT_8080="${QEMU_API_TEST_PORT:-18200}"
HOST_GRPC_PORT="${QEMU_API_GRPC_PORT:-18201}"
MARKER="JANUS_INIT_BOOT_OK"
FIRST_BOOT_MSG="pki: first boot - generated a new CA"

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
  echo "System API test FAILED: $*" >&2
  [ -f "$LOG" ] && { echo "--- console output ---" >&2; cat "$LOG" >&2; }
  exit 1
}

# No -no-reboot: Reboot and Reset must reboot the guest inside this same
# QEMU process, and Shutdown must make it exit.
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

http_code() { curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_PORT_8080}/" || true; }
markers() { grep -ac "$MARKER" "$LOG" 2>/dev/null || true; }
wait_boot() { # wait_boot N: the Nth boot answers HTTP
  local deadline=$((SECONDS + BOOT_TIMEOUT_SECS))
  until [ "$(markers)" -ge "$1" ] && [ "$(http_code)" = "200" ]; do
    [ "$SECONDS" -lt "$deadline" ] || fail "boot #$1 never answered HTTP 200 within ${BOOT_TIMEOUT_SECS}s"
    sleep 1
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
    [ -s "$WORKDIR/$f" ] || fail "couldn't extract pki/$f from STATE"
  done
}
ctl() { "$CTL_BIN" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" "$@"; }
expect() { # expect "description" "grep -E pattern" command...
  local what="$1" pattern="$2" out
  shift 2
  out="$("$@" 2>&1)" || fail "$what: janusctl $* failed: $out"
  echo "$out" | grep -Eq "$pattern" || fail "$what: output of janusctl $* doesn't match /$pattern/: $out"
  echo "  ok: $what"
}
haproxy_count() { ctl system ps | grep -c '/usr/local/sbin/haproxy -f' || true; }

wait_boot 1
extract_pki
echo "Boot 1 OK"

# --- read-only methods ---
expect "Hostname" '^[A-Za-z0-9.-]+$' ctl system hostname
expect "Stats lists janusd and haproxy" '^haproxy +[0-9.]+ ' ctl system stats
expect "SystemStat" '^processes created: [1-9]' ctl system systemstat
expect "Processes shows PID 1 (rootfs/init)" '^1 +[0-9.]+ +[0-9.]+[KM]iB +/sbin/init' ctl system ps
expect "NetworkDeviceStats counts eth0 traffic" '^eth0 +[0-9.]+[KM]iB' ctl system netdev
expect "Netstat sees the gRPC listener" '9505 +.* LISTEN' ctl system netstat
expect "Mounts shows the dm-verity root read-only" '^/dev/root \(squashfs\) +/ .* ro$' ctl system mounts
expect "Mounts shows STATE" '/dev/vda6 \(ext4\) +/etc/\.state ' ctl system mounts
expect "DiskUsage" '[0-9.]+(B|KiB)'$'\t''/etc/haproxy$' ctl system du /etc/haproxy
expect "List" 'haproxy\.cfg$' ctl system ls /etc/haproxy
expect "Read" '^frontend ' ctl system cat /etc/haproxy/haproxy.cfg
ctl system cp -o "$WORKDIR/etc-haproxy.tar" /etc/haproxy || fail "Copy failed"
tar tf "$WORKDIR/etc-haproxy.tar" | grep -qx 'haproxy/haproxy.cfg' || fail "Copy's tar doesn't hold haproxy/haproxy.cfg: $(tar tf "$WORKDIR/etc-haproxy.tar")"
echo "  ok: Copy (valid tar)"
if out="$(ctl system cat /dev/vda 2>&1)"; then fail "Read of a block device was allowed"; fi
echo "$out" | grep -q PermissionDenied || fail "Read(/dev/vda) didn't answer PermissionDenied: $out"
echo "  ok: Read refuses devices"
expect "Dmesg" 'Linux version' ctl system dmesg
expect "Logs janusd" 'listening on :9505' ctl system logs janusd
expect "Logs haproxy" 'NOTICE' ctl system logs haproxy
expect "ServiceList" '^haproxy +running +healthy' ctl system services
expect "BackendList" '^BACKEND' ctl haproxy backends
expect "NetworkService reports modules not in the image" 'bgp \(bird\): +not_enabled' ctl network modules

# A certificate uploaded at runtime: kept on STATE, it must be back in
# HAProxy after the reloads, the janusd restart and the reboot below.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 \
  -subj "/CN=persist.example.test" -keyout "$WORKDIR/persist.key" -out "$WORKDIR/persist.crt" 2>/dev/null
cat "$WORKDIR/persist.crt" "$WORKDIR/persist.key" > "$WORKDIR/persist.pem"
ctl haproxy cert-upload persist-test.pem "$WORKDIR/persist.pem" >/dev/null || fail "cert-upload"
has_cert() { ctl haproxy cert-list 2>/dev/null | grep -q '^persist-test\.pem'; }
has_cert || fail "the uploaded certificate isn't listed: $(ctl haproxy cert-list 2>&1)"

# --- service control ---
ctl system service restart haproxy >/dev/null || fail "ServiceRestart haproxy failed"
sleep 2
[ "$(haproxy_count)" -eq 1 ] || fail "after a seamless restart, $(haproxy_count) haproxy processes are running, want 1: $(ctl system ps)"
[ "$(http_code)" = "200" ] || fail "HTTP down after ServiceRestart haproxy"
has_cert || fail "the uploaded certificate is gone after a reload: $(ctl haproxy cert-list 2>&1)"
echo "  ok: ServiceRestart haproxy (seamless, old process gone, uploaded certificate kept)"
ctl system service stop haproxy >/dev/null || fail "ServiceStop haproxy failed"
[ "$(http_code)" != "200" ] || fail "HTTP still answering after ServiceStop haproxy"
ctl system service start haproxy >/dev/null || fail "ServiceStart haproxy failed"
sleep 1
[ "$(http_code)" = "200" ] || fail "HTTP not back after ServiceStart haproxy"
echo "  ok: ServiceStop / ServiceStart haproxy"
timeout 3 "$CTL_BIN" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" system events > "$WORKDIR/events.txt" || true
for ev in janusd.started haproxy.started service.stopped service.started 'haproxy.exited.*replaced by a reload'; do
  grep -Eq "$ev" "$WORKDIR/events.txt" || fail "Events has no '$ev': $(cat "$WORKDIR/events.txt")"
done
echo "  ok: Events"

# --- Restart: janusd only, HAProxy must keep answering throughout ---
old_haproxy="$(ctl system ps | awk '/\/usr\/local\/sbin\/haproxy -f/ {print $1}')"
ctl system restart >/dev/null || fail "Restart failed"
drops=0
for _ in $(seq 1 20); do
  [ "$(http_code)" = "200" ] || drops=$((drops + 1))
  sleep 0.5
done
[ "$drops" -eq 0 ] || fail "HTTP failed $drops times while janusd restarted"
deadline=$((SECONDS + 30))
until ctl version >/dev/null 2>&1; do
  [ "$SECONDS" -lt "$deadline" ] || fail "janusd didn't come back after Restart"
  sleep 1
done
sleep 2
ctl system ps | grep -q "/usr/local/sbin/haproxy -f /etc/haproxy/haproxy.cfg -sf $old_haproxy" || fail "the restarted janusd didn't take over haproxy $old_haproxy with -sf: $(ctl system ps)"
[ "$(haproxy_count)" -eq 1 ] || fail "$(haproxy_count) haproxy processes after the janusd restart, want 1: $(ctl system ps)"
[ "$(markers)" -eq 1 ] || fail "the machine rebooted during a janusd-only Restart"
echo "  ok: Restart (HAProxy served every request, new janusd took haproxy over)"

# --- Reboot: PKI survives ---
ctl system reboot >/dev/null || fail "Reboot failed"
wait_boot 2
[ "$(grep -ac "$FIRST_BOOT_MSG" "$LOG")" -eq 1 ] || fail "PKI was regenerated by a plain Reboot"
expect "same credentials after Reboot" '^Node:' ctl version
has_cert || fail "the uploaded certificate is gone after the reboot: $(ctl haproxy cert-list 2>&1)"
echo "  ok: Reboot (same credentials, uploaded certificate back in HAProxy)"

# --- Reset: STATE wiped, new PKI, old certificates refused ---
ctl system reset -wipe-state >/dev/null || fail "Reset failed"
wait_boot 3
[ "$(grep -ac "$FIRST_BOOT_MSG" "$LOG")" -eq 2 ] || fail "Reset didn't regenerate the PKI"
sleep 2
if ctl version >/dev/null 2>&1; then fail "the pre-reset admin certificate still works after Reset"; fi
extract_pki
expect "new credentials after Reset" '^Node:' ctl version
echo "  ok: Reset"

# --- Shutdown: QEMU exits ---
ctl system shutdown >/dev/null || fail "Shutdown failed"
deadline=$((SECONDS + 30))
while kill -0 "$QEMU_PID" 2>/dev/null; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the VM didn't power off after Shutdown"
  sleep 1
done
QEMU_PID=""
echo "  ok: Shutdown (VM powered off)"

if grep -a "avc:.*denied" "$LOG"; then
  fail "SELinux denials"
fi
echo "System API test OK: every method exercised on a real enforcing node over mTLS, zero AVC denials"
