#!/usr/bin/env bash
# Proves VRRP (the keepalived extension, docs/vrrp.md) with two real
# nodes built with it: UEFI, SELinux enforcing, a shared LAN between them
# (a QEMU multicast socket network) next to each one's own user-mode NIC
# for its API. Both get a static address on the LAN through the network
# API and the same keepalived.conf but for the priority; A (higher) holds
# the virtual IP, B is backup. When A's HAProxy stops answering, A gives
# the IP up (the health file janusd keeps, tracked by keepalived) and B
# takes it; when it answers again, A takes it back; when A is gone, B
# takes it again. A broken keepalived.conf is refused, so is use_vmac,
# the exporter reports the instance, and no AVC denial appears on either
# node - keepalived writes no kernel parameter. A janusd restart and a
# shutdown by the hypervisor (guest agent) hand the IP over first.
#
# Usage: hack/qemu-vrrp-test.sh <disk.img> <janusctl-bin>
#   VRRP_DISCOVERY=1: don't fail on AVC denials - list them (for writing
#   the keepalived_t rules, with a UKI built UKI_SELINUX_ENFORCING=0 and
#   UKI_EXTRA_CMDLINE=sysctl.kernel.printk_ratelimit=0).
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

SRC_DISK="${1:?usage: $0 <disk.img> <janusctl-bin>}"
CTL_BIN="${2:?usage: $0 <disk.img> <janusctl-bin>}"
TIMEOUT="${QEMU_VRRP_TIMEOUT:-90}"
BASE="${QEMU_VRRP_BASE_PORT:-$((18500 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
MCAST="${QEMU_VRRP_MCAST:-230.0.0.$((RANDOM % 200 + 20)):$((20000 + RANDOM % 20000))}"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"

WORKDIR="$(mktemp -d)"
declare -A PID QGA
# Each node's guest agent socket: a Unix socket path is 108 bytes at
# most, not under WORKDIR (a long TMPDIR puts it past that).
for n in a b; do QGA[$n]="$(mktemp -u /tmp/janus-qga-$n.XXXXXX)"; done
cleanup() {
  for n in "${!PID[@]}"; do kill "${PID[$n]}" 2>/dev/null || true; done
  rm -rf "$WORKDIR" "${QGA[a]}" "${QGA[b]}"
}
trap cleanup EXIT
fail() {
  echo "VRRP test FAILED: $*" >&2
  for n in a b; do
    [ -f "$WORKDIR/$n.log" ] && { echo "--- console $n ---" >&2; tail -150 "$WORKDIR/$n.log" >&2; }
  done
  exit 1
}

# node NAME INDEX: boot one node - http on BASE+10*i, API on BASE+10*i+1.
node() {
  local n=$1 i=$2
  cp "$SRC_DISK" "$WORKDIR/$n.img"
  cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/$n.vars"
  qemu-system-x86_64 -accel kvm -accel tcg \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$WORKDIR/$n.vars" \
    -drive file="$WORKDIR/$n.img",format=raw,if=virtio \
    -nographic -display none -m 512M \
    -netdev "user,id=net0,hostfwd=tcp::$((BASE + 10 * i))-:8080,hostfwd=tcp::$((BASE + 10 * i + 1))-:9505,hostfwd=tcp::$((BASE + 10 * i + 2))-:10056" \
    -device "virtio-net-pci,netdev=net0,mac=52:54:00:aa:00:0$i" \
    -netdev "socket,id=lan,mcast=$MCAST,localaddr=127.0.0.1" \
    -device "virtio-net-pci,netdev=lan,mac=52:54:00:bb:00:0$i" \
    -device virtio-serial \
    -chardev "socket,path=${QGA[$n]},server=on,wait=off,id=qga0" \
    -device virtserialport,chardev=qga0,name=org.qemu.guest_agent.0 \
    -serial file:"$WORKDIR/$n.log" &
  PID[$n]=$!
}
node a 1
node b 2

