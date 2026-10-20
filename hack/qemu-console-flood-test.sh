#!/usr/bin/env bash
# Proves the console never holds a service's output back
# (internal/consolelimit) on a real node: the full disk image under real
# OVMF, SELinux enforcing, 2 vCPUs. HAProxy logs every request to its
# stdout (`log stdout` + `option httplog`) while a load generator keeps it
# busy:
#   - janusd's API still answers a call that logs (MetricsConfigSet) as
#     fast as idle, give or take - before, it waited behind the console
#     (0.1 to 2 s instead of 15 ms);
#   - HAProxy serves at least 30 % of what it serves with no log at all -
#     before, the emulated serial port took most of a vCPU from it (1 800
#     requests/s instead of 98 000, 2 %);
#   - HAProxy drops at most 5 % of its log lines (its stdout full) -
#     before janusd's log ring took a line in constant time, 82 %;
#   - the console got no more than the services' budget, and says what
#     it left out; janusd's own log of HAProxy (`system logs`) has every
#     line, the last ones included;
#   - no AVC denial.
#
# Usage: hack/qemu-console-flood-test.sh <disk.img> <janusctl-bin>
set -euo pipefail
# shellcheck source=state-read.sh
. "$(dirname "$0")/state-read.sh"

export PATH="$PATH:/usr/sbin:/sbin"

SRC_DISK="${1:?usage: $0 <disk.img> <janusctl-bin>}"
CTL_BIN="${2:?usage: $0 <disk.img> <janusctl-bin>}"
LOAD_SECS="${QEMU_CONSOLE_FLOOD_SECS:-20}"
P_HTTP="${QEMU_CONSOLE_FLOOD_HTTP_PORT:-$((18351 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
P_GRPC="${QEMU_CONSOLE_FLOOD_GRPC_PORT:-$((18352 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
P_PROM="${QEMU_CONSOLE_FLOOD_PROM_PORT:-$((18353 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
# internal/consolelimit's Rate and Burst, and room for its notes.
CONSOLE_RATE=2048
CONSOLE_BURST=65536

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
  echo "Console flood test FAILED: $*" >&2
  [ -f "$LOG" ] && { echo "--- console output (last 100 lines) ---" >&2; tail -100 "$LOG" >&2; }
  exit 1
}

go build -o "$WORKDIR/httpload" ./hack/httpload

DISK="$WORKDIR/disk.img"
cp "$SRC_DISK" "$DISK"
cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/OVMF_VARS.fd"
qemu-system-x86_64 -accel kvm -accel tcg -smp 2 -m 1024M \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$WORKDIR/OVMF_VARS.fd" \
  -drive file="$DISK",format=raw,if=virtio \
  -display none -monitor none \
  -netdev "user,id=net0,hostfwd=tcp::${P_HTTP}-:8080,hostfwd=tcp::${P_GRPC}-:9505,hostfwd=tcp::${P_PROM}-:8404" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG" </dev/null &
QEMU_PID=$!

deadline=$((SECONDS + 120))
until grep -aq "listening on" "$LOG" 2>/dev/null; do
  [ "$SECONDS" -lt "$deadline" ] || fail "janusd never started listening"
  kill -0 "$QEMU_PID" 2>/dev/null || fail "QEMU exited"
  sleep 1
done
sleep 2

state_pki "$DISK" "$WORKDIR" || fail "couldn't extract the node's pki/ from STATE"
ctl() { "$CTL_BIN" -endpoint "127.0.0.1:${P_GRPC}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" "$@"; }

# config LOGLINE - an HTTP frontend answering every request itself, with
# that global log line.
config() {
  cat > "$WORKDIR/haproxy.cfg" <<EOF
global
    $1
    stats socket /run/janus/haproxy-admin.sock mode 660 level admin
    chroot /var/empty
    uid 1000
    gid 1000

defaults
    mode http
    log global
    option httplog
    timeout connect 5s
    timeout client 30s
    timeout server 30s

frontend janus-health
    bind *:8080
    http-request return status 200 content-type text/plain string "ok\n"

frontend prom
    bind *:8404
    no log
    http-request use-service prometheus-exporter if { path /metrics }
EOF
  ctl haproxy apply-config "$WORKDIR/haproxy.cfg" >/dev/null || fail "apply-config refused: $1"
  sleep 2
}

# probe - seconds a call that logs (api: SystemService/MetricsConfigSet)
# takes.
probe() {
  local t0 t1
  t0=$(date +%s%N)
  ctl system metrics -port 10056 >/dev/null || fail "MetricsConfigSet failed"
  t1=$(date +%s%N)
  echo $(((t1 - t0) / 1000000))
}

# dropped - the log lines HAProxy couldn't write (its stdout full: janusd
# reading behind) since it started.
dropped() {
  curl -s -m 5 "http://127.0.0.1:${P_PROM}/metrics" | awk '$1 == "haproxy_process_dropped_logs_total" {printf "%.0f\n", $2}'
}

