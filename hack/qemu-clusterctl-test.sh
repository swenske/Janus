#!/usr/bin/env bash
# A cluster without a Controller (docs/cluster.md phase 3): janusctl
# cluster, from a fleet context, on three real enforcing nodes with
# keepalived.
#
# The nodes boot the keepalived image, each with a user-mode NIC this
# machine reaches (hostfwd) and a shared multicast LAN where they reach
# each other. alice's machine makes a fleet (janusctl fleet) and adopts
# them. janusctl cluster create makes cluster edge of them, lb-1 the
# source: its configuration version 1, its key pulled by the others; the
# peers connect. The locks are advisory: alice's certificate is refused
# like the node's own admin's - only janusctl cluster's calls, sending
# the cluster's id, get through. A directory edited and applied brings a
# keepalived.conf template rendered per member (alice's variables): lb-1
# holds the virtual IP, the stick table replicates; a dry run changes
# nothing. A member released by hand drifts: check reports it and exits
# 1, the next apply puts it back. A certificate for every member. A
# member removed, added again; the ticket ring rotated. A fourth node
# replaces the VRRP master (janusctl cluster replace): it joins drained,
# learns the table, and the virtual IP moves once, to it. The cluster
# deleted - and no AVC denial.
#
# Usage: hack/qemu-clusterctl-test.sh <disk.img> <janusctl-bin>
set -euo pipefail
export PATH="$PATH:/usr/sbin:/sbin"

SRC_DISK="${1:?usage: $0 <disk.img> <janusctl-bin>}"
CTL_BIN="$(realpath "${2:?usage: $0 <disk.img> <janusctl-bin>}")"
TIMEOUT="${QEMU_CLUSTERCTL_TIMEOUT:-150}"
BASE="${QEMU_CLUSTERCTL_BASE_PORT:-$((19070 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
MCAST="${QEMU_CLUSTERCTL_MCAST:-230.0.0.$((RANDOM % 200 + 20)):$((20000 + RANDOM % 20000))}"
OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
NODES="1 2 3 4"
MEMBERS="1 2 3" # lb-4 waits to replace one

WORKDIR="$(mktemp -d)"
declare -A PID
cleanup() {
  for n in "${!PID[@]}"; do kill "${PID[$n]}" 2>/dev/null || true; done
  if [ "${QEMU_CLUSTERCTL_KEEP:-}" = 1 ]; then echo "kept $WORKDIR" >&2; else rm -rf "$WORKDIR"; fi
}
trap cleanup EXIT

fail() {
  echo "Clusterctl test FAILED: $*" >&2
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
    -device "virtio-net-pci,netdev=net0,mac=52:54:00:aa:01:0$i" \
    -netdev "socket,id=lan,mcast=$MCAST,localaddr=127.0.0.1" \
    -device "virtio-net-pci,netdev=lan,mac=52:54:00:bb:01:0$i" \
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
  {"name": "eth0", "mac": "52:54:00:aa:01:0$i", "mode": "ADDRESSING_MODE_DHCP"},
  {"name": "eth1", "mac": "52:54:00:bb:01:0$i", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.168.87.$i/24"]}
]}
EOF
  out="$(own "$i" network apply "$WORKDIR/$i-net.json" 2>&1)" || fail "network apply on $i: $out"
done
echo "  ok: three nodes up, on the LAN as lb-1..3"

# alice's machine: a fleet, the three nodes adopted on their first-boot
# credentials.
alice() { JANUSCONFIG="$WORKDIR/alice/janusctl.json" "$CTL_BIN" "$@"; }
mkdir -p "$WORKDIR/alice"
alice -context lab fleet init -name lab -issuer alice-laptop -user alice -yes "$WORKDIR/kit.age" >"$WORKDIR/passphrase" 2>/dev/null || fail "fleet init"
for i in $NODES; do
  out="$(alice fleet adopt "lb-$i" -endpoint "127.0.0.1:$((BASE + 10 * i + 1))" -ca "$WORKDIR/$i-ca.crt" -cert "$WORKDIR/$i-admin.crt" -key "$WORKDIR/$i-admin.key" 2>&1)" || fail "adopting lb-$i: $out"
