#!/usr/bin/env bash
# A cluster of three real enforcing nodes (ClusterService, docs/cluster.md).
#
# Three nodes boot the disk image, each with a user-mode NIC for the host
# (hostfwd) and a shared multicast LAN where they reach each other. On
# its own, a node's HAProxy already runs with the peers section janusd
# generates - the local peer alone, so a reload keeps its stick tables -
# and refuses a configuration that defines that section, names the local
# peer or binds the peers port. Then the nodes become a cluster the way an
# orchestrator does it: a signing request from each (its key stays on the
# node), a member certificate of a cluster CA made here (hack/clusterca),
# ClusterSet on each. Checked: the peers connect over mTLS, a stick-table
# entry made on one node shows on the others, a reload of one node gets
# its table back from a peer, a cold restart learns it back, a member
# certificate is renewed without a reload, the exporter says it all, the
# member key is never served, a member leaves - and no AVC denial.
#
# Usage: hack/qemu-cluster-test.sh <disk.img> <janusctl-bin>
set -euo pipefail
export PATH="$PATH:/usr/sbin:/sbin"

SRC_DISK="${1:?usage: $0 <disk.img> <janusctl-bin>}"
CTL_BIN="${2:?usage: $0 <disk.img> <janusctl-bin>}"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
TIMEOUT="${QEMU_CLUSTER_TIMEOUT:-150}"
BASE="${QEMU_CLUSTER_BASE_PORT:-$((18950 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
MCAST="${QEMU_CLUSTER_MCAST:-230.0.0.$((RANDOM % 200 + 20)):$((20000 + RANDOM % 20000))}"
OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
NODES="1 2 3"

WORKDIR="$(mktemp -d)"
declare -A PID
cleanup() {
  for n in "${!PID[@]}"; do kill "${PID[$n]}" 2>/dev/null || true; done
  if [ "${QEMU_CLUSTER_KEEP:-}" = 1 ]; then echo "kept $WORKDIR" >&2; else rm -rf "$WORKDIR"; fi
}
trap cleanup EXIT

