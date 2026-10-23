#!/usr/bin/env bash
# Proves the Controller's relay for Let's Encrypt (docs/guide/
# letsencrypt-controller.md, dashboard/backend/acme_relay.go) with a real
# enforcing node and a native dashboardd, against Pebble on the host:
#   - the node's QEMU network is restricted: its only way out is a
#     guestfwd to the relay's port - directly, the CA is out of reach;
#   - the relay is off at first, and the node's page says so; turned on,
#     it listens and gives a node page its address and the Controller's
#     identity;
#   - the node's path set through its page to "Through this Controller":
#     the CA's directory reached through the relay, then a certificate
#     obtained over HTTP-01 - HAProxy answering Pebble - and served,
#     verified against Pebble's root;
#   - the relay's journal names the node, the CA's host and the bytes;
#     a destination not allowed is refused and journaled, a client
#     certificate of another CA is refused, a node removed from the
#     Controller is refused;
#   - no AVC denial on the node.
#
# Usage: hack/qemu-acme-relay-test.sh <disk.img> <janusctl-bin> <pebble-bin-dir> <dashboardd-bin>
set -euo pipefail
# shellcheck source=state-read.sh
. "$(dirname "$0")/state-read.sh"

export PATH="$PATH:/usr/sbin:/sbin"

usage="usage: $0 <disk.img> <janusctl-bin> <pebble-bin-dir> <dashboardd-bin>"
SRC_DISK="${1:?$usage}"
CTL_BIN="${2:?$usage}"
PEBBLE_DIR="${3:?$usage}"
DASHBOARDD="${4:?$usage}"
TIMEOUT="${QEMU_ACME_TIMEOUT:-90}"
B="${QEMU_ACME_RELAY_BASE_PORT:-$((18820 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
P_HEALTH=$B P_GRPC=$((B + 1)) P_80=$((B + 2)) P_443=$((B + 3))
P_ACME=$((B + 4)) P_ACME_MGMT=$((B + 5)) P_DNS=$((B + 6)) P_DNS_MGMT=$((B + 7))
P_DASH=$((B + 8)) P_REG=$((B + 9)) P_RELAY=$((B + 10))
# The relay's address as the node sees it: a guestfwd, and a name in the
# Controller's identity (-advertise-address).
RELAY_GUEST=10.0.2.100

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"

WORKDIR="$(mktemp -d)"
PIDS=()
cleanup() {
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
  for p in "${PIDS[@]}"; do wait "$p" 2>/dev/null || true; done
  rm -rf "$WORKDIR"
}
trap cleanup EXIT
LOG="$WORKDIR/boot.log"
fail() {
  echo "ACME relay test FAILED: $*" >&2
  for f in pebble.log dash.log forward.log; do
    [ -f "$WORKDIR/$f" ] && { echo "--- $f ---" >&2; tail -40 "$WORKDIR/$f" >&2; }
  done
  [ -f "$LOG" ] && { echo "--- console output ---" >&2; tail -150 "$LOG" >&2; }
  exit 1
}
check() { # check DESCRIPTION PATTERN TEXT
  grep -Eq -- "$2" <<<"$3" || fail "$1 (no match for $2 in: $3)"
}

# --- the CA, on the host ------------------------------------------------
# The relay runs on the host: the node's janus-acme asks it for
# 127.0.0.1:<Pebble's port> - the node itself never connects there.
not_before="$(date -u -d '-1 hour' +%Y%m%d%H%M%SZ)"
not_after="$(date -u -d '+1 day' +%Y%m%d%H%M%SZ)"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -not_before "$not_before" -not_after "$not_after" \
  -subj "/CN=ACME test CA" -keyout "$WORKDIR/ca.key" -out "$WORKDIR/test-ca.pem" 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=pebble" \
  -keyout "$WORKDIR/pebble.key" -out "$WORKDIR/pebble.csr" 2>/dev/null
printf 'subjectAltName=IP:127.0.0.1\n' > "$WORKDIR/san.ext"
openssl x509 -req -in "$WORKDIR/pebble.csr" -CA "$WORKDIR/test-ca.pem" -CAkey "$WORKDIR/ca.key" -CAcreateserial \
  -not_before "$not_before" -not_after "$not_after" -extfile "$WORKDIR/san.ext" -out "$WORKDIR/pebble.pem" 2>/dev/null