done
echo "  ok: alice's fleet on the three nodes"

# lb-1's starting point: a stick table on the peers, a map, a certificate
# (its key stays on the nodes).
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
printf 'example.test be\n' > "$WORKDIR/hosts.map"
alice -n lb-1 haproxy file-put maps/hosts.map "$WORKDIR/hosts.map" >/dev/null || fail "file-put on lb-1"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -subj "/CN=lb.example" -days 2 -keyout "$WORKDIR/lb.key" -out "$WORKDIR/lb.crt" 2>/dev/null
cat "$WORKDIR/lb.crt" "$WORKDIR/lb.key" > "$WORKDIR/lb.pem"
alice -n lb-1 haproxy file-put certs/lb.pem "$WORKDIR/lb.pem" >/dev/null || fail "file-put the key on lb-1"

# The context's addresses are this machine's port forwards: the members
# reach each other on the LAN - a loopback is refused.
if out="$(alice cluster create edge -source lb-1 lb-1 lb-2 lb-3 2>&1)"; then fail "a cluster made on loopback addresses: $out"; fi
check "a loopback address refused" "not where the other members reach it" "$out"
DIR="$WORKDIR/edge"
out="$(alice cluster create edge -source lb-1 -address lb-1=192.168.87.1 -address lb-2=192.168.87.2 -address lb-3=192.168.87.3 -dir "$DIR" lb-1 lb-2 lb-3 2>&1)" || fail "cluster create: $out"
check "the journal says who joined" "lb-3 joined" "$out"
for i in $MEMBERS; do
  check "lb-$i is a member" "Cluster: +edge " "$(alice -n "lb-$i" cluster get)"
  check "lb-$i's locks are advisory" "advisory" "$(alice -n "lb-$i" cluster get)"
done
peers_up() { [ "$(alice -n "lb-$1" cluster status 2>/dev/null | grep -cE '^lb-[0-9]+ +192\.168\.87\.[0-9]+:10057 +up ')" = "$2" ]; }
for i in $MEMBERS; do wait_for 60 "lb-$i's peers up" peers_up "$i" 2; done
for i in 2 3; do
  check "lb-$i runs lb-1's configuration" "track-sc0 src" "$(alice -n "lb-$i" haproxy get-config)"
  check "lb-$i has the map" "maps/hosts.map" "$(alice -n "lb-$i" haproxy files)"
  check "lb-$i pulled the key" "certs/lb.pem" "$(alice -n "lb-$i" haproxy files)"
