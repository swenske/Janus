#!/usr/bin/env bash
# janusd's own DHCP client (internal/netmgr's dhcp.go) on a real node -
# the full disk image under OVMF, SELinux enforcing - on a network of
# its own: a tap device on the host, no slirp.
#
#   1. No DHCP server at all, the node configured statically (seeded
#      offline): it must answer on its address within seconds. With the
#      kernel's ip=dhcp - the UKI's command line until #162 - such a boot
#      first waited ~165 s in the kernel, before init.
#   2. dnsmasq on that network, the node with the default configuration
#      (DHCP): a 2-minute lease - dnsmasq's shortest - renewed at T1
#      while the test watches: dnsmasq acknowledges the same MAC again,
#      the node keeps its address and answers on it all along.
#   3. janusd restarted: it takes its lease back (no new discover) and
#      renews it when due.
#   4. dnsmasq gone: the lease runs out and its address goes; dnsmasq
#      back: the node asks again by itself and gets it back.
#
# Needs sudo (the tap device, dnsmasq on it) and dnsmasq (dnsmasq-base).
#
# Usage: hack/qemu-dhcp-test.sh <disk.img> <janusctl-bin>
set -euo pipefail
# shellcheck source=state-read.sh
. "$(dirname "$0")/state-read.sh"

export PATH="$PATH:/usr/sbin:/sbin"

SRC_DISK="${1:?usage: $0 <disk.img> <janusctl-bin>}"
CTL_BIN="${2:?usage: $0 <disk.img> <janusctl-bin>}"
OFFSET="${JANUS_TEST_PORT_OFFSET:-0}"
# One network per runner instance: tap jdhcp<n>, 10.66.<n>.0/24.
N=$(((OFFSET / 100) % 250))
TAP="jdhcp$N"
NET="10.66.$N"
HOST_IP="$NET.1"
STATIC_IP="$NET.2"
MAC="52:54:00:4a:4e:01"
DNSMASQ="$(command -v dnsmasq || true)"
[ -n "$DNSMASQ" ] || { echo "dnsmasq not found (package: dnsmasq-base)" >&2; exit 1; }

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf)" >&2; exit 1; }

WORKDIR="$(mktemp -d)"
QEMU_PID=""
stop_qemu() {
  if [ -n "$QEMU_PID" ]; then
    kill "$QEMU_PID" 2>/dev/null || true
    wait "$QEMU_PID" 2>/dev/null || true
    QEMU_PID=""
  fi
}
stop_dnsmasq() {
  if [ -f "$WORKDIR/dnsmasq.pid" ]; then
    sudo kill "$(cat "$WORKDIR/dnsmasq.pid")" 2>/dev/null || true
    for _ in $(seq 50); do sudo kill -0 "$(cat "$WORKDIR/dnsmasq.pid")" 2>/dev/null || break; sleep 0.1; done
    sudo rm -f "$WORKDIR/dnsmasq.pid"
  fi
}
cleanup() {
  stop_qemu
  stop_dnsmasq
  sudo ip link del "$TAP" 2>/dev/null || true
  sudo rm -rf "$WORKDIR"
}
trap cleanup EXIT

LOG="$WORKDIR/boot.log"
fail() {
  echo "DHCP test FAILED: $*" >&2
  [ -f "$LOG" ] && { echo "--- console output ---" >&2; cat "$LOG" >&2; }
  [ -f "$WORKDIR/dnsmasq.log" ] && { echo "--- dnsmasq ---" >&2; sudo cat "$WORKDIR/dnsmasq.log" >&2; }
  exit 1
}
check() { # check "description" "grep -E pattern" "text"
  grep -Eq "$2" <<<"$3" || fail "$1: /$2/ not found in:
$3"
  echo "  ok: $1"
}
check_not() {
  if grep -Eq "$2" <<<"$3"; then fail "$1: /$2/ found in:
$3"; fi
  echo "  ok: $1"
}

sudo ip link del "$TAP" 2>/dev/null || true
sudo ip tuntap add dev "$TAP" mode tap user "$(id -un)"
sudo ip addr add "$HOST_IP/24" dev "$TAP"
sudo ip link set "$TAP" up

