#!/usr/bin/env bash
# Connections handed over (docs/guide/clusters.md "Handing connections
# over", internal/handoff): an anycast cluster updated in place without
# cutting a connection - real enforcing nodes.
#
# Three nodes boot this build's bird + nftables image (release A), each
# with a user-mode NIC this machine reaches (hostfwd) and a shared
# multicast LAN. lb-1 and lb-2 make cluster edge: both hold 192.0.2.100
# and announce it to lb-3 over eBGP (janusd's HAProxy gate); lb-3 routes
# to both (ECMP, its kernel table) - the router. Its HAProxy relays TCP
# from this machine to the anycast address, where each member's HAProxy
# relays it to an echo server on this machine: a client here holds 16
# connections through lb-3 to a member, each sending a byte every 0.2 s
# and reading it back, each closed after 10 s and opened again.
#
# First, the control: the member holding the connections drained by
# hand - lb-3 sends their packets to the other member, which resets
# them: connections break. Then janusctl cluster upgrade -handoff to
# release B (this build again), one member at a time: before a member is
# drained it refuses new connections and the other reads its own, then
# copies their packets to it once lb-3's route is gone; the member
# serves them until they end (10 s), then updates; back, it holds what it
# has no trace of and copies the other's connections to it, then is
# undrained - lb-3 hashing part of them to it again. It passes when no
# connection broke and no echo was lost during the whole update, the
# members copied packets, the journal says each step, both members run B
# - and no AVC denial.
#
# Usage: hack/qemu-cluster-handoff-test.sh <disk-A.img> <bundle-B-dir> <janusctl-bin> <version-B>
set -euo pipefail
# shellcheck source=state-read.sh
. "$(dirname "$0")/state-read.sh"
export PATH="$PATH:/usr/sbin:/sbin"

SRC_DISK="${1:?usage: $0 <disk-A.img> <bundle-B-dir> <janusctl-bin> <version-B>}"
BUNDLE="$(realpath "${2:?usage: $0 <disk-A.img> <bundle-B-dir> <janusctl-bin> <version-B>}")"
CTL_BIN="$(realpath "${3:?usage: $0 <disk-A.img> <bundle-B-dir> <janusctl-bin> <version-B>}")"
VERSION_B="${4:?usage: $0 <disk-A.img> <bundle-B-dir> <janusctl-bin> <version-B>}"
TIMEOUT="${QEMU_CLUSTER_HANDOFF_TIMEOUT:-150}"
BASE="${QEMU_CLUSTER_HANDOFF_BASE_PORT:-$((19400 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
P_BUNDLE=$((BASE + 50))
P_ECHO=$((BASE + 51))
MCAST="${QEMU_CLUSTER_HANDOFF_MCAST:-230.0.0.$((RANDOM % 200 + 20)):$((20000 + RANDOM % 20000))}"
OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
NODES="1 2 3"
MEMBERS="1 2" # lb-3 is the router
VIP=192.0.2.100

WORKDIR="$(mktemp -d)"
declare -A PID
cleanup() {
  [ -n "${CLIENT_PID:-}" ] && kill "$CLIENT_PID" 2>/dev/null || true
  [ -n "${ECHO_PID:-}" ] && kill "$ECHO_PID" 2>/dev/null || true
  [ -n "${HTTP_PID:-}" ] && kill "$HTTP_PID" 2>/dev/null || true
  [ -n "${SAMPLE_PID:-}" ] && kill "$SAMPLE_PID" 2>/dev/null || true
  for n in "${!PID[@]}"; do kill "${PID[$n]}" 2>/dev/null || true; done
  if [ "${QEMU_CLUSTER_HANDOFF_KEEP:-}" = 1 ]; then echo "kept $WORKDIR" >&2; else rm -rf "$WORKDIR"; fi
}
trap cleanup EXIT

fail() {
  echo "Cluster handoff test FAILED: $*" >&2
  for i in $NODES; do
    [ -f "$WORKDIR/$i.log" ] && { echo "--- console $i ---" >&2; tail -60 "$WORKDIR/$i.log" >&2; }
  done
  exit 1
}

# wait_for SECONDS WHAT COMMAND...: polls COMMAND every second.
wait_for() {
  local timeout=$1 what=$2
  shift 2
  local deadline=$((SECONDS + timeout))
  until "$@"; do
    [ "$SECONDS" -lt "$deadline" ] || fail "$what - not within ${timeout}s"
    sleep 1
  done
}

