#!/usr/bin/env bash
# Proves log forwarding (internal/logship, docs/guide/log-forwarding.md)
# on a real node - the full disk image (with its LOGS partition) under
# real OVMF, SELinux enforcing - against a real syslog server: Debian's
# rsyslog in a container (hack/rsyslog), on UDP, TCP and TLS.
#   1. Three targets through the API: UDP RFC 5424 (janusd, events), TCP
#      RFC 3164 with LF framing (HAProxy's messages and access logs,
#      the kernel), TLS RFC 5424 with octet counting and a pinned CA
#      (the defaults: all but the access logs). Each gets what it takes,
#      with the node's hostname, the right facility and severity;
#      `log-forwarding test` sends one record and says when it can't.
#   2. The server stopped: the TCP and TLS targets fail (the event log
#      says so), lines logged meanwhile reach them once it's back - read
#      from the LOGS partition behind their cursors.
#   3. After a reboot, the new boot's kernel messages reach the TLS
#      target - all of them, under the node's hostname: each target goes
#      on from where it was.
#   4. HAProxy's own `log` lines, without janusd: UDP straight to the
#      server, and a ring with a TLS server (the CA an HAProxy file).
#   5. The audit: an admin's change and a reader's refused reboot, kept
#      across the reboot (system audit, admins only), sent as authpriv.
#   6. Secrets never come back (a client key), no AVC denial anywhere.
#
# QEMU_LOGFORWARD_WITHOUT_LOGS=1: the same on a node installed before the
# LOGS partition existed (partition 7 taken out of the test's copy) -
# every node updated to a release with log forwarding: the outage's lines
# and the new boot's kernel messages come from memory.
#
# Usage: hack/qemu-logforward-test.sh <disk.img> <janusctl-bin>
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

SRC_DISK="${1:?usage: $0 <disk.img> <janusctl-bin>}"
CTL_BIN="${2:?usage: $0 <disk.img> <janusctl-bin>}"
OFF="${JANUS_TEST_PORT_OFFSET:-0}"
P_HTTP=$((18371 + OFF))
P_GRPC=$((18372 + OFF))
P_UDP=$((18373 + OFF))
P_TCP=$((18374 + OFF))
P_TLS=$((18375 + OFF))
HOST_FROM_GUEST=10.0.2.2
IMAGE="${RSYSLOG_IMAGE:-janus-rsyslog-test}"
NAME="janus-rsyslog-test-$OFF"

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
ALL_LOGS="$WORKDIR/all-boots.log"
OUT="$WORKDIR/out"
fail() {
  echo "Log forwarding test FAILED: $*" >&2
  [ -f "$LOG" ] && { echo "--- console output (last 60 lines) ---" >&2; tail -60 "$LOG" >&2; }
  [ -f "$OUT/messages.jsonl" ] && { echo "--- rsyslog received (last 20) ---" >&2; tail -20 "$OUT/messages.jsonl" >&2; }
  exit 1
}

# --- the syslog server ---
docker build -q -t "$IMAGE" hack/rsyslog >/dev/null
CONF="$WORKDIR/rsyslog"
mkdir -p "$CONF" "$OUT"
chmod 777 "$OUT"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=Log test CA" \
  -keyout "$CONF/ca.key" -out "$CONF/ca.pem" >/dev/null 2>&1
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=logs.test" \
  -keyout "$CONF/server.key" -out "$CONF/server.csr" >/dev/null 2>&1
printf 'subjectAltName=DNS:logs.test\nextendedKeyUsage=serverAuth\n' > "$CONF/ext"
openssl x509 -req -in "$CONF/server.csr" -CA "$CONF/ca.pem" -CAkey "$CONF/ca.key" -CAcreateserial -days 1 \
  -extfile "$CONF/ext" -out "$CONF/server.pem" >/dev/null 2>&1
