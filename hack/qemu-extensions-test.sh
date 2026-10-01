#!/usr/bin/env bash
# Optional extensions and image schematics, end to end, on a real UEFI
# boot (the UKI's own signed command line, SELinux enforcing baked in) of
# a disk built with SCHEMATIC={prometheus-node-exporter, qemu-guest-agent}:
#
#   - the node knows its schematic (janus.schematic= in its signed
#     cmdline) and reports it with its extensions (janusctl version);
#   - janusd supervises both extension services: ServiceList, Logs,
#     stop/start through the API;
#   - node_exporter serves real metrics on :9100;
#   - the QEMU guest agent answers over its virtio-serial channel (ping,
#     OS info, interfaces, filesystem freeze/thaw), refuses the commands
#     that would run programs or touch files, and powers the node off
#     cleanly (janusd stops HAProxy and the extensions first);
#   - Upgrade refuses a bundle built from another schematic (here the
#     default one: it would drop the extensions), and accepts one built
#     from the same schematic - after which the node still runs both;
#   - zero AVC denials.
#
# Usage: hack/qemu-extensions-test.sh <disk.img> <bzImage> <build-dir> <janusctl> <schematic.json>
# The disk must have been built with the same SCHEMATIC (make
# qemu-extensions-test does both).
set -euo pipefail
export PATH="$PATH:/usr/sbin:/sbin"

SRC_DISK="${1:?usage: $0 <disk.img> <bzImage> <build-dir> <janusctl> <schematic.json>}"
KERNEL="${2:?}"
BUILD_DIR="${3:?}"
CTL_BIN="${4:?}"
SCHEMATIC_FILE="${5:?}"
HERE="$(cd "$(dirname "$0")" && pwd)"
BOOT_TIMEOUT_SECS="${QEMU_EXT_BOOT_TIMEOUT:-90}"
BASE_PORT="${QEMU_EXT_TEST_PORT:-$((18600 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
P_HTTP=$BASE_PORT P_GRPC=$((BASE_PORT + 1)) P_METRICS=$((BASE_PORT + 2)) P_JANUS=$((BASE_PORT + 3)) P_ALT=$((BASE_PORT + 4))
MARKER="JANUS_INIT_BOOT_OK"
OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"

WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT
LOG="$WORKDIR/console.log"
fail() {
  echo "Extensions test FAILED: $*" >&2
  [ -f "$LOG" ] && { echo "--- console output ---" >&2; cat "$LOG" >&2; }
  exit 1
}
check() { # check "description" "grep -E pattern" "text"
  # A here-string, not echo | grep -q: grep -q exits at its first match,
  # and with pipefail an echo still writing a large text (the metrics)
  # then fails the pipeline with SIGPIPE - a match reported as missing.
  grep -Eq "$2" <<<"$3" || fail "$1: /$2/ not found in:
$3"
  echo "  ok: $1"
}

SCHEMATIC_ID="$(cd "$HERE/.." && go run ./hack/extpack id -schematic "$SCHEMATIC_FILE")"
DEFAULT_ID="$(cd "$HERE/.." && go run ./hack/extpack id)"
DISK="$WORKDIR/disk.img"
cp "$SRC_DISK" "$DISK"

# --- two release bundles, injected into STATE before the first boot (as
# hack/qemu-lifecycle-upgrade-test.sh does): one built from the same
# schematic (the full slot-B bundle), one from the default schematic
# (only its UKI - Upgrade refuses it before reading anything else) ---
"$HERE/../image/release/assemble.sh" "$WORKDIR/bundle-same" "$KERNEL" "$BUILD_DIR/rootfs" >/dev/null
JANUS_SCHEMATIC="" "$HERE/../image/release/assemble.sh" "$WORKDIR/bundle-default" "$KERNEL" "$BUILD_DIR/rootfs" >/dev/null
START="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
SIZE="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
dd if="$DISK" of="$WORKDIR/state.img" bs=512 skip="$START" count="$SIZE" status=none
debugfs -w -R "mkdir same" "$WORKDIR/state.img" >/dev/null 2>&1
debugfs -w -R "mkdir default" "$WORKDIR/state.img" >/dev/null 2>&1
for f in rootfs.squashfs rootfs.verity uki-b.efi; do
  debugfs -w -R "write $WORKDIR/bundle-same/$f same/$f" "$WORKDIR/state.img" >/dev/null 2>&1