cat > "$WORKDIR/pebble.json" <<EOF
{"pebble": {"listenAddress": "127.0.0.1:$P_ACME", "managementListenAddress": "127.0.0.1:$P_ACME_MGMT",
  "certificate": "$WORKDIR/pebble.pem", "privateKey": "$WORKDIR/pebble.key",
  "httpPort": $P_80, "tlsPort": $((B + 11)), "ocspResponderURL": "", "externalAccountBindingRequired": false,
  "retryAfter": {"authz": 1, "order": 1}, "keyAlgorithm": "ecdsa",
  "profiles": {"default": {"description": "90 days", "validityPeriod": 7776000}}}}
EOF
# challtestsrv answers every name with 127.0.0.1: Pebble validates HTTP-01
# on the port forwarded to the guest's :80.
"$PEBBLE_DIR/pebble-challtestsrv" -defaultIPv6 "" -dnsserver "127.0.0.1:$P_DNS" -http01 "" -https01 "" \
  -tlsalpn01 "" -doh "" -management "127.0.0.1:$P_DNS_MGMT" > "$WORKDIR/challtestsrv.log" 2>&1 &
PIDS+=($!)
PEBBLE_VA_NOSLEEP=1 PEBBLE_WFE_NONCEREJECT=0 "$PEBBLE_DIR/pebble" -config "$WORKDIR/pebble.json" \
  -dnsserver "127.0.0.1:$P_DNS" > "$WORKDIR/pebble.log" 2>&1 &
PIDS+=($!)
deadline=$((SECONDS + 20))
until curl -sk -o /dev/null "https://127.0.0.1:$P_ACME/dir"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "Pebble never answered"
  sleep 0.5
done
(cd "$(dirname "$0")/.." && go build -o "$WORKDIR/connectproxy" ./hack/connectproxy) || fail "build the forwarder"
echo "  ok: Pebble and challtestsrv run"

# --- the node: no way out but the relay -----------------------------------
DISK="$WORKDIR/disk.img"
cp --sparse=always "$SRC_DISK" "$DISK"
cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/OVMF_VARS.fd"
: > "$WORKDIR/forward.log"
fwd="restrict=on,hostfwd=tcp::${P_HEALTH}-:8080,hostfwd=tcp::${P_GRPC}-:9505"
fwd="$fwd,hostfwd=tcp:127.0.0.1:${P_80}-:80,hostfwd=tcp::${P_443}-:443"
fwd="$fwd,guestfwd=tcp:$RELAY_GUEST:8444-cmd:$WORKDIR/connectproxy -stdio -forward 127.0.0.1:$P_RELAY -log $WORKDIR/forward.log"
qemu-system-x86_64 -accel kvm -accel tcg \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$WORKDIR/OVMF_VARS.fd" \
  -drive file="$DISK",format=raw,if=virtio \
  -nographic -display none -m 512M \
  -netdev "user,id=net0,$fwd" -device virtio-net-pci,netdev=net0 \
  -serial "file:$LOG" </dev/null &