fail() {
  echo "cluster test FAILED: $*" >&2
  for i in $NODES; do
    [ -f "$WORKDIR/$i.log" ] && { echo "--- console $i ---" >&2; tail -80 "$WORKDIR/$i.log" >&2; }
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

# node INDEX: boot one node - http on BASE+10*i, API on +1, metrics on +2.
node() {
  local i=$1
  cp --sparse=always "$SRC_DISK" "$WORKDIR/$i.img"
  cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/$i.vars"
  qemu-system-x86_64 -accel kvm -accel tcg \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$WORKDIR/$i.vars" \
    -drive file="$WORKDIR/$i.img",format=raw,if=virtio \
    -nographic -display none -monitor none -m 512M \
    -netdev "user,id=net0,hostfwd=tcp::$((BASE + 10 * i))-:8080,hostfwd=tcp::$((BASE + 10 * i + 1))-:9505,hostfwd=tcp::$((BASE + 10 * i + 2))-:10056,hostfwd=tcp::$((BASE + 10 * i + 3))-:8443" \
    -device "virtio-net-pci,netdev=net0,mac=52:54:00:aa:00:0$i" \
    -netdev "socket,id=lan,mcast=$MCAST,localaddr=127.0.0.1" \
    -device "virtio-net-pci,netdev=lan,mac=52:54:00:bb:00:0$i" \
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
ctl() { # ctl INDEX args...
  local i=$1
  shift
  "$CTL_BIN" -endpoint "127.0.0.1:$((BASE + 10 * i + 1))" -ca "$WORKDIR/$i-ca.crt" -cert "$WORKDIR/$i-admin.crt" -key "$WORKDIR/$i-admin.key" "$@"
}
for i in $NODES; do ctl "$i" version >/dev/null || fail "no API on node $i"; done
echo "  ok: three nodes up"

# The shared LAN: eth1, static, and a hostname each - the member name.
for i in $NODES; do
  cat > "$WORKDIR/$i-net.json" <<EOF
{"hostname": "lb-$i", "interfaces": [
  {"name": "eth0", "mac": "52:54:00:aa:00:0$i", "mode": "ADDRESSING_MODE_DHCP"},
  {"name": "eth1", "mac": "52:54:00:bb:00:0$i", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.168.80.$i/24"]}
]}
EOF
  out="$(ctl "$i" network apply "$WORKDIR/$i-net.json" 2>&1)" || fail "network apply on $i: $out"
done
echo "  ok: the nodes on the shared LAN, named lb-1..3"

# A configuration with a stick table on the generated peers section: the
# request counter per client, and the member's name as the answer.
cfg() { # cfg EXTRA-LINES
cat <<EOF
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
$1
EOF
}
cfg "" > "$WORKDIR/haproxy.cfg"
for i in $NODES; do
  out="$(ctl "$i" haproxy apply-config "$WORKDIR/haproxy.cfg" 2>&1)" || fail "apply-config on $i: $out"
done
# The local peer name follows the hostname: the apply reloaded HAProxy
# with -L lb-N and the section regenerated for it.
http() { curl -s --max-time 5 "http://127.0.0.1:$((BASE + 10 * $1))/"; }
wait_for 20 "node 1 answers with its member name" test "$(http 1)" = "lb-1"
for i in $NODES; do
  info="$(ctl "$i" cluster get)"
  check "node $i on its own" "Cluster: +none - the node is on its own, local peer lb-$i" "$info"
  check "node $i's section names it alone" "server lb-$i" "$info"
  check "node $i binds the local address" "bind 127.0.0.1:10057" "$info"
done
echo "  ok: on their own, the nodes run with the generated section and -L"

# A reload keeps the stick table on a node on its own (the local resync).
# HAProxy's resync window after a start is five seconds, and the apply
# above was the first with the new local peer name: a reload inside that
# window gets no table from a process still waiting for its own.
sleep 6
http 1 >/dev/null; http 1 >/dev/null
table() { ctl "$1" haproxy table fe 2>/dev/null || true; }
check "an entry for the host" "10\.0\.2\.2" "$(table 1)"
ctl 1 haproxy apply-config "$WORKDIR/haproxy.cfg" >/dev/null
sleep 2
check "the entry survived the reload" "10\.0\.2\.2" "$(table 1)"
echo "  ok: a reload keeps the stick table on a node on its own"

# What a configuration may not say.
cfg "
peers janus
    server lb-1" > "$WORKDIR/bad-section.cfg"
out="$(ctl 1 haproxy apply-config "$WORKDIR/bad-section.cfg" 2>&1 || true)"
check "the generated section is refused" "janusd's" "$out"
cfg "
frontend other
    bind :10057" > "$WORKDIR/bad-port.cfg"
out="$(ctl 1 haproxy apply-config "$WORKDIR/bad-port.cfg" 2>&1 || true)"
check "the peers port is refused" "peers listener" "$out"
cfg "" | sed 's/^    stats socket.*/&\n    localpeer other/' > "$WORKDIR/bad-localpeer.cfg"
out="$(ctl 1 haproxy apply-config "$WORKDIR/bad-localpeer.cfg" 2>&1 || true)"
check "localpeer is refused" "localpeer" "$out"
echo "  ok: the policy keeps the section, the port and the local peer janusd's"

# The cluster: a CA, a signing request from each node, its certificate.
ID="$(python3 -c 'import uuid; print(uuid.uuid4())')"
(cd "$REPO" && go run ./hack/clusterca new "$WORKDIR/ca" edge >/dev/null)
MEMBERS="lb-1=192.168.80.1 lb-2=192.168.80.2 lb-3=192.168.80.3"
for i in $NODES; do
  ctl "$i" cluster csr "$WORKDIR/lb-$i.csr" >/dev/null
  (cd "$REPO" && go run ./hack/clusterca sign "$WORKDIR/ca" "$WORKDIR/lb-$i.csr" "$ID" "lb-$i" "$WORKDIR/lb-$i.crt")
done
# A certificate naming another member, for this node's key: refused;
# another node's certificate too, for its key.
(cd "$REPO" && go run ./hack/clusterca sign "$WORKDIR/ca" "$WORKDIR/lb-1.csr" "$ID" lb-2 "$WORKDIR/lb-1-as-2.crt")
out="$(ctl 1 cluster set -id "$ID" -name edge -member lb-1 -ca "$WORKDIR/ca/ca.crt" -cert "$WORKDIR/lb-1-as-2.crt" $MEMBERS 2>&1 || true)"
check "a certificate naming another member is refused" "names lb-2" "$out"
out="$(ctl 1 cluster set -id "$ID" -name edge -member lb-1 -ca "$WORKDIR/ca/ca.crt" -cert "$WORKDIR/lb-2.crt" $MEMBERS 2>&1 || true)"
check "another node's certificate is refused" "not for the key this node made" "$out"
# An address that isn't the node's: refused.
out="$(ctl 1 cluster set -id "$ID" -name edge -member lb-1 -ca "$WORKDIR/ca/ca.crt" -cert "$WORKDIR/lb-1.crt" lb-1=192.168.80.9 lb-2=192.168.80.2 2>&1 || true)"
check "an address that isn't the node's is refused" "isn't one of this node's" "$out"
for i in $NODES; do
  # shellcheck disable=SC2086
  out="$(ctl "$i" cluster set -id "$ID" -name edge -member "lb-$i" -ca "$WORKDIR/ca/ca.crt" -cert "$WORKDIR/lb-$i.crt" -managed-by "the test" $MEMBERS 2>&1)" || fail "cluster set on $i: $out"
  check "node $i joined" "Cluster: +edge \($ID\)" "$out"
  check "node $i's section is TLS" "bind 192.168.80.$i:10057 ssl crt /etc/haproxy/cluster/member.pem ca-file /etc/haproxy/cluster/ca.pem verify required" "$out"
done
echo "  ok: three members"

peers_up() { # peers_up INDEX: both remote peers established
  [ "$(ctl "$1" cluster status 2>/dev/null | grep -cE '^lb-[0-9]+ +192\.168\.80\.[0-9]+:10057 +up ')" = 2 ]
}
for i in $NODES; do wait_for 60 "node $i's peers up" peers_up "$i"; done
echo "  ok: the peers connected over mTLS"

# Replication: entries made on node 1 show on 2 and 3.
http 1 >/dev/null; http 1 >/dev/null
has_entry() { grep -Eq '10\.0\.2\.2' <<<"$(table "$1")"; }
for i in 2 3; do wait_for 20 "node $i learnt node 1's entry" has_entry "$i"; done
# Node 1 made the updates: both peers acknowledged them, in sync (HAProxy's
# "update" for the peer - its "last_acked" is node 1's of theirs, 0 here:
# v2026.10.24's lab saw the VIP's master "catching up" for good).
synced() { [ "$(ctl 1 cluster status 2>/dev/null | grep -cE '^lb-[23] +192\.168\.80\.[0-9]+:10057 +up +[^ ]+ +1 +1/1$')" = 2 ]; }
wait_for 20 "node 1's peers acknowledged its updates" synced
echo "  ok: the stick table replicates"

# Under a steady stream of updates the peers are never at node 1's very
# last one, but they keep up: in sync all the same (v2026.10.24-2's lab
# saw the member serving the traffic "catching up" for as long as
# traffic flowed). Eight keep-alive clients, each request an update.
cat > "$WORKDIR/load.py" <<'PY'
import http.client, sys, threading, time
port, secs = int(sys.argv[1]), float(sys.argv[2])
end, counts = time.time() + secs, []
def run():
    n, c = 0, http.client.HTTPConnection("127.0.0.1", port, timeout=5)
    while time.time() < end:
        c.request("GET", "/"); c.getresponse().read(); n += 1
    counts.append(n)
ts = [threading.Thread(target=run) for _ in range(8)]
for t in ts: t.start()
for t in ts: t.join()
print(sum(counts), "requests in", int(secs), "s")
PY
# What is checked is that the indicator comes back while the traffic
# flows - the defect was "catching up" for as long as it did -, not that
# every reading is: on a busy CI runner (three VMs and the load on a few
# cores) a peer may lag more than janusd's 2 s window now and then
# (v2026.10.24-3's and -5's first builds).
metric_synced() { grep -Eq 'janus_cluster_peer_table_synced\{peer="lb-2",table="fe"\} 1' <<<"$(curl -s --max-time 10 "http://127.0.0.1:$((BASE + 12))/metrics")"; }
python3 "$WORKDIR/load.py" "$((BASE + 10))" 60 >"$WORKDIR/load.txt" 2>&1 &
load=$!
sleep 5
for n in 1 2 3; do
  wait_for 12 "node 1's peers in sync under load (sample $n)" synced
  wait_for 12 "janus_cluster_peer_table_synced under load (sample $n)" metric_synced
  sleep 4
done
kill -0 "$load" 2>/dev/null || fail "the load ended before the samples: $(cat "$WORKDIR/load.txt")"
wait "$load" || fail "the load: $(cat "$WORKDIR/load.txt")"
echo "  ok: in sync under a steady stream of updates ($(cat "$WORKDIR/load.txt"))"

# A reload on a member: the table comes back from a peer.
ctl 1 haproxy apply-config "$WORKDIR/haproxy.cfg" >/dev/null
wait_for 20 "node 1's table back after its reload" has_entry 1
# A cold restart: learnt back in full.
ctl 2 system service restart haproxy >/dev/null
wait_for 30 "node 2's table back after a restart" has_entry 2
wait_for 60 "node 2's peers up again" peers_up 2
echo "  ok: the table survives a reload and comes back after a restart"

# The exporter.
metrics() { curl -s --max-time 5 "http://127.0.0.1:$((BASE + 10 * $1 + 2))/metrics"; }
m="$(metrics 1)"
check "janus_cluster_info" "janus_cluster_info\{cluster_id=\"$ID\",cluster_name=\"edge\",member=\"lb-1\"\} 1" "$m"
check "janus_cluster_peer_up lb-2" 'janus_cluster_peer_up\{peer="lb-2"\} 1' "$m"
check "janus_cluster_peer_table_synced" 'janus_cluster_peer_table_synced\{peer="lb-3",table="fe"\} 1' "$m"
echo "  ok: the exporter reports the cluster"

# The member key is never served.
out="$(ctl 1 system cat /etc/haproxy/cluster/member.pem 2>&1 || true)"
if grep -q "PRIVATE KEY" <<<"$out"; then fail "the member key was served"; fi
out="$(ctl 1 system cat /etc/.state/haproxy/cluster/member.pem 2>&1 || true)"
if grep -q "PRIVATE KEY" <<<"$out"; then fail "the member key was served from STATE"; fi
echo "  ok: the member key stays on the node"

# A member certificate renewed: a new one for the same key, no reload.
before="$(ctl 3 cluster get | grep 'Member cert')"
(cd "$REPO" && go run ./hack/clusterca sign "$WORKDIR/ca" "$WORKDIR/lb-3.csr" "$ID" lb-3 "$WORKDIR/lb-3-renewed.crt")
uptime() { ctl "$1" haproxy show-info | sed -n 's/^Uptime: *\([0-9]*\)s/\1/p'; }
up_before="$(uptime 3)"
# shellcheck disable=SC2086
out="$(ctl 3 cluster set -id "$ID" -name edge -member lb-3 -cert "$WORKDIR/lb-3-renewed.crt" $MEMBERS 2>&1)" || fail "renew on 3: $out"
after="$(ctl 3 cluster get | grep 'Member cert')"
[ "$before" != "$after" ] || fail "the member certificate didn't change: $after"
up_after="$(uptime 3)"
# A reload would start the uptime over.
[ -n "$up_before" ] && [ "$up_after" -ge "$up_before" ] || fail "HAProxy reloaded for a certificate (uptime ${up_before}s -> ${up_after}s)"
wait_for 30 "node 3's peers still up with the renewed certificate" peers_up 3
echo "  ok: a member certificate renewed at runtime"

# What members carry to one another, pulled by node 2 from node 1 over
# mTLS with their member certificates: a certificate uploaded there, a
# file, and the TLS ticket key ring. Behind a cluster's shared address
# the public frontend serves the SAME certificate on every member (one
# that was uploaded and pulled, or a shared file as here): HAProxy binds
# a session's ticket to the serving certificate, so the ring lets a TLS
# session resume on another member only once that member serves the same
# certificate too. TLS 1.2 in the check: openssl s_client saves a TLS
# 1.3 session only after the post-handshake ticket, past the request here.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -subj "/CN=www.example" -days 2 -keyout "$WORKDIR/public.key" -out "$WORKDIR/public.crt" 2>/dev/null
cat "$WORKDIR/public.crt" "$WORKDIR/public.key" > "$WORKDIR/public.pem"
cfg "
frontend tls
    bind :8443 ssl crt /etc/haproxy/files/certs/public.pem tls-ticket-keys /etc/haproxy/cluster/tls-ticket-keys
    http-request return status 200 content-type text/plain lf-string \"%[var(proc.janus_member)]\n\"" > "$WORKDIR/haproxy-tls.cfg"
for i in $NODES; do
  # The shared public certificate first: the configuration loads it.
  out="$(ctl "$i" haproxy file-put certs/public.pem "$WORKDIR/public.pem" 2>&1)" || fail "file-put public cert on $i: $out"
  out="$(ctl "$i" haproxy apply-config "$WORKDIR/haproxy-tls.cfg" 2>&1)" || fail "apply-config (tls) on $i: $out"
done
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -subj "/CN=extra.example" -days 2 -keyout "$WORKDIR/extra.key" -out "$WORKDIR/extra.crt" 2>/dev/null
cat "$WORKDIR/extra.crt" "$WORKDIR/extra.key" > "$WORKDIR/extra.pem"
out="$(ctl 1 haproxy cert-upload extra "$WORKDIR/extra.pem" 2>&1)" || fail "cert-upload on 1: $out"
printf 'example.test be_example\n' > "$WORKDIR/hosts.map"
out="$(ctl 1 haproxy file-put maps/hosts.map "$WORKDIR/hosts.map" 2>&1)" || fail "file-put on 1: $out"
out="$(ctl 1 cluster rotate-tickets 2>&1)" || fail "rotate-tickets on 1: $out"
check "HAProxy took the new ticket key" "took the new key" "$out"
tls_port() { echo $((BASE + 10 * $1 + 3)); }
session() { # session INDEX: a TLS session with node INDEX, saved
  printf 'GET / HTTP/1.0\r\n\r\n' | openssl s_client -tls1_2 -connect "127.0.0.1:$(tls_port "$1")" -sess_out "$WORKDIR/sess.pem" >/dev/null 2>&1 || true
  [ -s "$WORKDIR/sess.pem" ] || fail "no TLS session from node $1"
}
resumed() { # resumed INDEX: the saved session resumes on node INDEX
  printf 'GET / HTTP/1.0\r\n\r\n' | openssl s_client -tls1_2 -connect "127.0.0.1:$(tls_port "$1")" -sess_in "$WORKDIR/sess.pem" 2>/dev/null | grep -Eq '^Reused,'
}
wait_for 20 "node 1's TLS frontend" session 1
resumed 1 || fail "a session doesn't resume on the node that made it"
if resumed 2; then fail "a session resumed on node 2 before it had node 1's ticket keys"; fi
out="$(ctl 2 cluster pull -from lb-1 2>&1)" || fail "pull on 2: $out"
check "the ring pulled and loaded" "tls-ticket-keys .*installed and handed to HAProxy" "$out"
check "the certificate pulled" "runtime-certs +extra +loaded" "$out"
check "the file pulled" "files +maps/hosts.map +written" "$out"
check "the pull done" "pulled from lb-1" "$out"
ring() { ctl "$1" cluster get | sed -n 's/.*sha256 \([0-9a-f]*\).*/\1/p'; }
[ -n "$(ring 1)" ] && [ "$(ring 1)" = "$(ring 2)" ] || fail "the rings differ after the pull: $(ring 1) vs $(ring 2)"
check "node 2 has the certificate" "extra" "$(ctl 2 haproxy cert-list)"
check "node 2 has the file" "maps/hosts.map" "$(ctl 2 haproxy files)"
resumed 2 || fail "a session from node 1 doesn't resume on node 2 after the pull"
# The pull's authentication: node 3, no longer... still a member here,
# may pull; what isn't a member of this cluster can't (the unit tests).
echo "  ok: secrets pulled from a member - a TLS session resumes on either"

# A drain: nothing to wait for on nodes without keepalived or BIRD, but
# the member says so and stays out until undrained.
out="$(ctl 2 cluster drain -timeout 10 2>&1)" || fail "drain on 2: $out"
check "the drain is done" "^done " "$out"
check "no VRRP to wait for" "no VRRP on this node" "$out"
check "node 2 says it is drained" "Drained: +yes" "$(ctl 2 cluster get)"
out="$(ctl 2 cluster undrain 2>&1)" || fail "undrain on 2: $out"
if grep -q "Drained" <<<"$out"; then fail "node 2 still drained: $out"; fi
echo "  ok: drained and back"

# A member leaves; the others drop it. It had the cluster's ring: it
# makes its own, and the cluster's sessions no longer resume on it.
out="$(ctl 3 cluster pull -from lb-1 -items tls-ticket-keys 2>&1)" || fail "pull on 3: $out"
session 1
resumed 3 || fail "a session from node 1 doesn't resume on node 3 after its pull"
out="$(ctl 3 cluster leave 2>&1)" || fail "leave on 3: $out"
check "node 3 on its own again" "Cluster: +none" "$out"
[ -n "$(ring 3)" ] && [ "$(ring 3)" != "$(ring 1)" ] || fail "node 3 kept the cluster's ring after leaving: $(ring 3)"
if resumed 3; then fail "a session from the cluster still resumes on node 3 after it left"; fi
for i in 1 2; do
  out="$(ctl "$i" cluster set -id "$ID" -name edge -member "lb-$i" lb-1=192.168.80.1 lb-2=192.168.80.2 2>&1)" || fail "cluster set on $i: $out"
  if grep -q "lb-3" <<<"$out"; then fail "node $i still names lb-3: $out"; fi
done
one_peer() { [ "$(ctl "$1" cluster status 2>/dev/null | grep -cE '^lb-[0-9]+ +192\.168\.80\.[0-9]+:10057 +up ')" = 1 ]; }
for i in 1 2; do wait_for 30 "node $i with one peer" one_peer "$i"; done
wait_for 20 "node 3 answers on its own" test "$(http 3)" = "lb-3"
# The others renew theirs: every key replaced - a session made before
# resumes no more.
before="$(ring 1)"
out="$(ctl 1 cluster rotate-tickets -renew 2>&1)" || fail "rotate-tickets -renew on 1: $out"
check "the ring renewed" "ring renewed: 3 keys, .*took the new ring" "$out"
[ "$(ring 1)" != "$before" ] || fail "the ring didn't change"
if resumed 1; then fail "a session made before the renewal still resumes on node 1"; fi
echo "  ok: a member left, with a ticket key ring of its own - the cluster's sessions no longer resume on it, nor after the others' renewal"

for i in $NODES; do
  if grep -a "avc:.*denied" "$WORKDIR/$i.log"; then fail "AVC denials on node $i"; fi
done
echo "cluster test PASSED: three enforcing nodes, peers over mTLS, tables replicated, kept and learnt back, a member renewed and one gone"
