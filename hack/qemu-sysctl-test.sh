#!/usr/bin/env bash
# Proves the kernel parameters (internal/sysctl, docs/guide/kernel-tuning.md)
# on a real node: UEFI, SELinux enforcing. The CIS benchmark holds at boot
# - 33/33, eth0's own IPv6 values included; every whitelisted parameter
# takes its minimum, then its maximum, on trial, with the benchmark still
# holding; a trial reverts by itself when its time runs out; a cancel puts
# the values back at once; a confirmed change is saved and back after a
# reboot; a CIS key, a read-only or forbidden one, an out-of-bounds value
# and a port range over a listening port are refused, nothing changed; a
# saved file tampered with on STATE is refused at boot line by line, the
# benchmark intact; the history says who did what; no AVC denial.
#
# Usage: hack/qemu-sysctl-test.sh <disk.img> <janusctl-bin>
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

SRC_DISK="${1:?usage: $0 <disk.img> <janusctl-bin>}"
CTL_BIN="${2:?usage: $0 <disk.img> <janusctl-bin>}"
TIMEOUT="${QEMU_SYSCTL_TIMEOUT:-60}"
P_HTTP="${QEMU_SYSCTL_HTTP_PORT:-$((18451 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
P_GRPC="${QEMU_SYSCTL_GRPC_PORT:-$((18452 + ${JANUS_TEST_PORT_OFFSET:-0}))}"

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
  echo "Sysctl test FAILED: $*" >&2
  [ -f "$LOG" ] && { echo "--- console output ---" >&2; cat "$LOG" >&2; }
  exit 1
}

DISK="$WORKDIR/disk.img"
cp "$SRC_DISK" "$DISK"
cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/OVMF_VARS.fd"

boot() {
  qemu-system-x86_64 -accel kvm -accel tcg \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$WORKDIR/OVMF_VARS.fd" \
    -drive file="$DISK",format=raw,if=virtio \
    -nographic -display none -m 1G \
    -netdev "user,id=net0,hostfwd=tcp::${P_HTTP}-:8080,hostfwd=tcp::${P_GRPC}-:9505" \
    -device virtio-net-pci,netdev=net0 \
    -serial file:"$LOG" \
    &
  QEMU_PID=$!
}

# waitUp waits for janusd's API - not for HAProxy, which starts first.
waitUp() {
  local deadline=$((SECONDS + TIMEOUT))
  until grep -aq "listening on" "$LOG" 2>/dev/null && ctl version >/dev/null 2>&1; do
    [ "$SECONDS" -lt "$deadline" ] || fail "janusd never answered"
    sleep 1
  done
}

STATE_START=""
STATE_SIZE=""
stateGeometry() {
  STATE_START="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
  STATE_SIZE="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
}
ctl() { "$CTL_BIN" -endpoint "127.0.0.1:${P_GRPC}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" "$@"; }

# value prints a parameter's live value, as `sysctl get` reports it.
value() { ctl system sysctl get "$1" | sed -n "1s/^$1 = //p"; }

boot
# The PKI is written once janusd listens - HAProxy answers before that.
deadline=$((SECONDS + TIMEOUT))
until grep -aq "listening on" "$LOG" 2>/dev/null; do
  [ "$SECONDS" -lt "$deadline" ] || fail "janusd never listened"
  sleep 1
done
stateGeometry
dd if="$DISK" of="$WORKDIR/state.img" bs=512 skip="$STATE_START" count="$STATE_SIZE" status=none
for f in ca.crt admin.crt admin.key; do
  debugfs -R "dump pki/$f $WORKDIR/$f" "$WORKDIR/state.img" >/dev/null 2>&1
  [ -s "$WORKDIR/$f" ] || fail "couldn't extract pki/$f from STATE"
done
waitUp

# --- the benchmark at boot ---
grep -aqE "^init: sysctl: CIS Debian Linux 13 Benchmark v1.0.0 \(Level 2 - Server\): 33/33 controls compliant" <(tr -d '\r' < "$LOG") || fail "no compliant CIS audit at boot"
for leaf in accept_ra accept_redirects; do
  grep -aqF "init: sysctl /proc/sys/net/ipv6/conf/eth0/${leaf}=0" "$LOG" || fail "eth0's IPv6 ${leaf} wasn't written"
