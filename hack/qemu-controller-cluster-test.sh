#!/usr/bin/env bash
# A cluster the Controller drives (docs/cluster.md sections 2-4), on three
# real enforcing nodes with keepalived and a native dashboardd.
#
# The nodes boot the keepalived image, each with a user-mode NIC the
# Controller reaches (hostfwd) and a shared multicast LAN where they reach
# each other; they get the Controller's fleet. Through the Controller's
# API only: a cluster made of them, lb-1 the source - its configuration
# version 1, its certificates the others'; the peers connect. A direct
# write to a managed domain is refused by the member's lock. Version 2
# brings a keepalived.conf template rendered per member (priority, unicast
# peers): lb-1 holds the virtual IP, and stick tables replicate. A member
# released by hand drifts: reported, not overwritten; the next apply puts
# it back and locks it again. A certificate uploaded to the cluster
# reaches every member. A drain through the Controller moves the virtual
# IP to lb-2, which has the tables - a balancer hook (a receiver on the
# host) told lb-1 goes before it is drained, signed, and back after. A
# member removed - the hook told before, then the list without it -, the
# cluster deleted - and no AVC denial.
#
# Usage: hack/qemu-controller-cluster-test.sh <disk.img> <dashboardd-bin> <janusctl-bin>
set -euo pipefail
export PATH="$PATH:/usr/sbin:/sbin"

SRC_DISK="${1:?usage: $0 <disk.img> <dashboardd-bin> <janusctl-bin>}"
DASHBOARDD="${2:?usage: $0 <disk.img> <dashboardd-bin> <janusctl-bin>}"
CTL_BIN="${3:?usage: $0 <disk.img> <dashboardd-bin> <janusctl-bin>}"
TIMEOUT="${QEMU_CCLUSTER_TIMEOUT:-150}"
BASE="${QEMU_CCLUSTER_BASE_PORT:-$((19030 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
MCAST="${QEMU_CCLUSTER_MCAST:-230.0.0.$((RANDOM % 200 + 20)):$((20000 + RANDOM % 20000))}"
OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
NODES="1 2 3"
P_DASH=$BASE
P_REG=$((BASE + 1))
API="https://127.0.0.1:$P_DASH"

WORKDIR="$(mktemp -d)"
declare -A PID
cleanup() {
  for n in "${!PID[@]}"; do kill "${PID[$n]}" 2>/dev/null || true; done
  if [ "${QEMU_CCLUSTER_KEEP:-}" = 1 ]; then echo "kept $WORKDIR" >&2; else rm -rf "$WORKDIR"; fi
}
trap cleanup EXIT

