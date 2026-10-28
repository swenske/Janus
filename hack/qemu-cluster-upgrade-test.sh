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

WORKDIR="$(mktemp -d)"
declare -A PID
cleanup() {
  [ -n "${LOAD_PID:-}" ] && kill "$LOAD_PID" 2>/dev/null || true
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
    -netdev "user,id=net0,hostfwd=tcp::$((BASE + 10 * i))-:8080,hostfwd=tcp::$((BASE + 10 * i + 1))-:9505" \
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
  cat > "$WORKDIR/$i-net.json" <<EOF
{"hostname": "lb-$i", "interfaces": [
  {"name": "eth0", "mac": "52:54:00:aa:02:0$i", "mode": "ADDRESSING_MODE_DHCP"},
  {"name": "eth1", "mac": "52:54:00:bb:02:0$i", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.168.87.$i/24"]}
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
master() { grep -Eq '^front +MASTER ' <<<"$(alice -n "lb-$1" network vrrp status 2>/dev/null)"; }
peers_up() { [ "$(alice -n "lb-$1" cluster status 2>/dev/null | grep -cE '^lb-[0-9]+ +192\.168\.87\.[0-9]+:10057 +up ')" = "$2" ]; }
wait_for 30 "lb-1 holding the virtual IP" master 1
for i in $MEMBERS; do wait_for 60 "lb-$i's peers up" peers_up "$i" 1; done
curl -s --max-time 5 "http://127.0.0.1:$((BASE + 10))/" >/dev/null
has_entry() { grep -Eq '10\.0\.2\.2' <<<"$(alice -n "lb-$1" haproxy table fe 2>/dev/null)"; }
wait_for 20 "lb-2 learnt lb-1's entry" has_entry 2
synced() { alice cluster check edge >/dev/null 2>&1; }
wait_for 30 "the cluster clean" synced
echo "  ok: cluster edge of lb-1 (MASTER of 192.168.87.100) and lb-2, a stick table replicated"

# The client: lb-3 asks the virtual IP over HTTP in a loop, each answer
# the member that served it.
via_vip() { alice -n lb-3 system probe -v http http://192.168.87.100:8080/ 2>&1; }
out="$(via_vip)" || fail "lb-3 doesn't reach the virtual IP: $out"
check "lb-1 serves the virtual IP" "lb-1" "$out"
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
[ "$served2" -gt 0 ] || fail "lb-2 never served the virtual IP while lb-1 was updated"
echo "  ok: $total requests through the virtual IP during the update, none failed ($served2 served by lb-2 while lb-1 was out)"

# Order, steps, the result.
journal="$(alice cluster journal edge -n 200)"
check "the order: lb-2 first, the master last" "updating edge to Janus $VERSION_B, one at a time: lb-2, lb-1" "$out"
for i in 2 1; do
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
wait_for 30 "lb-1 holding the virtual IP again" master 1
for i in $MEMBERS; do wait_for 60 "lb-$i's peers up after the update" peers_up "$i" 1; done
echo "  ok: lb-2 then lb-1 updated - each drained, on Janus $VERSION_B on slot B, its tables learnt, back in service; the stick table entry kept; lb-1 holds the virtual IP again"

# Again: both on B, nothing touched.
boots() { grep -ac "listening on .* (mTLS required)" "$WORKDIR/$1.log" || true; }
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
echo "Cluster upgrade test PASSED: a two-member VRRP cluster updated in place from $VERSION_A to $VERSION_B by janusctl cluster upgrade - the BACKUP first, the MASTER last, each drained, updated on its other slot, its tables learnt, undrained; $total requests through the virtual IP, none failed; the stick table kept; a second run skipped both${BUNDLE_C:+; an update to a release that cannot serve stopped at its first member, reverted by itself}; no AVC denial"
