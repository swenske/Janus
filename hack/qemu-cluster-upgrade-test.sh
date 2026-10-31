#!/usr/bin/env bash
# A cluster updated in place (docs/guide/clusters.md "Updating a
# cluster", #375): janusctl cluster upgrade moves the two keepalived
# members of a fleet's cluster to another release, one at a time, under
# constant traffic through the virtual IP - real enforcing nodes.
#
# Three nodes boot this build's keepalived image (release A), each with a
# user-mode NIC this machine reaches (hostfwd) and a shared multicast LAN
# where they reach each other. alice's machine makes a fleet and adopts
# them; lb-1 and lb-2 make cluster edge, a keepalived.conf template gives
# lb-1 the higher priority - it holds 192.168.87.100 - and a stick table
# replicates. lb-3 is no member: it is the client, its Probe RPC asking
# the virtual IP over HTTP in a loop for the whole update.
#
# The same build as release B is served over HTTP from this machine
# (10.0.2.2 for the nodes). janusctl cluster upgrade edge: lb-2 (BACKUP)
# first, lb-1 (MASTER) last - each drained, its node's own A/B update
# with the automatic revert, back on the other slot, its tables learnt,
# undrained. It passes when no request through the virtual IP failed,
# the stick table entry is on both members after it, each member runs B
# on slot B, the virtual IP moved back to lb-1, the journal says every
# step, a second run skips both members - and no AVC denial.
#
# QEMU_CLUSTER_UPGRADE_KIND=bgp: the same with BIRD instead of
# keepalived (the bird image) - the members announce the anycast address
# 192.0.2.100 to lb-3 over eBGP, lb-3 routes to both (ECMP, its kernel
# table) and asks it; the members keep their order, lb-1 first. Before
# the update, lb-1 drained and rebooted: lb-3, which retries its sessions
# every second, hears nothing of the anycast address from it - not even
# for a moment after its BIRD starts - until it is undrained.
#
# With a bundle C whose HAProxy can't start: the update to C stops at its
# first member, lb-2, which reverts to B on its own (the new slot never
# confirms healthy) and is back in service; lb-1 is left alone - no
# request through the virtual IP lost meanwhile either.
#
# Usage: hack/qemu-cluster-upgrade-test.sh <disk-A.img> <bundle-B-dir> <janusctl-bin> <version-B> [<bundle-C-dir> <version-C>]
set -euo pipefail
export PATH="$PATH:/usr/sbin:/sbin"

SRC_DISK="${1:?usage: $0 <disk-A.img> <bundle-B-dir> <janusctl-bin> <version-B>}"
BUNDLE="$(realpath "${2:?usage: $0 <disk-A.img> <bundle-B-dir> <janusctl-bin> <version-B>}")"
CTL_BIN="$(realpath "${3:?usage: $0 <disk-A.img> <bundle-B-dir> <janusctl-bin> <version-B>}")"
VERSION_B="${4:?usage: $0 <disk-A.img> <bundle-B-dir> <janusctl-bin> <version-B>}"
BUNDLE_C="${5:+$(realpath "$5")}"
VERSION_C="${6:-}"
TIMEOUT="${QEMU_CLUSTER_UPGRADE_TIMEOUT:-150}"
BASE="${QEMU_CLUSTER_UPGRADE_BASE_PORT:-$((19260 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
P_BUNDLE=$((BASE + 50))
MCAST="${QEMU_CLUSTER_UPGRADE_MCAST:-230.0.0.$((RANDOM % 200 + 20)):$((20000 + RANDOM % 20000))}"
OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
NODES="1 2 3"
MEMBERS="1 2" # lb-3 is the client
KIND="${QEMU_CLUSTER_UPGRADE_KIND:-vrrp}"
case "$KIND" in
vrrp) VIP=192.168.87.100 ORDER="2 1" ;;
bgp) VIP=192.0.2.100 ORDER="1 2" ;;
*)
  echo "QEMU_CLUSTER_UPGRADE_KIND: vrrp or bgp" >&2
  exit 2
  ;;
esac