done
out="$(ctl system sysctl list -cis)"
grep -q "33/33 controls compliant" <<<"$out" || fail "the API's audit: $out"
grep -qE "^net.core.somaxconn +60000 +60000 +default" <<<"$out" || fail "somaxconn isn't at Janus's default: $out"
grep -qE "^net.ipv4.ip_local_port_range +10240 65023 " <<<"$out" || fail "the source port range: $out"
grep -qE "^net.ipv4.tcp_max_syn_backlog " <<<"$out" || fail "the read-only parameters aren't listed: $out"
echo "  ok: the CIS benchmark holds at boot (33/33, eth0 included), Janus's defaults are in"

# IPv6: off at boot (the UKI's cmdline), on once the benchmark's values
# are on every interface - eth0 has its link-local address, and nothing
# a router advertisement gives.
grep -qw "ipv6.disable_ipv6=1" <<<"$(ctl system cat /proc/cmdline)" || fail "the kernel's cmdline doesn't start with IPv6 off"
for c in all default eth0; do
  [ "$(ctl system cat "/proc/sys/net/ipv6/conf/$c/disable_ipv6")" = 0 ] || fail "IPv6 isn't on for $c"
done
addrs="$(ctl system cat /proc/net/if_inet6)"
grep -qE '^fe80[0-9a-f]{28} [0-9a-f]+ 40 20 .* eth0$' <<<"$addrs" || fail "eth0 has no IPv6 link-local address: $addrs"
if grep -E ' eth0$' <<<"$addrs" | grep -qvE '^fe80'; then fail "eth0 has an IPv6 address besides its link-local one: $addrs"; fi
routes="$(ctl system cat /proc/net/ipv6_route)"
if grep -qE '^0{32} 00 .* eth0$' <<<"$routes"; then fail "eth0 has an IPv6 default route: $routes"; fi
echo "  ok: IPv6 came up after the benchmark's values - eth0's link-local address only, no default route"

# --- suggestions: what the node observes, what it suggests - never applied ---
# The observer samples every 15 seconds: HAProxy's listener, found by its
# inode, and this VM's 1 GiB make the memory rule suggest a lower somaxconn.
deadline=$((SECONDS + 90))
until out="$(ctl system sysctl get net.core.somaxconn)" && grep -q "^Suggested: 16384 - never applied by itself (rule somaxconn.memory)" <<<"$out"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "no memory suggestion for somaxconn: $out"
  sleep 5
done
grep -q "HAProxy's listeners: 1: 0.0.0.0:8080 (backlog 60000)" <<<"$out" || fail "the suggestion doesn't rest on HAProxy's listener: $out"
[ "$(value net.core.somaxconn)" = "60000" ] || fail "a suggestion was applied"
# Never `ctl ... | grep -q`: grep quits at its match, janusctl's next
# write of the table gets SIGPIPE, and under pipefail that's a failure -
# a race a faster boot made real. Capture, then grep the string.
listing="$(ctl system sysctl list)"
grep -qE "^net.core.somaxconn +60000 +60000 +default +16384$" <<<"$listing" || fail "list's SUGGESTED column: $listing"
# An accept queue overflowing for real: HAProxy at its frontend's maxconn
# (one idle connection held) stops accepting, its backlog of 1 fills, the
# kernel drops the next SYNs - TcpExt ListenOverflows, in this hour.
ctl haproxy get-config > "$WORKDIR/haproxy.cfg" || fail "get-config"
sed 's/^    bind \*:8080$/    bind *:8080 backlog 1\n    maxconn 1/' "$WORKDIR/haproxy.cfg" > "$WORKDIR/tiny.cfg"
grep -q "backlog 1" "$WORKDIR/tiny.cfg" || fail "couldn't shrink the health frontend: $(cat "$WORKDIR/haproxy.cfg")"
ctl haproxy apply-config "$WORKDIR/tiny.cfg" >/dev/null || fail "apply the tiny frontend"
exec 7<>"/dev/tcp/127.0.0.1/${P_HTTP}"
sleep 1
# The connections only - a bare wait would wait for QEMU too.
pids=()
for _ in $(seq 20); do
  curl -s -m 2 -o /dev/null "http://127.0.0.1:${P_HTTP}/" &
  pids+=($!)
