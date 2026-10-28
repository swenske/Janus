#!/usr/bin/env bash
# The node's network configuration, end to end, on a real enforcing
# boot of the real disk (dm-verity root, STATE):
#
#   - provisioning: a configuration seeded offline onto the disk
#     (`janusctl image seed-network`) applies from the first boot - the
#     DHCP interface renamed by MAC, a static address on a second NIC, a
#     VLAN on a third, a hostname, and NTP over that VLAN;
#   - VLAN and NTP for real: the third NIC is a QEMU socket netdev wired
#     to hack/vlan-ntp-responder.py, which answers ARP and NTP only for
#     frames tagged with VLAN 100. The guest's clock starts in 2020 (no
#     battery-backed clock, like a Raspberry Pi), so the first boot waits
#     for NTP before generating its certificates, and must step the clock
#     to now through that VLAN;
#   - the server certificate follows the node's addresses and hostname;
#   - a trial confirmed over an address it keeps (janusctl network
#     apply), one that reverts without confirmation, and one that cuts
#     the caller off - it must come back by itself;
#   - persistence: after a reboot the confirmed configuration is back,
#     the reverted ones aren't, and the clock is set again by NTP.
#
# DHCP is janusd's own client, on the interface the seeded configuration
# puts in DHCP mode (wan) - the second NIC is slirp too, with its own
# DHCP server, and must get nothing from it. -kernel/-append rather than
# the UKI: the 2020 clock (-rtc) needs no kvmclock.
#
# Usage: hack/qemu-network-config-test.sh <bzImage> <rootfs-dir> <disk.img> <janusctl>
set -euo pipefail
# shellcheck source=state-read.sh
. "$(dirname "$0")/state-read.sh"

export PATH="$PATH:/usr/sbin:/sbin"

KERNEL="${1:?usage: $0 <bzImage> <rootfs-dir> <disk.img> <janusctl>}"
ROOTFS_DIR="${2:?}"
SRC_DISK="${3:?}"
CTL_BIN="${4:?}"
BOOT_TIMEOUT_SECS="${QEMU_NET_BOOT_TIMEOUT:-150}"
BASE_PORT="${QEMU_NET_TEST_PORT:-$((18300 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
P_HTTP=$BASE_PORT P_GRPC=$((BASE_PORT + 1)) P_B10=$((BASE_PORT + 2)) P_B20=$((BASE_PORT + 3)) P_VLAN=$((BASE_PORT + 4))
MARKER="JANUS_INIT_BOOT_OK"
HERE="$(cd "$(dirname "$0")" && pwd)"

WORKDIR="$(mktemp -d)"
QEMU_PID="" RESP_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  [ -n "$RESP_PID" ] && kill "$RESP_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

LOG="$WORKDIR/console.log"
RLOG="$WORKDIR/responder.log"
fail() {
  echo "Network config test FAILED: $*" >&2
  [ -f "$LOG" ] && { echo "--- console output ---" >&2; cat "$LOG" >&2; }
  [ -f "$RLOG" ] && { echo "--- VLAN responder ---" >&2; cat "$RLOG" >&2; }
  exit 1
}

DISK="$WORKDIR/disk.img"
cp "$SRC_DISK" "$DISK"