# load SECONDS - requests/s HAProxy served, probing janusd once a second
# meanwhile into $WORKDIR/probes.
load() {
  : > "$WORKDIR/probes"
  "$WORKDIR/httpload" -url "http://127.0.0.1:${P_HTTP}/" -c 64 -d "${1}s" > "$WORKDIR/load.out" &
  local pid=$!
  sleep 2
  while kill -0 "$pid" 2>/dev/null; do
    probe >> "$WORKDIR/probes"
    sleep 1
  done
  wait "$pid" || fail "the load generator failed: $(cat "$WORKDIR/load.out")"
  awk '{print $6}' "$WORKDIR/load.out"
}

idle_ms="$(probe) $(probe) $(probe)"
echo "MetricsConfigSet idle: $idle_ms ms"

config "# no log"
base_rps="$(load 10)"
echo "No log: $base_rps requests/s"

config "log stdout format raw local0"
before="$(stat -c %s "$LOG")"
t0=$SECONDS
dropped0="$(dropped)"
rps="$(load "$LOAD_SECS")"
lost=$(($(dropped) - dropped0))
requests="$(awk '{print $2}' "$WORKDIR/load.out")"
elapsed=$((SECONDS - t0))
console_bytes=$(($(stat -c %s "$LOG") - before))
sort -n "$WORKDIR/probes" > "$WORKDIR/probes.sorted"
median="$(awk '{a[NR]=$1} END {print a[int((NR+1)/2)]}' "$WORKDIR/probes.sorted")"
worst="$(tail -1 "$WORKDIR/probes.sorted")"
echo "Every request logged: $rps requests/s; MetricsConfigSet median ${median} ms, worst ${worst} ms; console +${console_bytes} bytes in ${elapsed}s; HAProxy dropped $lost of $requests log lines"

[ "$median" -le 200 ] || fail "janusd's API waited behind HAProxy's output: median ${median} ms under load (idle: $idle_ms ms)"
[ "$worst" -le 1000 ] || fail "janusd's API waited behind HAProxy's output: ${worst} ms under load (idle: $idle_ms ms)"
[ $((rps * 10)) -ge $((base_rps * 3)) ] || fail "HAProxy slowed down by its own logging: $rps requests/s, $base_rps without a log line"
# janusd reads HAProxy's stdout as fast as it fills: HAProxy drops a line
# only when the pipe is full (82 % of them when each line cost janusd a
# copy of its whole log ring).
[ $((lost * 100)) -le $((requests * 5)) ] || fail "janusd read HAProxy's output too slowly: HAProxy dropped $lost of $requests log lines"
limit=$((CONSOLE_BURST + CONSOLE_RATE * (elapsed + 5) + 4096))
[ "$console_bytes" -le "$limit" ] || fail "the console got $console_bytes bytes in ${elapsed}s - more than the services' budget ($limit)"

# The notes come every 10 s.
deadline=$((SECONDS + 15))
until grep -aq "lines of haproxy's output not shown on the console - janusctl system logs haproxy" "$LOG"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the console never said it left HAProxy's lines out"
  sleep 1
done

# janusd's own copy has every line, up to the last ones.
last="$(ctl system logs -n 5 haproxy)" || fail "system logs haproxy failed"
grep -q '"GET / HTTP/1.1"' <<<"$last" || fail "janusd's log of HAProxy doesn't end with the access log lines: $last"

# The budget turned off (ConsoleBudgetSet 0): nothing of HAProxy's output
# on the console under the same load - janusd's own log still -, every
# line still in janusd's copy; then the default back, and the console
# gets its share again.
off="$(ctl system console-budget -rate 0 2>&1)" || fail "console-budget -rate 0 failed: $off"
grep -q "none of the services' output" <<<"$off" || fail "console-budget -rate 0 didn't take: $off"
sleep 2
before="$(stat -c %s "$LOG")"
rps_off="$(load 10)"
off_bytes=$(($(stat -c %s "$LOG") - before))
[ "$(tail -c +$((before + 1)) "$LOG" | grep -ac '"GET / HTTP/1.1"')" -eq 0 ] || fail "with the budget at 0, HAProxy's lines still reached the console"
[ "$off_bytes" -le 4096 ] || fail "with the budget at 0, the console still got $off_bytes bytes in 10 s"
last="$(ctl system logs -n 5 haproxy)" || fail "system logs haproxy failed"
grep -q '"GET / HTTP/1.1"' <<<"$last" || fail "with the budget at 0, janusd's log of HAProxy lost its lines: $last"
def="$(ctl system console-budget -default 2>&1)" || fail "console-budget -default failed: $def"
grep -q "(default)" <<<"$def" || fail "console-budget -default didn't take: $def"
echo "Budget off: $rps_off requests/s, the console got $off_bytes bytes in 10 s, janusd's log whole"

if grep -aq "avc:  denied" "$LOG"; then
  fail "AVC denials: $(grep -a 'avc:  denied' "$LOG" | head -5)"
fi
echo "Console flood test passed: janusd answered in ${median} ms (median) while HAProxy logged $rps requests/s to its stdout ($base_rps without logs), $lost of $requests lines dropped; the console got ${console_bytes} bytes and said what it left out"