done
[ -f "$DIR/haproxy.cfg" ] && [ -f "$DIR/files/maps/hosts.map" ] || fail "version 1 not written to $DIR: $(find "$DIR")"
[ ! -e "$DIR/files/certs/lb.pem" ] || fail "a key written to the cluster's directory"
if grep -rlq "PRIVATE KEY" "$WORKDIR/alice/lab/clusters"/*/versions; then fail "a key reached the context's versions"; fi
key="$(find "$WORKDIR/alice/lab/clusters" -name ca.key)"
[ "$(stat -c %a "$key")" = 600 ] || fail "the cluster's CA key isn't 0600: $(stat -c %a "$key")"
echo "  ok: cluster edge created from lb-1 - peers over mTLS, version 1 everywhere and in $(basename "$DIR"), the key pulled member to member, the CA's key 0600"

# The locks: alice's certificate and the node's own admin, refused.
out="$(alice -n lb-2 haproxy apply-config "$WORKDIR/v1.cfg" 2>&1 || true)"
check "alice's direct apply refused" "managed by cluster edge" "$out"
out="$(own 2 haproxy file-put maps/other.map "$WORKDIR/hosts.map" 2>&1 || true)"
check "the node's own admin refused" "managed by cluster edge" "$out"
echo "  ok: the members' locks hold - for alice's certificate too"

# Version 2 from the directory: VRRP, rendered per member.
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
    virtual_router_id 62
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
check "version 2 everywhere" "version 2 on every member" "$out"
check "lb-2's priority rendered" "priority 100" "$(alice -n lb-2 network vrrp get)"
check "lb-2's peers rendered" "192\.168\.87\.3" "$(alice -n lb-2 network vrrp get)"
master() { grep -Eq '^front +MASTER ' <<<"$(alice -n "lb-$1" network vrrp status 2>/dev/null)"; }
wait_for 30 "lb-1 holding the virtual IP" master 1
curl -s --max-time 5 "http://127.0.0.1:$((BASE + 10))/" >/dev/null
has_entry() { grep -Eq '10\.0\.2\.2' <<<"$(alice -n "lb-$1" haproxy table fe 2>/dev/null)"; }
wait_for 20 "lb-2 learnt lb-1's entry" has_entry 2
echo "  ok: version 2's keepalived.conf rendered per member - lb-1 holds 192.168.87.100, the stick table replicates"

# A dry run says, changes nothing.
sed -i 's/timeout client 10s/timeout client 12s/' "$DIR/haproxy.cfg"
out="$(alice cluster apply edge "$DIR" -dry-run 2>&1)" || fail "dry run: $out"
check "the dry run's change" "haproxy.cfg: \+1 -1 lines" "$out"
check "nothing applied by a dry run" "timeout client 10s" "$(alice -n lb-2 haproxy get-config)"

# A member released by hand drifts: check says so (exit 1), the next
# apply - version 3 - puts it back and locks it again.
alice -n lb-3 cluster release haproxy >/dev/null || fail "release on lb-3"
sed 's/timeout client 10s/timeout client 11s/' "$WORKDIR/v1.cfg" > "$WORKDIR/hand.cfg"
out="$(alice -n lb-3 haproxy apply-config "$WORKDIR/hand.cfg" 2>&1)" || fail "apply-config on lb-3 once released: $out"
if out="$(alice cluster check edge 2>&1)"; then fail "check passed over a drifted member: $out"; fi
check "lb-3 drifted" "lb-3 .*drifted: haproxy" "$out"
check "lb-3 released" "released: haproxy" "$out"
check "lb-3 left as it is" "timeout client 11s" "$(alice -n lb-3 haproxy get-config)"
out="$(alice cluster apply edge "$DIR" 2>&1)" || fail "apply version 3: $out"
for i in $MEMBERS; do check "lb-$i runs version 3" "timeout client 12s" "$(alice -n "lb-$i" haproxy get-config)"; done
out="$(alice -n lb-3 haproxy apply-config "$WORKDIR/hand.cfg" 2>&1 || true)"
check "lb-3 locked again" "managed by cluster edge" "$out"
clean() { alice cluster check edge >/dev/null 2>&1; }
wait_for 30 "check clean after the apply (peers back after the reload)" clean
echo "  ok: a dry run changed nothing; a drift reported (check exits 1), put back by the next apply - the lock with it"

# A certificate for every member.
out="$(alice cluster certificate edge "$WORKDIR/lb.pem" -name www 2>&1)" || fail "certificate: $out"
for i in $MEMBERS; do check "lb-$i has the cluster's certificate" "www" "$(alice -n "lb-$i" haproxy cert-list)"; done
echo "  ok: a certificate reached every member"

# A drain moves the virtual IP, with the tables.
alice -n lb-1 cluster drain >/dev/null || fail "drain lb-1"
wait_for 20 "lb-2 holding the virtual IP" master 2
has_entry 2 || fail "lb-2 lost the table after the failover"
alice -n lb-1 cluster undrain >/dev/null || fail "undrain lb-1"
wait_for 30 "lb-1 holding the virtual IP again" master 1
echo "  ok: a drain moved the virtual IP to lb-2, which had the table"

# lb-3 leaves, joins again; the ticket ring rotates.
out="$(alice cluster remove edge lb-3 2>&1)" || fail "remove lb-3: $out"
check "lb-3 on its own" "Cluster: +none" "$(alice -n lb-3 cluster get)"
for i in 1 2; do wait_for 60 "lb-$i with one peer" peers_up "$i" 1; done
# Its variables went with it: the cluster's version can't render for it,
# refused before anything is touched.
if out="$(alice cluster add edge lb-3 -address 192.168.87.3 2>&1)"; then fail "lb-3 added without its template's variable: $out"; fi
check "a missing variable refused first" "lb-3 can't run version 3: .*priority" "$out"
check "lb-3 still on its own" "Cluster: +none" "$(alice -n lb-3 cluster get)"
out="$(alice cluster add edge lb-3 -address 192.168.87.3 -var priority=50 2>&1)" || fail "add lb-3: $out"
check "lb-3's priority rendered" "priority 50" "$(alice -n lb-3 network vrrp get)"
check "lb-3 a member again" "Cluster: +edge " "$(alice -n lb-3 cluster get)"
for i in $MEMBERS; do wait_for 60 "lb-$i's peers up again" peers_up "$i" 2; done
out="$(alice cluster tickets edge 2>&1)" || fail "tickets: $out"
check "the ring pulled" "pulled from lb-1" "$out"
check "the journal" "version 3 applied on lb-3" "$(alice cluster journal edge -n 200)"
echo "  ok: a member removed and added again - refused first without its template's variable; the ticket ring rotated and pulled"

# lb-4 takes the VRRP master's place: it joins drained, learns the
# table, comes into service as lb-1 is drained - the virtual IP moves
# once, to lb-4 (lb-1's priority, copied); lb-2 and lb-3 never take it.
masters() { alice -n "lb-$1" system logs keepalived 2>/dev/null | grep -c "Entering MASTER STATE" || true; }
wait_for 30 "lb-1 holding the virtual IP before the replacement" master 1
curl -s --max-time 5 "http://127.0.0.1:$((BASE + 10))/" >/dev/null
wait_for 20 "lb-2 learnt the entry" has_entry 2
m2="$(masters 2)" m3="$(masters 3)"
out="$(alice cluster replace edge lb-1 lb-4 -address 192.168.87.4 2>&1)" || fail "replace lb-1 with lb-4: $out"
check "lb-4 joined drained" "lb-4 joins drained" "$out"
check "lb-1 drained as lb-4 came in" "lb-1 drained, lb-4 in service" "$out"
check "lb-4 a member" "Cluster: +edge " "$(alice -n lb-4 cluster get)"
check "lb-1 on its own" "Cluster: +none" "$(alice -n lb-1 cluster get)"
check "lb-4 has lb-1's priority" "priority 150" "$(alice -n lb-4 network vrrp get)"
wait_for 30 "lb-4 holding the virtual IP" master 4
has_entry 4 || fail "lb-4 hasn't the stick table"
[ "$(masters 2)" = "$m2" ] && [ "$(masters 3)" = "$m3" ] || fail "the virtual IP went through lb-2 or lb-3: $(masters 2) $(masters 3) MASTER transitions, $m2 $m3 before"
for i in 2 3 4; do wait_for 60 "lb-$i's peers up after the replacement" peers_up "$i" 2; done
echo "  ok: lb-4 replaced lb-1, the VRRP master - joined drained, the table learnt, the virtual IP moved once"

# The cluster goes: the nodes on their own, unlocked.
out="$(alice cluster delete edge 2>&1)" || fail "delete: $out"
for i in 2 3 4; do check "lb-$i on its own" "Cluster: +none" "$(alice -n "lb-$i" cluster get)"; done
out="$(alice -n lb-2 haproxy apply-config "$WORKDIR/v1.cfg" 2>&1)" || fail "lb-2 unlocked once the cluster went: $out"
[ -z "$(alice cluster list | tail -n +2)" ] || fail "the cluster still listed: $(alice cluster list)"
echo "  ok: the cluster deleted - the nodes on their own and unlocked"

for i in $NODES; do
  if grep -a "avc:.*denied" "$WORKDIR/$i.log"; then fail "AVC denials on node $i"; fi
done
echo "Clusterctl test PASSED: a fleet's cluster made by janusctl - advisory locks, VRRP from a directory, a dry run, drift reported and put back, a certificate, a drain, a member out and back, tickets, the master replaced with the virtual IP moving once, deleted"
