#!/usr/bin/env bash
# Proves log forwarding to Elasticsearch (internal/logship,
# docs/guide/log-forwarding.md) on a real node - the full disk image
# under real OVMF, SELinux enforcing - against a real cluster: Elastic's
# own image in a container (hack/elasticsearch), security on.
#   1. An API key that may only create documents in logs-janus*, the
#      cluster's HTTPS trusted by its CA's fingerprint (what it prints at
#      first start): `log-forwarding test` stores one record, a wrong
#      fingerprint is refused with the server's own.
#   2. janusd's log, the event log and HAProxy's access logs - formatted
#      as JSON by HAProxy (%{+json}o) - land in their data streams
#      (logs-janus.<source>-default) with ECS fields: @timestamp,
#      message, log.level, host.name and host.id (the node's CA SHA-256),
#      event.action for an event, haproxy.* for an access log.
#   3. The cluster paused: the target fails; once back, what was logged
#      meanwhile is stored - once each, even sent twice.
#   4. A member of a Janus cluster (of one, its CA made here by
#      hack/clusterca): its documents carry ECS's orchestrator.cluster.id
#      and .name, orchestrator.type janus and janus.cluster.member - once
#      it left, none of them.
#   5. No AVC denial.
#
# Usage: hack/qemu-logforward-es-test.sh <disk.img> <janusctl-bin>
set -euo pipefail
# shellcheck source=state-read.sh
. "$(dirname "$0")/state-read.sh"

export PATH="$PATH:/usr/sbin:/sbin"

SRC_DISK="${1:?usage: $0 <disk.img> <janusctl-bin>}"
CTL_BIN="${2:?usage: $0 <disk.img> <janusctl-bin>}"
OFF="${JANUS_TEST_PORT_OFFSET:-0}"
P_HTTP=$((18381 + OFF))
P_GRPC=$((18382 + OFF))
P_ES=$((18383 + OFF))
P_ES_T=$((18384 + OFF))
HOST_FROM_GUEST=10.0.2.2
IMAGE="${ES_IMAGE:-janus-es-test}"
NAME="janus-es-test-$OFF"
PW="janus-test-$RANDOM$RANDOM"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf)" >&2; exit 1; }

WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

LOG="$WORKDIR/boot.log"
fail() {
  echo "Elasticsearch log forwarding test FAILED: $*" >&2
  [ -f "$LOG" ] && { echo "--- console output (last 40 lines) ---" >&2; tail -40 "$LOG" >&2; }
  docker logs "$NAME" 2>&1 | tail -5 | cut -c1-400 >&2 || true
  exit 1
}

# --- the cluster ---
docker build -q -t "$IMAGE" hack/elasticsearch >/dev/null
docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" --network host \
  -e discovery.type=single-node -e ELASTIC_PASSWORD="$PW" -e ES_JAVA_OPTS="-Xms1g -Xmx1g" \
  -e http.port="$P_ES" -e transport.port="$P_ES_T" -e xpack.ml.enabled=false \
  "$IMAGE" >/dev/null
deadline=$((SECONDS + 180))
until docker exec "$NAME" test -s /usr/share/elasticsearch/config/certs/http_ca.crt 2>/dev/null; do
  [ "$SECONDS" -lt "$deadline" ] || fail "Elasticsearch made no HTTP CA"
  sleep 2
done
docker exec "$NAME" cat /usr/share/elasticsearch/config/certs/http_ca.crt > "$WORKDIR/http_ca.crt"
CA_FP="$(openssl x509 -in "$WORKDIR/http_ca.crt" -noout -fingerprint -sha256 | cut -d= -f2)"
es() { curl -s --cacert "$WORKDIR/http_ca.crt" -u "elastic:$PW" -H 'Content-Type: application/json' "$@"; }
# Ready once its own security index answers: the elastic user signs in.
until curl -sf -o /dev/null --cacert "$WORKDIR/http_ca.crt" -u "elastic:$PW" "https://localhost:$P_ES/_security/_authenticate"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "Elasticsearch didn't start"
  sleep 2
