#!/usr/bin/env bash
# Proves the Controller managing the nodes' Consul agents (docs/guide/
# consul-controller.md) end to end: a real Consul server, a native
# dashboardd, two enforcing nodes with the consul extension.
#   - the Consul server runs in a container (hack/consul-server), set up
#     as in production: ACLs denying by default, gossip encrypted, the RPC
#     over TLS verified both ways (verify_incoming, verify_outgoing,
#     verify_server_hostname), auto_encrypt for the clients; the
#     Controller's token can write ACLs, read the keyring and the
#     operator's state, write nodes (to deregister one), read services and
#     agents - nothing more;
#   - the nodes and the server share a LAN: a bridge on the host with a
#     tap per node (the gossip goes both ways), each node also on its own
#     user-mode NIC for its first contact;
#   - the Controller is set up through its API: the settings tested, then
#     saved; the datacenter, the servers and the gossip key come from
#     Consul. Each node gets its own token (node identity + janus-nodes),
#     its agent joins (alive), its node metadata carries its labels and
#     its ID, it registers janus, one haproxy-<frontend> per frontend and
#     one haproxy-url per host its hosts.map and its Host ACL route - and
#     its exporters where they really listen, while they run, tag
#     prometheus with metrics_path and scheme: janus-exporter (its main
#     address, then the one it's moved to, absent while off),
#     node-exporter (absent while stopped), haproxy-exporter (a frontend
#     serving HAProxy's prometheus-exporter);
#   - a map entry added at runtime is a URL in the catalog within 60 s;
#   - drained, the node's services are in maintenance; undrained, passing;
#   - a node removed from the Controller: its layer off, its token
#     deleted, its entry deregistered;
#   - no AVC denial on either node.
#
# Needs sudo (the bridge and the taps) and Docker (the server).
#
# Usage: hack/qemu-consul-controller-test.sh <disk.img> <janusctl-bin> <dashboardd-bin>
#   CONSUL_DISCOVERY=1: don't fail on AVC denials - list them (with a UKI
#   built UKI_EXTRA_CMDLINE=sysctl.kernel.printk_ratelimit=0, the domains
#   being measured permissive).
set -euo pipefail
# shellcheck source=state-read.sh
. "$(dirname "$0")/state-read.sh"

export PATH="$PATH:/usr/sbin:/sbin"

usage="usage: $0 <disk.img> <janusctl-bin> <dashboardd-bin>"
SRC_DISK="${1:?$usage}"
CTL_BIN="${2:?$usage}"
DASHBOARDD="${3:?$usage}"
TIMEOUT="${QEMU_CONSUL_TIMEOUT:-120}"
OFFSET="${JANUS_TEST_PORT_OFFSET:-0}"
B="${QEMU_CONSUL_CTL_BASE_PORT:-$((19110 + OFFSET))}"
P_DASH=$((B + 1)) P_REG=$((B + 2)) P_CONSUL=$((B + 3))
# One LAN per runner instance: bridge jcsl<n>, 10.67.<n>.0/24, the host
# (the Consul server) .1, the nodes .11 and .12.
N=$(((OFFSET / 100) % 250))
BR="jcsl$N"
NET="10.67.$N"
SERVER_IP="$NET.1"
NAME="janus-consul-server-$N"
IMAGE="janus-consul-server"
MGMT="$(cat /proc/sys/kernel/random/uuid)"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"

