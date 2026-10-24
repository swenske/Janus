#!/usr/bin/env bash
# Proves a node follows the vCPUs and memory a hypervisor plugs into it
# or takes out while it runs (the Controller's auto-scaling): UEFI,
# SELinux enforcing, KVM. Two vCPUs plugged in (QMP device_add, as
# libvirt's setvcpus --live does) are onlined by init, janusd tells it
# once - one event - and reloads HAProxy, which then runs a thread per
# CPU; memory plugged through virtio-mem is onlined by the kernel into
# ZONE_MOVABLE and taken back out, the kernel parameters it sized at
# boot (tcp_mem, file-max) suggested in proportion meanwhile; the vCPUs
# waiting for a host short of CPU counted as steal time; a vCPU
# unplugged, HAProxy follows; a configuration that fixes nbthread isn't
# reloaded for nothing; an OOM kill is an event and counted; no AVC
# denial.
#
# Needs KVM: the kernel only counts QEMU's hot-pluggable vCPUs under a
# hypervisor it recognizes (acpi_is_processor_usable) - under TCG it
# sees none.
#
# Usage: hack/qemu-hotplug-test.sh <disk.img> <janusctl-bin>
set -euo pipefail
# shellcheck source=state-read.sh
. "$(dirname "$0")/state-read.sh"

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
state_pki "$DISK" "$WORKDIR" || fail "couldn't extract the node's pki/ from STATE"
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
# init's line and the kernel's share the serial console: a printk can
# land in the middle of it ("init: cpu1 onlin" "ACPI: CPU2 has been
# hot-added" "e (plugged in)", v2026.10.24's build) - read across.
saidOnline() {
  python3 - "$LOG" "$1" <<'PYEOF'
import re, sys
log = open(sys.argv[1], errors="replace").read()
sys.exit(0 if re.search(r"init: cpu%s onl.{0,600}?\(plugged in\)" % sys.argv[2], log, re.S) else 1)
PYEOF
}
saidOnline 1 || fail "init didn't say it onlined cpu1"
waitFor "no system.cpus_changed event" eventCount system.cpus_changed 1
ev="$(events system.cpus_changed)"
[ "$(wc -l <<<"$ev")" = 1 ] || fail "two vCPUs plugged together gave several events: $ev"
grep -q '"after":3' <<<"$ev" && grep -q '"before":1' <<<"$ev" && grep -q '"haproxy":"reloaded"' <<<"$ev" \
  || fail "the cpus_changed event: $ev"
waitFor "HAProxy doesn't run a thread per CPU after the reload: $(haproxyThreads)" threadsAre 3
curl -fsS "http://127.0.0.1:${P_HTTP}/" >/dev/null || fail "HAProxy doesn't answer after the reload"
echo "  ok: two vCPUs plugged in - online, one event, HAProxy reloaded with 3 threads"

# --- a host short of CPU: the vCPUs wait for it, the node says so ---
# QEMU's threads and a busy loop on one host CPU while HAProxy answers:
# the 3 vCPUs wait for it (steal), and SystemStat counts it apart from
# the busy time - what keeps auto-scaling from adding a vCPU then.
steal() { ctl system systemstat | awk '/^cpu ticks:/ {print $7}'; }
cpu="$(taskset -cp "$$" | awk '{print $NF}' | cut -d, -f1 | cut -d- -f1)"
s0="$(steal)"
taskset -a -p -c "$cpu" "$QEMU_PID" >/dev/null
taskset -c "$cpu" timeout 10 sh -c 'while :; do :; done' &
spin=$!
end=$((SECONDS + 10))
while [ "$SECONDS" -lt "$end" ]; do curl -s -o /dev/null "http://127.0.0.1:${P_HTTP}/" || true; done
wait "$spin" || true
taskset -a -p -c "$(taskset -cp "$$" | awk '{print $NF}')" "$QEMU_PID" >/dev/null
s1="$(steal)"
[ -n "$s0" ] && [ -n "$s1" ] && [ "$s1" -gt "$s0" ] || fail "no steal time counted with the vCPUs waiting for one host CPU: $s0 then $s1"
echo "  ok: the vCPUs waited for the host - $((s1 - s0)) steal ticks in 10 s, counted apart"

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