fail() {
  echo "Controller cluster test FAILED: $*" >&2
  for i in $NODES; do
    [ -f "$WORKDIR/$i.log" ] && { echo "--- console $i ---" >&2; tail -60 "$WORKDIR/$i.log" >&2; }
  done
  [ -f "$WORKDIR/dash.log" ] && { echo "--- dashboardd ---" >&2; tail -60 "$WORKDIR/dash.log" >&2; }
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
ctl() { # ctl INDEX args... - the node's own admin, straight to it
  local i=$1
  shift
  "$CTL_BIN" -endpoint "127.0.0.1:$((BASE + 10 * i + 1))" -ca "$WORKDIR/$i-ca.crt" -cert "$WORKDIR/$i-admin.crt" -key "$WORKDIR/$i-admin.key" "$@"
}
for i in $NODES; do
  cat > "$WORKDIR/$i-net.json" <<EOF
{"hostname": "lb-$i", "interfaces": [
  {"name": "eth0", "mac": "52:54:00:aa:00:0$i", "mode": "ADDRESSING_MODE_DHCP"},
  {"name": "eth1", "mac": "52:54:00:bb:00:0$i", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.168.86.$i/24"]}
]}
EOF
  out="$(ctl "$i" network apply "$WORKDIR/$i-net.json" 2>&1)" || fail "network apply on $i: $out"
done
echo "  ok: three nodes up, on the LAN as lb-1..3"

# The Controller, its fleet, the nodes.
"$DASHBOARDD" -addr "127.0.0.1:$P_DASH" -register-addr "127.0.0.1:$P_REG" -data-dir "$WORKDIR/data" -image-factory "" > "$WORKDIR/dash.log" 2>&1 &
PID[dash]=$!
for _ in $(seq 30); do curl -sk -o /dev/null "$API/api/auth/status" && break; sleep 1; done
JAR="$WORKDIR/jar"
api() { # api METHOD PATH [JSON] - the body; fails on an HTTP error
  local out code
  out="$(curl -sk -b "$JAR" -c "$JAR" -X "$1" -H 'Content-Type: application/json' ${3:+--data-binary "$3"} -w '\n%{http_code}' "$API$2")"
  code="${out##*$'\n'}"
  out="${out%$'\n'*}"
  [ "${code:0:1}" = 2 ] || { echo "$1 $2: $code $out" >&2; return 1; }
  printf '%s' "$out"
}
jq_() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
api POST /api/auth/setup '{"password":"cluster-test-admin-pw","mfa_required":"nobody"}' >/dev/null || fail "setup"
declare -A ID
for i in $NODES; do
  body="$(python3 - "$WORKDIR" "$i" "$((BASE + 10 * i + 1))" <<'PY'
import json, sys
w, i, port = sys.argv[1:]
r = lambda n: open(f"{w}/{i}-{n}").read()
print(json.dumps({"name": f"lb-{i}", "address": f"127.0.0.1:{port}", "ca_cert_pem": r("ca.crt"), "bootstrap_cert_pem": r("admin.crt"), "bootstrap_key_pem": r("admin.key")}))
PY
)"
  ID[$i]="$(api POST /api/nodes "$body" | jq_ 'd["id"]')" || fail "add lb-$i"
done
fleet="$(api POST /api/fleet/setup)" || fail "fleet setup"
kit="$(api GET /api/fleet/recovery-kit)" || fail "recovery kit"
pass="$(jq_ 'd["passphrase"]' <<<"$fleet")"
api POST /api/fleet/confirm "$(python3 -c 'import json,sys; print(json.dumps({"kit": sys.argv[1], "passphrase": sys.argv[2]}))' "$kit" "$pass")" >/dev/null || fail "fleet confirm"
trusted() {
  [ "$(api GET /api/fleet | python3 -c "import json,sys; d=json.load(sys.stdin); print(sum(1 for i in sys.argv[1:] if d.get('nodes',{}).get(i,{}).get('state')=='trusted'))" "${ID[@]}")" = 3 ]
}
wait_for 120 "the nodes to trust the fleet" trusted
echo "  ok: the Controller's fleet on the three nodes"

# lb-1's starting point: a configuration with a stick table on the
# peers, a map, a certificate (a key: it stays on the nodes).
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
out="$(ctl 1 haproxy apply-config "$WORKDIR/v1.cfg" 2>&1)" || fail "apply-config on lb-1: $out"
printf 'example.test be\n' > "$WORKDIR/hosts.map"
ctl 1 haproxy file-put maps/hosts.map "$WORKDIR/hosts.map" >/dev/null || fail "file-put on lb-1"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -subj "/CN=lb.example" -days 2 -keyout "$WORKDIR/lb.key" -out "$WORKDIR/lb.crt" 2>/dev/null
cat "$WORKDIR/lb.crt" "$WORKDIR/lb.key" > "$WORKDIR/lb.pem"
ctl 1 haproxy file-put certs/lb.pem "$WORKDIR/lb.pem" >/dev/null || fail "file-put the key on lb-1"

# The cluster, through the Controller.
create="$(python3 - "${ID[1]}" "${ID[2]}" "${ID[3]}" <<'PY'
import json, sys
ids = sys.argv[1:]
print(json.dumps({"name": "edge", "source": ids[0],
  "members": [{"node_id": ids[i], "name": f"lb-{i+1}", "address": f"192.168.86.{i+1}"} for i in range(3)]}))
PY
)"
CID="$(api POST /api/clusters "$create" | jq_ 'd["id"]')" || fail "create the cluster"
idle() { [ "$(api GET "/api/clusters/$CID" | jq_ 'd["busy"]')" = False ]; }
cluster_error() { api GET "/api/clusters/$CID" | jq_ 'd.get("error","")'; }
wait_for 180 "the cluster created" idle
err="$(cluster_error)"
[ -z "$err" ] || fail "the cluster's creation: $err"
for i in $NODES; do
  check "lb-$i is a member" "Cluster: +edge \($CID\)" "$(ctl "$i" cluster get)"