cat > "$WORKDIR/net1.json" <<'EOF'
{
  "hostname": "lb-test",
  "interfaces": [
    {"name": "wan", "mac": "52:54:00:12:34:01", "mode": "ADDRESSING_MODE_DHCP"},
    {"name": "eth1", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.168.77.10/24"]},
    {"name": "eth2", "mode": "ADDRESSING_MODE_NONE"},
    {"name": "eth2.100", "vlan": {"parent": "eth2", "id": 100}, "mode": "ADDRESSING_MODE_STATIC", "addresses": ["10.100.0.5/24"]}
  ],
  "ntp": {"servers": ["10.100.0.1"]}
}
EOF
sed -e 's|"lb-test"|"lb-test2"|' -e 's|192.168.77.10/24|192.168.77.20/24|' "$WORKDIR/net1.json" > "$WORKDIR/net2.json"
sed -e 's|192.168.77.20/24|192.168.77.30/24|' "$WORKDIR/net2.json" > "$WORKDIR/net3.json"
sed -e 's|"mac": "52:54:00:12:34:01", "mode": "ADDRESSING_MODE_DHCP"|"mac": "52:54:00:12:34:01", "mode": "ADDRESSING_MODE_DISABLED"|' "$WORKDIR/net2.json" > "$WORKDIR/net4.json"
# eth1's default route ahead of the lease's: eth1 the main interface.
sed -e 's|"addresses": \["192.168.77.20/24"\]|"addresses": ["192.168.77.20/24"], "gateway": "192.168.77.2", "route_metric": 10|' "$WORKDIR/net2.json" > "$WORKDIR/net5.json"

"$CTL_BIN" image seed-network -config "$WORKDIR/net1.json" "$DISK" || fail "seed-network"
echo "  ok: network configuration seeded offline"

python3 "$HERE/vlan-ntp-responder.py" "$P_VLAN" 100 10.100.0.1 "$RLOG" &
RESP_PID=$!
for _ in $(seq 50); do grep -q listening "$RLOG" 2>/dev/null && break; sleep 0.1; done

qemu-system-x86_64 -accel kvm -accel tcg \
  -kernel "$KERNEL" \
  -append "console=ttyS0 panic=-1 dm-mod.create=\"$("$HERE/dm-verity-cmdline.sh" "$ROOTFS_DIR" /dev/vda2 /dev/vda3)\" root=/dev/dm-0 rootfstype=squashfs ro enforcing=1" \
  -nographic -display none -m 512M \
  -rtc base=2020-01-01T00:00:00 \
  -cpu qemu64,-kvmclock \
  -drive file="$DISK",format=raw,if=virtio \
  -netdev "user,id=net0,hostfwd=tcp::${P_HTTP}-:8080,hostfwd=tcp::${P_GRPC}-:9505" \
  -device virtio-net-pci,netdev=net0,mac=52:54:00:12:34:01 \
  -netdev "user,id=net1,net=192.168.77.0/24,hostfwd=tcp::${P_B10}-192.168.77.10:9505,hostfwd=tcp::${P_B20}-192.168.77.20:9505" \
  -device virtio-net-pci,netdev=net1,mac=52:54:00:12:34:02 \
  -netdev "socket,id=net2,connect=127.0.0.1:${P_VLAN}" \
  -device virtio-net-pci,netdev=net2,mac=52:54:00:12:34:03 \
  -serial file:"$LOG" \
  &
QEMU_PID=$!

markers() { local n; n="$(grep -ac "$MARKER" "$LOG" 2>/dev/null)" || true; echo "${n:-0}"; }
http_code() { curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${P_HTTP}/" || true; }
ctl_at() { local port="$1"; shift; "$CTL_BIN" -endpoint "127.0.0.1:${port}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" "$@"; }
ctl() { ctl_at "$P_GRPC" "$@"; }
wait_boot() { # wait_boot N: the Nth boot answers HTTP
  local deadline=$((SECONDS + BOOT_TIMEOUT_SECS))
  until [ "$(markers)" -ge "$1" ] && [ "$(http_code)" = "200" ]; do
    [ "$SECONDS" -lt "$deadline" ] || fail "boot #$1 never answered HTTP 200 within ${BOOT_TIMEOUT_SECS}s"
    sleep 1
  done
}
wait_grpc() { # wait_grpc PORT: janusd answers there (its clock may still be wrong - see boot 2)
  local deadline=$((SECONDS + 60))
  until ctl_at "$1" version >/dev/null 2>&1; do
    [ "$SECONDS" -lt "$deadline" ] || fail "janusd never answered on port $1: $(ctl_at "$1" version 2>&1)"
    sleep 1
  done
}
extract_pki() { state_pki "$DISK" "$WORKDIR" || fail "couldn't extract the node's pki/ from STATE"; }
server_sans() { # server_sans PORT: the node's server certificate's SANs
  openssl s_client -connect "127.0.0.1:$1" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" -CAfile "$WORKDIR/ca.crt" </dev/null 2>/dev/null |
    openssl x509 -noout -ext subjectAltName 2>/dev/null | tail -n +2
}
check() { # check "description" "grep -E pattern" "text"
  echo "$3" | grep -Eq "$2" || fail "$1: /$2/ not found in:
$3"
  echo "  ok: $1"
}
check_not() {
  if echo "$3" | grep -Eq "$2"; then fail "$1: /$2/ found in:
$3"; fi
  echo "  ok: $1"
}

# --- boot 1: the seeded configuration --------------------------------
wait_boot 1
deadline=$((SECONDS + 30))
until grep -q "pki: first boot" "$LOG"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "no PKI bootstrap"
  sleep 1
done
sleep 1
extract_pki
wait_grpc "$P_GRPC"
echo "Boot 1 OK"

console="$(cat "$LOG")"
check "the first boot waited for NTP (clock in 2020)" 'timesync: first boot and the clock reads 2020' "$console"
check "janusd synchronized with the VLAN's NTP server" 'timesync: synchronized with 10\.100\.0\.1' "$console"
check "janusd stepped the clock" 'timesync: clock stepped by' "$console"
responder="$(cat "$RLOG")"
check "ARP answered on VLAN 100" 'arp-request vid=100 from 10\.100\.0\.5' "$responder"
check "NTP answered on VLAN 100" 'ntp-request vid=100 from 10\.100\.0\.5' "$responder"
year="$(openssl x509 -in "$WORKDIR/admin.crt" -noout -startdate | grep -oE '[0-9]{4} GMT' | cut -d' ' -f1)"
[ "$year" -ge 2026 ] || fail "the admin certificate starts in $year - generated before the clock was set"
echo "  ok: certificates dated after the NTP sync ($year)"

st="$(ctl network status)"
check "hostname from the configuration" '^hostname: lb-test$' "$st"
check "DHCP interface renamed by MAC" '^  wan +physical +52:54:00:12:34:01 .*\(up\) dhcp' "$st"
check "DHCP lease on it" '10\.0\.2\.15/24' "$st"
check "DHCP lease reported" 'dhcp \(until [0-9-]+ [0-9:]+\): 10\.0\.2\.15/24 from 10\.0\.2\.2, router 10\.0\.2\.2' "$st"
# Taken in 2020, before NTP stepped the clock: still valid, slirp's
# 24 hours from now - its times run on the time since the boot, not on
# the wall clock.
until="$(grep -oE 'dhcp \(until [0-9-]+ [0-9:]+' <<<"$st" | sed 's/dhcp (until //')"
left=$(( $(date -d "$until" +%s) - $(date +%s) ))
[ "$left" -gt $((23 * 3600)) ] && [ "$left" -le $((24 * 3600)) ] || fail "the DHCP lease ends at $until, ${left}s from now - not slirp's 24 hours"
echo "  ok: the lease taken before the clock step ends in ${left}s"
check_not "nothing from eth1's DHCP server" '192\.168\.77\.15' "$st"
check "the lease logged" 'netmgr: wan: DHCP lease 10\.0\.2\.15/24 from 10\.0\.2\.2 \(router 10\.0\.2\.2\)' "$(cat "$LOG")"
check "static address on eth1" '192\.168\.77\.10/24' "$st"
check "VLAN created" '^  eth2\.100 +vlan 100 on eth2 ' "$st"
check "VLAN address" '10\.100\.0\.5/24' "$st"
check "VLAN parent up without address" '^  eth2 +physical .*\(up\) none' "$st"
check "default route via the lease" 'default via 10\.0\.2\.2 dev wan metric 1024' "$st"
check "DNS from the lease" '^dns: 10\.0\.2\.3' "$st"
# The kernel drops the synchronized status when its clocksource changes
# (the TSC refined a second after boot) and janusd restores it within
# its next recheck (internal/timesync, 5 s): poll, don't read once.
wait_synced() { # wait_synced PATTERN: network status matches within 30 s, st holds it
  for _ in $(seq 1 30); do
    st="$(ctl network status)"
    grep -Eq "$1" <<<"$st" && return 0
    sleep 1
  done
  return 0
}
wait_synced '^time: synchronized, servers 10\.100\.0\.1 \(configured\)'
check "clock synchronized from the configured server" '^time: synchronized, servers 10\.100\.0\.1 \(configured\)' "$st"
check "network get returns the seeded configuration" '"hostname": "lb-test"' "$(ctl network get)"
check "hostname RPC" '^lb-test$' "$(ctl system hostname)"
ctl_at "$P_B10" version >/dev/null || fail "janusd unreachable on the static address 192.168.77.10"
echo "  ok: janusd reachable on the static address"
sans="$(server_sans "$P_B10")"
check "server certificate covers the static address" 'IP Address:192\.168\.77\.10' "$sans"
check "server certificate covers the VLAN address" 'IP Address:10\.100\.0\.5' "$sans"
check "server certificate covers the hostname" 'DNS:lb-test' "$sans"
logs="$(ctl system logs janusd)"
check "janusd's own log has the network setup" 'netmgr: hostname lb-test' "$logs"
check_not "janusd's log API never holds the admin key" 'PRIVATE KEY' "$logs"

# --- a trial, confirmed over an address it keeps ----------------------
out="$(ctl network apply -timeout 20s "$WORKDIR/net2.json" 2>&1)" || fail "apply net2: $out"
check "apply confirmed over the kept DHCP address" 'confirmed via 10\.0\.2\.15' "$out"
st="$(ctl network status)"
check "new static address" '192\.168\.77\.20/24' "$st"
check_not "old static address gone" '192\.168\.77\.10/' "$st"
check_not "no trial pending after confirmation" '^TRIAL' "$st"
check "new hostname" '^hostname: lb-test2$' "$st"
ctl_at "$P_B20" version >/dev/null || fail "janusd unreachable on the new address"
if ctl_at "$P_B10" version >/dev/null 2>&1; then fail "janusd still answers on the removed address"; fi
echo "  ok: reachable on the new address only"
sans="$(server_sans "$P_B20")"
check "server certificate reissued for the new address" 'IP Address:192\.168\.77\.20' "$sans"
check_not "and not the old one" 'IP Address:192\.168\.77\.10' "$sans"
check "and the new hostname" 'DNS:lb-test2' "$sans"
# Read through the node (debugfs on the host would miss what's still in
# the running guest's ext4 journal); the reboot below proves it's on disk.
check "the confirmed configuration is saved" '192\.168\.77\.20/24' "$(ctl system cat /etc/janus/network/config.json)"

# --- a trial nobody confirms ------------------------------------------
out="$(ctl network apply -no-confirm -timeout 8s "$WORKDIR/net3.json" 2>&1)" || fail "apply net3: $out"
st="$(ctl network status)"
check "unconfirmed configuration in effect" '192\.168\.77\.30/24' "$st"
check "trial reported" '^TRIAL: ' "$st"
sleep 10
st="$(ctl network status)"
check "reverted by itself" '192\.168\.77\.20/24' "$st"
check_not "the trial's address is gone" '192\.168\.77\.30/' "$st"
check_not "no trial pending after the revert" '^TRIAL' "$st"
check "revert logged" 'netmgr: network configuration not confirmed in time - reverted' "$(cat "$LOG")"

# --- the exporter follows the main interface ---------------------------
check "the exporter on the main interface (the lease's)" 'http://10\.0\.2\.15:10056/metrics \(main interface\)' "$(ctl system metrics)"
out="$(ctl network apply -no-confirm -timeout 8s "$WORKDIR/net5.json" 2>&1)" || fail "apply net5: $out"
check "eth1's default route first: the exporter moves to eth1" '192\.168\.77\.20:10056 +[^ ]+ +LISTEN' "$(ctl system netstat)"
check_not "and leaves the lease's address" '10\.0\.2\.15:10056 +[^ ]+ +LISTEN' "$(ctl system netstat)"
check "the exporter says its new address" 'http://192\.168\.77\.20:10056/metrics \(main interface\)' "$(ctl system metrics)"
sleep 10
check "the trial reverted: the exporter back on the lease's address" '10\.0\.2\.15:10056 +[^ ]+ +LISTEN' "$(ctl system netstat)"

# --- a trial that cuts the caller off ---------------------------------
if out="$(ctl network apply -timeout 10s "$WORKDIR/net4.json" 2>&1)"; then
  fail "a configuration disabling the only reachable interface was confirmed: $out"
fi
check "janusctl couldn't confirm" "couldn't confirm" "$out"
sleep 3
wait_grpc "$P_GRPC"
st="$(ctl network status)"
check "the node came back on its own" '^  wan +physical .*\(up\) dhcp' "$st"
check "with its DHCP lease" '10\.0\.2\.15/24' "$st"
check "and the default route" 'default via 10\.0\.2\.2 dev wan' "$st"
[ "$(http_code)" = "200" ] || fail "HAProxy unreachable after the revert"
echo "  ok: HAProxy reachable again"

# --- persistence across a reboot --------------------------------------
ctl system reboot >/dev/null || fail "Reboot failed"
wait_boot 2
wait_grpc "$P_GRPC" # rejects the client certificate until NTP fixes the clock again
st="$(ctl network status)"
check "after reboot: the confirmed configuration" '192\.168\.77\.20/24' "$st"
check_not "after reboot: not a reverted one" '192\.168\.77\.30/' "$st"
check "after reboot: hostname" '^hostname: lb-test2$' "$st"
check "after reboot: VLAN" '^  eth2\.100 +vlan 100 on eth2 ' "$st"
wait_synced '^time: synchronized'
check "after reboot: clock set again" '^time: synchronized' "$st"
# Once synchronized, the kernel copies system time to the hardware clock
# (CONFIG_GENERIC_CMOS_UPDATE): the second boot starts on time, not in
# 2020 - the reboot stays inside the same QEMU process, which keeps its
# emulated RTC.
[ "$(grep -c 'timesync: synchronized with 10.100.0.1' "$LOG")" -ge 2 ] || fail "the second boot never synchronized"
boot2="$(awk "/$MARKER/{n++} n>=1" "$LOG" | sed -n '/Linux version/,$p' | tail -n +2)"
if echo "$boot2" | grep -q '^2020/'; then fail "the second boot started in 2020 again - the kernel never wrote the synchronized time back to the RTC"; fi
echo "  ok: the RTC was updated after the first sync: the second boot started on time"
[ "$(grep -c 'pki: first boot' "$LOG")" -eq 1 ] || fail "PKI regenerated on the second boot"
echo "  ok: PKI kept"
# A node new since the exporters' main-interface default: its second
# boot (PKI there now) doesn't take it for an older one - the marker.
out="$(ctl system metrics)"
check "after reboot: the exporter's settings still the defaults" '\(default settings\)' "$out"
check "after reboot: the exporter on the main interface" 'http://10\.0\.2\.15:10056/metrics \(main interface\)' "$out"

if grep -q "avc:.*denied" "$LOG"; then
  grep "avc:.*denied" "$LOG" >&2
  fail "AVC denials under enforcing"
fi
echo "  ok: zero AVC denials (enforcing)"

echo "Network config test OK: offline-seeded configuration with a MAC rename, a static address and a VLAN applied from the first boot; NTP over that VLAN set a 2020 clock before the certificates were made; trials confirmed, reverted when unconfirmed, and reverted when they cut the caller off; the server certificate followed; the confirmed configuration survived a reboot - all under SELinux enforcing"