http() { curl -s -m 3 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$((BASE + 10 * $1))/" || true; }
for i in 1 2; do
  deadline=$((SECONDS + TIMEOUT))
  until [ "$(http $i)" = "200" ]; do
    [ "$SECONDS" -lt "$deadline" ] || fail "node $i never answered"
    sleep 1
  done
done
sleep 2
# Credentials from each node's console (each has its own PKI).
python3 - "$WORKDIR" <<'PY'
import re, sys
w = sys.argv[1]
for n in "ab":
    log = open(f"{w}/{n}.log", errors="replace").read().replace("\r", "")
    pems = re.findall(r"(-----BEGIN ([A-Z ]+)-----.*?-----END \2-----)", log, re.S)
    for (pem, _), name in zip(pems, ["ca.crt", "admin.crt", "admin.key"]):
        open(f"{w}/{n}-{name}", "w").write(pem + "\n")
PY
ctl() { # ctl NODE-INDEX args...
  local n=$1 i
  shift
  i=$([ "$n" = a ] && echo 1 || echo 2)
  "$CTL_BIN" -endpoint "127.0.0.1:$((BASE + 10 * i + 1))" -ca "$WORKDIR/$n-ca.crt" -cert "$WORKDIR/$n-admin.crt" -key "$WORKDIR/$n-admin.key" "$@"
}
for n in a b; do ctl $n version >/dev/null || fail "no API on node $n"; done

# The shared LAN: eth1, static.
for n in a b; do
  addr=$([ "$n" = a ] && echo 192.168.77.1 || echo 192.168.77.2)
  cat > "$WORKDIR/$n-net.json" <<EOF
{"interfaces": [
  {"name": "eth0", "mac": "52:54:00:aa:00:0$([ "$n" = a ] && echo 1 || echo 2)", "mode": "ADDRESSING_MODE_DHCP"},
  {"name": "eth1", "mac": "52:54:00:bb:00:0$([ "$n" = a ] && echo 1 || echo 2)", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["$addr/24"]}
]}
EOF
  out="$(ctl $n network apply "$WORKDIR/$n-net.json" 2>&1)" || fail "network apply on $n: $out"
done
echo "  ok: both nodes on the shared LAN"

out="$(ctl a network vrrp status)"
grep -q "Saved: *no" <<<"$out" || fail "VRRP status before any config: $out"

# A broken keepalived.conf is refused.
printf 'vrrp_instance VI_1 {\n  state MASTER\n}\n' > "$WORKDIR/bad.conf"
if out="$(ctl a network vrrp check "$WORKDIR/bad.conf" 2>&1)"; then fail "a broken keepalived.conf passed the check"; fi
grep -q "virtual router id" <<<"$out" || fail "keepalived's complaint isn't shown: $out"
echo "  ok: a broken keepalived.conf is refused with keepalived's own message"

# What a Janus node can't do is refused with the reason (internal/vrrp):
# a VMAC needs net.ipv4.conf.all.rp_filter at 0, a CIS control.
printf 'vrrp_instance VI_1 {\n  interface eth1\n  use_vmac\n  virtual_router_id 51\n  priority 100\n  virtual_ipaddress {\n    10.9.0.100/24\n  }\n}\n' > "$WORKDIR/vmac.conf"
if out="$(ctl a network vrrp check "$WORKDIR/vmac.conf" 2>&1)"; then fail "a keepalived.conf with use_vmac passed the check"; fi
grep -q "Line 3) use_vmac isn't supported on Janus - .*3.3.1.12" <<<"$out" || fail "use_vmac's refusal doesn't say why: $out"
echo "  ok: use_vmac is refused, with the reason"

for n in a b; do
  prio=$([ "$n" = a ] && echo 150 || echo 100)
  cat > "$WORKDIR/$n.conf" <<EOF
track_file haproxy {
  file /run/janus/keepalived/haproxy-health
}
vrrp_instance VI_1 {
  state BACKUP
  interface eth1
  virtual_router_id 51
  priority $prio
  advert_int 1
  track_file {
    haproxy weight 0
  }
  virtual_ipaddress {
    192.168.77.100/24
  }
}
EOF
  out="$(ctl $n network vrrp apply "$WORKDIR/$n.conf" 2>&1)" || fail "vrrp apply on $n: $out"
done

state() { ctl "$1" network vrrp status 2>/dev/null | awk '$1 == "VI_1" {print $2}'; }
has_vip() { ctl "$1" network status 2>/dev/null | grep -q "192.168.77.100/24"; }
# keepalived reports MASTER a moment before the address is up: a node
# that just took over gets a few seconds to show it.
got_vip() {
  local deadline=$((SECONDS + 10))
  until has_vip "$1"; do
    [ "$SECONDS" -lt "$deadline" ] || return 1
    sleep 1
  done
}
wait_state() { # wait_state NODE STATE
  local deadline=$((SECONDS + 30))
  until [ "$(state "$1")" = "$2" ]; do
    [ "$SECONDS" -lt "$deadline" ] || fail "node $1 isn't $2 (it's '$(state "$1")'): $(ctl "$1" network vrrp status 2>&1)"
    sleep 1
  done
}
wait_state a MASTER
wait_state b BACKUP
got_vip a || fail "A is MASTER without the virtual IP"
if has_vip b; then fail "B holds the virtual IP while backup"; fi
echo "  ok: A is master with the virtual IP, B is backup"

# keepalived, and the VRRP process it forks, behind HAProxy and ahead of
# the other extensions for the OOM killer (its manifest, internal/oomadj).
pids="$(ctl a system ps | awk 'index($0, "/usr/local/sbin/keepalived") {print $1}')"
[ "$(wc -w <<<"$pids")" -ge 2 ] || fail "keepalived and its VRRP process not both in Processes: $pids"
for pid in $pids; do
  got="$(ctl a system cat "/proc/$pid/oom_score_adj" | tr -d '[:space:]')"
  [ "$got" = -300 ] || fail "keepalived (pid $pid): oom_score_adj $got, want -300"
done
echo "  ok: keepalived's processes at oom_score_adj -300"

# A's HAProxy stops answering: A gives the IP up, B takes it.
ctl a system service stop haproxy >/dev/null || fail "stopping HAProxy on A"
wait_state a FAULT
wait_state b MASTER
got_vip b || fail "B is MASTER without the virtual IP"
echo "  ok: A's HAProxy down - A in FAULT, B took the virtual IP"
ctl a system service start haproxy >/dev/null || fail "starting HAProxy on A"
wait_state a MASTER
wait_state b BACKUP
echo "  ok: A's HAProxy back - A took the virtual IP back"

# A's janusd restarted: the virtual IP goes to B first - A leaves MASTER
# while its janusd still answers (the node drained for the HAProxy drain
# delay) - and keepalived stops last; the next janusd starts it again and
# A takes the IP back. Stopped at once, keepalived dropped the IP before
# B had it (v2026.10.24-4's lab: 3 requests lost under load).
ctl a system restart >/dev/null || fail "restart janusd on A"
left=
for _ in $(seq 60); do
  s="$(state a || true)" # "" once A's janusd is gone
  case "$s" in
  FAULT | BACKUP) left=$s; break ;;
  "") break ;;
  esac
  sleep 0.25
