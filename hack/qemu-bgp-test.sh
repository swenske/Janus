#!/usr/bin/env bash
# Proves BGP (the bird extension, docs/bgp.md) with two real nodes built
# with it: UEFI, SELinux enforcing, a shared LAN between them (a QEMU
# multicast socket network) next to each one's own user-mode NIC for its
# API. The session has a TCP MD5 password and BFD, next to OSPF (for
# their sockets under enforcing). A (AS 65001) announces an anycast route from a haproxy_* static
# protocol and a plain one; B (AS 65002) learns both, into its kernel
# table. When A's HAProxy stops answering, janusd holds haproxy_anycast
# down and B loses that route - and only that one; a bird.conf applied
# meanwhile doesn't bring it back; when HAProxy answers again, it comes
# back. A broken bird.conf is refused with BIRD's message, the exporter
# reports the session, BIRD's log is the service's, an empty bird.conf
# stops BIRD, and no AVC denial appears on either node.
#
# Usage: hack/qemu-bgp-test.sh <disk.img> <janusctl-bin>
#   BGP_DISCOVERY=1: don't fail on AVC denials - list them (for writing
#   the bird_t rules, with a UKI built UKI_SELINUX_ENFORCING=0 and
#   UKI_EXTRA_CMDLINE=sysctl.kernel.printk_ratelimit=0).
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

SRC_DISK="${1:?usage: $0 <disk.img> <janusctl-bin>}"
CTL_BIN="${2:?usage: $0 <disk.img> <janusctl-bin>}"
TIMEOUT="${QEMU_BGP_TIMEOUT:-90}"
BASE="${QEMU_BGP_BASE_PORT:-18600}"
MCAST="${QEMU_BGP_MCAST:-230.0.0.$((RANDOM % 200 + 20)):$((20000 + RANDOM % 20000))}"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"