# The kernel sized tcp_mem and file-max from the boot's memory: both
# suggested in proportion to the memory now, never applied by themselves.
out="$(ctl system sysctl get net.ipv4.tcp_mem)"
grep -q "^Suggested: .* - never applied by itself (rule tcp_mem.follow_memory)" <<<"$out" || fail "no memory-following suggestion for tcp_mem: $out"
bootPressure="$(ctl system cat /proc/sys/net/ipv4/tcp_mem | awk '{print $2}')"
suggested="$(sed -n 's/^Suggested: [0-9]* \([0-9]*\) .*/\1/p' <<<"$out")"
[ "$suggested" -gt $((bootPressure * 3)) ] || fail "tcp_mem's suggestion ($suggested) isn't about $((bootPressure * (base + 2048) / base)) for $(memTotalMiB) MiB"
grep -q "^Suggested: .*(rule file_max.follow_memory)" <<<"$(ctl system sysctl get fs.file-max)" || fail "no memory-following suggestion for file-max"
echo "  ok: tcp_mem and file-max suggested for the memory now ($suggested pages under pressure, $bootPressure at boot)"

# --- and taken back out ---
qmp qom-set '{"path":"/machine/peripheral/vmem","property":"requested-size","value":0}' >/dev/null \
  || fail "QEMU refused to shrink the virtio-mem device"
shrunk() { [ "$(memTotalMiB)" -le "$base" ]; }
waitFor "the memory wasn't taken back: MemTotal $(memTotalMiB) MiB" shrunk
[ "$(qmp qom-get '{"path":"/machine/peripheral/vmem","property":"size"}')" = 0 ] || fail "the virtio-mem device still has memory plugged"
waitFor "no second system.memory_changed event" eventCount system.memory_changed 2
if grep -q "follow_memory" <<<"$(ctl system sysctl get net.ipv4.tcp_mem)"; then fail "tcp_mem is still suggested at the boot's memory"; fi
echo "  ok: the 2 GiB taken back out - $(memTotalMiB) MiB, nothing to follow any more"

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

# --- an OOM kill: an event, counted, HAProxy serving again ---
# Three caches HAProxy writes all through when it starts (their block
# headers), each smaller than the memory - each allocation passes the
# kernel's overcommit check -, together larger: the kernel kills it. The
# configuration is refused; the previous HAProxy keeps serving - or, when
# the OOM killer takes it too (v2026.10.14-2's CI: janusd allocating
# before the killed process's shared memory was freed), janusd starts it
# again with the previous configuration.
cat > "$WORKDIR/oom.cfg" <<'EOF'
global
    stats socket /run/janus/haproxy-admin.sock mode 660 level admin
    chroot /var/empty
    uid 1000
    gid 1000

cache c1
    total-max-size 400
cache c2
    total-max-size 400
cache c3
    total-max-size 400

defaults
    mode http
    timeout connect 5s
    timeout client 30s
    timeout server 30s

frontend janus-health
    bind *:8080
    http-request cache-use c1
    http-request cache-use c2
    http-request cache-use c3
    http-request return status 200 content-type text/plain string "Janus: haproxy is up\n"
EOF
if out="$(ctl haproxy apply-config "$WORKDIR/oom.cfg" 2>&1)"; then fail "a configuration larger than the memory was applied: $out"; fi
waitFor "no kernel.oom_kill event" eventCount kernel.oom_kill 1
ev="$(events kernel.oom_kill | tail -1)"
grep -q '"process":"haproxy"' <<<"$ev" || fail "the OOM kill event: $ev"
grep -q "^oom kills since boot: [1-9]" <<<"$(ctl system info)" || fail "the OOM kill isn't counted: $(ctl system info | grep oom)"
serves() { curl -fsS -m 2 "http://127.0.0.1:${P_HTTP}/" >/dev/null 2>&1; }
waitFor "HAProxy doesn't serve again after the OOM kill" serves
how="the previous HAProxy still serving"
if [ -n "$(events haproxy.restarted)" ]; then how="the previous HAProxy killed too, started again by janusd ($(events haproxy.restarted | tail -1))"; fi
echo "  ok: an OOM kill - event $ev, counted, $how"

if grep -aq "avc:  denied" "$LOG"; then
  grep -a "avc:  denied" "$LOG" >&2
  fail "AVC denials"
fi
echo "  ok: no AVC denial"
echo "Hot-plug test PASSED"