PIDS+=($!)
health() { curl -s -m 3 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${P_HEALTH}/" || true; }
deadline=$((SECONDS + TIMEOUT))
until [ "$(health)" = "200" ] && grep -aq "listening on .* (mTLS required)" "$LOG"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the node never came up"
  sleep 1
done
sleep 2
state_pki "$DISK" "$WORKDIR" || fail "couldn't extract the node's pki/ from STATE"
openssl pkcs12 -export -inkey "$WORKDIR/admin.key" -in "$WORKDIR/admin.crt" -certfile "$WORKDIR/ca.crt" \
  -out "$WORKDIR/admin.pfx" -passout pass:t >/dev/null 2>&1
ctl() { "$CTL_BIN" -endpoint "127.0.0.1:${P_GRPC}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" "$@"; }
out="$(ctl haproxy acme status)"
check "the extension runs" "^State: +running" "$out"
THUMB="$(awk '/^Thumbprint:/ {print $2}' <<<"$out")"
echo "  ok: the node is up, its account key made"

# --- the Controller -------------------------------------------------------
"$DASHBOARDD" -addr "127.0.0.1:$P_DASH" -register-addr "127.0.0.1:$P_REG" -advertise-address "$RELAY_GUEST" \
  -data-dir "$WORKDIR/data" > "$WORKDIR/dash.log" 2>&1 &
PIDS+=($!)
API="https://127.0.0.1:$P_DASH"
for _ in $(seq 30); do curl -sk -o /dev/null "$API/api/auth/status" && break; sleep 1; done
JAR="$WORKDIR/jar"
curl -sk -c "$JAR" -o /dev/null -X POST "$API/api/auth/setup" -H 'Content-Type: application/json' -d '{"password":"relay-test-admin-pw"}'
curl -sk -b "$JAR" -X POST "$API/api/auth/mfa/totp/setup" -o "$WORKDIR/totp.json"
SECRET="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["secret"])' "$WORKDIR/totp.json")"
curl -sk -b "$JAR" -o /dev/null -X POST "$API/api/auth/mfa/totp/enable" -H 'Content-Type: application/json' \
  -d "{\"code\":\"$(python3 "$(dirname "$0")/totp.py" "$SECRET")\"}"
api() { curl -sk -b "$JAR" -H 'Content-Type: application/json' "$@"; }
ID="$(curl -sk -b "$JAR" -X POST "$API/api/nodes" -F "name=edge-relay" -F "address=127.0.0.1:$P_GRPC" -F "pfx_password=t" \
  -F "pfx=@$WORKDIR/admin.pfx;type=application/x-pkcs12" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')" ||
  fail "add the node"
NODE="$API/nodes/$ID/api"
echo "  ok: the Controller runs, the node added"

out="$(api "$NODE/haproxy/acme/relay")"
check "the node page sees the relay off" '"enabled": ?false' "$out"
api -X PUT "$API/api/acme-relay" -d "{\"enabled\": true, \"listen\": \"127.0.0.1:$P_RELAY\", \"address\": \"$RELAY_GUEST:8444\", \"allowed\": [\"127.0.0.1:$P_ACME\"]}" > "$WORKDIR/relay.json"
check "the relay turned on" "\"listening\": ?\"127.0.0.1:$P_RELAY\"" "$(cat "$WORKDIR/relay.json")"
api "$NODE/haproxy/acme/relay" > "$WORKDIR/info.json"
check "the node page gets the relay's address" "\"url\": ?\"https://$RELAY_GUEST:8444\"" "$(cat "$WORKDIR/info.json")"
echo "  ok: the relay is on; a node page gets its address and the Controller's identity"

# --- the node through the relay -------------------------------------------
python3 - "$WORKDIR" "$P_ACME" > "$WORKDIR/apply.json" <<'PY'
import json, sys
w, acme = sys.argv[1:3]
info = json.load(open(w + "/info.json"))
print(json.dumps({"config": {
  "account": {"directory": "https://127.0.0.1:%s/dir" % acme, "directory_ca": open(w + "/test-ca.pem").read(),
              "email": "ops@site.test", "accept_terms": True},
  "certificates": [{"name": "site", "domains": ["site.test", "www.site.test"]}],
  "path": {"mode": "ACME_PATH_MODE_CONTROLLER_RELAY", "proxy_url": info["url"], "proxy_ca": info["ca_cert_pem"]},
}}))
PY
# Directly, the CA is out of reach: a restricted network.
if out="$(ctl haproxy acme test-path direct 2>&1)"; then fail "the node reached the CA directly: $out"; fi
out="$(api -X POST "$NODE/haproxy/acme/apply" -d @"$WORKDIR/apply.json")"
check "applied through the node's page" '"accepted": ?true' "$out"
out="$(ctl haproxy acme test-path)" || fail "the CA through the relay: $out"
check "the CA reached through the relay" "reached through the Controller's relay https://$RELAY_GUEST:8444" "$out"
check "janusctl names the path" "through the Controller's relay" "$(ctl haproxy acme path)"
echo "  ok: directly the CA is out of reach; through the relay its directory comes"

ctl haproxy get-config > "$WORKDIR/haproxy.cfg"
rule="$(ctl haproxy acme status | sed -n 's/^HTTP-01 rule: //p')"
cat >> "$WORKDIR/haproxy.cfg" <<EOF

frontend acme-http
    bind *:80
    $rule
    http-request redirect scheme https code 301

frontend tls-file
    bind *:443 ssl crt /etc/haproxy/acme/site.pem
    http-request return status 200 content-type text/plain string "through-the-relay\n"
EOF
out="$(ctl haproxy apply-config "$WORKDIR/haproxy.cfg" 2>&1)" || fail "haproxy.cfg: $out"
got="$(curl -s -m 3 "http://127.0.0.1:$P_80/.well-known/acme-challenge/tok")"
[ "$got" = "tok.$THUMB" ] || fail "the HTTP-01 rule answered '$got'"
deadline=$((SECONDS + 120))
until out="$(ctl haproxy acme status)" && grep -Eq "^site +valid" <<<"$out"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the certificate wasn't obtained: $out"
  sleep 2
done
curl -s "https://127.0.0.1:$P_ACME_MGMT/roots/0" -k > "$WORKDIR/pebble-root.pem"
got="$(curl -s -m 5 --cacert "$WORKDIR/pebble-root.pem" --resolve "site.test:$P_443:127.0.0.1" "https://site.test:$P_443/")"
[ "$got" = "through-the-relay" ] || fail "the certificate isn't served (verified against Pebble's root): '$got'"
echo "  ok: a certificate obtained through the relay, served"

# --- the relay's journal and refusals ---------------------------------------
api "$API/api/acme-relay" > "$WORKDIR/relay.json"
python3 - "$WORKDIR/relay.json" "$P_ACME" <<'PY' || fail "the relay's journal: $(cat "$WORKDIR/relay.json")"
import json, sys
v = json.load(open(sys.argv[1]))
done = [t for t in v["recent"] if t["node"] == "edge-relay" and t["target"] == "127.0.0.1:" + sys.argv[2] and not t.get("refused") and not t.get("error")]
assert done and sum(t["bytes_up"] for t in done) > 0 and sum(t["bytes_down"] for t in done) > 0, v["recent"]
PY
grep -q "acme relay: edge-relay -> 127.0.0.1:$P_ACME: [0-9]* bytes up" "$WORKDIR/dash.log" || fail "no tunnel in the Controller's log"
echo "  ok: the relay's journal names the node, the CA and the bytes"

out="$(api -X POST "$NODE/haproxy/acme/check-path" -d "{\"directory\": \"https://127.0.0.1:$P_ACME_MGMT/dir\"}")"
if grep -Eq '"ok": ?true' <<<"$out"; then fail "a destination not allowed went through: $out"; fi
check "the refusal says why" "Forbidden" "$out"
api "$API/api/acme-relay" > "$WORKDIR/relay.json"
check "the refusal journaled" "\"refused\": ?\"not a destination the relay allows" "$(cat "$WORKDIR/relay.json")"
echo "  ok: a destination not allowed is refused, and journaled"

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=stranger" \
  -keyout "$WORKDIR/stranger.key" -out "$WORKDIR/stranger.pem" -addext "extendedKeyUsage=clientAuth" 2>/dev/null
if curl -s -m 10 -o /dev/null --proxy "https://127.0.0.1:$P_RELAY" --proxy-insecure \
  --proxy-cert "$WORKDIR/stranger.pem" --proxy-key "$WORKDIR/stranger.key" -k "https://127.0.0.1:$P_ACME/dir"; then
  fail "a client certificate of another CA went through the relay"
fi
grep -q "not a certificate of an approved node's CA" "$WORKDIR/dash.log" || fail "the stranger's refusal isn't in the Controller's log"
echo "  ok: a certificate of another CA is refused"

api -X DELETE "$API/api/nodes/$ID" >/dev/null
if out="$(ctl haproxy acme test-path 2>&1)"; then fail "a node removed from the Controller still went through: $out"; fi
echo "  ok: a node removed from the Controller is refused ($out)"

denials="$(grep -a "avc:.*denied" "$LOG" || true)"
if [ -n "$denials" ]; then
  echo "$denials" >&2
  fail "AVC denials under enforcing"
fi
echo "ACME relay test OK: a node with no way out but the Controller got its certificate through the relay - only to allowed destinations, only as a node the Controller knows - zero AVC denials"
