#!/usr/bin/env bash
# Proves the Controller's log forwarding (dashboard/backend/logforward.go,
# docs/guide/log-forwarding.md) with two real enforcing nodes, a native
# dashboardd and a real syslog server (hack/rsyslog):
#   1. The Controller's own targets: its audit (a change made through it)
#      and its log reach the server.
#   2. A fleet profile picks a node by its labels: that node is given the
#      profile's targets (managed_by names the Controller and the profile)
#      and its logs reach the server under its hostname; the other node,
#      picked by none, keeps its own (empty) configuration.
#   3. A change made on the managed node is undone; a label changed puts
#      the other node in the profile; the profile removed is taken off
#      both - what the Controller set, never a node's own.
#   4. No AVC denial on either node.
#
# Usage: hack/qemu-logforward-fleet-test.sh <disk.img> <janusctl-bin> <dashboardd-bin>
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

SRC_DISK="${1:?usage: $0 <disk.img> <janusctl-bin> <dashboardd-bin>}"
CTL_BIN="${2:?usage: $0 <disk.img> <janusctl-bin> <dashboardd-bin>}"
DASHBOARDD="${3:?usage: $0 <disk.img> <janusctl-bin> <dashboardd-bin>}"
OFF="${JANUS_TEST_PORT_OFFSET:-0}"
P_GRPC_A=$((18391 + OFF))
P_GRPC_B=$((18392 + OFF))
P_DASH=$((18393 + OFF))
P_REG=$((18394 + OFF))
P_UDP=$((18395 + OFF))
P_TCP=$((18396 + OFF))
P_TLS=$((18397 + OFF))
HOST_FROM_GUEST=10.0.2.2
IMAGE="${RSYSLOG_IMAGE:-janus-rsyslog-test}"
NAME="janus-rsyslog-fleet-$OFF"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf)" >&2; exit 1; }