done
wait "${pids[@]}" || true
deadline=$((SECONDS + 60))
until out="$(ctl system sysctl observed)" && grep -qE "^Accept queue overflows +1 h +[0-9,]+ in its busiest hour" <<<"$out"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the overflows weren't observed: $out"
  sleep 5
done
exec 7>&-
ctl haproxy apply-config "$WORKDIR/haproxy.cfg" >/dev/null || fail "put the health frontend back"
echo "  ok: suggested from the node's memory and HAProxy's listener, never applied; real accept queue overflows observed in the hour"

# --- every parameter's minimum, then its maximum ---
MINS=(
  "net.core.somaxconn=4096" "net.ipv4.ip_local_port_range=1024 5119" "net.ipv4.ip_local_reserved_ports=1024-1055,65535"
  "net.ipv4.tcp_tw_reuse=0" "net.ipv4.tcp_fin_timeout=30" "net.ipv4.tcp_synack_retries=1"
  "net.ipv4.ip_nonlocal_bind=0" "net.ipv6.ip_nonlocal_bind=0" "net.core.netdev_max_backlog=1000"
  "net.ipv4.tcp_rmem=4096 4096 65536" "net.ipv4.tcp_wmem=4096 4096 65536" "fs.file-max=8192"
  "net.ipv4.tcp_keepalive_time=60" "net.ipv4.tcp_keepalive_intvl=10" "net.ipv4.tcp_keepalive_probes=3"
  "net.ipv4.tcp_fastopen=0" "net.netfilter.nf_conntrack_max=16384"
)
MAXS=(
  "net.core.somaxconn=65535" "net.ipv4.ip_local_port_range=32768 65535" "net.ipv4.ip_local_reserved_ports="
  "net.ipv4.tcp_tw_reuse=2" "net.ipv4.tcp_fin_timeout=120" "net.ipv4.tcp_synack_retries=5"
  "net.ipv4.ip_nonlocal_bind=1" "net.ipv6.ip_nonlocal_bind=1" "net.core.netdev_max_backlog=65536"
  "net.ipv4.tcp_rmem=65536 4194304 67108864" "net.ipv4.tcp_wmem=65536 4194304 67108864" "fs.file-max=67108864"
  "net.ipv4.tcp_keepalive_time=7200" "net.ipv4.tcp_keepalive_intvl=75" "net.ipv4.tcp_keepalive_probes=20"
  "net.ipv4.tcp_fastopen=3" "net.netfilter.nf_conntrack_max=262144"
)
fileMax="$(value fs.file-max)"
for set in MINS MAXS; do
  declare -n values="$set"
  out="$(ctl system sysctl set -no-confirm "${values[@]}" 2>&1)" || fail "$set refused: $out"
  grep -q "on trial" <<<"$out" || fail "$set: $out"
  for nv in "${values[@]}"; do
    name="${nv%%=*}"
    want="${nv#*=}"
    [ -n "$want" ] || want="(none)"
    got="$(value "$name")"
    [ "$got" = "$want" ] || fail "$name is $got on trial, want $want"
  done
  listing="$(ctl system sysctl list)"
  grep -q "33/33 controls compliant" <<<"$listing" || fail "$set broke the CIS benchmark: $(ctl system sysctl list -cis)"
  ctl system sysctl cancel >/dev/null || fail "cancel after $set"
  [ "$(value net.core.somaxconn)" = "60000" ] || fail "somaxconn not back after cancelling $set"
  [ "$(value fs.file-max)" = "$fileMax" ] || fail "file-max not back after cancelling $set"
  unset -n values
done
echo "  ok: every parameter's minimum and maximum on trial, the benchmark holding, cancelled back"