done
until es -X POST "https://localhost:$P_ES/_security/api_key" -d '{"name":"janus-node","role_descriptors":{"janus":{"index":[{"names":["logs-janus*"],"privileges":["create_doc","auto_configure"]}]}}}' > "$WORKDIR/key.json" &&
  grep -q '"encoded"' "$WORKDIR/key.json"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "no API key: $(cat "$WORKDIR/key.json")"
  sleep 2
done
API_KEY="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["encoded"])' "$WORKDIR/key.json")"
echo "Elasticsearch $(es "https://localhost:$P_ES/" | python3 -c 'import json,sys; print(json.load(sys.stdin)["version"]["number"])') up, CA SHA-256 $CA_FP"

# search QUERY - the hits of a query string over logs-janus*, as JSON
# lines (the documents' _source).
search() {
  es "https://localhost:$P_ES/logs-janus*/_search?size=100" -d "$(python3 -c 'import json,sys; print(json.dumps({"query": {"query_string": {"query": sys.argv[1]}}}))' "$1")" |
    python3 -c 'import json,sys; d=json.load(sys.stdin); [print(json.dumps(h["_source"])) for h in d.get("hits",{}).get("hits",[])]'
}
# wait_doc DESCRIPTION QUERY [SECONDS] - until a document matches.
wait_doc() {
  local deadline=$((SECONDS + ${3:-30}))
  until [ -n "$(search "$2")" ]; do
    [ "$SECONDS" -lt "$deadline" ] || fail "Elasticsearch never got $1 ($2)"
    sleep 1
  done
}

# --- the node ---
DISK="$WORKDIR/disk.img"
cp --sparse=always "$SRC_DISK" "$DISK"
cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/OVMF_VARS.fd"
qemu-system-x86_64 -accel kvm -accel tcg -m 1024M \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$WORKDIR/OVMF_VARS.fd" \
  -drive file="$DISK",format=raw,if=virtio \
  -display none -monitor none \
  -netdev "user,id=net0,hostfwd=tcp::${P_HTTP}-:8080,hostfwd=tcp::${P_GRPC}-:9505" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG" </dev/null &
QEMU_PID=$!
deadline=$((SECONDS + 150))
until grep -aq "listening on" "$LOG" 2>/dev/null; do
  [ "$SECONDS" -lt "$deadline" ] || fail "janusd never started listening"
  kill -0 "$QEMU_PID" 2>/dev/null || fail "QEMU exited"
  sleep 1
done
sleep 2
state_pki "$DISK" "$WORKDIR" || fail "couldn't extract the node's pki/ from STATE"
ctl() { "$CTL_BIN" -endpoint "127.0.0.1:${P_GRPC}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" "$@"; }
NODE_FP="$(openssl x509 -in "$WORKDIR/ca.crt" -outform der | sha256sum | cut -d' ' -f1)"

# --- 1. the target, tested ---
target() { # target FINGERPRINT - the configuration's JSON
  python3 -c 'import json,sys; print(json.dumps({"labels": {"env": "test"}, "targets": [{"name": "es", "sources": ["janusd", "events", "access", "audit"],
    "elasticsearch": {"url": "https://%s:%s" % (sys.argv[1], sys.argv[2]), "apiKey": sys.argv[3]},
    "tls": {"fingerprint": sys.argv[4], "serverName": "localhost"}}]}))' "$HOST_FROM_GUEST" "$P_ES" "$API_KEY" "$1"
}
target "$NODE_FP" > "$WORKDIR/wrong.json" # another certificate's
if out="$(ctl system log-forwarding test -file "$WORKDIR/wrong.json" es 2>&1)"; then
  fail "a wrong fingerprint was trusted"
fi
grep -q "no certificate the server presented has the SHA-256 fingerprint" <<<"$out" || fail "a wrong fingerprint: $out"
target "$CA_FP" > "$WORKDIR/es.json"
ctl system log-forwarding set "$WORKDIR/es.json" >/dev/null || fail "log-forwarding set refused"
if ctl system log-forwarding get | grep -q "$API_KEY"; then fail "the API key came back"; fi
ctl system log-forwarding test es >/dev/null || fail "test es failed"
wait_doc "the test record" 'message:"log forwarding test from"'
echo "Part 1 OK: an API key limited to logs-janus*, the cluster trusted by its CA's fingerprint - a wrong one refused - the test record stored, the key never shown"