chmod 644 "$CONF"/*
sed -e "s/@UDP@/$P_UDP/; s/@TCP@/$P_TCP/; s/@TLS@/$P_TLS/" hack/rsyslog/rsyslog.conf.in > "$CONF/rsyslog.conf"
start_rsyslog() {
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  docker run -d --name "$NAME" --network host -v "$CONF:/etc/rsyslog-test:ro" -v "$OUT:/out" "$IMAGE" >/dev/null
  local deadline=$((SECONDS + 30))
  until ss -ltn | grep -q ":$P_TLS "; do
    [ "$SECONDS" -lt "$deadline" ] || fail "rsyslog didn't listen: $(docker logs "$NAME" 2>&1 | tail -5)"
    sleep 0.5
  done
}
start_rsyslog

# received FILTER - the JSON lines rsyslog wrote that the jq-less python
# filter (an expression on m) keeps, one per line.
received() {
  python3 - "$OUT/messages.jsonl" "$1" <<'EOF'
import json, sys
path, expr = sys.argv[1], sys.argv[2]
try:
    lines = open(path, encoding="utf-8", errors="replace").read().splitlines()
except FileNotFoundError:
    lines = []
for l in lines:
    try:
        m = json.loads(l)
    except ValueError:
        continue
    if eval(expr, {}, {"m": m}):
        print(l)
EOF
}

# wait_received DESCRIPTION FILTER [SECONDS] - until rsyslog has one.
wait_received() {
  local deadline=$((SECONDS + ${3:-30}))
  until [ -n "$(received "$2")" ]; do
    [ "$SECONDS" -lt "$deadline" ] || fail "rsyslog never got $1"
    sleep 1
  done
}

# --- the node ---
DISK="$WORKDIR/disk.img"
cp --sparse=always "$SRC_DISK" "$DISK"
WITHOUT_LOGS="${QEMU_LOGFORWARD_WITHOUT_LOGS:-}"
if [ "$WITHOUT_LOGS" = 1 ]; then
  sgdisk -d 7 "$DISK" >/dev/null || fail "couldn't take the LOGS partition out"
fi
cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/OVMF_VARS.fd"
start_qemu() {
  [ -f "$LOG" ] && cat "$LOG" >> "$ALL_LOGS"
  : > "$LOG"
  qemu-system-x86_64 -accel kvm -accel tcg -m 1024M \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$WORKDIR/OVMF_VARS.fd" \
    -drive file="$DISK",format=raw,if=virtio \
    -display none -monitor none \
    -netdev "user,id=net0,hostfwd=tcp::${P_HTTP}-:8080,hostfwd=tcp::${P_GRPC}-:9505" \
    -device virtio-net-pci,netdev=net0 \
    -serial file:"$LOG" </dev/null &
  QEMU_PID=$!
}
wait_janusd() {
  local deadline=$((SECONDS + 150))
  until [ "$(grep -ac "listening on" "$LOG" 2>/dev/null || true)" -ge "$1" ]; do
    [ "$SECONDS" -lt "$deadline" ] || fail "janusd never started listening (#$1)"
    kill -0 "$QEMU_PID" 2>/dev/null || fail "QEMU exited"
    sleep 1
  done
  sleep 2
}
start_qemu
wait_janusd 1

STATE_START_SECTOR="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
STATE_SIZE_SECTORS="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
dd if="$DISK" of="$WORKDIR/state.img" bs=512 skip="$STATE_START_SECTOR" count="$STATE_SIZE_SECTORS" status=none
for f in ca.crt admin.crt admin.key; do
  debugfs -R "dump pki/$f $WORKDIR/$f" "$WORKDIR/state.img" >/dev/null 2>&1
  [ -s "$WORKDIR/$f" ] || fail "couldn't extract pki/$f from STATE"
done
ctl() { "$CTL_BIN" -endpoint "127.0.0.1:${P_GRPC}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" "$@"; }

# A client certificate for the TLS target - rsyslog doesn't ask for it,
# but its key is a secret the API must never give back.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=node client" \
  -keyout "$WORKDIR/client.key" -out "$WORKDIR/client.pem" >/dev/null 2>&1
python3 - "$WORKDIR/forwarding.json" "$CONF/ca.pem" "$WORKDIR/client.pem" "$WORKDIR/client.key" "$HOST_FROM_GUEST" "$P_UDP" "$P_TCP" "$P_TLS" <<'EOF'
import json, sys
out, ca, cert, key, host, pu, pt, ps = sys.argv[1:]
cfg = {"labels": {"env": "test"}, "targets": [
    {"name": "udp", "sources": ["janusd", "events"], "syslog": {"address": f"{host}:{pu}", "transport": "udp"}},
    {"name": "tcp", "sources": ["haproxy", "access", "kernel"], "syslog": {"address": f"{host}:{pt}", "transport": "tcp", "format": "rfc3164"}},
    {"name": "tls", "syslog": {"address": f"{host}:{ps}", "transport": "tls"},
     "tls": {"caPem": open(ca).read(), "serverName": "logs.test", "clientCertPem": open(cert).read(), "clientKeyPem": open(key).read()}},
]}
json.dump(cfg, open(out, "w"))
EOF
ctl system log-forwarding set "$WORKDIR/forwarding.json" > "$WORKDIR/set.out" || fail "log-forwarding set refused: $(cat "$WORKDIR/set.out")"
ctl system log-forwarding get > "$WORKDIR/get.json"
if grep -q "PRIVATE KEY" "$WORKDIR/get.json"; then fail "the client key came back"; fi
python3 -c 'import json,sys; t={x["name"]: x for x in json.load(open(sys.argv[1]))["targets"]}; assert t["tcp"]["syslog"]["framing"] == "lf" and t["udp"]["syslog"]["facility"] == "local0" and t["tls"]["syslog"]["address"].endswith(":" + sys.argv[2]), t' "$WORKDIR/get.json" "$P_TLS" ||
  fail "the defaults weren't filled in: $(cat "$WORKDIR/get.json")"
ctl system log-forwarding test tls >/dev/null || fail "test tls failed"
wait_received "the test record over TLS" 'm["input"] == "tls" and "log forwarding test from" in m["msg"]'

# --- 1. what each target takes ---
# janusd's own log and an event (UDP); HAProxy's warning, its access logs
# (TCP, RFC 3164).
cat > "$WORKDIR/haproxy.cfg" <<'EOF'
global
    log stdout format raw local0
    stats socket /run/janus/haproxy-admin.sock mode 660 level admin
    chroot /var/empty
    uid 1000
    gid 1000

defaults
    mode http
    timeout connect 5s
    timeout client 30s
    timeout server 30s

frontend janus-health
    bind *:8080
    log global
    option httplog
    http-request return status 200 content-type text/plain string "ok\n"

frontend unlogged
    bind *:8081
    option httplog
EOF
ctl haproxy apply-config "$WORKDIR/haproxy.cfg" >/dev/null || fail "apply-config refused"
ctl system metrics -port 10081 >/dev/null || fail "MetricsConfigSet failed"
for _ in $(seq 5); do curl -s -o /dev/null "http://127.0.0.1:${P_HTTP}/logfwd-access-line"; done

wait_received "janusd's API line over UDP" 'm["input"] == "udp" and m["app"] == "janusd" and "api: SystemService/MetricsConfigSet: admin" in m["msg"]'
wait_received "the haproxy.config.applied event over UDP" 'm["input"] == "udp" and m["msgid"] == "events" and m["msg"].startswith("haproxy.config.applied")'
wait_received "HAProxy's warning over TCP" 'm["input"] == "tcp" and m["app"] == "haproxy" and "[WARNING]" in m["msg"] and m["severity"] == "4"'
wait_received "the access logs over TCP" 'm["input"] == "tcp" and m["app"] == "haproxy" and "/logfwd-access-line" in m["msg"]'
[ -z "$(received 'm["input"] == "tls" and "/logfwd-access-line" in m["msg"]')" ] || fail "the TLS target took the access logs (not one of its default sources)"
[ -z "$(received 'm["input"] == "udp" and m["app"] == "haproxy"')" ] || fail "the UDP target took HAProxy's lines"
wait_received "janusd's line over TLS" 'm["input"] == "tls" and m["app"] == "janusd" and m["protocol"] == "1" and "exporter: metrics served on :10081" in m["msg"]'
line="$(received 'm["input"] == "udp" and "api: SystemService/MetricsConfigSet" in m["msg"]' | head -1)"
python3 -c 'import json,sys; m=json.loads(sys.argv[1]); assert m["facility"] == "16" and m["severity"] == "5" and m["host"] and m["protocol"] == "1", m' "$line" ||
  fail "janusd's API line: $line"
status="$(ctl system log-forwarding status)"
echo "$status"
for t in udp tcp tls; do
  grep -qE "^$t +ok " <<<"$status" || fail "target $t isn't ok: $status"
done
echo "Part 1 OK: each target gets what it takes - janusd and events over UDP, HAProxy's messages and access logs over TCP (RFC 3164), the defaults over TLS with a pinned CA"

# --- the audit: an admin's change, a reader refused ---
ctl pki generate-client-config -role os:reader -name ci-reader -ttl 1h "$WORKDIR/reader" >/dev/null || fail "generate-client-config failed"
reader() { "$CTL_BIN" -endpoint "127.0.0.1:${P_GRPC}" -ca "$WORKDIR/reader/ca.crt" -cert "$WORKDIR/reader/client.crt" -key "$WORKDIR/reader/client.key" "$@"; }
if reader system reboot 2>/dev/null; then fail "a reader rebooted the node"; fi
audit="$(timeout 5 "$CTL_BIN" -endpoint "127.0.0.1:${P_GRPC}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" system audit || true)"
grep -qE $'\tSystemService/MetricsConfigSet\tadmin \(os:admin\)\t[0-9.:]+\tOK, [0-9]+ ms$' <<<"$audit" || fail "system audit lacks the admin's change: $audit"
grep -qE $'\tSystemService/Reboot\tci-reader \(os:reader\)\t[0-9.:]+\trefused: ' <<<"$audit" || fail "system audit lacks the reader's refused reboot: $audit"
if reader system audit 2>/dev/null | grep -q .; then fail "a reader read the audit"; fi
wait_received "the reader's refused reboot over TLS, as authpriv" 'm["input"] == "tls" and m["msgid"] == "audit" and m["facility"] == "10" and m["severity"] == "4" and "SystemService/Reboot by ci-reader (os:reader)" in m["msg"] and "refused" in m["msg"]'
wait_received "the admin's change in the audit over TLS" 'm["input"] == "tls" and m["msgid"] == "audit" and "SystemService/MetricsConfigSet by admin (os:admin)" in m["msg"] and ": OK in " in m["msg"]'
echo "Part 1b OK: the audit keeps an admin's change and a reader's refused reboot (system audit, admins only), sent as authpriv"

# --- 2. the server goes away ---
docker stop -t 1 "$NAME" >/dev/null
ctl system metrics -port 10082 >/dev/null
for _ in $(seq 3); do curl -s -o /dev/null "http://127.0.0.1:${P_HTTP}/during-outage"; done
sleep 40 # past the 30 s before a target is said to fail
ctl system metrics -port 10083 >/dev/null
sleep 3
status="$(ctl system log-forwarding status)"
grep -qE "^tcp +failing " <<<"$status" && grep -qE "^tls +failing " <<<"$status" || fail "the stream targets aren't failing with the server down: $status"
start_rsyslog
wait_received "the outage's lines over TLS, once back" 'm["input"] == "tls" and "exporter: metrics served on :10083" in m["msg"]' 90
wait_received "the outage's first line over TLS" 'm["input"] == "tls" and "exporter: metrics served on :10082" in m["msg"]' 10
wait_received "the access logs of the outage over TCP (from memory)" 'm["input"] == "tcp" and "/during-outage" in m["msg"]' 30
wait_received "the logship.failing event over TLS" 'm["input"] == "tls" and m["msg"].startswith("logship.failing")' 30
wait_received "the logship.recovered event" 'm["msg"].startswith("logship.recovered")' 30
echo "Part 2 OK: with the server down the stream targets fail and say so; what was logged meanwhile arrives once it's back - from the LOGS partition, and the access logs from memory"

# --- 3. a reboot: each target goes on from where it was ---
ctl system reboot >/dev/null
wait_janusd 2
wait_received "the new boot's kernel messages over TLS" 'm["input"] == "tls" and m["app"] == "kernel" and m["facility"] == "0" and "Linux version" in m["msg"]' 60
wait_received "the new boot's kernel messages over TCP" 'm["input"] == "tcp" and m["app"] == "kernel" and "Linux version" in m["msg"]' 30
# All of them, under the node's name - not just what came once its
# configuration was read, nor under the address it had before its
# hostname was set (v2026.10.16 on a node without LOGS: 9 lines a boot).
host="$(ctl system hostname)"
deadline=$((SECONDS + 30))
until [ "$(received 'm["input"] == "tls" and m["app"] == "kernel"' | wc -l)" -ge 100 ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "only $(received 'm["input"] == "tls" and m["app"] == "kernel"' | wc -l) of the new boot's kernel messages over TLS"
  sleep 1
done
other="$(received "m['input'] == 'tls' and m['app'] == 'kernel' and m['host'] != '$host'")"
[ -z "$other" ] || fail "kernel messages under another name than $host: $(head -3 <<<"$other")"
audit="$(timeout 5 "$CTL_BIN" -endpoint "127.0.0.1:${P_GRPC}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" system audit || true)"
grep -q $'\tSystemService/Reboot\tci-reader' <<<"$audit" && grep -q $'\tSystemService/Reboot\tadmin (os:admin)' <<<"$audit" || fail "the audit didn't come through the reboot: $audit"
echo "Part 3 OK: after a reboot, the new boot's kernel messages reach the targets ($(received 'm["input"] == "tls" and m["app"] == "kernel"' | wc -l) over TLS, all as $host) - each went on from its cursor; the audit is still there (STATE)"

# --- 4. HAProxy's own log lines, without janusd ---
ctl haproxy file-put certs/logs-ca.pem "$CONF/ca.pem" >/dev/null || fail "file-put the CA"
cat > "$WORKDIR/haproxy-direct.cfg" <<EOF
global
    log ${HOST_FROM_GUEST}:${P_UDP} local1
    log ring@logs local2
    stats socket /run/janus/haproxy-admin.sock mode 660 level admin
    chroot /var/empty
    uid 1000
    gid 1000

ring logs
    format rfc5424
    timeout connect 5s
    timeout server 10s
    server tls ${HOST_FROM_GUEST}:${P_TLS} ssl verify required ca-file /etc/haproxy/files/certs/logs-ca.pem verifyhost logs.test log-proto octet-count

defaults
    mode http
    log global
    option httplog
    timeout connect 5s
    timeout client 30s
    timeout server 30s

frontend janus-health
    bind *:8080
    http-request return status 200 content-type text/plain string "ok\n"
EOF
ctl haproxy apply-config "$WORKDIR/haproxy-direct.cfg" >/dev/null || fail "apply-config (direct) refused"
for _ in $(seq 5); do curl -s -o /dev/null "http://127.0.0.1:${P_HTTP}/haproxy-direct-line"; done
wait_received "HAProxy's own UDP log line" 'm["input"] == "udp" and m["facility"] == "17" and "/haproxy-direct-line" in m["msg"]'
wait_received "HAProxy's own TLS ring log line" 'm["input"] == "tls" and m["facility"] == "18" and "/haproxy-direct-line" in m["msg"]'
echo "Part 4 OK: HAProxy's own log lines - UDP straight to the server, a ring with a TLS server verified against an HAProxy file"

cat "$LOG" >> "$ALL_LOGS"
if grep -aq "avc:  denied" "$ALL_LOGS"; then
  fail "AVC denials: $(grep -a 'avc:  denied' "$ALL_LOGS" | head -5)"
fi
[ "$WITHOUT_LOGS" != 1 ] || grep -aq "no LOGS partition on this disk" "$ALL_LOGS" "$LOG" || fail "janusd didn't say the node has no LOGS partition"
echo "Log forwarding test passed${WITHOUT_LOGS:+ without a LOGS partition}: a real rsyslog got each target's sources over UDP, TCP and TLS, the outage's lines once back, the new boot's kernel messages after a reboot, HAProxy's own UDP and TLS log lines; no secret came back, no AVC denial"