# --- refusals: nothing changes ---
refused() {
  local want="$1"
  shift
  local out
  if out="$(ctl system sysctl set -no-confirm "$@" 2>&1)"; then fail "accepted: $* ($out)"; fi
  grep -q "$want" <<<"$out" || fail "$*: refused, but not for \"$want\": $out"
}
refused "locked: CIS Debian Linux 13 Benchmark v1.0.0 control 3.3.1.18" "net.ipv4.tcp_syncookies=0"
refused "locked: .* control 3.3.2.7" "net.ipv6.conf.all.accept_ra=1"
refused "read-only" "fs.nr_open=2000000"
refused "forbidden" "kernel.core_pattern=|/bin/x"
refused "not a parameter Janus lets anyone change" "vm.swappiness=10"
refused "between 4096 and 65535" "net.core.somaxconn=100"
refused "listens on 8080" "net.ipv4.ip_local_port_range=8000 20000"
refused "with this memory" "net.netfilter.nf_conntrack_max=4194304"
listing="$(ctl system sysctl list)"
if grep -q "On trial" <<<"$listing"; then fail "a refused change went on trial"; fi
[ "$(value net.core.somaxconn)" = "60000" ] || fail "a refusal changed somaxconn"
echo "  ok: CIS, read-only, forbidden, unknown, out-of-bounds and context refusals - nothing changed"

