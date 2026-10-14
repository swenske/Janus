#!/usr/bin/env bash
# Proves a node follows the vCPUs and memory a hypervisor plugs into it
# or takes out while it runs (the Controller's auto-scaling): UEFI,
# SELinux enforcing, KVM. Two vCPUs plugged in (QMP device_add, as
# libvirt's setvcpus --live does) are onlined by init, janusd tells it
# once - one event - and reloads HAProxy, which then runs a thread per
# CPU; memory plugged through virtio-mem is onlined by the kernel into
# ZONE_MOVABLE and taken back out; a vCPU unplugged, HAProxy follows;
# a configuration that fixes nbthread isn't reloaded for nothing; no AVC
# denial.
#
# Needs KVM: the kernel only counts QEMU's hot-pluggable vCPUs under a
# hypervisor it recognizes (acpi_is_processor_usable) - under TCG it
# sees none.
#
# Usage: hack/qemu-hotplug-test.sh <disk.img> <janusctl-bin>
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

SRC_DISK="${1:?usage: $0 <disk.img> <janusctl-bin>}"
CTL_BIN="${2:?usage: $0 <disk.img> <janusctl-bin>}"
TIMEOUT="${QEMU_HOTPLUG_TIMEOUT:-90}"
P_HTTP="${QEMU_HOTPLUG_HTTP_PORT:-$((18471 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
P_GRPC="${QEMU_HOTPLUG_GRPC_PORT:-$((18472 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
QMP_PY="$(dirname "$0")/qmp.py"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"

WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT
LOG="$WORKDIR/boot.log"
fail() {
  echo "Hot-plug test FAILED: $*" >&2
  [ -f "$LOG" ] && { echo "--- console output ---" >&2; cat "$LOG" >&2; }
  exit 1
}

[ -w /dev/kvm ] || fail "/dev/kvm isn't usable: CPU hot-plug needs KVM (the kernel ignores QEMU's hot-pluggable vCPUs under TCG)"

DISK="$WORKDIR/disk.img"
cp "$SRC_DISK" "$DISK"
cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/OVMF_VARS.fd"
QMP="$WORKDIR/qmp.sock"

# Room to grow: 1 vCPU of 4, 1 GiB plus a 4 GiB virtio-mem device with
# nothing plugged - a libvirt domain the Controller makes scalable.
qemu-system-x86_64 -machine q35 -accel kvm -cpu host \
  -smp 1,maxcpus=4 \
  -m 1G,maxmem=5G,slots=4 \
  -object memory-backend-ram,id=vmem0,size=4G \
  -device virtio-mem-pci,id=vmem,memdev=vmem0,node=0,requested-size=0 \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$WORKDIR/OVMF_VARS.fd" \
  -drive file="$DISK",format=raw,if=virtio \
  -nographic -display none \
  -netdev "user,id=net0,hostfwd=tcp::${P_HTTP}-:8080,hostfwd=tcp::${P_GRPC}-:9505" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG" \
  -qmp unix:"$QMP",server=on,wait=off \
  &
QEMU_PID=$!

qmp() { python3 "$QMP_PY" "$QMP" "$@"; }
ctl() { "$CTL_BIN" -endpoint "127.0.0.1:${P_GRPC}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" "$@"; }

# The PKI is written once janusd listens - HAProxy answers before that.
deadline=$((SECONDS + TIMEOUT))
until grep -aq "listening on" "$LOG" 2>/dev/null; do
  [ "$SECONDS" -lt "$deadline" ] || fail "janusd never listened"
  sleep 1
done
start="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
size="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
dd if="$DISK" of="$WORKDIR/state.img" bs=512 skip="$start" count="$size" status=none
for f in ca.crt admin.crt admin.key; do
  debugfs -R "dump pki/$f $WORKDIR/$f" "$WORKDIR/state.img" >/dev/null 2>&1
  [ -s "$WORKDIR/$f" ] || fail "couldn't extract pki/$f from STATE"