# --- 2. what lands where ---
cat > "$WORKDIR/haproxy.cfg" <<'EOF'
global
    log stdout format raw local0
    stats socket /run/janus/haproxy-admin.sock mode 660 level admin
    chroot /var/empty
    uid 1000
    gid 1000

defaults
    mode http
    log global
    timeout connect 5s
    timeout client 30s
    timeout server 30s

frontend janus-health
    bind *:8080
    log-format "%{+json}o %(client_ip)ci %(method)HM %(path)HP %(status)ST"
    http-request return status 200 content-type text/plain string "ok\n"
EOF
ctl haproxy apply-config "$WORKDIR/haproxy.cfg" >/dev/null || fail "apply-config refused"
ctl system metrics -port 10091 >/dev/null
for _ in $(seq 3); do curl -s -o /dev/null "http://127.0.0.1:${P_HTTP}/es-access-line"; done
wait_doc "janusd's API line" 'message:"api: SystemService/MetricsConfigSet"'
wait_doc "the config applied event" 'event.action:"haproxy.config.applied"'
wait_doc "an access log, its JSON as fields" 'haproxy.path:"/es-access-line"'
doc="$(search 'message:"api: SystemService/MetricsConfigSet"' | head -1)"
python3 -c '
import json, sys
d, fp = json.loads(sys.argv[1]), sys.argv[2]
assert d["data_stream"] == {"type": "logs", "dataset": "janus.janusd", "namespace": "default"}, d["data_stream"]
assert d["log"]["level"] == "notice" and d["service"]["name"] == "janusd", d
assert d["host"]["id"] == fp and d["host"]["name"], d["host"]
assert d["labels"] == {"env": "test"} and d["@timestamp"].endswith("Z"), d
' "$doc" "$NODE_FP" || fail "janusd's document: $doc"
doc="$(search 'haproxy.path:"/es-access-line"' | head -1)"
python3 -c '
import json, sys
d = json.loads(sys.argv[1])
assert d["data_stream"]["dataset"] == "janus.access" and d["haproxy"]["status"] == 200 and d["haproxy"]["method"] == "GET", d
' "$doc" || fail "the access log's document: $doc"
ctl pki generate-client-config -role os:reader -name ci-reader -ttl 1h "$WORKDIR/reader" >/dev/null || fail "generate-client-config failed"
if "$CTL_BIN" -endpoint "127.0.0.1:${P_GRPC}" -ca "$WORKDIR/reader/ca.crt" -cert "$WORKDIR/reader/client.crt" -key "$WORKDIR/reader/client.key" system reboot 2>/dev/null; then
  fail "a reader rebooted the node"
fi
wait_doc "the reader's refused reboot in the audit" 'event.action:"SystemService/Reboot" AND user.name:"ci-reader"'
doc="$(search 'event.action:"SystemService/Reboot" AND user.name:"ci-reader"' | head -1)"
python3 -c '
import json, sys
d = json.loads(sys.argv[1])
assert d["data_stream"]["dataset"] == "janus.audit" and d["event"]["outcome"] == "failure" and d["source"]["ip"] and d["janus"]["audit"]["denied"], d
' "$doc" || fail "the audit document: $doc"
# What it sent took bytes - the gzipped bodies -, through the interface the
# node routes the cluster by (janusctl's BYTES and VIA).
status="$(ctl system log-forwarding status)"
read -r bytes via < <(awk '$1 == "es" {print $4, $7}' <<<"$status")
[ "$bytes" != "0B" ] && [ "$via" = eth0 ] || fail "es: $bytes sent, by ${via:-?} (want eth0): $status"
echo "Part 2 OK: janusd's log, the events, the audit (a reader's refused reboot: user.name, source.ip, event.outcome) and HAProxy's JSON access logs in their data streams, with ECS fields and the node's CA as host.id - $bytes sent through $via"