check() { # check DESCRIPTION PATTERN TEXT
  grep -Eq -- "$2" <<<"$3" || fail "$1 (no match for $2 in: $3)"
}

[ -f "$BUNDLE/rootfs.squashfs" ] && [ -f "$BUNDLE/uki-b.efi" ] || fail "no bundle in $BUNDLE"

node() { # node INDEX: http on BASE+10*i, API +1, exporter +2, the relay +3
  local i=$1
  cp --sparse=always "$SRC_DISK" "$WORKDIR/$i.img"
  cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/$i.vars"
  qemu-system-x86_64 -accel kvm -accel tcg \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$WORKDIR/$i.vars" \
    -drive file="$WORKDIR/$i.img",format=raw,if=virtio \
    -nographic -display none -monitor none -m 512M \
    -netdev "user,id=net0,hostfwd=tcp::$((BASE + 10 * i))-:8080,hostfwd=tcp::$((BASE + 10 * i + 1))-:9505,hostfwd=tcp::$((BASE + 10 * i + 2))-:10056,hostfwd=tcp::$((BASE + 10 * i + 3))-:9000" \
    -device "virtio-net-pci,netdev=net0,mac=52:54:00:aa:03:0$i" \
    -netdev "socket,id=lan,mcast=$MCAST,localaddr=127.0.0.1" \
    -device "virtio-net-pci,netdev=lan,mac=52:54:00:bb:03:0$i" \
    -serial file:"$WORKDIR/$i.log" </dev/null &
  PID[$i]=$!
}
for i in $NODES; do node "$i"; done
for i in $NODES; do
  deadline=$((SECONDS + TIMEOUT))
  until grep -aq "listening on .* (mTLS required)" "$WORKDIR/$i.log" 2>/dev/null; do
    [ "$SECONDS" -lt "$deadline" ] || fail "node $i's janusd never listened"
    sleep 1
  done