done
deadline=$((SECONDS + TIMEOUT))
until ctl version >/dev/null 2>&1; do
  [ "$SECONDS" -lt "$deadline" ] || fail "janusd never answered"
  sleep 1
done

# waitFor DESCRIPTION FUNCTION...: polls the function until it succeeds.
waitFor() {
  local what="$1"; shift
  local deadline=$((SECONDS + 30))
  until "$@"; do
    [ "$SECONDS" -lt "$deadline" ] || fail "$what"
    sleep 1
  done
}
online() { ctl system cat /sys/devices/system/cpu/online; }
memTotalMiB() { ctl system cat /proc/meminfo | awk '/^MemTotal:/ {print int($2 / 1024)}'; }
# events TYPE: the payloads of the node's events of that type, oldest first.
events() { { timeout 3 "$CTL_BIN" -endpoint "127.0.0.1:${P_GRPC}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" system events 2>/dev/null || true; } | awk -F'\t' -v t="$1" '$3 == t {print $4}'; }
# haproxyThreads: the threads of the newest HAProxy process.
haproxyThreads() {
  local pid
  pid="$(ctl system ps | awk '/\/usr\/local\/sbin\/haproxy/ {print $1}' | sort -n | tail -1)"
  [ -n "$pid" ] || return 1
  ctl system cat "/proc/$pid/status" | awk '/^Threads:/ {print $2}'
}
onlineIs() { [ "$(online)" = "$1" ]; }
eventCount() { [ "$(events "$1" | wc -l)" -ge "$2" ]; }
threadsAre() { [ "$(haproxyThreads)" = "$1" ]; }

# --- at boot ---
[ "$(online)" = 0 ] || fail "the node didn't start with one CPU: $(online)"
[ "$(ctl system cat /sys/devices/system/cpu/possible)" = 0-3 ] || fail "the kernel doesn't count the hot-pluggable vCPUs: possible $(ctl system cat /sys/devices/system/cpu/possible)"
[ "$(ctl system cat /sys/module/memory_hotplug/parameters/online_policy)" = auto-movable ] || fail "init didn't set the auto-movable memory policy"
[ "$(ctl system cat /sys/devices/system/memory/auto_online_blocks)" = online ] || fail "the kernel doesn't online plugged-in memory itself"
base="$(memTotalMiB)"
[ "$base" -lt 1024 ] || fail "MemTotal at boot is ${base} MiB, more than the 1 GiB the machine has"
waitFor "HAProxy doesn't run one thread at boot" threadsAre 1
echo "  ok: one vCPU of four, ${base} MiB, room for both to grow"

# --- two vCPUs plugged in: onlined, one event, HAProxy follows ---
for core in 1 2; do
  qmp device_add "{\"driver\":\"host-x86_64-cpu\",\"id\":\"cpu$core\",\"socket-id\":0,\"core-id\":$core,\"thread-id\":0}" >/dev/null \
    || fail "QEMU refused to plug vCPU $core"
done
waitFor "the plugged vCPUs weren't onlined: $(online)" onlineIs 0-2
grep -aq "init: cpu1 online (plugged in)" "$LOG" || fail "init didn't say it onlined cpu1"
waitFor "no system.cpus_changed event" eventCount system.cpus_changed 1
ev="$(events system.cpus_changed)"
[ "$(wc -l <<<"$ev")" = 1 ] || fail "two vCPUs plugged together gave several events: $ev"
grep -q '"after":3' <<<"$ev" && grep -q '"before":1' <<<"$ev" && grep -q '"haproxy":"reloaded"' <<<"$ev" \
  || fail "the cpus_changed event: $ev"
waitFor "HAProxy doesn't run a thread per CPU after the reload: $(haproxyThreads)" threadsAre 3
curl -fsS "http://127.0.0.1:${P_HTTP}/" >/dev/null || fail "HAProxy doesn't answer after the reload"
echo "  ok: two vCPUs plugged in - online, one event, HAProxy reloaded with 3 threads"