# --- 3. the cluster away, then back ---
docker pause "$NAME" >/dev/null
ctl system metrics -port 10092 >/dev/null
for _ in $(seq 2); do curl -s -o /dev/null "http://127.0.0.1:${P_HTTP}/es-during-outage"; done
sleep 45
grep -qE "^es +failing " <<<"$(ctl system log-forwarding status)" || fail "the target isn't failing with the cluster paused: $(ctl system log-forwarding status)"
docker unpause "$NAME" >/dev/null
wait_doc "the outage's janusd line" 'message:"exporter: metrics served on :10092/metrics"' 120
wait_doc "the outage's access logs" 'haproxy.path:"/es-during-outage"' 60
wait_doc "the logship.failing event" 'event.action:"logship.failing"' 60
sleep 5
n="$(search 'message:"exporter: metrics served on :10092/metrics"' | wc -l)"
[ "$n" = 1 ] || fail "the outage's line was stored $n times"
n="$(search 'haproxy.path:"/es-during-outage"' | wc -l)"
[ "$n" = 2 ] || fail "the outage's 2 access lines were stored $n times"
echo "Part 3 OK: with the cluster paused the target fails and says so; once back, what was logged meanwhile is stored, once each"

# --- 4. a member of a Janus cluster: its documents say which ---
CLUSTER_ID="$(python3 -c 'import uuid; print(uuid.uuid4())')"
go run ./hack/clusterca new "$WORKDIR/cluster-ca" logs >/dev/null || fail "clusterca new failed"
ctl cluster csr "$WORKDIR/member.csr" >/dev/null || fail "cluster csr failed"
go run ./hack/clusterca sign "$WORKDIR/cluster-ca" "$WORKDIR/member.csr" "$CLUSTER_ID" lb-1 "$WORKDIR/member.crt" || fail "clusterca sign failed"
out="$(ctl cluster set -id "$CLUSTER_ID" -name logs -member lb-1 -ca "$WORKDIR/cluster-ca/ca.crt" -cert "$WORKDIR/member.crt" lb-1=10.0.2.15 2>&1)" ||
  fail "cluster set: $out"
ctl system metrics -port 10093 >/dev/null
wait_doc "janusd's line as a member, by its cluster's name" 'orchestrator.cluster.name:"logs" AND message:"exporter: metrics served on :10093/metrics"'
doc="$(search 'message:"exporter: metrics served on :10093/metrics"' | head -1)"
python3 -c '
import json, sys
d = json.loads(sys.argv[1])
assert d["orchestrator"] == {"type": "janus", "cluster": {"id": sys.argv[2], "name": "logs"}}, d
assert d["janus"]["cluster"] == {"member": "lb-1"}, d
' "$doc" "$CLUSTER_ID" || fail "a member's document: $doc"
n="$(search 'orchestrator.cluster.id:"'"$CLUSTER_ID"'" AND message:"exporter: metrics served on :10091/metrics"' | wc -l)"
[ "$n" = 0 ] || fail "a line sent before the node joined carries the cluster"
out="$(ctl cluster leave 2>&1)" || fail "cluster leave: $out"
ctl system metrics -port 10094 >/dev/null
wait_doc "janusd's line once the node left" 'message:"exporter: metrics served on :10094/metrics"'
doc="$(search 'message:"exporter: metrics served on :10094/metrics"' | head -1)"
python3 -c '
import json, sys
d = json.loads(sys.argv[1])
assert "orchestrator" not in d and "cluster" not in d["janus"], d
' "$doc" || fail "a document once the node left: $doc"
echo "Part 4 OK: a member's documents carry its cluster (orchestrator.cluster.id/name, orchestrator.type janus, janus.cluster.member), searchable by name; once it left, none"

if grep -aq "avc:  denied" "$LOG"; then
  fail "AVC denials: $(grep -a 'avc:  denied' "$LOG" | head -5)"
fi
echo "Elasticsearch log forwarding test passed: a real cluster (security on, an API key limited to logs-janus*, trusted by its CA's fingerprint) got janusd's log, the events and HAProxy's JSON access logs as ECS documents in their data streams, an outage's lines once back and once each, a cluster member's documents with its cluster; no AVC denial"