# --- a trial reverts by itself ---
ctl system sysctl set -timeout 1m -no-confirm net.ipv4.tcp_fin_timeout=45 >/dev/null || fail "trial"
[ "$(value net.ipv4.tcp_fin_timeout)" = "45" ] || fail "fin_timeout not on trial"
deadline=$((SECONDS + 90))
until [ "$(value net.ipv4.tcp_fin_timeout)" = "30" ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the trial never reverted"
  sleep 3
done
grep -aq "sysctl: trial not confirmed in time - the previous values are back" "$LOG" || fail "no revert in janusd's log"
echo "  ok: an unconfirmed trial reverts by itself"

# --- somaxconn: HAProxy reloads for it ---
out="$(ctl system sysctl set -no-confirm net.core.somaxconn=30000)" || fail "somaxconn trial: $out"
grep -q "HAProxy reloaded" <<<"$out" || fail "HAProxy wasn't reloaded for somaxconn: $out"
[ "$(curl -s -m 3 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${P_HTTP}/" || true)" = "200" ] || fail "HAProxy doesn't answer after the reload"
ctl system sysctl cancel >/dev/null || fail "cancel"
echo "  ok: somaxconn's trial reloads HAProxy, which keeps serving"

# --- confirmed: saved, back after a reboot ---
out="$(ctl system sysctl set net.core.somaxconn=30000 net.ipv4.ip_local_reserved_ports=12345 net.ipv4.tcp_fastopen=3)" || fail "set: $out"
grep -q "confirmed over a new connection" <<<"$out" || fail "not confirmed: $out"
boots="$(grep -ac "J A N U S" "$LOG" || true)"
ctl system reboot >/dev/null || fail "reboot"
deadline=$((SECONDS + TIMEOUT + 30))
until [ "$(grep -ac "J A N U S" "$LOG" || true)" -gt "$boots" ] && ctl version >/dev/null 2>&1; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the node didn't come back"
  sleep 2
done
grep -aqF "init: sysctl /proc/sys/net/core/somaxconn=30000" "$LOG" || fail "the saved somaxconn wasn't applied at boot"
[ "$(value net.core.somaxconn)" = "30000" ] || fail "somaxconn after the reboot"
[ "$(value net.ipv4.ip_local_reserved_ports)" = "12345" ] || fail "reserved ports after the reboot"
[ "$(value net.ipv4.tcp_fastopen)" = "3" ] || fail "fastopen after the reboot"
listing="$(ctl system sysctl list)"
grep -q "33/33 controls compliant" <<<"$listing" || fail "the benchmark after a reboot with saved values"
echo "  ok: confirmed values saved, applied at the next boot, the benchmark holding"
# Saved before the reboot (internal/shutdown): the hour's overflows are still there.
out="$(ctl system sysctl observed)"
grep -qE "^Accept queue overflows +1 h " <<<"$out" || fail "the observations didn't survive the reboot: $out"
echo "  ok: what the node observed survives a reboot"

# --- reset: back to the defaults, the saved file gone ---
out="$(ctl system sysctl reset -all)" || fail "reset -all: $out"
grep -q "confirmed over a new connection" <<<"$out" || fail "reset not confirmed: $out"
[ "$(value net.core.somaxconn)" = "60000" ] || fail "somaxconn after the reset"
[ "$(value net.ipv4.ip_local_reserved_ports)" = "(none)" ] || fail "reserved ports after the reset"
listing="$(ctl system sysctl list)"
if grep -qE " saved$" <<<"$listing"; then fail "something still saved after a reset"; fi
echo "  ok: reset to Janus's defaults"

# --- a saved file tampered with on STATE ---
ctl system shutdown >/dev/null || fail "shutdown"
wait "$QEMU_PID" || true
QEMU_PID=""
dd if="$DISK" of="$WORKDIR/state.img" bs=512 skip="$STATE_START" count="$STATE_SIZE" status=none
# STATE isn't unmounted at power-off: its journal is replayed at the next
# mount - over anything written beside it. Replayed here first, offline.
e2fsck -fy "$WORKDIR/state.img" >/dev/null 2>&1 || [ $? -le 2 ] || fail "e2fsck STATE"
cat > "$WORKDIR/tampered.conf" <<'EOF'
# tampered with, offline
net.core.somaxconn = 20000
net.ipv4.tcp_syncookies = 0
net.ipv4.conf.all.accept_redirects = 1
kernel.core_pattern = |/bin/x
net.ipv4.tcp_fin_timeout = 5
not an assignment
EOF
debugfs -w -R "mkdir config/sysctl.d" "$WORKDIR/state.img" >/dev/null 2>&1 || true
debugfs -w -R "rm config/sysctl.d/90-haproxy-tuning.conf" "$WORKDIR/state.img" >/dev/null 2>&1 || true
debugfs -w -R "write $WORKDIR/tampered.conf config/sysctl.d/90-haproxy-tuning.conf" "$WORKDIR/state.img" >/dev/null 2>&1 || fail "debugfs write"
dd if="$WORKDIR/state.img" of="$DISK" bs=512 seek="$STATE_START" conv=notrunc status=none
cat "$LOG" >> "$WORKDIR/earlier-boots.log"
: > "$LOG"
boot
waitUp
for want in "line 3: net.ipv4.tcp_syncookies: locked: " "line 4: net.ipv4.conf.all.accept_redirects: locked: " "line 5: kernel.core_pattern: forbidden: " "line 6: net.ipv4.tcp_fin_timeout: the value must be between 30 and 120" 'line 7: not a "name = value" line'; do
  grep -aqF "init: sysctl: sysctl.d/90-haproxy-tuning.conf ${want}" "$LOG" || fail "the boot didn't refuse: $want"
done
grep -aqE "^init: sysctl: CIS .*: 33/33 controls compliant" <(tr -d '\r' < "$LOG") || fail "the benchmark after a tampered file"
[ "$(value net.core.somaxconn)" = "20000" ] || fail "the tampered file's valid line wasn't applied"
[ "$(value net.ipv4.tcp_fin_timeout)" = "30" ] || fail "an out-of-bounds line was applied"
out="$(ctl system sysctl history -n 1)"
grep -q "boot-refused" <<<"$out" || fail "no boot-refused entry in the history: $out"
echo "  ok: a tampered saved file is refused line by line at boot, the benchmark intact"

# --- the history ---
out="$(ctl system sysctl history -n 50)"
for action in trial confirm cancel revert; do
  grep -qE "  ${action} " <<<"$out" || fail "no ${action} in the history: $out"
done
grep -q "admin" <<<"$out" || fail "the history doesn't say who: $out"
echo "  ok: the history records trials, confirmations, cancels and reverts, and who"

cat "$LOG" >> "$WORKDIR/earlier-boots.log"
if grep -aq "avc:.*denied" "$WORKDIR/earlier-boots.log"; then
  grep -a "avc:.*denied" "$WORKDIR/earlier-boots.log" >&2
  fail "AVC denials under enforcing"
fi
echo "Sysctl test OK: the CIS benchmark enforced at boot and never broken, IPv6 on only once hardened, suggestions never applied, real overflows observed and kept across a reboot, every parameter's bounds on trial, reverts, cancels, confirmations kept across a reboot, refusals, a tampered file refused at boot, the history, zero AVC denials"