WORKDIR="$(mktemp -d)"
declare -A PID
DASH_PID=""
cleanup() {
  for n in "${!PID[@]}"; do kill "${PID[$n]}" 2>/dev/null || true; done
  [ -n "$DASH_PID" ] && kill "$DASH_PID" 2>/dev/null || true
  for n in "${!PID[@]}"; do wait "${PID[$n]}" 2>/dev/null || true; done
  [ -n "$DASH_PID" ] && wait "$DASH_PID" 2>/dev/null || true
  docker rm -f -v "$NAME" >/dev/null 2>&1 || true
  for t in a b; do sudo ip link del "$BR$t" 2>/dev/null || true; done
  sudo ip link del "$BR" 2>/dev/null || true
  sudo rm -rf "$WORKDIR/consul" # the image's user owns it now
  rm -rf "$WORKDIR"
}
trap cleanup EXIT
fail() {
  echo "Consul Controller test FAILED: $*" >&2
  [ -f "$WORKDIR/dash.log" ] && { echo "--- dashboardd ---" >&2; grep -a -i "consul\|node" "$WORKDIR/dash.log" | tail -40 >&2; }
  echo "--- consul server ---" >&2
  docker logs "$NAME" 2>&1 | tail -30 >&2 || true
  for n in a b; do
    [ -f "$WORKDIR/$n.log" ] && { echo "--- console $n ---" >&2; tail -120 "$WORKDIR/$n.log" >&2; }
  done
  exit 1
}
check() { # check DESCRIPTION PATTERN TEXT
  grep -Eq -- "$2" <<<"$3" || fail "$1 (no match for $2 in: $3)"
}

# --- the LAN: a bridge, a tap per node, the host on it -------------------
for t in a b; do sudo ip link del "$BR$t" 2>/dev/null || true; done
sudo ip link del "$BR" 2>/dev/null || true
sudo ip link add "$BR" type bridge
sudo ip addr add "$SERVER_IP/24" dev "$BR"
sudo ip link set "$BR" up
for t in a b; do
  sudo ip tuntap add dev "$BR$t" mode tap user "$(id -un)"
  sudo ip link set "$BR$t" master "$BR"
  sudo ip link set "$BR$t" up
done

# --- the Consul server ------------------------------------------------------
# The RPC's CA and the server's certificate (server.<dc>.consul, what
# verify_server_hostname checks; 127.0.0.1 for its HTTPS API).
mkdir -p "$WORKDIR/consul"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 -subj "/CN=Consul test CA" \
  -keyout "$WORKDIR/ca.key" -out "$WORKDIR/consul/ca.pem" 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=server.lgs.consul" \
  -keyout "$WORKDIR/consul/server.key" -out "$WORKDIR/server.csr" 2>/dev/null
printf 'subjectAltName=DNS:server.lgs.consul,DNS:localhost,IP:127.0.0.1,IP:%s\nextendedKeyUsage=serverAuth,clientAuth\n' "$SERVER_IP" > "$WORKDIR/server.ext"
openssl x509 -req -in "$WORKDIR/server.csr" -CA "$WORKDIR/consul/ca.pem" -CAkey "$WORKDIR/ca.key" -CAcreateserial \
  -days 2 -extfile "$WORKDIR/server.ext" -out "$WORKDIR/consul/server.pem" 2>/dev/null