done
[ -n "$left" ] || fail "A's janusd went away with A still MASTER - the virtual IP wasn't handed over first"
wait_state b MASTER
wait_state a MASTER
wait_state b BACKUP
[ "$(ctl a system ps | grep -c '/usr/local/sbin/keepalived')" = 2 ] || fail "keepalived on A after the restart: $(ctl a system ps | grep keepalived)"
echo "  ok: A's janusd restarted - A went $left with its janusd still up, B took the IP, A took it back (one keepalived)"

m="$(curl -fsS -m 5 "http://127.0.0.1:$((BASE + 12))/metrics")" || fail "no metrics on A"
grep -q '^janus_vrrp_instance_state{instance="VI_1",interface="eth1",state="MASTER"} 1$' <<<"$m" || fail "janus_vrrp_instance_state: $(grep vrrp <<<"$m")"
echo "  ok: the exporter reports the instance"

# A shut down by its hypervisor through the guest agent (Proxmox's
# Shutdown button): janusd's SIGTERM path soft-stops HAProxy first, whose
# drain has A leave MASTER while keepalived still runs - B takes the IP
# before A's keepalived stops. Extensions first, A dropped the IP at once
# (v2026.10.24-4's lab: 2 requests lost to a hypervisor's shutdown).
qga() { python3 "$(dirname "$0")/qga-client.py" "${QGA[$1]}" "${@:2}"; }
deadline=$((SECONDS + 30))
until qga a guest-ping >/dev/null 2>&1; do
  [ "$SECONDS" -lt "$deadline" ] || fail "A's guest agent never answered"
  sleep 1