# --- memory plugged in through virtio-mem: onlined, movable, one event ---
qmp qom-set '{"path":"/machine/peripheral/vmem","property":"requested-size","value":2147483648}' >/dev/null \
  || fail "QEMU refused the virtio-mem request"
grown() { [ "$(memTotalMiB)" -ge $((base + 2000)) ]; }
waitFor "the plugged memory wasn't onlined: MemTotal $(memTotalMiB) MiB" grown
movable="$(ctl system cat /proc/zoneinfo | awk '/^Node .* zone +Movable/ {m=1; next} /^Node/ {m=0} m && $1 == "present" {print $2}')"
[ "${movable:-0}" -gt 0 ] || fail "the plugged memory isn't in ZONE_MOVABLE"
waitFor "no system.memory_changed event" eventCount system.memory_changed 1
ev="$(events system.memory_changed | tail -1)"
grep -q "\"before_mib\":${base}" <<<"$ev" || fail "the memory_changed event: $ev"
echo "  ok: 2 GiB plugged in - $(memTotalMiB) MiB, $((movable / 256)) MiB of it movable, one event"

# --- and taken back out ---
qmp qom-set '{"path":"/machine/peripheral/vmem","property":"requested-size","value":0}' >/dev/null \
  || fail "QEMU refused to shrink the virtio-mem device"
shrunk() { [ "$(memTotalMiB)" -le "$base" ]; }
waitFor "the memory wasn't taken back: MemTotal $(memTotalMiB) MiB" shrunk
[ "$(qmp qom-get '{"path":"/machine/peripheral/vmem","property":"size"}')" = 0 ] || fail "the virtio-mem device still has memory plugged"
waitFor "no second system.memory_changed event" eventCount system.memory_changed 2
echo "  ok: the 2 GiB taken back out - $(memTotalMiB) MiB"

# --- a vCPU unplugged: the kernel offlines it, HAProxy follows ---
qmp device_del '{"id":"cpu2"}' >/dev/null || fail "QEMU refused to unplug vCPU 2"
waitFor "vCPU 2 is still there: $(online)" onlineIs 0-1
waitFor "no second system.cpus_changed event" eventCount system.cpus_changed 2
waitFor "HAProxy doesn't follow the unplugged vCPU: $(haproxyThreads) threads" threadsAre 2
echo "  ok: a vCPU unplugged - HAProxy back to 2 threads"

# --- a configuration that fixes its threads isn't reloaded for nothing ---
cat > "$WORKDIR/nbthread.cfg" <<'EOF'
global
    stats socket /run/janus/haproxy-admin.sock mode 660 level admin
    chroot /var/empty
    uid 1000
    gid 1000
    nbthread 2

defaults
    mode http
    timeout connect 5s
    timeout client 30s
    timeout server 30s

frontend janus-health
    bind *:8080
    http-request return status 200 content-type text/plain string "Janus: haproxy is up\n"
EOF
ctl haproxy apply-config "$WORKDIR/nbthread.cfg" >/dev/null || fail "a configuration with nbthread 2 was refused"
qmp device_add '{"driver":"host-x86_64-cpu","id":"cpu3","socket-id":0,"core-id":3,"thread-id":0}' >/dev/null \
  || fail "QEMU refused to plug vCPU 3"
waitFor "no third system.cpus_changed event" eventCount system.cpus_changed 3
ev="$(events system.cpus_changed | tail -1)"
grep -q 'not reloaded: its configuration' <<<"$ev" && grep -q 'nbthread' <<<"$ev" || fail "a configuration with nbthread was reloaded anyway: $ev"
threadsAre 2 || fail "HAProxy's fixed 2 threads changed: $(haproxyThreads)"
echo "  ok: nbthread fixed - the plugged vCPU is online, HAProxy left alone"

if grep -aq "avc:  denied" "$LOG"; then
  grep -a "avc:  denied" "$LOG" >&2
  fail "AVC denials"
fi
echo "  ok: no AVC denial"
echo "Hot-plug test PASSED"