docker build -q -t "$IMAGE" "$(dirname "$0")/consul-server" >/dev/null
KEY="$(docker run --rm "$IMAGE" keygen)"
cat > "$WORKDIR/consul/server.hcl" <<EOF
datacenter       = "lgs"
node_name        = "consul-test"
server           = true
bootstrap_expect = 1
data_dir         = "/consul/data"
bind_addr        = "$SERVER_IP"
advertise_addr   = "$SERVER_IP"
client_addr      = "127.0.0.1"
ports {
  http     = -1
  https    = $P_CONSUL
  dns      = -1
  grpc     = -1
  grpc_tls = -1
}
encrypt = "$KEY"
acl {
  enabled                  = true
  default_policy           = "deny"
  enable_token_persistence = true
  tokens {
    initial_management = "$MGMT"
    agent              = "$MGMT"
  }
}
tls {
  defaults {
    ca_file         = "/consul/config/ca.pem"
    cert_file       = "/consul/config/server.pem"
    key_file        = "/consul/config/server.key"
    verify_incoming = true
    verify_outgoing = true
  }
  internal_rpc {
    verify_server_hostname = true
  }
  https {
    verify_incoming = false
  }
}
auto_encrypt {
  allow_tls = true
}
EOF
chmod 0755 "$WORKDIR/consul"
chmod 0644 "$WORKDIR/consul"/*
docker rm -f -v "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" --network host -v "$WORKDIR/consul:/consul/config" "$IMAGE" agent -config-dir=/consul/config >/dev/null
CA="$WORKDIR/consul/ca.pem"
consul_api() { # consul_api METHOD PATH [BODY]: with the management token
  curl -s -m 10 --cacert "$CA" -H "X-Consul-Token: $MGMT" -X "$1" "https://127.0.0.1:$P_CONSUL$2" ${3:+-d "$3"}
}
deadline=$((SECONDS + 60))
until [ "$(consul_api GET /v1/status/leader 2>/dev/null)" = "\"$SERVER_IP:8300\"" ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the Consul server never elected itself"
  sleep 1
done
# The Controller's token, as docs/guide/consul-controller.md has it made.
python3 -c 'import json; print(json.dumps({"Name": "janus-controller", "Rules": """acl = "write"
keyring = "read"
operator = "read"
node_prefix "" { policy = "write" }
service_prefix "" { policy = "read" }
agent_prefix "" { policy = "read" }
"""}))' > "$WORKDIR/policy-in.json"
consul_api PUT /v1/acl/policy "@$WORKDIR/policy-in.json" > "$WORKDIR/policy.json"
check "the Controller's policy" '"ID"' "$(cat "$WORKDIR/policy.json")"
TOKEN="$(consul_api PUT /v1/acl/token '{"Description": "janus-controller", "Policies": [{"Name": "janus-controller"}]}' |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["SecretID"])')" || fail "the Controller's token"
echo "  ok: the Consul server runs (ACLs deny by default, gossip encrypted, RPC over TLS both ways), the Controller's token made"

# --- the nodes ----------------------------------------------------------------
# node NAME INDEX: http on B+10*i, API +1; eth1 on the bridge.
node() {
  local n=$1 i=$2
  cp --sparse=always "$SRC_DISK" "$WORKDIR/$n.img"
  cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/$n.vars"
  qemu-system-x86_64 -accel kvm -accel tcg \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$WORKDIR/$n.vars" \
    -drive file="$WORKDIR/$n.img",format=raw,if=virtio \
    -nographic -display none -m 1G \
    -netdev "user,id=net0,hostfwd=tcp::$((B + 10 * i))-:8080,hostfwd=tcp::$((B + 10 * i + 1))-:9505" \
    -device "virtio-net-pci,netdev=net0,mac=52:54:00:ca:00:0$i" \
    -netdev "tap,id=lan,ifname=$BR$n,script=no,downscript=no" \
    -device "virtio-net-pci,netdev=lan,mac=52:54:00:cb:00:0$i" \
    -serial file:"$WORKDIR/$n.log" </dev/null &
  PID[$n]=$!
}
idx() { [ "$1" = a ] && echo 1 || echo 2; }
node a 1
node b 2
for n in a b; do
  i=$(idx $n)
  deadline=$((SECONDS + TIMEOUT))
  until [ "$(curl -s -m 3 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$((B + 10 * i))/" || true)" = 200 ] &&
    grep -aq "listening on .* (mTLS required)" "$WORKDIR/$n.log"; do
    [ "$SECONDS" -lt "$deadline" ] || fail "node $n never came up"
    sleep 1
  done
done
sleep 2
for n in a b; do
  state_pki "$WORKDIR/$n.img" "$WORKDIR/$n" || fail "couldn't extract node $n's pki/ from STATE"
done
ctl() { # ctl NODE args...: over the user-mode NIC
  local n=$1 i
  shift
  i=$(idx $n)
  "$CTL_BIN" -endpoint "127.0.0.1:$((B + 10 * i + 1))" -ca "$WORKDIR/$n/ca.crt" -cert "$WORKDIR/$n/admin.crt" -key "$WORKDIR/$n/admin.key" "$@"
}
for n in a b; do
  i=$(idx $n)
  cat > "$WORKDIR/$n-net.json" <<EOF
{"interfaces": [
  {"name": "eth0", "mac": "52:54:00:ca:00:0$i", "mode": "ADDRESSING_MODE_DHCP"},
  {"name": "eth1", "mac": "52:54:00:cb:00:0$i", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["$NET.1$i/24"]}
]}
EOF
  out="$(ctl $n network apply "$WORKDIR/$n-net.json" 2>&1)" || fail "network apply on $n: $out"
done
for n in a b; do
  i=$(idx $n)
  deadline=$((SECONDS + 30))
  until timeout 3 bash -c "</dev/tcp/$NET.1$i/9505" 2>/dev/null; do # mTLS: a TCP connection is all it takes
    [ "$SECONDS" -lt "$deadline" ] || fail "node $n isn't reachable on the LAN at $NET.1$i"
    sleep 1
  done
done
out="$(ctl a network consul status)"
check "the agent waits for a configuration" "Service: *waiting" "$out"
echo "  ok: both nodes up, on the LAN with the server ($NET.11, $NET.12)"

# A's sites: a hosts.map read by the Host header, a Host ACL.
printf 'www.example.test be_www\nshop.example.test be_www\n*.wild.example.test be_www\n10.0.0.1 be_www\n' > "$WORKDIR/hosts.map"
out="$(ctl a haproxy file-put hosts.map "$WORKDIR/hosts.map" 2>&1)" || fail "hosts.map: $out"
ctl a haproxy get-config > "$WORKDIR/a-haproxy.cfg"
cat >> "$WORKDIR/a-haproxy.cfg" <<'EOF'

frontend fe_sites
    mode http
    bind :8081
    acl host_status hdr(host) -i status.example.test
    use_backend be_www if host_status
    use_backend %[req.hdr(host),lower,map(/etc/haproxy/files/hosts.map,be_default)]
    default_backend be_default

frontend fe_stats
    mode http
    bind :8405
    http-request use-service prometheus-exporter if { path /metrics }
    http-request return status 404

backend be_www
    mode http
    http-request return status 200 content-type text/plain string "www\n"

backend be_default
    mode http
    http-request return status 404 content-type text/plain string "unknown\n"
EOF
out="$(ctl a haproxy apply-config "$WORKDIR/a-haproxy.cfg" 2>&1)" || fail "A's haproxy.cfg: $out"
echo "  ok: A serves sites from a hosts.map and a Host ACL"

# --- the Controller ---------------------------------------------------------------
"$DASHBOARDD" -addr "127.0.0.1:$P_DASH" -register-addr "127.0.0.1:$P_REG" -data-dir "$WORKDIR/data" > "$WORKDIR/dash.log" 2>&1 &
DASH_PID=$!
API="https://127.0.0.1:$P_DASH"
for _ in $(seq 30); do curl -sk -o /dev/null "$API/api/auth/status" && break; sleep 1; done
JAR="$WORKDIR/jar"
curl -sk -c "$JAR" -o /dev/null -X POST "$API/api/auth/setup" -H 'Content-Type: application/json' -d '{"password":"consul-test-admin-pw"}'
curl -sk -b "$JAR" -X POST "$API/api/auth/mfa/totp/setup" -o "$WORKDIR/totp.json"
SECRET="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["secret"])' "$WORKDIR/totp.json")"
curl -sk -b "$JAR" -o /dev/null -X POST "$API/api/auth/mfa/totp/enable" -H 'Content-Type: application/json' \
  -d "{\"code\":\"$(python3 "$(dirname "$0")/totp.py" "$SECRET")\"}"
api() { curl -sk -b "$JAR" -H 'Content-Type: application/json' "$@"; }
declare -A ID
for n in a b; do
  i=$(idx $n)
  openssl pkcs12 -export -inkey "$WORKDIR/$n/admin.key" -in "$WORKDIR/$n/admin.crt" -certfile "$WORKDIR/$n/ca.crt" \
    -out "$WORKDIR/$n/admin.pfx" -passout pass:t >/dev/null 2>&1
  ID[$n]="$(curl -sk -b "$JAR" -X POST "$API/api/nodes" -F "name=node-$n" -F "address=$NET.1$i:9505" -F "pfx_password=t" \
    -F "pfx=@$WORKDIR/$n/admin.pfx;type=application/x-pkcs12" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')" ||
    fail "add node $n"
done
out="$(api -X PATCH "$API/api/nodes/${ID[a]}" -d '{"labels": {"dc": "lgs", "room": "lab"}}')"
check "A's labels" '"dc": ?"lgs"' "$out"
echo "  ok: the Controller runs, both nodes added (A labelled dc=lgs, room=lab)"

ca_json="$(python3 -c 'import json,sys; print(json.dumps(open(sys.argv[1]).read()))' "$CA")"
settings="{\"enabled\": true, \"address\": \"https://127.0.0.1:$P_CONSUL\", \"tls\": {\"ca_pem\": $ca_json}, \"token\": \"$TOKEN\",
  \"rpc_ca_pem\": $ca_json, \"selector\": \"\", \"services\": {\"janus\": true, \"exporters\": true, \"frontends\": true, \"urls\": true},
  \"node_meta_from_labels\": true}"
out="$(api -X POST "$API/api/consul/test" -d "$settings")"
check "the settings test" '"ok": ?true' "$out"
check "the test read the datacenter" 'datacenter lgs' "$out"
check "the test read the gossip key" 'the nodes encrypt their gossip with it' "$out"
out="$(api -X PUT "$API/api/consul" -d "$settings")"
check "the settings saved" '"has_token": ?true' "$out"
echo "  ok: the settings tested (datacenter, servers, gossip key, the token's rights) and saved"

# --- tokens, members, metadata, services ------------------------------------------
members() { consul_api GET /v1/agent/members | python3 -c 'import json,sys; print(" ".join(sorted("%s=%d" % (m["Name"], m["Status"]) for m in json.load(sys.stdin))))'; }
deadline=$((SECONDS + 180))
until out="$(members)" && grep -q "node-a=1" <<<"$out" && grep -q "node-b=1" <<<"$out"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the nodes never joined: members $out; Controller $(api "$API/api/consul/nodes")"
  sleep 3
done
echo "  ok: both agents joined the server's LAN pool, alive ($out)"

consul_api GET /v1/acl/tokens > "$WORKDIR/tokens.json"
python3 - "$WORKDIR/tokens.json" "${ID[a]}" "${ID[b]}" <<'PY' || fail "the nodes' tokens: $(cat "$WORKDIR/tokens.json")"
import json, sys
toks, a, b = json.load(open(sys.argv[1])), sys.argv[2], sys.argv[3]
for name, nid in (("node-a", a), ("node-b", b)):
    t = [t for t in toks if any(ni["NodeName"] == name and ni["Datacenter"] == "lgs" for ni in t.get("NodeIdentities") or [])]
    assert len(t) == 1, (name, t)
    assert [p["Name"] for p in t[0]["Policies"]] == ["janus-nodes"], t[0]
    assert t[0]["Description"].startswith("janus node %s (%s) - Controller " % (name, nid)), t[0]["Description"]
PY
echo "  ok: one token per node - its node identity and janus-nodes, described by its name and ID"

consul_api GET /v1/catalog/node/node-a > "$WORKDIR/node-a.json"
python3 - "$WORKDIR/node-a.json" "${ID[a]}" "$NET.11" <<'PY' || fail "A's node metadata: $(cat "$WORKDIR/node-a.json")"
import json, sys
n, nid, addr = json.load(open(sys.argv[1]))["Node"], sys.argv[2], sys.argv[3]
assert n["Address"] == addr, n
m = n["Meta"]
assert m["dc"] == "lgs" and m["room"] == "lab" and m["janus_node_id"] == nid, m
PY
echo "  ok: A's node metadata: its labels (dc=lgs, room=lab) and its ID; its address the LAN's"

services() { # services NODE: its services in the catalog, "ID name" per line
  consul_api GET "/v1/catalog/node-services/$1" | python3 -c 'import json,sys; [print(s["ID"], s["Service"]) for s in (json.load(sys.stdin) or {}).get("Services") or []]'
}
deadline=$((SECONDS + 90))
until out="$(services node-a)" && grep -q "^janus:url:fe_sites:www.example.test haproxy-url" <<<"$out"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "A's services never all came: $out; node: $(ctl a network consul managed 2>&1)"
  sleep 3
done
for want in "janus:janus janus" "janus:janus-exporter janus-exporter" "janus:node-exporter node-exporter" \
  "janus:haproxy-exporter:fe_stats haproxy-exporter" "janus:haproxy:fe_sites haproxy-fe-sites" \
  "janus:url:fe_sites:www.example.test haproxy-url" "janus:url:fe_sites:shop.example.test haproxy-url" \
  "janus:url:fe_sites:status.example.test haproxy-url"; do
  grep -qx "$want" <<<"$out" || fail "A's catalog has no $want: $out"
done
if grep -q "wild\|10.0.0.1" <<<"$out"; then fail "a wildcard or an IP became a URL: $out"; fi
consul_api GET /v1/catalog/service/haproxy-url > "$WORKDIR/urls.json"
python3 - "$WORKDIR/urls.json" "$NET.11" <<'PY' || fail "the URLs' metadata: $(cat "$WORKDIR/urls.json")"
import json, sys
urls = {s["ServiceID"]: s for s in json.load(open(sys.argv[1]))}
www = urls["janus:url:fe_sites:www.example.test"]
assert www["ServiceMeta"] == {"url": "http://www.example.test:8081", "frontend": "fe_sites", "backend": "be_www",
                              "source": "map:/etc/haproxy/files/hosts.map"}, www["ServiceMeta"]
assert www["ServiceAddress"] == sys.argv[2] and www["ServicePort"] == 8081, www
assert urls["janus:url:fe_sites:status.example.test"]["ServiceMeta"]["source"] == "acl"
PY
# The exporters: where they really listen - the Janus and node exporters
# on the main interface (eth0 here, the default route's), HAProxy's on
# fe_stats's bind (every address: the node's) -, for one Prometheus job.
exporter() { # exporter SERVICE: "address:port metrics_path scheme tags" of A's instances
  consul_api GET "/v1/catalog/service/$1" | python3 -c '
import json, sys
for s in json.load(sys.stdin):
    if s["Node"] == "node-a":
        print("%s:%d %s %s %s" % (s["ServiceAddress"], s["ServicePort"], s["ServiceMeta"].get("metrics_path"), s["ServiceMeta"].get("scheme"), ",".join(s["ServiceTags"])))'
}
MAIN_A="$(ctl a system metrics | sed -n 's|^Exporter: http://\([0-9.]*\):10056/metrics.*|\1|p')"
[ -n "$MAIN_A" ] || fail "A's exporter address: $(ctl a system metrics)"
check "janus-exporter where it listens" "^$MAIN_A:10056 /metrics http janus,prometheus$" "$(exporter janus-exporter)"
check "node-exporter where it listens" "^$MAIN_A:9100 /metrics http janus,prometheus$" "$(exporter node-exporter)"
check "haproxy-exporter on fe_stats" "^$NET.11:8405 /metrics http haproxy,prometheus$" "$(exporter haproxy-exporter)"
health() { # health NODE SERVICE-ID: its checks' worst status
  consul_api GET "/v1/health/node/$1" | python3 -c '
import json, sys
rank = {"passing": 1, "warning": 2, "critical": 3}
sid = sys.argv[1]
cs = [c for c in json.load(sys.stdin) if c["ServiceID"] == sid]
if any(c["CheckID"].startswith("_service_maintenance:") for c in cs):
    print("maintenance")
else:
    print(max((c["Status"] for c in cs), key=lambda s: rank.get(s, 0), default="none"))' "$2"
}
deadline=$((SECONDS + 60))
until [ "$(health node-a janus:janus)" = passing ] && [ "$(health node-a janus:haproxy:fe_sites)" = passing ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "A's checks never passed: janus $(health node-a janus:janus), fe_sites $(health node-a janus:haproxy:fe_sites)"
  sleep 2
done
services node-b | grep -qx "janus:janus janus" || fail "B registered no janus: $(services node-b)"
out="$(ctl a network consul managed)"
check "janusctl shows the managed layer" "^Managed by: janus-controller " "$out"
check "the token never comes back" "^ACL token: +set" "$out"
check "the services registered" "^Services: +[0-9]+ registered" "$out"
out="$(ctl a network consul services)"
check "janusctl lists the agent's services" "^haproxy-url +janus:url:fe_sites:www.example.test +$NET.11:8081 +haproxy,url,http +http://www.example.test:8081 +- +Janus$" "$out"
check "janusctl shows an exporter's path" "^haproxy-exporter +janus:haproxy-exporter:fe_stats .* http /metrics, frontend fe_stats +passing +Janus$" "$out"
api -X POST "$API/api/consul/run" >/dev/null
deadline=$((SECONDS + 30))
until api "$API/api/consul/nodes" > "$WORKDIR/nodes.json" && python3 - "$WORKDIR/nodes.json" <<'PY'; do
import json, sys
a = [n for n in json.load(open(sys.argv[1])) if n["name"] == "node-a"][0]
sys.exit(0 if a["state"] == "applied" and a.get("member") == "alive" and a["urls"] >= 3 and a["services"] > a["urls"] else 1)
PY
  [ "$SECONDS" -lt "$deadline" ] || fail "the Controller's view of A: $(cat "$WORKDIR/nodes.json")"
  sleep 2
done
deadline=$((SECONDS + 60))
until [ "$(health node-a janus:janus-exporter)" = passing ] && [ "$(health node-a janus:node-exporter)" = passing ] && [ "$(health node-a janus:haproxy-exporter:fe_stats)" = passing ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "A's exporters' checks: $(health node-a janus:janus-exporter) $(health node-a janus:node-exporter) $(health node-a janus:haproxy-exporter:fe_stats)"
  sleep 2
done
echo "  ok: A registered janus, haproxy-fe-sites and a haproxy-url per host (the map's, the ACL's - no wildcard, no IP), its exporters where they listen ($MAIN_A:10056, $MAIN_A:9100, $NET.11:8405, tag prometheus); their checks pass; B its own"

# --- the exporters follow ---------------------------------------------------------
out="$(ctl a system metrics -address "$NET.11" 2>&1)" || fail "move A's exporter: $out"
deadline=$((SECONDS + 30))
until [ "$(exporter janus-exporter | cut -d' ' -f1)" = "$NET.11:10056" ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the exporter moved, its service didn't: $(exporter janus-exporter)"
  sleep 1
done
out="$(ctl a system metrics -disable 2>&1)" || fail "turn A's exporter off: $out"
deadline=$((SECONDS + 30))
until [ -z "$(exporter janus-exporter)" ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the exporter off, its service stayed: $(exporter janus-exporter)"
  sleep 1
done
out="$(ctl a system metrics -enable -address auto 2>&1)" || fail "turn A's exporter on: $out"
out="$(ctl a system service stop prometheus-node-exporter 2>&1)" || fail "stop node_exporter: $out"
deadline=$((SECONDS + 30))
until [ -z "$(exporter node-exporter)" ] && [ "$(exporter janus-exporter | cut -d' ' -f1)" = "$MAIN_A:10056" ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "node_exporter stopped: $(exporter node-exporter); janus-exporter back: $(exporter janus-exporter)"
  sleep 1
done
out="$(ctl a system service start prometheus-node-exporter 2>&1)" || fail "start node_exporter: $out"
deadline=$((SECONDS + 30))
until [ -n "$(exporter node-exporter)" ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "node_exporter started again, its service didn't come back"
  sleep 1
done
echo "  ok: the exporter moved to $NET.11: its service followed; off: gone; node_exporter stopped: gone, started: back"

# --- a map entry added at runtime -------------------------------------------------
t0=$SECONDS
out="$(ctl a haproxy map-set /etc/haproxy/files/hosts.map new.example.test be_www 2>&1)" || fail "map-set: $out"
until services node-a | grep -qx "janus:url:fe_sites:new.example.test haproxy-url"; do
  [ $((SECONDS - t0)) -lt 60 ] || fail "the new host never became a URL: $(services node-a)"
  sleep 1
done
echo "  ok: a host added to the map at runtime is a URL in the catalog $((SECONDS - t0)) s later"

# --- drain, undrain ------------------------------------------------------------------
out="$(ctl a cluster drain -timeout 5 2>&1)" || true
deadline=$((SECONDS + 30))
until [ "$(health node-a janus:janus)" = maintenance ] && [ "$(health node-a janus:url:fe_sites:www.example.test)" = maintenance ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "drained, A's services aren't in maintenance: $(health node-a janus:janus) ($out)"
  sleep 1
done
echo "  ok: drained, A's services are in maintenance"
ctl a cluster undrain >/dev/null 2>&1 || fail "undrain"
deadline=$((SECONDS + 30))
until [ "$(health node-a janus:janus)" = passing ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "undrained, A's janus isn't passing: $(health node-a janus:janus)"
  sleep 1
done
echo "  ok: undrained, they pass again"

# --- a node removed ---------------------------------------------------------------------
acc_b="$(python3 -c '
import json, sys
for t in json.load(open(sys.argv[1])):
    if any(ni["NodeName"] == "node-b" for ni in t.get("NodeIdentities") or []):
        print(t["AccessorID"])' "$WORKDIR/tokens.json")"
api -X DELETE "$API/api/nodes/${ID[b]}" >/dev/null
deadline=$((SECONDS + 60))
until [ "$(consul_api GET /v1/catalog/node/node-b)" = "null" ] && consul_api GET "/v1/acl/token/$acc_b" | grep -q "not found"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "B removed: token $(consul_api GET "/v1/acl/token/$acc_b"), catalog $(consul_api GET /v1/catalog/node/node-b)"
  sleep 2
done
out="$(ctl b network consul managed)"
check "B's layer is off" "^Managed: +no" "$out"
echo "  ok: B removed from the Controller: its layer off, its token deleted, its entry deregistered"

denials="$(cat "$WORKDIR"/a.log "$WORKDIR"/b.log | grep -a "avc:.*denied" || true)"
if [ -n "$denials" ]; then
  if [ "${CONSUL_DISCOVERY:-}" = 1 ]; then
    echo "--- AVC denials (discovery) ---"
    sed -E 's/.*avc:  denied  //; s/pid=[0-9]+ //; s/ino=[0-9]+ //; s/ permissive=[01]//' <<<"$denials" | sort | uniq -c
  else
    echo "$denials" >&2
    fail "AVC denials under enforcing"
  fi
fi
echo "Consul Controller test OK: the Controller made each node's token and its layer from what Consul says, the nodes joined and registered their services and URLs, a map change followed, drain meant maintenance, a node removed was cleaned up - zero AVC denials"