done
debugfs -w -R "write $WORKDIR/bundle-default/uki-b.efi default/uki-b.efi" "$WORKDIR/state.img" >/dev/null 2>&1
dd if="$WORKDIR/state.img" of="$DISK" bs=512 seek="$START" conv=notrunc status=none
SAME_SHA="$(cut -d' ' -f1 "$WORKDIR/bundle-same/rootfs.squashfs.sha256")"
echo "  ok: bundles injected (schematic $SCHEMATIC_ID and the default one)"

cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/OVMF_VARS.fd"
qemu-system-x86_64 -accel kvm -accel tcg \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$WORKDIR/OVMF_VARS.fd" \
  -drive file="$DISK",format=raw,if=virtio \
  -nographic -display none -m 512M -monitor none \
  -netdev "user,id=net0,hostfwd=tcp::${P_HTTP}-:8080,hostfwd=tcp::${P_GRPC}-:9505,hostfwd=tcp::${P_METRICS}-:9100,hostfwd=tcp::${P_JANUS}-:10056,hostfwd=tcp::${P_ALT}-:9200" \
  -device virtio-net-pci,netdev=net0 \
  -device virtio-serial \
  -chardev "socket,path=$WORKDIR/qga.sock,server=on,wait=off,id=qga0" \
  -device virtserialport,chardev=qga0,name=org.qemu.guest_agent.0 \
  -serial file:"$LOG" </dev/null &
QEMU_PID=$!