done
peers_up() { [ "$(ctl "$1" cluster status 2>/dev/null | grep -cE '^lb-[0-9]+ +192\.168\.86\.[0-9]+:10057 +up ')" = "$2" ]; }
for i in $NODES; do wait_for 60 "lb-$i's peers up" peers_up "$i" 2; done
# Version 1 is lb-1's; the others run it, and have its key by pull.
for i in 2 3; do
  check "lb-$i runs lb-1's configuration" "track-sc0 src" "$(ctl "$i" haproxy get-config)"
  check "lb-$i has the map" "maps/hosts.map" "$(ctl "$i" haproxy files)"
  check "lb-$i pulled the key" "certs/lb.pem" "$(ctl "$i" haproxy files)"
done
v1="$(api GET "/api/clusters/$CID/versions/1")"
if grep -q "PRIVATE KEY" <<<"$v1"; then fail "a key reached the Controller's version"; fi
echo "  ok: cluster edge created from lb-1 - members joined, peers over mTLS, version 1 everywhere, the key pulled member to member"

# The lock: a direct write to a managed domain is refused.
out="$(ctl 2 haproxy apply-config "$WORKDIR/v1.cfg" 2>&1 || true)"
check "the lock refuses a direct apply" "managed by cluster edge" "$out"
out="$(ctl 2 haproxy file-put maps/other.map "$WORKDIR/hosts.map" 2>&1 || true)"
check "the lock refuses a direct file" "managed by cluster edge" "$out"
echo "  ok: the members' locks hold"

# Version 2: VRRP, its template rendered per member.
api PATCH "/api/clusters/$CID" '{"vars": {"lb-1": {"priority": "150"}, "lb-2": {"priority": "100"}, "lb-3": {"priority": "50"}}}' >/dev/null || fail "member variables"
keepalived='global_defs {
    vrrp_version 3
}

track_file haproxy {
    file /run/janus/keepalived/haproxy-health
}

vrrp_instance front {
    state BACKUP
    interface eth1
    virtual_router_id 61
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
        192.168.86.100/24
    }
}
'
v2="$(python3 -c 'import json,sys; print(json.dumps({"files": {"keepalived.conf": sys.argv[1]}, "note": "VRRP"}))' "$keepalived")"
api POST "/api/clusters/$CID/versions" "$v2" >/dev/null || fail "version 2"
api POST "/api/clusters/$CID/apply" '{"version": 2, "managed": ["haproxy", "files", "vrrp"]}' >/dev/null || fail "apply version 2"
wait_for 180 "version 2 applied" idle
err="$(cluster_error)"
[ -z "$err" ] || fail "version 2: $err"
check "lb-2's priority rendered" "priority 100" "$(ctl 2 network vrrp get)"
check "lb-2's peers rendered" "192\.168\.86\.3" "$(ctl 2 network vrrp get)"
master() { grep -Eq '^front +MASTER ' <<<"$(ctl "$1" network vrrp status 2>/dev/null)"; }
wait_for 30 "lb-1 holding the virtual IP" master 1
echo "  ok: version 2's keepalived.conf rendered per member - lb-1 holds 192.168.86.100"

