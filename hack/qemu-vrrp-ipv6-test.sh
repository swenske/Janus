#!/usr/bin/env bash
# Proves an IPv6 virtual IP with VRRP (the keepalived extension,
# docs/vrrp.md): three real nodes, UEFI, SELinux enforcing, a shared LAN
# (a QEMU multicast socket network) next to each one's own user-mode NIC
# for its API. A and B run one VRRPv3 instance over unicast - the IPv4
# virtual IP advertised, the IPv6 one in virtual_ipaddress_excluded, moved
# with it - tracking HAProxy's health with a negative weight; C only
# watches, from the LAN. A holds both addresses, B none; C reaches A on the
# IPv6 one. A's HAProxy stops answering: B takes both, and C's neighbour
# entry for the IPv6 address moves to B's MAC from B's unsolicited
# neighbour advertisement - before C sends anything to it -, its ARP entry
# for the IPv4 one the same; C reaches B. A's HAProxy back: A takes them
# back. No AVC denial on any node.
#
# Usage: hack/qemu-vrrp-ipv6-test.sh <disk.img> <janusctl-bin>
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

SRC_DISK="${1:?usage: $0 <disk.img> <janusctl-bin>}"
CTL_BIN="${2:?usage: $0 <disk.img> <janusctl-bin>}"
TIMEOUT="${QEMU_VRRP6_TIMEOUT:-120}"
BASE="${QEMU_VRRP6_BASE_PORT:-$((19200 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
MCAST="${QEMU_VRRP6_MCAST:-230.0.1.$((RANDOM % 200 + 20)):$((20000 + RANDOM % 20000))}"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"

VIP4=192.168.78.100
VIP6=fd00:78::100

WORKDIR="$(mktemp -d)"
declare -A PID
cleanup() {
  for n in "${!PID[@]}"; do kill "${PID[$n]}" 2>/dev/null || true; done
  for n in "${!PID[@]}"; do wait "${PID[$n]}" 2>/dev/null || true; done
  rm -rf "$WORKDIR"
}
trap cleanup EXIT
fail() {
  echo "VRRP IPv6 test FAILED: $*" >&2
  for n in a b c; do
    [ -f "$WORKDIR/$n.log" ] && { echo "--- console $n ---" >&2; tail -80 "$WORKDIR/$n.log" >&2; }
  done
  exit 1
}

idx() { case $1 in a) echo 1 ;; b) echo 2 ;; c) echo 3 ;; esac; }
lanmac() { echo "52:54:00:bb:00:0$(idx "$1")"; }

# node NAME: boot one node - http on BASE+10*i, API on BASE+10*i+1.
node() {
  local n=$1 i
  i=$(idx "$n")
  cp "$SRC_DISK" "$WORKDIR/$n.img"
  cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/$n.vars"
  qemu-system-x86_64 -accel kvm -accel tcg \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$WORKDIR/$n.vars" \
    -drive file="$WORKDIR/$n.img",format=raw,if=virtio \
    -nographic -display none -m 512M \
    -netdev "user,id=net0,hostfwd=tcp::$((BASE + 10 * i))-:8080,hostfwd=tcp::$((BASE + 10 * i + 1))-:9505" \
    -device "virtio-net-pci,netdev=net0,mac=52:54:00:aa:00:0$i" \
    -netdev "socket,id=lan,mcast=$MCAST,localaddr=127.0.0.1" \
    -device "virtio-net-pci,netdev=lan,mac=$(lanmac "$n")" \
    -serial file:"$WORKDIR/$n.log" </dev/null &
  PID[$n]=$!
}
for n in a b c; do node $n; done