markers() { grep -ac "$MARKER" "$LOG" 2>/dev/null || true; }
http_code() { curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${P_HTTP}/" || true; }
wait_boot() {
  local deadline=$((SECONDS + BOOT_TIMEOUT_SECS))
  until [ "$(markers)" -ge "$1" ] && [ "$(http_code)" = "200" ]; do
    [ "$SECONDS" -lt "$deadline" ] || fail "boot #$1 never answered HTTP 200 within ${BOOT_TIMEOUT_SECS}s"
    sleep 1
  done
}
extract_pki() {
  dd if="$DISK" of="$WORKDIR/state.img" bs=512 skip="$START" count="$SIZE" status=none
  for f in ca.crt admin.crt admin.key; do
    rm -f "$WORKDIR/$f"
    debugfs -R "dump pki/$f $WORKDIR/$f" "$WORKDIR/state.img" >/dev/null 2>&1
    [ -s "$WORKDIR/$f" ] || fail "couldn't extract pki/$f from STATE"
  done
}
ctl() { "$CTL_BIN" -endpoint "127.0.0.1:${P_GRPC}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" "$@"; }
qga() { python3 "$HERE/qga-client.py" "$WORKDIR/qga.sock" "$@"; }
metrics() { curl -s -m 5 "http://127.0.0.1:${P_METRICS}/metrics" || true; }
metrics_alt() { curl -s -m 5 "http://127.0.0.1:${P_ALT}/metrics" || true; } # node_exporter moved to :9200
wait_metrics_alt() {
  local deadline=$((SECONDS + 30))
  until grep -q '^node_uname_info' <<<"$(metrics_alt)"; do
    [ "$SECONDS" -lt "$deadline" ] || fail "node_exporter never served metrics on :9200: $(ctl system node-exporter 2>&1)"
    sleep 1
  done
}
# HTTP 200 doesn't mean the API is up: janusd starts HAProxy before it.
wait_api() {
  local deadline=$((SECONDS + 60))
  until ctl version >/dev/null 2>&1; do
    [ "$SECONDS" -lt "$deadline" ] || fail "janusd's API never answered: $(ctl version 2>&1)"
    sleep 1
  done
}
wait_metrics() {
  local deadline=$((SECONDS + 30))
  until grep -q '^node_uname_info' <<<"$(metrics)"; do
    [ "$SECONDS" -lt "$deadline" ] || fail "node_exporter never served metrics"
    sleep 1
  done
}
wait_service() { # wait_service ID STATE
  local deadline=$((SECONDS + 20))
  until ctl system services 2>/dev/null | grep -Eq "^$1 +$2"; do
    [ "$SECONDS" -lt "$deadline" ] || fail "service $1 never reached $2: $(ctl system services 2>&1)"
    sleep 1
  done
}

# --- boot 1 -----------------------------------------------------------
wait_boot 1
sleep 2
extract_pki
echo "Boot 1 OK (UEFI, enforcing)"

check "the signed cmdline carries the schematic" "Kernel command line: .*janus\.schematic=$SCHEMATIC_ID" "$(cat "$LOG")"
v="$(ctl version)"
check "janusd reports the schematic" "^Image schematic: $SCHEMATIC_ID$" "$v"
check "janusd reports prometheus-node-exporter" '^Extension: prometheus-node-exporter ' "$v"
check "janusd reports qemu-guest-agent" '^Extension: qemu-guest-agent ' "$v"
wait_service prometheus-node-exporter running
wait_service qemu-guest-agent running
echo "  ok: both extension services running"
wait_metrics
m="$(metrics)"
check "node_exporter: OS info from /usr/lib/os-release" 'node_os_info\{.*name="Janus"' "$m"
check "node_exporter: disk statistics" '^node_disk_reads_completed_total\{device="vda"\}' "$m"
check "node_exporter: STATE filesystem" 'node_filesystem_size_bytes\{.*mountpoint="/etc/.state"' "$m"
check "node_exporter: network statistics" '^node_network_receive_bytes_total\{device="eth0"\}' "$m"
check "node_exporter logs captured" 'Starting node_exporter' "$(ctl system logs prometheus-node-exporter)"
# janusd reads every /proc/<pid> for Processes and Stats (the
# Controller's metrics): the extensions' processes must be there too.
ps_out="$(ctl system ps)"
check "node_exporter listed by Processes" 'node_exporter' "$ps_out"
check "qemu-ga listed by Processes" 'qemu-ga' "$ps_out"
# Janus's own exporter reports the extensions and their services.
jm="$(curl -s -m 5 "http://127.0.0.1:${P_JANUS}/metrics" || true)"
check "janus exporter: the image's extensions" '^janus_extension_info\{extension="prometheus-node-exporter",version="[0-9.]+"\} 1$' "$jm"
check "janus exporter: the schematic" "^janus_build_info\\{.*schematic=\"$SCHEMATIC_ID\"\\} 1$" "$jm"
check "janus exporter: prometheus-node-exporter running" '^janus_service_state\{service="prometheus-node-exporter",extension="prometheus-node-exporter",state="running"\} 1$' "$jm"
check "janus exporter: the guest agent running" '^janus_service_state\{service="qemu-guest-agent",extension="qemu-guest-agent",state="running"\} 1$' "$jm"

ctl system service stop prometheus-node-exporter >/dev/null || fail "ServiceStop prometheus-node-exporter"
wait_service prometheus-node-exporter stopped
[ -z "$(metrics)" ] || fail "node_exporter still answers after ServiceStop"
ctl system service start prometheus-node-exporter >/dev/null || fail "ServiceStart prometheus-node-exporter"
wait_metrics
echo "  ok: extension service stopped and started through the API"

# --- node_exporter's settings (NodeExporterConfigSet) ------------------
all="$(ctl system node-exporter | awk '$1 ~ /^[a-z_]+$/ {print $1}' | paste -sd, -)"
check "the settings list the optional collectors" 'softnet' "$all"
ctl system node-exporter -port 9200 -collectors "$all" >/dev/null || fail "NodeExporterConfigSet: $(ctl system node-exporter -port 9200 -collectors "$all" 2>&1)"
wait_metrics_alt
[ -z "$(metrics)" ] || fail "node_exporter still answers on :9100 after moving to :9200"
ma="$(metrics_alt)"
# Every collector the settings offer must work on a Janus node: what it
# reads is in the kernel and allowed by the SELinux policy (the AVC
# check at the end covers the latter).
for c in ${all//,/ }; do
  check "collector $c works" "^node_scrape_collector_success\\{collector=\"$c\"\\} 1\$" "$ma"
done
check "every collector on: softnet" '^node_softnet_processed_total\{' "$ma"
check "every collector on: netclass" '^node_network_carrier\{device="eth0"\}' "$ma"
check "every collector on: pressure (PSI)" '^node_pressure_cpu_waiting_seconds_total ' "$ma"
ctl system node-exporter -disable >/dev/null || fail "disabling node_exporter"
wait_service prometheus-node-exporter disabled
[ -z "$(metrics_alt)$(metrics)" ] || fail "node_exporter answers while disabled"
if out="$(ctl system service start prometheus-node-exporter 2>&1)"; then
  fail "a service disabled by its settings was started: $out"
fi
check "starting it by hand is refused" 'disabled by its settings' "$out"
# Kept across the reboot below: on, :9200, softnet on top of the defaults.
ctl system node-exporter -enable -collectors default >/dev/null || fail "re-enabling node_exporter"
defaults="$(ctl system node-exporter | awk '$1 ~ /^[a-z_]+$/ && $2 == "on" {print $1}' | paste -sd, -)"
ctl system node-exporter -collectors "$defaults,softnet" >/dev/null || fail "adding softnet to the defaults"
wait_metrics_alt
check "back on with fewer collectors: entropy is off" '^node_softnet_processed_total\{' "$(metrics_alt)"
if grep -q '^node_entropy_available_bits ' <<<"$(metrics_alt)"; then fail "the entropy collector still runs after being turned off"; fi
echo "  ok: node_exporter's settings - port, every collector, disabled, back on"

# --- the guest agent --------------------------------------------------
deadline=$((SECONDS + 30))
until qga guest-ping >/dev/null 2>&1; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the guest agent never answered"
  sleep 1
done
echo "  ok: guest agent answers on its virtio-serial port"
check "guest-get-osinfo" '"id": "janus"' "$(qga guest-get-osinfo)"
check "guest-network-get-interfaces" '"ip-address": "10\.0\.2\.15"' "$(qga guest-network-get-interfaces)"
check "guest-get-fsinfo" '"mountpoint": "/etc/.state"' "$(qga guest-get-fsinfo)"
check "guest-fsfreeze-freeze" '"return": [1-9]' "$(qga guest-fsfreeze-freeze)"
check "frozen" '"return": "frozen"' "$(qga guest-fsfreeze-status)"
check "guest-fsfreeze-thaw" '"return": [1-9]' "$(qga guest-fsfreeze-thaw)"
for cmd in guest-exec guest-file-open guest-set-user-password guest-ssh-add-authorized-keys; do
  out="$(qga "$cmd" '{}' || true)"
  echo "$out" | grep -q '"CommandNotFound"' || fail "$cmd isn't disabled: $out"
done
echo "  ok: commands that run programs or touch files are disabled"

# --- Upgrade keeps the schematic --------------------------------------
if out="$(ctl lifecycle upgrade -insecure-skip-signature-check -sha256 "$SAME_SHA" /etc/.state/default 2>&1)"; then
  fail "an update built from the default schematic was accepted: $out"
fi
check "an update dropping the extensions is refused" "built from image schematic ${DEFAULT_ID:0:12}, but the node runs schematic ${SCHEMATIC_ID:0:12}" "$out"
out="$(ctl lifecycle upgrade -insecure-skip-signature-check -sha256 "$SAME_SHA" /etc/.state/same 2>&1)" || fail "same-schematic upgrade: $out"
check "an update from the same schematic is accepted" 'same image schematic' "$out"
wait_boot 2
wait_api
check "slot B booted" 'Kernel command line: .*PARTLABEL=BOOT-B-DATA' "$(awk "/$MARKER/{n++} n>=1" "$LOG")"
v="$(ctl version)"
check "after the upgrade: same schematic" "^Image schematic: $SCHEMATIC_ID$" "$v"
wait_service prometheus-node-exporter running
wait_metrics_alt
check "node_exporter's settings survived the reboot" '^node_softnet_processed_total\{' "$(metrics_alt)"
ctl system node-exporter -port 9100 -collectors default >/dev/null || fail "back to the default settings"
wait_metrics
echo "  ok: after the upgrade, the extensions still run, node_exporter as it was set"

# --- clean power-off requested by the hypervisor ----------------------
deadline=$((SECONDS + 30))
until qga guest-ping >/dev/null 2>&1; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the guest agent never answered after the upgrade"
  sleep 1
done
qga guest-shutdown >/dev/null
deadline=$((SECONDS + 40))
while kill -0 "$QEMU_PID" 2>/dev/null; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the node didn't power off after guest-shutdown"
  sleep 1
done
QEMU_PID=""
tail="$(awk "/$MARKER/{n++} n>=2" "$LOG")"
check "init got the power-off request" 'init: power-off requested' "$tail"
check "janusd stopped HAProxy and the extensions first" 'extensions: stopped prometheus-node-exporter' "$tail"
check "the kernel powered down" 'reboot: Power down' "$tail"

if grep -q "avc:.*denied" "$LOG"; then
  grep "avc:.*denied" "$LOG" >&2
  fail "AVC denials under enforcing"
fi
echo "  ok: zero AVC denials (enforcing)"

echo "Extensions test OK: a node built from schematic $SCHEMATIC_ID knows it, runs node_exporter and the QEMU guest agent under janusd and SELinux enforcing, keeps its extensions through an upgrade that refuses a different schematic, and powers off cleanly when the hypervisor asks"