WORKDIR="$(mktemp -d)"
declare -A PID
cleanup() {
  [ -n "${LOAD_PID:-}" ] && kill "$LOAD_PID" 2>/dev/null || true
  [ -n "${SAMPLE_PID:-}" ] && kill "$SAMPLE_PID" 2>/dev/null || true
  [ -n "${HTTP_PID:-}" ] && kill "$HTTP_PID" 2>/dev/null || true
  [ -n "${HTTP_C_PID:-}" ] && kill "$HTTP_C_PID" 2>/dev/null || true
  for n in "${!PID[@]}"; do kill "${PID[$n]}" 2>/dev/null || true; done
  if [ "${QEMU_CLUSTER_UPGRADE_KEEP:-}" = 1 ]; then echo "kept $WORKDIR" >&2; else rm -rf "$WORKDIR"; fi
}
trap cleanup EXIT

fail() {
  echo "Cluster upgrade test FAILED: $*" >&2
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

boots() { grep -ac "listening on .* (mTLS required)" "$WORKDIR/$1.log" || true; }

[ -f "$BUNDLE/rootfs.squashfs" ] && [ -f "$BUNDLE/uki-b.efi" ] || fail "no bundle in $BUNDLE"

node() { # node INDEX: http on BASE+10*i, API on +1
  local i=$1
  cp --sparse=always "$SRC_DISK" "$WORKDIR/$i.img"
  cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/$i.vars"
  qemu-system-x86_64 -accel kvm -accel tcg \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$WORKDIR/$i.vars" \
    -drive file="$WORKDIR/$i.img",format=raw,if=virtio \
    -nographic -display none -monitor none -m 512M \
    -netdev "user,id=net0,hostfwd=tcp::$((BASE + 10 * i))-:8080,hostfwd=tcp::$((BASE + 10 * i + 1))-:9505,hostfwd=tcp::$((BASE + 10 * i + 2))-:10056" \
    -device "virtio-net-pci,netdev=net0,mac=52:54:00:aa:02:0$i" \
    -netdev "socket,id=lan,mcast=$MCAST,localaddr=127.0.0.1" \
    -device "virtio-net-pci,netdev=lan,mac=52:54:00:bb:02:0$i" \
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
python3 - "$WORKDIR" "$NODES" <<'PY'
import re, sys
w = sys.argv[1]
for n in sys.argv[2].split():
    log = open(f"{w}/{n}.log", errors="replace").read().replace("\r", "")
    pems = re.findall(r"(-----BEGIN ([A-Z ]+)-----.*?-----END \2-----)", log, re.S)
    for (pem, _), name in zip(pems, ["ca.crt", "admin.crt", "admin.key"]):
        open(f"{w}/{n}-{name}", "w").write(pem + "\n")
PY
own() { # own INDEX args... - the node's own first-boot admin
  local i=$1
  shift
  "$CTL_BIN" -endpoint "127.0.0.1:$((BASE + 10 * i + 1))" -ca "$WORKDIR/$i-ca.crt" -cert "$WORKDIR/$i-admin.crt" -key "$WORKDIR/$i-admin.key" "$@"
}
for i in $NODES; do
  extra=""
  # BGP: each member holds the anycast address it announces.
  if [ "$KIND" = bgp ] && [ "$i" != 3 ]; then extra=', "192.0.2.100/32"'; fi
  cat > "$WORKDIR/$i-net.json" <<EOF
{"hostname": "lb-$i", "interfaces": [
  {"name": "eth0", "mac": "52:54:00:aa:02:0$i", "mode": "ADDRESSING_MODE_DHCP"},
  {"name": "eth1", "mac": "52:54:00:bb:02:0$i", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.168.87.$i/24"$extra]}
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

cat > "$WORKDIR/v1.cfg" <<'EOF'
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
EOF
out="$(alice -n lb-1 haproxy apply-config "$WORKDIR/v1.cfg" 2>&1)" || fail "apply-config on lb-1: $out"
DIR="$WORKDIR/edge"
out="$(alice cluster create edge -source lb-1 -address lb-1=192.168.87.1 -address lb-2=192.168.87.2 -dir "$DIR" lb-1 lb-2 2>&1)" || fail "cluster create: $out"
if [ "$KIND" = vrrp ]; then
for i in $MEMBERS; do
  alice cluster vars edge "lb-$i" "priority=$((200 - 50 * i))" >/dev/null || fail "lb-$i's variables"
done
cat > "$DIR/keepalived.conf" <<'EOF'
global_defs {
    vrrp_version 3
}

track_file haproxy {
    file /run/janus/keepalived/haproxy-health
}

vrrp_instance front {
    state BACKUP
    interface eth1
    virtual_router_id 63
    priority {{ .Member.Vars.priority }}
    advert_int 0.2
    unicast_src_ip {{ .Member.Address }}
    unicast_peer {
    {{- range .Peers }}
        {{ .Address }}
    {{- end }}
    }
    track_file {
        haproxy weight -100
    }
    virtual_ipaddress {
        192.168.87.100/24
    }
}
EOF
out="$(alice cluster apply edge "$DIR" -manage haproxy,files,vrrp -m VRRP 2>&1)" || fail "apply version 2: $out"
else
  # Each member announces the anycast address to lb-3 while its HAProxy
  # answers (haproxy_*: janusd's gate); lb-3 routes to both.
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
  # BIRD's own timers (240 s hold, 80 s keepalive), like a router that
  # trusts its peers to say goodbye: a member's reboot stops its BIRD
  # before reboot(2), which closes the session (a Cease) - lb-3 sees it
  # end before the member is even back (#446). Left running into the
  # reboot, the session lived on until a keepalive met the member back up.
  cat > "$WORKDIR/router.conf" <<'EOF'
router id 192.168.87.3;
protocol device {}
protocol kernel { ipv4 { export all; }; merge paths on; }
protocol bgp m1 { local 192.168.87.3 as 65000; neighbor 192.168.87.1 as 65001; connect retry time 1; error wait time 1, 2; ipv4 { import all; export none; }; }
protocol bgp m2 { local 192.168.87.3 as 65000; neighbor 192.168.87.2 as 65001; connect retry time 1; error wait time 1, 2; ipv4 { import all; export none; }; }
EOF
  out="$(alice -n lb-3 network bgp apply "$WORKDIR/router.conf" 2>&1)" || fail "lb-3's bird.conf: $out"
fi
master() { grep -Eq '^front +MASTER ' <<<"$(alice -n "lb-$1" network vrrp status 2>/dev/null)"; }
# routed: lb-3 learnt the anycast route from both members (BGP).
routed() {
  local st
  st="$(alice -n lb-3 network bgp status 2>/dev/null)"
  for p in m1 m2; do grep -Eq "^$p .*Established.*ipv4 1/0" <<<"$st" || return 1; done
}
in_front() { if [ "$KIND" = vrrp ]; then master 1; else routed; fi; }
peers_up() { [ "$(alice -n "lb-$1" cluster status 2>/dev/null | grep -cE '^lb-[0-9]+ +192\.168\.87\.[0-9]+:10057 +up ')" = "$2" ]; }
wait_for 60 "lb-1 holding the virtual IP, or lb-3 routing to both" in_front
for i in $MEMBERS; do wait_for 60 "lb-$i's peers up" peers_up "$i" 1; done
curl -s --max-time 5 "http://127.0.0.1:$((BASE + 10))/" >/dev/null
has_entry() { grep -Eq '10\.0\.2\.2' <<<"$(alice -n "lb-$1" haproxy table fe 2>/dev/null)"; }
wait_for 20 "lb-2 learnt lb-1's entry" has_entry 2
synced() { alice cluster check edge >/dev/null 2>&1; }
wait_for 30 "the cluster clean" synced
echo "  ok: cluster edge of lb-1 and lb-2 ($KIND: $VIP), a stick table replicated"

if [ "$KIND" = bgp ]; then
  # A drained member's route isn't announced after its reboot, not even
  # for a moment: its BIRD starts with the haproxy_* protocols disabled
  # while the persisted drain holds (internal/bgp HoldDown), not enabled
  # until janusd's gate takes them down a second later. lb-3 retries its
  # sessions every second, so it is established with lb-1 as soon as
  # lb-1's BIRD listens; its exporter's BIRD counters for that session -
  # every update it received - are sampled from before lb-1's reboot until
  # well after the session is back.
  m1_view() { # established imported updates-received withdraws-received ("-": not shown, "x": no answer)
    local m
    m="$(curl -fsS -m 2 "http://127.0.0.1:$((BASE + 32))/metrics" 2>/dev/null)" || { echo "x x x x"; return; }
    awk '
      function v(k) { return (k in s) ? s[k] : "-" }
      { s[$1] = $2 }
      index($0, "janus_bgp_session_established{protocol=\"m1\",") == 1 { e = $2 }
      END {
        p = "janus_bgp_route_changes_total{protocol=\"m1\",channel=\"ipv4\",direction=\"import\","
        print (e == "" ? "-" : e), v("janus_bgp_routes{protocol=\"m1\",channel=\"ipv4\",direction=\"imported\"}"),
          v(p "kind=\"updates\",outcome=\"received\"}"), v(p "kind=\"withdraws\",outcome=\"received\"}")
      }' <<<"$m"
  }
  m1_is() { [ "$(m1_view | cut -d' ' -f1,2)" = "$1" ]; }
  alice -n lb-1 cluster drain >/dev/null || fail "drain lb-1"
  wait_for 30 "lb-3 kept lb-1's route after its drain: $(m1_view)" m1_is "1 0"
  ( while :; do echo "$(date +%s.%N) $(m1_view)"; sleep 0.2; done >"$WORKDIR/m1.log" 2>&1 ) &
  SAMPLE_PID=$!
  b1="$(boots 1)"
  rebooted1() { [ "$(boots 1)" -gt "$b1" ]; }
  # The session lb-1's reboot ended, then the one after it (x: lb-3's
  # exporter didn't answer that time).
  went_down() { awk '$2 == "0" { d = 1 } END { exit !d }' "$WORKDIR/m1.log"; }
  back_up() { awk '$2 == "0" { d = 1 } d && $2 == "1" { u = 1 } END { exit !u }' "$WORKDIR/m1.log"; }
  alice -n lb-1 system reboot >/dev/null || fail "reboot lb-1"
  wait_for "$TIMEOUT" "lb-1 back from its reboot" rebooted1
  back_at="$(date +%s.%N)"
  wait_for 60 "lb-3's session with lb-1 never went down" went_down
  # Down while lb-1 was away - its BIRD closed the session on its way
  # out - not once its janusd listened again (#446).
  down_at="$(awk '$2 == "0" { print $1; exit }' "$WORKDIR/m1.log")"
  awk -v d="$down_at" -v b="$back_at" 'BEGIN { exit !(d < b) }' ||
    fail "lb-3's session with lb-1 went down only once lb-1 was back (at $down_at, lb-1 listening by $back_at): its BIRD didn't close the session before the reboot"
  echo "  ok: lb-3's session with lb-1 ended with its reboot, $(awk -v d="$down_at" -v b="$back_at" 'BEGIN { printf "%.1f", b - d }') s before lb-1 listened again"
  wait_for 90 "lb-3's session with lb-1 never came back: $(m1_view)" back_up
  sleep 10
  kill "$SAMPLE_PID" 2>/dev/null || true
  wait "$SAMPLE_PID" 2>/dev/null || true
  SAMPLE_PID=
  grep -aq "bgp: BIRD starts with haproxy_anycast down" "$WORKDIR/1.log" || fail "lb-1's BIRD didn't start with haproxy_anycast down after its drained reboot"
  # From the session's return on: nothing imported, no update received.
  read -r n heard first last <<<"$(awk '$2 == "0" { d = 1; next }
    d && $2 == "1" { n++; if (!f) f = $1; l = $1; if (($3 != "-" && $3 != "0") || ($4 != "-" && $4 != "0")) h++ }
    END { printf "%d %d %s %s\n", n, h, f, l }' "$WORKDIR/m1.log")"
  [ "$heard" = 0 ] || fail "lb-3 heard lb-1's anycast route after its drained reboot: $(awk '$2 == "1" && $3 != "0"' "$WORKDIR/m1.log" | head -5)"
  [ "$n" -ge 20 ] || fail "only $n samples of lb-3's session with lb-1 after its reboot"
  span="$(awk -v a="$first" -v b="$last" 'BEGIN { printf "%.1f", b - a }')"
  alice -n lb-1 cluster undrain >/dev/null || fail "undrain lb-1"
  wait_for 30 "lb-3 didn't get lb-1's route after its undrain: $(m1_view)" m1_is "1 1"
  read -r _ _ upd wd <<<"$(m1_view)"
  [ "$upd" = 1 ] && [ "$wd" = 0 ] || fail "lb-3 counted $upd updates and $wd withdraws from lb-1 since its reboot, want 1 and 0"
  for i in $MEMBERS; do wait_for 60 "lb-$i's peers up after lb-1's reboot" peers_up "$i" 1; done
  wait_for 60 "lb-1 announcing again, the cluster clean" synced
  echo "  ok: lb-1 drained and rebooted - its BIRD started with haproxy_anycast down; lb-3, its session back, heard nothing of $VIP from it over $n samples ($span s) until the undrain, then one update"
fi

# The client: lb-3 asks the virtual IP over HTTP in a loop, each answer
# the member that served it.
via_vip() { alice -n lb-3 system probe -v http "http://$VIP:8080/" 2>&1; }
out="$(via_vip)" || fail "lb-3 doesn't reach the virtual IP: $out"
check "a member serves $VIP" "$([ "$KIND" = vrrp ] && echo lb-1 || echo "lb-[12]")" "$out"
load() { # load LOG: lb-3 asks the virtual IP until killed
  while :; do
    if out="$(via_vip)"; then
      echo "ok $(date +%s.%N) $(grep -oE 'lb-[0-9]' <<<"$out" | tail -1)"
    else
      echo "FAILED $(date +%s.%N) $(tr '\n' ' ' <<<"$out")"
    fi
    sleep 0.2
  done >"$1" 2>&1
}
load "$WORKDIR/load.log" &
LOAD_PID=$!
sleep 3

# Release B over HTTP, for the nodes.
( cd "$BUNDLE" && exec python3 -m http.server "$P_BUNDLE" --bind 0.0.0.0 >"$WORKDIR/http.log" 2>&1 ) &
HTTP_PID=$!
sleep 1
SUM="$(cut -d' ' -f1 "$BUNDLE/rootfs.squashfs.sha256")"
out="$(alice cluster upgrade edge "http://10.0.2.2:$P_BUNDLE" -version "$VERSION_B" -sha256 "$SUM" -insecure-skip-signature-check 2>&1)" || fail "cluster upgrade: $out"
echo "$out" | sed 's/^/    /'
kill "$LOAD_PID" 2>/dev/null || true
wait "$LOAD_PID" 2>/dev/null || true
LOAD_PID=
total="$(grep -c '' "$WORKDIR/load.log" || true)"
failed="$(grep -c '^FAILED' "$WORKDIR/load.log" || true)"
served2="$(grep -c '^ok .* lb-2$' "$WORKDIR/load.log" || true)"
[ "$failed" = 0 ] || fail "$failed of $total requests through the virtual IP failed during the update: $(grep '^FAILED' "$WORKDIR/load.log" | head -5)"
[ "$total" -gt 20 ] || fail "only $total requests through the virtual IP during the update"
[ "$served2" -gt 0 ] || fail "lb-2 never served $VIP"
echo "  ok: $total requests through the virtual IP during the update, none failed ($served2 served by lb-2 while lb-1 was out)"

# Order, steps, the result.
journal="$(alice cluster journal edge -n 200)"
want="lb-${ORDER% *}, lb-${ORDER#* }"
check "the order" "updating edge to Janus $VERSION_B, one at a time: $want" "$out"
for i in $ORDER; do
  check "lb-$i drained" "lb-$i drained" "$journal"
  check "lb-$i updated" "lb-$i updating from Janus $VERSION_A \(slot A\) to $VERSION_B" "$journal"
  check "lb-$i back on slot B" "lb-$i runs Janus $VERSION_B on slot B, in service" "$journal"
  check "lb-$i back in service" "lb-$i back in service" "$journal"
  v="$(alice -n "lb-$i" version)"
  check "lb-$i runs B" "^Node: $VERSION_B$" "$v"
  check "lb-$i on slot B" "^Active slot: B$" "$v"
  if alice -n "lb-$i" cluster get | grep -q "Drained"; then fail "lb-$i still drained after the update"; fi
done
check "the update's end" "edge runs Janus $VERSION_B: 2 member\(s\) updated, 0 already on it" "$journal"
for i in $MEMBERS; do has_entry "$i" || fail "lb-$i lost the stick table entry: $(alice -n "lb-$i" haproxy table fe)"; done
wait_for 60 "lb-1 holding the virtual IP again, or lb-3 routing to both" in_front
for i in $MEMBERS; do wait_for 60 "lb-$i's peers up after the update" peers_up "$i" 1; done
echo "  ok: $want updated in this order - each drained, on Janus $VERSION_B on slot B, its tables learnt, back in service; the stick table entry kept; $VIP served as before"

# Again: both on B, nothing touched.
b1="$(boots 1)" b2="$(boots 2)"
out="$(alice cluster upgrade edge "http://10.0.2.2:$P_BUNDLE" -version "$VERSION_B" -sha256 "$SUM" -insecure-skip-signature-check 2>&1)" || fail "cluster upgrade again: $out"
check "lb-2 skipped" "lb-2 runs Janus $VERSION_B already: skipped" "$out"
check "lb-1 skipped" "lb-1 runs Janus $VERSION_B already: skipped" "$out"
[ "$(boots 1)" = "$b1" ] && [ "$(boots 2)" = "$b2" ] || fail "a member rebooted on a second run"
echo "  ok: run again, both members on $VERSION_B already: skipped, no reboot"

if [ -n "$BUNDLE_C" ]; then
  # Release C's HAProxy can't start: lb-2, first, reverts to B by itself
  # once its new slot doesn't confirm healthy; the update stops there.
  ( cd "$BUNDLE_C" && exec python3 -m http.server "$((P_BUNDLE + 1))" --bind 0.0.0.0 >"$WORKDIR/http-c.log" 2>&1 ) &
  HTTP_C_PID=$!
  load "$WORKDIR/load-c.log" &
  LOAD_PID=$!
  sleep 3
  b1="$(boots 1)" b2="$(boots 2)"
  if out="$(alice cluster upgrade edge "http://10.0.2.2:$((P_BUNDLE + 1))" -version "$VERSION_C" -sha256 "$(cut -d' ' -f1 "$BUNDLE_C/rootfs.squashfs.sha256")" -insecure-skip-signature-check 2>&1)"; then
    fail "an update to a release whose HAProxy can't start passed: $out"
  fi
  echo "$out" | sed 's/^/    /'
  kill "$LOAD_PID" "$HTTP_C_PID" 2>/dev/null || true
  wait "$LOAD_PID" 2>/dev/null || true
  LOAD_PID=
  check "lb-2 reverted" "lb-2 reverted to Janus $VERSION_B on slot B" "$out"
  check "the update stopped at lb-2" "the update stops at lb-2: lb-2 reverted to Janus $VERSION_B" "$out"
  v="$(alice -n lb-2 version)"
  check "lb-2 back on B" "^Node: $VERSION_B$" "$v"
  check "lb-2 back on slot B" "^Active slot: B$" "$v"
  [ "$(boots 2)" -ge $((b2 + 2)) ] || fail "lb-2 didn't boot twice (C, then back to B): $(boots 2) boots, $b2 before"
  grep -aq "bootcommit: rebooting to complete the revert" "$WORKDIR/2.log" || fail "lb-2's console doesn't show the revert"
  if alice -n lb-2 cluster get | grep -q "Drained"; then fail "lb-2 left drained after its revert"; fi
  [ "$(boots 1)" = "$b1" ] || fail "lb-1 rebooted: the update didn't stop at lb-2"
  check "lb-1 untouched" "^Node: $VERSION_B$" "$(alice -n lb-1 version)"
  total_c="$(grep -c '' "$WORKDIR/load-c.log" || true)"
  failed_c="$(grep -c '^FAILED' "$WORKDIR/load-c.log" || true)"
  [ "$failed_c" = 0 ] || fail "$failed_c of $total_c requests through the virtual IP failed during the revert: $(grep '^FAILED' "$WORKDIR/load-c.log" | head -5)"
  for i in $MEMBERS; do wait_for 60 "lb-$i's peers up after the revert" peers_up "$i" 1; done
  echo "  ok: an update to $VERSION_C (its HAProxy can't start) stopped at lb-2, which reverted to $VERSION_B by itself and is back in service; lb-1 untouched; $total_c requests through the virtual IP, none failed"
fi

for i in $NODES; do
  if grep -a "avc:.*denied" "$WORKDIR/$i.log"; then fail "AVC denials on node $i"; fi
done
echo "Cluster upgrade test PASSED: a two-member $KIND cluster updated in place from $VERSION_A to $VERSION_B by janusctl cluster upgrade - $want in this order, each drained, updated on its other slot, its tables learnt, undrained; $total requests through the virtual IP, none failed; the stick table kept; a second run skipped both${BUNDLE_C:+; an update to a release that cannot serve stopped at its first member, reverted by itself}; no AVC denial"