# The balancer hook: a receiver on the host, checking each call's
# signature, writing one line a call: EVENT MEMBER IN-SERVICE...
cat > "$WORKDIR/hook.py" <<'PY'
import hashlib, hmac, json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
SECRET, LOG = sys.argv[2].encode(), sys.argv[3]
class Hook(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers["Content-Length"]))
        want = "sha256=" + hmac.new(SECRET, body, hashlib.sha256).hexdigest()
        call = json.loads(body)
        ok = hmac.compare_digest(want, self.headers.get("X-Janus-Signature-256", "")) and self.headers.get("X-Janus-Event") == call["event"]
        line = "%s %s %s %s\n" % ("signed" if ok else "BADSIG", call["event"], call.get("member") or "-", ",".join(m["name"] for m in call["members"] if m["in_service"]) or "-")
        with open(LOG, "a") as f:
            f.write(line)
        self.send_response(204)
        self.end_headers()
    def log_message(self, *a):
        pass
HTTPServer(("127.0.0.1", int(sys.argv[1])), Hook).serve_forever()
PY
P_HOOK=$((BASE + 50))
python3 "$WORKDIR/hook.py" "$P_HOOK" hook-secret "$WORKDIR/hook.log" &
PID[hook]=$!
hook_calls() { [ -s "$WORKDIR/hook.log" ] && [ "$(wc -l <"$WORKDIR/hook.log")" -ge "$1" ]; }
view="$(api PUT "/api/clusters/$CID/balancer" "{\"url\": \"http://127.0.0.1:$P_HOOK/janus\", \"secret\": \"hook-secret\", \"drain_delay_seconds\": 2}")" || fail "set the balancer hook"
check "the hook's secret not shown" '"has_secret":true' "$view"
if grep -q hook-secret <<<"$view"; then fail "the hook's secret shown: $view"; fi
wait_for 10 "the hook's first call" hook_calls 1
check "the first call: every member in service, signed" "^signed sync - lb-1,lb-2,lb-3$" "$(cat "$WORKDIR/hook.log")"

# Stick tables replicate; a drain through the Controller moves the
# virtual IP to lb-2, which has them - the balancer told first.
curl -s --max-time 5 "http://127.0.0.1:$((BASE + 10))/" >/dev/null
curl -s --max-time 5 "http://127.0.0.1:$((BASE + 10))/" >/dev/null
has_entry() { grep -Eq '10\.0\.2\.2' <<<"$(ctl "$1" haproxy table fe 2>/dev/null)"; }
wait_for 20 "lb-2 learnt lb-1's entry" has_entry 2
api POST "/api/clusters/$CID/members/lb-1/drain" >/dev/null || fail "drain lb-1"
wait_for 90 "the drain" idle
err="$(cluster_error)"
[ -z "$err" ] || fail "drain: $err"
wait_for 20 "lb-2 holding the virtual IP" master 2
master 1 && fail "lb-1 still MASTER after its drain"
has_entry 2 || fail "lb-2 lost the table after the failover"
check "the hook told lb-1 goes" "^signed out lb-1 lb-2,lb-3$" "$(sed -n 2p "$WORKDIR/hook.log")"
# The journal, newest first: told, then drained.
order="$(api GET "/api/clusters/$CID/journal" | python3 -c '
import json, sys
msgs = [e["message"] for e in json.load(sys.stdin)]
told = next(i for i, m in enumerate(msgs) if m.startswith("the balancer was told lb-1 going out"))
drained = next(i for i, m in enumerate(msgs) if m.startswith("lb-1 drained"))
print("told first" if told > drained else "drained first")')"
[ "$order" = "told first" ] || fail "the balancer and lb-1's drain: $order"
api POST "/api/clusters/$CID/members/lb-1/undrain" >/dev/null || fail "undrain lb-1"
wait_for 60 "the undrain" idle
wait_for 30 "lb-1 holding the virtual IP again" master 1
wait_for 10 "the hook told lb-1 is back" hook_calls 3
check "the hook told lb-1 is back" "^signed in lb-1 lb-1,lb-2,lb-3$" "$(sed -n 3p "$WORKDIR/hook.log")"
echo "  ok: a drain moved the virtual IP to lb-2, which had the stick table, the balancer hook told lb-1 goes first (signed); undrained, lb-1 took it back, the hook told"