WORKDIR="$(mktemp -d)"
declare -A PID
cleanup() {
  for n in "${!PID[@]}"; do kill "${PID[$n]}" 2>/dev/null || true; done
  rm -rf "$WORKDIR"
}
trap cleanup EXIT
fail() {
  echo "BGP test FAILED: $*" >&2
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
  qemu-system-x86_64 \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$WORKDIR/$n.vars" \
    -drive file="$WORKDIR/$n.img",format=raw,if=virtio \
    -nographic -display none -m 512M \
    -netdev "user,id=net0,hostfwd=tcp::$((BASE + 10 * i))-:8080,hostfwd=tcp::$((BASE + 10 * i + 1))-:9505,hostfwd=tcp::$((BASE + 10 * i + 2))-:10056" \
    -device "virtio-net-pci,netdev=net0,mac=52:54:00:aa:00:0$i" \
    -netdev "socket,id=lan,mcast=$MCAST,localaddr=127.0.0.1" \
    -device "virtio-net-pci,netdev=lan,mac=52:54:00:bb:00:0$i" \
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
  addr=$([ "$n" = a ] && echo 192.168.78.1 || echo 192.168.78.2)
  cat > "$WORKDIR/$n-net.json" <<EOF
{"interfaces": [
  {"name": "eth0", "mac": "52:54:00:aa:00:0$([ "$n" = a ] && echo 1 || echo 2)", "mode": "ADDRESSING_MODE_DHCP"},
  {"name": "eth1", "mac": "52:54:00:bb:00:0$([ "$n" = a ] && echo 1 || echo 2)", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["$addr/24"]}
]}
EOF
  out="$(ctl $n network apply "$WORKDIR/$n-net.json" 2>&1)" || fail "network apply on $n: $out"
done
echo "  ok: both nodes on the shared LAN"

out="$(ctl a network bgp status)"
grep -q "Saved: *no" <<<"$out" || fail "BGP status before any config: $out"

# A broken bird.conf is refused, with BIRD's own message and line.
printf 'router id 192.168.78.1;\nprotocol bgp x { bogus; }\n' > "$WORKDIR/bad.conf"
if out="$(ctl a network bgp check "$WORKDIR/bad.conf" 2>&1)"; then fail "a broken bird.conf passed the check"; fi
grep -q "bird.conf:2:" <<<"$out" || fail "BIRD's complaint isn't shown: $out"
echo "  ok: a broken bird.conf is refused with BIRD's own message"

cat > "$WORKDIR/a.conf" <<'EOF'
router id 192.168.78.1;
protocol device {}
# OSPF and BFD next to BGP, for their raw and UDP sockets under enforcing.
protocol bfd {}
protocol ospf v2 ospf1 { ipv4 { import none; export none; }; area 0 { interface "eth1" { hello 1; }; }; }
# Announced only while this node's HAProxy answers.
protocol static haproxy_anycast { ipv4; route 192.0.2.10/32 blackhole; }
protocol static plain { ipv4; route 198.51.100.1/32 blackhole; }
protocol bgp peer_b {
  local 192.168.78.1 as 65001;
  neighbor 192.168.78.2 as 65002;
  password "janus-test";  # TCP MD5 (the kernel's TCP_MD5SIG)
  bfd on;
  ipv4 { import none; export where source = RTS_STATIC; };
}
EOF
cat > "$WORKDIR/b.conf" <<'EOF'
router id 192.168.78.2;
protocol device {}
protocol kernel { ipv4 { export all; }; }
protocol bfd {}
protocol ospf v2 ospf1 { ipv4 { import none; export none; }; area 0 { interface "eth1" { hello 1; }; }; }
protocol bgp peer_a {
  local 192.168.78.2 as 65002;
  neighbor 192.168.78.1 as 65001;
  password "janus-test";
  bfd on;
  ipv4 { import all; export none; };
}
EOF
for n in a b; do
  out="$(ctl $n network bgp apply "$WORKDIR/$n.conf" 2>&1)" || fail "bgp apply on $n: $out"
done

# A protocol's line in the status table: PROTOCOL TYPE STATE SINCE INFO...
proto_line() { ctl "$1" network bgp status 2>/dev/null | awk -v p="$2" '$1 == p'; }
routes_b() { ctl b network status 2>/dev/null | grep -oE '(192\.0\.2\.10|198\.51\.100\.1)/32 via 192\.168\.78\.1' | sort | tr '\n' ' '; }
# wait_for DESCRIPTION COMMAND...: COMMAND runs again every second - a
# function, so what it checks is read each time.
wait_for() {
  local what=$1 deadline=$((SECONDS + 60))
  shift
  until "$@"; do
    [ "$SECONDS" -lt "$deadline" ] || fail "$what${last_routes:+ - routes on B: $last_routes}"
    sleep 1
  done
}
last_routes=""
established() { grep -q Established <<<"$(proto_line "$1" "$2")"; }
routes_are() { [ "$(routes_b)" = "$1" ] || { last_routes="$(routes_b)"; false; }; }
wait_for "A's session never came up" established a peer_b
wait_for "B's session never came up" established b peer_a
both="192.0.2.10/32 via 192.168.78.1 198.51.100.1/32 via 192.168.78.1 "
wait_for "B didn't learn both routes" routes_are "$both"
grep -q "ipv4 2/0" <<<"$(proto_line b peer_a)" || fail "B's session doesn't count 2 routes in: $(proto_line b peer_a)"
echo "  ok: sessions established (TCP MD5, BFD), B learned the anycast and the plain route into its kernel table"
for n in a b; do
  for p in ospf1 bfd1; do
    [ "$(proto_line $n $p | awk '{print $3}')" = up ] || fail "$p isn't up on $n: $(proto_line $n $p)"
  done
done
echo "  ok: OSPF and BFD run next to BGP"

# A's HAProxy stops answering: haproxy_anycast is held down, B loses
# that route and keeps the other.
ctl a system service stop haproxy >/dev/null || fail "stopping HAProxy on A"
plain="198.51.100.1/32 via 192.168.78.1 "
wait_for "B kept the anycast route" routes_are "$plain"
grep -q "held down" <<<"$(proto_line a haproxy_anycast)" || fail "A doesn't show haproxy_anycast held: $(proto_line a haproxy_anycast)"
echo "  ok: A's HAProxy down - haproxy_anycast held down, B lost only that route"

# A bird.conf applied meanwhile brings the protocol back in BIRD - janusd
# puts it down again at once.
printf '# reapplied while HAProxy is down\n' >> "$WORKDIR/a.conf"
out="$(ctl a network bgp apply "$WORKDIR/a.conf" 2>&1)" || fail "re-apply on A: $out"
sleep 5
[ "$(routes_b)" = "$plain" ] || fail "the anycast route came back with HAProxy still down: '$(routes_b)'"
grep -q "held down" <<<"$(proto_line a haproxy_anycast)" || fail "haproxy_anycast isn't held after a re-apply: $(proto_line a haproxy_anycast)"
echo "  ok: a bird.conf applied while HAProxy is down keeps it held"

ctl a system service start haproxy >/dev/null || fail "starting HAProxy on A"
wait_for "B didn't get the anycast route back" routes_are "$both"
echo "  ok: A's HAProxy back - the anycast route is back"

m="$(curl -fsS -m 5 "http://127.0.0.1:$((BASE + 12))/metrics")" || fail "no metrics on A"
grep -q '^janus_bgp_session_established{protocol="peer_b",neighbor="192.168.78.2"} 1$' <<<"$m" || fail "janus_bgp_session_established: $(grep bgp <<<"$m")"
grep -q '^janus_bgp_protocol_held_down{protocol="haproxy_anycast"} 0$' <<<"$m" || fail "janus_bgp_protocol_held_down: $(grep bgp <<<"$m")"
grep -q '^janus_bgp_routes{protocol="peer_b",channel="ipv4",direction="exported"} 2$' <<<"$m" || fail "janus_bgp_routes: $(grep bgp <<<"$m")"
echo "  ok: the exporter reports the session, the gate and the routes"

out="$(ctl a system logs bird 2>&1)"
grep -q "peer_b: BGP session established\|peer_b: Connected\|Started" <<<"$out" || fail "BIRD's log isn't the service's: $(tail -5 <<<"$out")"
echo "  ok: BIRD's messages are in the service log"

# Removing B's configuration stops its BIRD; A's session goes down.
: > "$WORKDIR/empty.conf"
ctl b network bgp apply "$WORKDIR/empty.conf" >/dev/null || fail "removing the configuration"
grep -q "Saved: *no" <<<"$(ctl b network bgp status)" || fail "B's bird.conf is still saved"
not_established() { ! established a peer_b; }
wait_for "A's session stayed up after B's BIRD stopped" not_established
echo "  ok: an empty bird.conf stops BIRD, the peer's session goes down"

denials="$(cat "$WORKDIR"/a.log "$WORKDIR"/b.log | grep -a "avc:.*denied" || true)"
if [ -n "$denials" ]; then
  if [ "${BGP_DISCOVERY:-}" = 1 ]; then
    echo "--- AVC denials (discovery) ---"
    sed -E 's/.*avc:  denied  //; s/pid=[0-9]+ //; s/ino=[0-9]+ //; s/ permissive=[01]//' <<<"$denials" | sort | uniq -c
  else
    echo "$denials" >&2
    fail "AVC denials under enforcing"
  fi
fi
echo "BGP test OK: sessions, routes learned into the kernel, the anycast route withdrawn while HAProxy doesn't answer and back after, zero AVC denials"