WORKDIR="$(mktemp -d)"
PIDS=()
cleanup() {
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT
OUT="$WORKDIR/out"
fail() {
  echo "Fleet log forwarding test FAILED: $*" >&2
  for n in a b; do [ -f "$WORKDIR/$n.log" ] && { echo "--- node $n console (last 20) ---" >&2; tail -20 "$WORKDIR/$n.log" >&2; }; done
  [ -f "$WORKDIR/dash.log" ] && { echo "--- dashboardd (last 20) ---" >&2; tail -20 "$WORKDIR/dash.log" >&2; }
  [ -f "$OUT/messages.jsonl" ] && { echo "--- rsyslog (last 10) ---" >&2; tail -10 "$OUT/messages.jsonl" >&2; }
  exit 1
}

# --- the syslog server ---
docker build -q -t "$IMAGE" hack/rsyslog >/dev/null
CONF="$WORKDIR/rsyslog"
mkdir -p "$CONF" "$OUT"
chmod 777 "$OUT"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=t" -keyout "$CONF/server.key" -out "$CONF/server.pem" >/dev/null 2>&1
cp "$CONF/server.pem" "$CONF/ca.pem"
chmod 644 "$CONF"/*
sed -e "s/@UDP@/$P_UDP/; s/@TCP@/$P_TCP/; s/@TLS@/$P_TLS/" hack/rsyslog/rsyslog.conf.in > "$CONF/rsyslog.conf"
docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" --network host -v "$CONF:/etc/rsyslog-test:ro" -v "$OUT:/out" "$IMAGE" >/dev/null
deadline=$((SECONDS + 30))
until ss -ltn | grep -q ":$P_TCP "; do
  [ "$SECONDS" -lt "$deadline" ] || fail "rsyslog didn't listen"
  sleep 0.5
done
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
wait_received() {
  local deadline=$((SECONDS + ${3:-60}))
  until [ -n "$(received "$2")" ]; do
    [ "$SECONDS" -lt "$deadline" ] || fail "rsyslog never got $1"
    sleep 1
  done
}

# --- two nodes, another MAC each (another hostname each) ---
boot() { # boot NAME GRPC-PORT MAC
  cp --sparse=always "$SRC_DISK" "$WORKDIR/$1.img"
  cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/$1.vars"
  qemu-system-x86_64 -accel kvm -accel tcg -m 1024M \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$WORKDIR/$1.vars" \
    -drive file="$WORKDIR/$1.img",format=raw,if=virtio \
    -display none -monitor none \
    -netdev "user,id=net0,hostfwd=tcp::$2-:9505" -device virtio-net-pci,netdev=net0,mac="$3" \
    -serial file:"$WORKDIR/$1.log" </dev/null &
  PIDS+=($!)
}
boot a "$P_GRPC_A" 52:54:00:aa:00:01
boot b "$P_GRPC_B" 52:54:00:bb:00:02
for n in a b; do
  deadline=$((SECONDS + 150))
  until grep -aq "listening on" "$WORKDIR/$n.log" 2>/dev/null; do
    [ "$SECONDS" -lt "$deadline" ] || fail "node $n's janusd never listened"
    sleep 1
  done
done
sleep 2
creds() { # creds NAME - extracts the node's admin credentials from its STATE
  # Read while it runs: what janusd just wrote may still be only in
  # ext4's journal - replayed on the copy (e2fsck), read again a moment
  # later until it's there (two nodes booting at once on a busy runner).
  local s n try got f
  s="$(sgdisk -i 6 "$WORKDIR/$1.img" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
  n="$(sgdisk -i 6 "$WORKDIR/$1.img" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
  mkdir -p "$WORKDIR/$1"
  for try in $(seq 15); do
    dd if="$WORKDIR/$1.img" of="$WORKDIR/$1.state" bs=512 skip="$s" count="$n" status=none
    e2fsck -fy "$WORKDIR/$1.state" >/dev/null 2>&1 || true
    got=1
    for f in ca.crt admin.crt admin.key; do
      rm -f "$WORKDIR/$1/$f"
      debugfs -R "dump pki/$f $WORKDIR/$1/$f" "$WORKDIR/$1.state" >/dev/null 2>&1
      [ -s "$WORKDIR/$1/$f" ] || got=0
    done
    [ "$got" = 1 ] && break
    [ "$try" -lt 15 ] || fail "couldn't extract node $1's pki/"
    sleep 2
  done
  openssl pkcs12 -export -inkey "$WORKDIR/$1/admin.key" -in "$WORKDIR/$1/admin.crt" -certfile "$WORKDIR/$1/ca.crt" -out "$WORKDIR/$1/admin.pfx" -passout pass:t >/dev/null 2>&1
}
creds a
creds b
ctl() { # ctl NAME ARGS...
  local n="$1" port="$P_GRPC_A"
  shift
  [ "$n" = b ] && port="$P_GRPC_B"
  "$CTL_BIN" -endpoint "127.0.0.1:$port" -ca "$WORKDIR/$n/ca.crt" -cert "$WORKDIR/$n/admin.crt" -key "$WORKDIR/$n/admin.key" "$@"
}
HOST_A="$(ctl a system hostname)"
HOST_B="$(ctl b system hostname)"
[ "$HOST_A" != "$HOST_B" ] || fail "both nodes are named $HOST_A"

# --- the Controller ---
"$DASHBOARDD" -addr "127.0.0.1:$P_DASH" -register-addr "127.0.0.1:$P_REG" -data-dir "$WORKDIR/data" > "$WORKDIR/dash.log" 2>&1 &
PIDS+=($!)
API="https://127.0.0.1:$P_DASH"
for _ in $(seq 30); do curl -sk -o /dev/null "$API/api/auth/status" && break; sleep 1; done
JAR="$WORKDIR/jar"
curl -sk -c "$JAR" -o /dev/null -X POST "$API/api/auth/setup" -H 'Content-Type: application/json' -d '{"password":"fleet-test-admin-pw"}'
curl -sk -b "$JAR" -X POST "$API/api/auth/mfa/totp/setup" -o "$WORKDIR/totp.json"
SECRET="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["secret"])' "$WORKDIR/totp.json")"
curl -sk -b "$JAR" -o /dev/null -X POST "$API/api/auth/mfa/totp/enable" -H 'Content-Type: application/json' -d "{\"code\":\"$(python3 hack/totp.py "$SECRET")\"}"
api() { curl -sk -b "$JAR" -H 'Content-Type: application/json' "$@"; }
add() { # add NAME PORT - the node's id
  curl -sk -b "$JAR" -X POST "$API/api/nodes" -F "name=node-$1" -F "address=127.0.0.1:$2" -F "pfx_password=t" -F "pfx=@$WORKDIR/$1/admin.pfx;type=application/x-pkcs12" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])'
}
ID_A="$(add a "$P_GRPC_A")" || fail "add node a"
ID_B="$(add b "$P_GRPC_B")" || fail "add node b"
api -X PATCH "$API/api/nodes/$ID_A" -d '{"labels":{"team":"web"}}' >/dev/null
api -X PATCH "$API/api/nodes/$ID_B" -d '{"labels":{"team":"db"}}' >/dev/null

# node_state ID - the node's state with the profiles, and its profile.
node_state() {
  api "$API/api/log-forwarding" | python3 -c 'import json,sys; n=[n for n in json.load(sys.stdin)["nodes"] if n["id"]==sys.argv[1]][0]; print(n["state"], n.get("profile",""))' "$1"
}
wait_state() { # wait_state ID WANT
  local deadline=$((SECONDS + 60)) got
  until got="$(node_state "$1")" && [ "$got" = "$2" ]; do
    [ "$SECONDS" -lt "$deadline" ] || fail "node $1 is \"$got\", want \"$2\""
    sleep 1
  done
}
managed_by() { ctl "$1" system log-forwarding get | python3 -c 'import json,sys; print(json.load(sys.stdin).get("managedBy",""))'; }

# --- 1 and 2: the Controller's own targets, a profile for team=web ---
python3 - "$WORKDIR/settings.json" "$HOST_FROM_GUEST" "$P_TCP" <<'EOF'
import json, sys
out, host, tcp = sys.argv[1:]
json.dump({
    "controller": {"targets": [{"name": "rsyslog", "syslog": {"address": "127.0.0.1:" + tcp, "transport": "tcp"}}]},
    "profiles": [{"name": "web", "selector": "team=web", "config": {"targets": [
        {"name": "rsyslog", "sources": ["janusd", "events"], "syslog": {"address": host + ":" + tcp, "transport": "tcp"}}]}}],
}, open(out, "w"))
EOF
out="$(api -X PUT "$API/api/log-forwarding" -d @"$WORKDIR/settings.json")"
grep -q '"profiles"' <<<"$out" || fail "PUT /api/log-forwarding: $out"
wait_state "$ID_A" "applied web"
wait_state "$ID_B" "unmanaged "
mb="$(managed_by a)"
grep -q "^janus-controller .* profile web " <<<"$mb" || fail "node a's managed_by: $mb"
[ -z "$(managed_by b)" ] || fail "node b got a managed_by"
wait_received "the Controller's audit of the PUT" 'm["app"] == "dashboardd" and m["msgid"] == "audit" and "PUT /api/log-forwarding 200 by admin" in m["msg"]'
wait_received "the Controller's log" 'm["app"] == "dashboardd" and m["msgid"] == "controller" and "log forwarding profile web applied" in m["msg"]'
ctl a system metrics -port 10101 >/dev/null
wait_received "node a's janusd log, under its hostname" "m[\"host\"] == \"$HOST_A\" and m[\"app\"] == \"janusd\" and \"api: SystemService/MetricsConfigSet\" in m[\"msg\"]"
[ -z "$(received "m[\"host\"] == \"$HOST_B\"")" ] || fail "node b's logs were sent, no profile picks it"
echo "Part 1-2 OK: the Controller's audit and log reach the server; the web profile is node a's (managed_by), its logs reach the server; node b, picked by none, keeps its own"

# --- 3: drift undone, a label changed, the profile removed ---
echo '{}' > "$WORKDIR/empty.json"
ctl a system log-forwarding set "$WORKDIR/empty.json" >/dev/null
api -X PATCH "$API/api/nodes/$ID_B" -d '{"labels":{"team":"web"}}' >/dev/null # pokes the loop: a's drift undone too
wait_state "$ID_B" "applied web"
deadline=$((SECONDS + 60))
until grep -q "profile web" <<<"$(managed_by a)"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "node a's configuration changed by hand wasn't undone"
  sleep 1
done
ctl b system metrics -port 10102 >/dev/null
wait_received "node b's janusd log once in the profile" "m[\"host\"] == \"$HOST_B\" and \"api: SystemService/MetricsConfigSet\" in m[\"msg\"]"
api -X PUT "$API/api/log-forwarding" -d '{"controller":{"targets":[]},"profiles":[]}' >/dev/null
wait_state "$ID_A" "removed "
wait_state "$ID_B" "removed "
[ -z "$(managed_by a)$(managed_by b)" ] || fail "the profile wasn't taken off: $(managed_by a) / $(managed_by b)"
echo "Part 3 OK: a change made on the managed node undone, a label changed puts node b in the profile, the profile removed is taken off both"

for n in a b; do
  if grep -aq "avc:  denied" "$WORKDIR/$n.log"; then fail "AVC denials on node $n: $(grep -a 'avc:  denied' "$WORKDIR/$n.log" | head -3)"; fi
done
echo "Fleet log forwarding test passed: the Controller's audit and log sent; a profile given to the node its labels pick, kept applied, extended by a label, taken off when removed - two real nodes, no AVC denial"