done
sleep 2
for n in $NODES; do state_creds "$WORKDIR/$n.img" "$WORKDIR/$n" || fail "node $n's pki/ from STATE"; done
own() { # own INDEX args... - the node's own first-boot admin
  local i=$1
  shift
  "$CTL_BIN" -endpoint "127.0.0.1:$((BASE + 10 * i + 1))" -ca "$WORKDIR/$i-ca.crt" -cert "$WORKDIR/$i-admin.crt" -key "$WORKDIR/$i-admin.key" "$@"
}
for i in $NODES; do
  extra=""
  [ "$i" != 3 ] && extra=", \"$VIP/32\""
  cat > "$WORKDIR/$i-net.json" <<EOF
{"hostname": "lb-$i", "interfaces": [
  {"name": "eth0", "mac": "52:54:00:aa:03:0$i", "mode": "ADDRESSING_MODE_DHCP"},
  {"name": "eth1", "mac": "52:54:00:bb:03:0$i", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.168.87.$i/24"$extra]}
]}
EOF
  out="$(own "$i" network apply "$WORKDIR/$i-net.json" 2>&1)" || fail "network apply on $i: $out"
done
VERSION_A="$(own 1 version | sed -n 's/^Node: //p')"
[ -n "$VERSION_A" ] && [ "$VERSION_A" != "$VERSION_B" ] || fail "release A is $VERSION_A, B $VERSION_B: they must differ"
echo "  ok: three nodes up on Janus $VERSION_A, on the LAN as lb-1..3"

alice() { JANUSCONFIG="$WORKDIR/alice/janusctl.json" "$CTL_BIN" "$@"; }
mkdir -p "$WORKDIR/alice"
alice -context lab fleet init -name lab -issuer alice-laptop -user alice -yes "$WORKDIR/kit.age" >"$WORKDIR/passphrase" 2>/dev/null || fail "fleet init"
for i in $NODES; do
  out="$(alice fleet adopt "lb-$i" -endpoint "127.0.0.1:$((BASE + 10 * i + 1))" -ca "$WORKDIR/$i-ca.crt" -cert "$WORKDIR/$i-admin.crt" -key "$WORKDIR/$i-admin.key" 2>&1)" || fail "adopting lb-$i: $out"
done
for i in $NODES; do
  if out="$(alice -n "lb-$i" cluster get 2>&1)" && grep -q "Handoff: *unavailable" <<<"$out"; then fail "lb-$i can't hand connections over: $out"; fi
done

# The members relay TCP on :9000 to the echo server on this machine.
cat > "$WORKDIR/v1.cfg" <<EOF
global
    chroot /var/empty
    uid 1000
    gid 1000
    stats socket /run/janus/haproxy-admin.sock mode 660 level admin

defaults
    mode http
    timeout connect 2s
    timeout client 10s
    timeout server 10s

frontend fe
    bind :8080
    stick-table type ip size 10k expire 1h store http_req_cnt peers janus
    http-request track-sc0 src
    http-request return status 200 content-type text/plain lf-string "%[var(proc.janus_member)]\n"

listen relay
    mode tcp
    bind :9000
    timeout client 30s
    timeout server 30s
    server echo 10.0.2.2:$P_ECHO
EOF
out="$(alice -n lb-1 haproxy apply-config "$WORKDIR/v1.cfg" 2>&1)" || fail "apply-config on lb-1: $out"
DIR="$WORKDIR/edge"
out="$(alice cluster create edge -source lb-1 -address lb-1=192.168.87.1 -address lb-2=192.168.87.2 -dir "$DIR" lb-1 lb-2 2>&1)" || fail "cluster create: $out"
cat > "$DIR/bird.conf" <<'EOF'
router id {{ .Member.Address }};
protocol device {}
protocol static haproxy_anycast { ipv4; route 192.0.2.100/32 blackhole; }
protocol bgp router {
  local {{ .Member.Address }} as 65001;
  neighbor 192.168.87.3 as 65000;
  ipv4 { import none; export where source = RTS_STATIC; };
}
EOF
out="$(alice cluster apply edge "$DIR" -manage haproxy,files,bgp -m BGP 2>&1)" || fail "apply version 2: $out"
cat > "$WORKDIR/router.conf" <<'EOF'
router id 192.168.87.3;
protocol device {}
protocol kernel { ipv4 { export all; }; merge paths on; }
protocol bgp m1 { local 192.168.87.3 as 65000; neighbor 192.168.87.1 as 65001; connect retry time 1; error wait time 1, 2; ipv4 { import all; export none; }; }
protocol bgp m2 { local 192.168.87.3 as 65000; neighbor 192.168.87.2 as 65001; connect retry time 1; error wait time 1, 2; ipv4 { import all; export none; }; }
EOF
out="$(alice -n lb-3 network bgp apply "$WORKDIR/router.conf" 2>&1)" || fail "lb-3's bird.conf: $out"
# lb-3 relays this machine's connections to the anycast address.
cat > "$WORKDIR/router.cfg" <<'EOF'
global
    chroot /var/empty
    uid 1000
    gid 1000
    stats socket /run/janus/haproxy-admin.sock mode 660 level admin

defaults
    mode tcp
    timeout connect 2s
    timeout client 1h
    timeout server 1h

listen front
    bind :9000
    server anycast 192.0.2.100:9000
EOF
out="$(alice -n lb-3 haproxy apply-config "$WORKDIR/router.cfg" 2>&1)" || fail "lb-3's haproxy.cfg: $out"
routed() {
  local st
  st="$(alice -n lb-3 network bgp status 2>/dev/null)"
  for p in m1 m2; do grep -Eq "^$p .*Established.*ipv4 1/0" <<<"$st" || return 1; done
}
peers_up() { [ "$(alice -n "lb-$1" cluster status 2>/dev/null | grep -cE '^lb-[0-9]+ +192\.168\.87\.[0-9]+:10057 +up ')" = "$2" ]; }
synced() { alice cluster check edge >/dev/null 2>&1; }
wait_for 60 "lb-3 routing to both members" routed
for i in $MEMBERS; do wait_for 60 "lb-$i's peers up" peers_up "$i" 1; done
wait_for 30 "the cluster clean" synced
echo "  ok: cluster edge of lb-1 and lb-2 announcing $VIP to lb-3, which routes to both"

# The echo server, and the client: WORKERS connections through lb-3,
# each echoing a byte every 0.2 s for LIFE seconds, then opened again.
cat > "$WORKDIR/echo.py" <<'PY'
import asyncio, sys
async def serve(r, w):
    try:
        while data := await r.read(64):
            w.write(data)
            await w.drain()
    except Exception:
        pass
    w.close()
async def main():
    s = await asyncio.start_server(serve, "0.0.0.0", int(sys.argv[1]))
    async with s:
        await s.serve_forever()
asyncio.run(main())
PY
cat > "$WORKDIR/client.py" <<'PY'
import json, os, socket, sys, threading, time
port, workers, life, out, stop = int(sys.argv[1]), int(sys.argv[2]), float(sys.argv[3]), sys.argv[4], sys.argv[5]
lock = threading.Lock()
stats = {"echoes": 0, "opened": 0, "closed": 0, "broken": [], "refused": 0}
def work(n):
    while not os.path.exists(stop):
        try:
            c = socket.create_connection(("127.0.0.1", port), timeout=10)
        except OSError:
            with lock:
                stats["refused"] += 1
            time.sleep(0.5)
            continue
        c.settimeout(5)
        with lock:
            stats["opened"] += 1
        end, ok = time.time() + life, True
        while time.time() < end and not os.path.exists(stop):
            try:
                c.sendall(b"x")
                if c.recv(1) != b"x":
                    raise OSError("closed by the other end")
                with lock:
                    stats["echoes"] += 1
            except OSError as e:
                with lock:
                    stats["broken"].append(f"{time.time():.1f} worker {n}: {e}")
                ok = False
                break
            time.sleep(0.2)
        c.close()
        if ok:
            with lock:
                stats["closed"] += 1
def report():
    while not os.path.exists(stop):
        with lock:
            json.dump(stats, open(out + ".tmp", "w"))
        os.replace(out + ".tmp", out)
        time.sleep(0.5)
ts = [threading.Thread(target=work, args=(n,), daemon=True) for n in range(workers)]
for t in ts:
    t.start()
    time.sleep(life / workers)  # spread their ends over a lifetime
threading.Thread(target=report, daemon=True).start()
for t in ts:
    t.join()
with lock:
    json.dump(stats, open(out, "w"))
PY
python3 "$WORKDIR/echo.py" "$P_ECHO" >"$WORKDIR/echo.log" 2>&1 &
ECHO_PID=$!
sleep 1
stat() { python3 -c 'import json,sys; s=json.load(open(sys.argv[1])); print(s["echoes"], len(s["broken"]), s["opened"], s["closed"], s["refused"])' "$1"; }
client() { # client NAME: start it, its results in $WORKDIR/NAME.json
  rm -f "$WORKDIR/$1.stop"
  python3 "$WORKDIR/client.py" "$((BASE + 33))" 16 10 "$WORKDIR/$1.json" "$WORKDIR/$1.stop" >"$WORKDIR/$1.out" 2>&1 &
  CLIENT_PID=$!
}
opened() { python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["opened"])' "$WORKDIR/$1.json" 2>/dev/null || echo 0; }
all_open() { [ "$(opened "$1")" -ge 16 ]; }
stop_client() { # stop_client NAME
  touch "$WORKDIR/$1.stop"
  wait "$CLIENT_PID" 2>/dev/null || true
  CLIENT_PID=
}
# held MEMBER: its relayed connections from lb-3.
held() { alice -n "lb-$1" system netstat 2>/dev/null | grep -cE "^tcp +$VIP:9000 +192\.168\.87\.3:[0-9]+ +ESTABLISHED" || true; }

# The control: the member holding the connections drained by hand - lb-3
# sends their packets to the other, which resets them.
client control
wait_for 30 "the client's connections through lb-3" all_open control
sleep 3
h1="$(held 1)" h2="$(held 2)"
X=1
[ "$h2" -gt "$h1" ] && X=2
[ "$((h1 + h2))" -ge 10 ] || fail "only $h1 + $h2 relayed connections on the members"
read -r e0 b0 _ <<<"$(stat "$WORKDIR/control.json")"
[ "$b0" = 0 ] || fail "connections broke before anything was done: $(cat "$WORKDIR/control.json")"
alice -n "lb-$X" cluster drain >/dev/null || fail "drain lb-$X"
sleep 6
read -r e1 b1 _ <<<"$(stat "$WORKDIR/control.json")"
# The client gone before lb-$X is back - its connections reopened on the
# other member close there, not reset by lb-$X once lb-3 hashes them to
# it again; the ones it reset leave lb-$X's HAProxy their sockets until
# its 30 s client timeout.
stop_client control
alice -n "lb-$X" cluster undrain >/dev/null || fail "undrain lb-$X"
[ "$b1" -gt 0 ] || fail "lb-$X drained without a handoff, yet no connection broke ($h1 on lb-1, $h2 on lb-2): the test doesn't see what it is for"
wait_for 60 "lb-3 routing to both members again" routed
wait_for 60 "the cluster clean again" synced
none_left() { [ "$(held 1)" = 0 ] && [ "$(held 2)" = 0 ]; }
wait_for 60 "the members' sockets of the reset connections gone" none_left
echo "  ok: the control - lb-$X held $((h1 > h2 ? h1 : h2)) connections, drained without a handoff: $b1 broke within 6 s"

# The update with the handoff, under the same traffic.
client update
wait_for 30 "the client's connections through lb-3" all_open update
sleep 3
( while :; do for i in $MEMBERS; do echo "lb-$i $(date +%s) $(alice -n "lb-$i" cluster get 2>/dev/null | grep '^Handoff:' | tr '\n' '|')"; done; sleep 1; done >"$WORKDIR/handoffs.log" 2>&1 ) &
SAMPLE_PID=$!
( cd "$BUNDLE" && exec python3 -m http.server "$P_BUNDLE" --bind 0.0.0.0 >"$WORKDIR/http.log" 2>&1 ) &
HTTP_PID=$!
sleep 1
SUM="$(cut -d' ' -f1 "$BUNDLE/rootfs.squashfs.sha256")"
out="$(alice cluster upgrade edge "http://10.0.2.2:$P_BUNDLE" -version "$VERSION_B" -sha256 "$SUM" -insecure-skip-signature-check -handoff -handoff-grace 60s 2>&1)" || fail "cluster upgrade -handoff: $out"
echo "$out" | sed 's/^/    /'
sleep 5
stop_client update
kill "$SAMPLE_PID" 2>/dev/null || true
wait "$SAMPLE_PID" 2>/dev/null || true
SAMPLE_PID=
read -r echoes broken opened closed refused <<<"$(stat "$WORKDIR/update.json")"
# Denials first: a copy SELinux drops is a connection that breaks.
for i in $NODES; do
  if grep -a "avc:.*denied" "$WORKDIR/$i.log" | head -3 | grep .; then fail "AVC denials on node $i (and $broken connection(s) broke)"; fi
done
[ "$broken" = 0 ] || fail "$broken connection(s) broke during the update with the handoff: $(python3 -c 'import json,sys; print("; ".join(json.load(open(sys.argv[1]))["broken"][:5]))' "$WORKDIR/update.json")"
[ "$echoes" -gt 1000 ] || fail "only $echoes echoes during the update"
echo "  ok: $opened connections opened, $closed closed on their own, $echoes echoes during the update - none broke"

journal="$(alice cluster journal edge -n 300)"
# The anycast address alone: 10.0.2.15, on every node's user-mode NIC,
# is a /24's - no service address.
check "the shared addresses" "connections on $VIP handed over \(a grace of 1m0s" "$journal"
for i in $MEMBERS; do
  o=$((3 - i))
  check "lb-$i's connections read by lb-$o" "lb-$o copy lb-$i's connections to it while it is out" "$journal"
  check "lb-$i went once its connections ended" "lb-$i has no connection left after" "$journal"
  check "lb-$i copied lb-$o's back" "lb-$i copies the others' connections to them for 1h0m0s: lb-$o's [0-9]+" "$journal"
  check "lb-$i back" "lb-$i runs Janus $VERSION_B on slot B, in service" "$journal"
  check "lb-$i runs B" "^Node: $VERSION_B$" "$(alice -n "lb-$i" version)"
done
# Packets copied: the member that held the connections had them while out.
copied="$(grep -oE 'copying lb-[12].s [0-9]+ connection\(s\) to it until [^,]+, [0-9]+ packet' "$WORKDIR/handoffs.log" | awk '{print $(NF-1)}' | sort -n | tail -1)"
[ -n "$copied" ] && [ "$copied" -gt 0 ] || fail "no member copied a packet: $(grep -c '' "$WORKDIR/handoffs.log") samples, $(grep -m3 'Handoff' "$WORKDIR/handoffs.log")"
grep -q "refusing new connections" "$WORKDIR/handoffs.log" || fail "no member was seen refusing new connections"
grep -q "holding the packets" "$WORKDIR/handoffs.log" || fail "no member was seen holding packets"
for i in $MEMBERS; do grep -aq "cluster: handing lb-$((3 - i))'s connections over" "$WORKDIR/$i.log" || fail "lb-$i's console doesn't show its handoff"; done
echo "  ok: each member refused new connections, was read and copied to while out ($copied packets at most), held and copied back once in - the journal says each step; both on $VERSION_B"

for i in $NODES; do
  if grep -a "avc:.*denied" "$WORKDIR/$i.log"; then fail "AVC denials on node $i"; fi
done
echo "Cluster handoff test PASSED: drained without a handoff, a member's connections broke ($b1); janusctl cluster upgrade -handoff moved lb-1 and lb-2 from $VERSION_A to $VERSION_B one at a time under $opened connections through the anycast address - none broke, $echoes echoes; no AVC denial"