# A member released by hand drifts: reported, never overwritten.
ctl 3 cluster release haproxy >/dev/null || fail "release on lb-3"
sed 's/timeout client 10s/timeout client 11s/' "$WORKDIR/v1.cfg" > "$WORKDIR/hand.cfg"
out="$(ctl 3 haproxy apply-config "$WORKDIR/hand.cfg" 2>&1)" || fail "apply-config on lb-3 once released: $out"
view="$(api POST "/api/clusters/$CID/check")" || fail "check"
check "lb-3 drifted" "lb-3 has drifted: haproxy" "$view"
check "lb-3 released" "lb-3's users took back haproxy" "$view"
check "lb-3 left as it is" "timeout client 11s" "$(ctl 3 haproxy get-config)"
api POST "/api/clusters/$CID/apply" '{"version": 2}' >/dev/null || fail "apply again"
wait_for 180 "the apply over the drift" idle
check "lb-3 back to the cluster's" "timeout client 10s" "$(ctl 3 haproxy get-config)"
out="$(ctl 3 haproxy apply-config "$WORKDIR/hand.cfg" 2>&1 || true)"
check "lb-3 locked again" "managed by cluster edge" "$out"
view="$(api POST "/api/clusters/$CID/check")" || fail "check"
if grep -q "drifted" <<<"$view"; then fail "still drifted: $view"; fi
echo "  ok: a drift reported, left alone, put back by the next apply - the lock with it"

# A certificate uploaded to the cluster: on every member.
cert="$(python3 -c 'import json,sys; print(json.dumps({"name": "www", "pem": open(sys.argv[1]).read()}))' "$WORKDIR/lb.pem")"
api POST "/api/clusters/$CID/certificates" "$cert" >/dev/null || fail "certificate"
wait_for 90 "the certificate" idle
err="$(cluster_error)"
[ -z "$err" ] || fail "certificate: $err"
for i in $NODES; do
  check "lb-$i has the cluster's certificate" "www" "$(ctl "$i" haproxy cert-list)"
done
echo "  ok: a certificate uploaded to the cluster reached every member"

# lb-3 leaves; then the cluster goes.
api DELETE "/api/clusters/$CID/members/lb-3" >/dev/null || fail "remove lb-3"
wait_for 120 "lb-3 removed" idle
err="$(cluster_error)"
[ -z "$err" ] || fail "remove lb-3: $err"
check "lb-3 on its own" "Cluster: +none" "$(ctl 3 cluster get)"
for i in 1 2; do wait_for 60 "lb-$i with one peer" peers_up "$i" 1; done
wait_for 10 "the hook told lb-3 left" hook_calls 5
check "the hook told lb-3 goes" "^signed out lb-3 lb-1,lb-2$" "$(sed -n 4p "$WORKDIR/hook.log")"
check "then the list without lb-3" "^signed sync - lb-1,lb-2$" "$(sed -n 5p "$WORKDIR/hook.log")"
if grep -q BADSIG "$WORKDIR/hook.log"; then fail "a hook call badly signed: $(cat "$WORKDIR/hook.log")"; fi
api DELETE "/api/clusters/$CID" >/dev/null || fail "delete the cluster"
gone() { ! api GET "/api/clusters/$CID" >/dev/null 2>&1; }
wait_for 60 "the cluster gone" gone
for i in 1 2; do check "lb-$i on its own" "Cluster: +none" "$(ctl "$i" cluster get)"; done
out="$(ctl 2 haproxy apply-config "$WORKDIR/v1.cfg" 2>&1)" || fail "lb-2 unlocked once the cluster went: $out"
echo "  ok: a member removed - the balancer hook told before, then the list without it -, the cluster deleted - the nodes on their own and unlocked"

for i in $NODES; do
  if grep -a "avc:.*denied" "$WORKDIR/$i.log"; then fail "AVC denials on node $i"; fi
done
echo "Controller cluster test PASSED: created, locked, VRRP rendered, failover with tables, balancer hook told, drift reported and put back, certificate pulled, member removed, cluster deleted"