done
qga a guest-shutdown >/dev/null 2>&1 || true # no answer: it goes down
for _ in $(seq 60); do kill -0 "${PID[a]}" 2>/dev/null || break; sleep 1; done
if kill -0 "${PID[a]}" 2>/dev/null; then fail "A didn't power off after the guest agent's shutdown"; fi
# Read on A's console - its API is busy stopping HAProxy meanwhile:
# after janusd's SIGTERM, VI_1 left MASTER before keepalived stopped.
left="$(tr -d '\r' <"$WORKDIR/a.log" | awk '
  /janusd: SIGTERM/ { s = 1; next }
  s && !l && /\(VI_1\) Entering (FAULT|BACKUP) STATE/ { l = NR; st = $0; sub(/.*Entering /, "", st); sub(/ STATE.*/, "", st) }
  s && !k && /[0-9]: Stopping$/ { k = NR }
  END { if (l && k && l < k) print st }')"
[ -n "$left" ] || fail "A's keepalived stopped with A still MASTER - the virtual IP wasn't handed over first: $(tr -d '\r' <"$WORKDIR/a.log" | grep -a -A 12 'janusd: SIGTERM')"
unset 'PID[a]'
wait_state b MASTER
got_vip b || fail "B is MASTER without the virtual IP after A went away"
echo "  ok: A shut down by its hypervisor - A went $left first, B took the virtual IP, A powered off"

# Removing the configuration stops keepalived.
: > "$WORKDIR/empty.conf"
ctl b network vrrp apply "$WORKDIR/empty.conf" >/dev/null || fail "removing the configuration"
out="$(ctl b network vrrp status)"
grep -q "Saved: *no" <<<"$out" || fail "the configuration is still saved: $out"
deadline=$((SECONDS + 15))
while has_vip b; do
  [ "$SECONDS" -lt "$deadline" ] || fail "B kept the virtual IP after keepalived stopped"
  sleep 1
done
echo "  ok: an empty configuration stops keepalived, the virtual IP goes"

denials="$(cat "$WORKDIR"/a.log "$WORKDIR"/b.log | grep -a "avc:.*denied" || true)"
if [ -n "$denials" ]; then
  if [ "${VRRP_DISCOVERY:-}" = 1 ]; then
    echo "--- AVC denials (discovery) ---"
    sed -E 's/.*avc:  denied  //; s/pid=[0-9]+ //; s/ino=[0-9]+ //; s/ permissive=[01]//' <<<"$denials" | sort | uniq -c
  else
    echo "$denials" >&2
    fail "AVC denials under enforcing"
  fi
fi
echo "VRRP test OK: master/backup, failover on HAProxy health and on a node going away, failback, zero AVC denials"