boot() { # boot DISK: the node on the tap network alone
  cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/OVMF_VARS.fd"
  : >"$LOG"
  qemu-system-x86_64 -accel kvm -accel tcg \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$WORKDIR/OVMF_VARS.fd" \
    -drive file="$1",format=raw,if=virtio \
    -nographic -display none -m 512M \
    -netdev "tap,id=net0,ifname=$TAP,script=no,downscript=no" \
    -device "virtio-net-pci,netdev=net0,mac=$MAC" \
    -serial file:"$LOG" \
    </dev/null &
  QEMU_PID=$!
}
http_code() { curl -s -m 2 -o /dev/null -w '%{http_code}' "http://$1:8080/" || true; }
wait_http() { # wait_http IP SECONDS
  local deadline=$((SECONDS + $2))
  until [ "$(http_code "$1")" = "200" ]; do
    [ "$SECONDS" -lt "$deadline" ] || fail "HAProxy never answered on $1 within $2 s"
    sleep 0.5
  done
}
ctl() { "$CTL_BIN" -endpoint "$NODE_IP:9505" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" "$@"; }
denials() { local n; n="$(grep -ac 'avc:  denied' "$LOG")" || true; echo "${n:-0}"; }

# --- 1. static, no DHCP server ------------------------------------------
echo "Part 1: a static node on a network without DHCP"
DISK="$WORKDIR/static.img"
cp "$SRC_DISK" "$DISK"
cat >"$WORKDIR/static.json" <<EOF
{"interfaces": [{"name": "eth0", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["$STATIC_IP/24"], "gateway": "$HOST_IP"}]}
EOF
"$CTL_BIN" image seed-network -config "$WORKDIR/static.json" "$DISK" >/dev/null || fail "seed-network"
start=$SECONDS
boot "$DISK"
wait_http "$STATIC_IP" 90
took=$((SECONDS - start))
echo "  ok: HAProxy answers on $STATIC_IP, ${took} s after QEMU started (firmware included)"
[ "$took" -le 45 ] || fail "the static node took ${took} s to answer"
for _ in $(seq 60); do grep -q 'boot: api listening' "$LOG" && break; sleep 0.5; done
console="$(cat "$LOG")"
check "janusd's boot timing" 'boot: api listening [0-9.]+s after the kernel started' "$console"
kernel_s="$(grep -oE 'boot: api listening .*\(kernel [0-9.]+s' "$LOG" | grep -oE '[0-9.]+s$' | tr -d s)"
awk -v k="$kernel_s" 'BEGIN { exit !(k < 10) }' || fail "the kernel's part of the boot took ${kernel_s} s"
echo "  ok: the kernel's part of the boot took ${kernel_s} s (no DHCP wait)"
check_not "the kernel asked no DHCP" 'Sending DHCP requests|IP-Config' "$console"
check_not "janusd's client didn't run" 'netmgr: .*DHCP' "$console"
NODE_IP="$STATIC_IP"
state_pki "$DISK" "$WORKDIR" || fail "couldn't extract the node's pki/ from STATE"
st="$(ctl network status)"
check "eth0 static, with its address" '^  eth0 +physical .*\(up\) static' "$st"
check "the address" "$STATIC_IP/24" "$st"
check_not "no DHCP lease" 'dhcp \(' "$st"
[ "$(denials)" = 0 ] || fail "AVC denials on the static boot"
echo "  ok: zero AVC denials"
stop_qemu

# --- 2. DHCP, a 2-minute lease renewed ------------------------------------
echo "Part 2: a DHCP node, its 2-minute lease renewed"
start_dnsmasq() {
  sudo "$DNSMASQ" --keep-in-foreground --conf-file=/dev/null --port=0 \
    --interface="$TAP" --bind-interfaces --except-interface=lo \
    --dhcp-range="$NET.50,$NET.59,255.255.255.0,2m" --dhcp-authoritative \
    --dhcp-option=option:router,"$HOST_IP" --dhcp-option=option:dns-server,"$HOST_IP" \
    --dhcp-option=option:domain-name,dhcp.test \
    --dhcp-leasefile="$WORKDIR/leases" --pid-file="$WORKDIR/dnsmasq.pid" \
    --log-dhcp --log-facility="$WORKDIR/dnsmasq.log" &
  for _ in $(seq 50); do [ -s "$WORKDIR/dnsmasq.pid" ] && return; sleep 0.1; done
  fail "dnsmasq didn't start"
}
dlog() { sudo cat "$WORKDIR/dnsmasq.log" 2>/dev/null || true; }
acks() { local n; n="$(dlog | grep -c "DHCPACK($TAP) [0-9.]* $MAC")" || true; echo "${n:-0}"; }
discovers() { local n; n="$(dlog | grep -c "DHCPDISCOVER($TAP) $MAC")" || true; echo "${n:-0}"; }
start_dnsmasq
DISK="$WORKDIR/dhcp.img"
cp "$SRC_DISK" "$DISK"
boot "$DISK"
deadline=$((SECONDS + 90))
until [ "$(acks)" -ge 1 ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "no DHCPACK for $MAC within 90 s"
  sleep 0.5
done
NODE_IP="$(dlog | grep -oE "DHCPACK\($TAP\) [0-9.]+ $MAC" | head -1 | awk '{print $2}')"
# dnsmasq pings an address before offering it: the client's discover is
# often sent twice. From now on, none.
DISCOVERS="$(discovers)"
echo "  ok: dnsmasq leased $NODE_IP to the node"
wait_http "$NODE_IP" 60
echo "  ok: HAProxy answers on $NODE_IP"
for _ in $(seq 60); do grep -q 'listening on :9505' "$LOG" && break; sleep 0.5; done
check "the lease logged" "netmgr: eth0: DHCP lease $NODE_IP/24 from $HOST_IP \\(router $HOST_IP\\) for 2m0s" "$(cat "$LOG")"
state_pki "$DISK" "$WORKDIR" || fail "couldn't extract the node's pki/ from STATE"
st="$(ctl network status)"
check "eth0 in DHCP mode" '^  eth0 +physical .*\(up\) dhcp' "$st"
check "the lease reported" "dhcp \\(until [0-9-]+ [0-9:]+\\): $NODE_IP/24 from $HOST_IP, router $HOST_IP, dns $HOST_IP" "$st"
check "the default route via the lease's router" "default via $HOST_IP dev eth0" "$st"
check "DNS and search domain from the lease" "nameserver $HOST_IP" "$(ctl system cat /etc/resolv.conf)"
check "search domain from the lease" "search dhcp.test" "$(ctl system cat /etc/resolv.conf)"
until1="$(grep -oE 'dhcp \(until [0-9-]+ [0-9:]+' <<<"$st")"
# T1 is a minute after the lease: renewed by 90 s, the node answering
# on its address every second meanwhile.
deadline=$((SECONDS + 90))
until [ "$(acks)" -ge 2 ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the lease wasn't renewed within 90 s"
  [ "$(http_code "$NODE_IP")" = "200" ] || fail "the node stopped answering on $NODE_IP before its renewal"
  sleep 1
done
check "dnsmasq saw a renewal from the same MAC, same address" "DHCPREQUEST\\($TAP\\) $NODE_IP $MAC" "$(dlog)"
[ "$(discovers)" = "$DISCOVERS" ] || fail "the renewal wasn't one: a discover since the lease"
echo "  ok: renewed without a new discover"
for _ in $(seq 20); do grep -q 'DHCP lease .* renewed for 2m0s' "$LOG" && break; sleep 0.5; done
check "the renewal logged" "netmgr: eth0: DHCP lease $NODE_IP/24 renewed for 2m0s" "$(cat "$LOG")"
st="$(ctl network status)"
until2="$(grep -oE 'dhcp \(until [0-9-]+ [0-9:]+' <<<"$st")"
[ "$until1" != "$until2" ] || fail "the lease's end didn't move with its renewal ($until1)"
echo "  ok: the lease's end moved (${until1#dhcp (} -> ${until2#dhcp (})"
[ "$(http_code "$NODE_IP")" = "200" ] || fail "the node doesn't answer after its renewal"

# --- 3. janusd restarted: it takes its lease back ---------------------------
echo "Part 3: janusd restarted"
ctl system restart >/dev/null 2>&1 || true
sleep 2
deadline=$((SECONDS + 60))
until ctl version >/dev/null 2>&1; do
  [ "$SECONDS" -lt "$deadline" ] || fail "janusd didn't come back after its restart"
  sleep 1
done
acks_before="$(acks)"
check "the restarted janusd took the lease back" "$NODE_IP/24" "$(ctl network status)"
[ "$(discovers)" = "$DISCOVERS" ] || fail "the restarted janusd asked for a new lease"
echo "  ok: no new discover"
deadline=$((SECONDS + 75))
until [ "$(acks)" -gt "$acks_before" ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the restarted janusd didn't renew the lease within 75 s"
  [ "$(http_code "$NODE_IP")" = "200" ] || fail "the node stopped answering on $NODE_IP after janusd's restart"
  sleep 1
done
echo "  ok: the restarted janusd renewed it when due"
[ "$(discovers)" = "$DISCOVERS" ] || fail "a discover after the restart"
check "the restarted janusd said it had the lease" "netmgr: eth0: DHCP lease $NODE_IP/24 in hand, renewed in" "$(cat "$LOG")"

# --- 4. the server gone, then back -------------------------------------------
echo "Part 4: dnsmasq gone, then back"
stop_dnsmasq
deadline=$((SECONDS + 150))
until grep -q "netmgr: eth0: DHCP lease $NODE_IP/24 expired" "$LOG"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the lease didn't expire within 150 s of dnsmasq stopping"
  sleep 1
done
echo "  ok: the lease expired"
sleep 1
[ "$(http_code "$NODE_IP")" = "000" ] || fail "the node still answers on $NODE_IP after its lease expired"
echo "  ok: its address is gone"
start_dnsmasq
deadline=$((SECONDS + 60))
until [ "$(http_code "$NODE_IP")" = "200" ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the node didn't get its address back within 60 s of dnsmasq's return"
  sleep 1
done
echo "  ok: back on $NODE_IP with a new lease"

[ "$(denials)" = 0 ] || fail "AVC denials: $(grep -a 'avc:  denied' "$LOG")"
echo "  ok: zero AVC denials"
echo "DHCP test OK"