http() { curl -s -m 3 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$((BASE + 10 * $1))/" || true; }
for i in 1 2 3; do
  deadline=$((SECONDS + TIMEOUT))
  until [ "$(http $i)" = "200" ]; do
    [ "$SECONDS" -lt "$deadline" ] || fail "node $i never answered"
    sleep 1
  done
done
for n in a b c; do
  deadline=$((SECONDS + TIMEOUT))
  until grep -aq "listening on" "$WORKDIR/$n.log"; do
    [ "$SECONDS" -lt "$deadline" ] || fail "janusd on $n never listened"
    sleep 1
  done
done
sleep 2
# Credentials from each node's console (each has its own PKI).
python3 - "$WORKDIR" <<'PY'
import re, sys
w = sys.argv[1]
for n in "abc":
    log = open(f"{w}/{n}.log", errors="replace").read().replace("\r", "")
    pems = re.findall(r"(-----BEGIN ([A-Z ]+)-----.*?-----END \2-----)", log, re.S)
    for (pem, _), name in zip(pems, ["ca.crt", "admin.crt", "admin.key"]):
        open(f"{w}/{n}-{name}", "w").write(pem + "\n")
PY
ctl() { # ctl NODE args...
  local n=$1
  shift
  "$CTL_BIN" -endpoint "127.0.0.1:$((BASE + 10 * $(idx "$n") + 1))" -ca "$WORKDIR/$n-ca.crt" -cert "$WORKDIR/$n-admin.crt" -key "$WORKDIR/$n-admin.key" "$@"
}
for n in a b c; do ctl $n version >/dev/null || fail "no API on node $n"; done
ctl a version | grep -q "Extension: keepalived" || fail "the image has no keepalived: $(ctl a version)"

# The shared LAN: eth1, static, IPv4 and IPv6 - as the production members.
for n in a b c; do
  i=$(idx "$n")
  cat > "$WORKDIR/$n-net.json" <<EOF
{"hostname": "node-$n", "interfaces": [
  {"name": "eth0", "mac": "52:54:00:aa:00:0$i", "mode": "ADDRESSING_MODE_DHCP"},
  {"name": "eth1", "mac": "$(lanmac "$n")", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.168.78.$i/24", "fd00:78::$i/64"]}
]}
EOF
  out="$(ctl $n network apply "$WORKDIR/$n-net.json" 2>&1)" || fail "network apply on $n: $out"
done
echo "  ok: the three nodes on the shared LAN, IPv4 and IPv6"

# The production configuration (kluster-prod's keepalived.conf) on this LAN.
for n in a b; do
  self=192.168.78.$(idx "$n")
  peer=192.168.78.$([ "$n" = a ] && echo 2 || echo 1)
  prio=$([ "$n" = a ] && echo 150 || echo 100)
  cat > "$WORKDIR/$n.conf" <<EOF
global_defs {
    vrrp_version 3
    router_id node-$n
}

track_file haproxy {
    file /run/janus/keepalived/haproxy-health
}

vrrp_instance lb_pub {
    state BACKUP
    interface eth1
    virtual_router_id 151
    priority $prio
    advert_int 0.2
    unicast_src_ip $self
    unicast_peer {
        $peer
    }
    track_file {
        haproxy weight -100
    }
    garp_master_delay 1
    garp_master_refresh 60
    virtual_ipaddress {
        $VIP4/24
    }
    virtual_ipaddress_excluded {
        $VIP6/64
    }
}
EOF
  out="$(ctl $n network vrrp apply "$WORKDIR/$n.conf" 2>&1)" || fail "vrrp apply on $n: $out"
done

state() { ctl "$1" network vrrp status 2>/dev/null | awk '$1 == "lb_pub" {print $2}'; }
has() { ctl "$1" network status 2>/dev/null | grep -qF "$2"; } # has NODE ADDR/PREFIX
wait_state() { # wait_state NODE STATE
  local deadline=$((SECONDS + 30))
  until [ "$(state "$1")" = "$2" ]; do
    [ "$SECONDS" -lt "$deadline" ] || fail "node $1 isn't $2 (it's '$(state "$1")'): $(ctl "$1" network vrrp status 2>&1)"
    sleep 0.5
  done
}
wait_has() { # wait_has NODE ADDR/PREFIX
  local deadline=$((SECONDS + 10))
  until has "$1" "$2"; do
    [ "$SECONDS" -lt "$deadline" ] || fail "node $1 hasn't $2: $(ctl "$1" network status 2>&1)"
    sleep 0.5
  done
}
# The MAC C's neighbour table gives an address (ARP or NDP), "" if none.
neigh() { ctl c network neighbors -interface eth1 2>/dev/null | awk -v a="$1" '$0 ~ (a "[ /]") || $1 == a || $2 == a {for (i = 1; i <= NF; i++) if ($i ~ /^([0-9a-f]{2}:){5}[0-9a-f]{2}$/) {print $i; exit}}'; }
wait_neigh() { # wait_neigh ADDR MAC: C's entry for ADDR moves to MAC, nothing sent by C
  local deadline=$((SECONDS + 8))
  until [ "$(neigh "$1")" = "$2" ]; do
    [ "$SECONDS" -lt "$deadline" ] || fail "C's neighbour entry for $1 is '$(neigh "$1")', want $2: $(ctl c network neighbors -interface eth1 2>&1)"
    sleep 0.5
  done
}
# Which node answers on ADDR: the API's certificate names it.
holder() { ctl c system probe -v -timeout 3s tls "$1" 2>&1 | grep -o 'node-[ab]' | head -1 || true; }

wait_state a MASTER
wait_state b BACKUP
wait_has a "$VIP4/24"
wait_has a "$VIP6/64"
if has b "$VIP6/64" || has b "$VIP4/24"; then fail "B holds a virtual IP while backup"; fi
echo "  ok: A is master with both virtual IPs ($VIP4, $VIP6), B backup without them"

out="$(ctl c system probe -timeout 3s tcp "[$VIP6]:9505" 2>&1)" || fail "C can't reach [$VIP6]:9505: $out"
h="$(holder "[$VIP6]:9505")"
[ "$h" = node-a ] || fail "C reaches '$h' on [$VIP6]:9505, want node-a: $(ctl c system probe -v -timeout 3s tls "[$VIP6]:9505" 2>&1 | head -30)"
ctl c system probe -timeout 3s tcp "$VIP4:9505" >/dev/null 2>&1 || fail "C can't reach $VIP4:9505"
wait_neigh "$VIP6" "$(lanmac a)"
wait_neigh "$VIP4" "$(lanmac a)"
echo "  ok: C reaches A on [$VIP6]:9505 and $VIP4:9505 - its neighbour entries give A's MAC"

# A's HAProxy stops answering: A's priority drops to 50, B preempts it.
t0=$SECONDS
ctl a system service stop haproxy >/dev/null || fail "stopping HAProxy on A"
wait_state b MASTER
wait_has b "$VIP6/64"
wait_has b "$VIP4/24"
wait_state a BACKUP
if has a "$VIP6/64"; then fail "A kept $VIP6 as backup"; fi
# C sent nothing to the virtual IPs since: B's unsolicited NA and
# gratuitous ARP moved its entries.
wait_neigh "$VIP6" "$(lanmac b)"
wait_neigh "$VIP4" "$(lanmac b)"
echo "  ok: A's HAProxy down - B took both virtual IPs in $((SECONDS - t0)) s, C's NDP and ARP entries moved to B's MAC unasked"
h="$(holder "[$VIP6]:9505")"
[ "$h" = node-b ] || fail "C reaches '$h' on [$VIP6]:9505 after the failover, want node-b"
echo "  ok: C reaches B on [$VIP6]:9505"

ctl a system service start haproxy >/dev/null || fail "starting HAProxy on A"
wait_state a MASTER
wait_has a "$VIP6/64"
wait_state b BACKUP
if has b "$VIP6/64"; then fail "B kept $VIP6 as backup"; fi
wait_neigh "$VIP6" "$(lanmac a)"
h="$(holder "[$VIP6]:9505")"
[ "$h" = node-a ] || fail "C reaches '$h' on [$VIP6]:9505 after A took it back, want node-a"
echo "  ok: A's HAProxy back - A took both virtual IPs back, C follows"

# keepalived said nothing wrong about the IPv6 address - its log read,
# not empty, and B's announcing the addresses when it took them.
for n in a b; do
  ctl $n system logs keepalived >"$WORKDIR/$n-ka.log" 2>&1 || fail "keepalived's log on $n: $(cat "$WORKDIR/$n-ka.log")"
  [ -s "$WORKDIR/$n-ka.log" ] || fail "keepalived's log on $n is empty"
  if grep -i -E 'error|fail|denied|cannot|unable' "$WORKDIR/$n-ka.log" >"$WORKDIR/$n-ka.txt"; then
    fail "keepalived on $n: $(cat "$WORKDIR/$n-ka.txt")"
  fi
done
echo "  keepalived on B ($(wc -l <"$WORKDIR/b-ka.log") lines), its announcements:"
grep -i -E 'gratuitous|unsolicited|neighbo|NA |entering master' "$WORKDIR/b-ka.log" | tail -6 | sed 's/^/    /'
for n in a b c; do
  if grep -a "avc:  denied" "$WORKDIR/$n.log" >"$WORKDIR/$n-avc.txt"; then
    fail "AVC denials on $n: $(cat "$WORKDIR/$n-avc.txt")"
  fi
done
echo "  ok: keepalived logged no error, no AVC denial on any node"
echo "VRRP IPv6 test passed"
