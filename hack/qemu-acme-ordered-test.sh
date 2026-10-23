#!/usr/bin/env bash
# Proves certificates ordered by the Controller (docs/guide/
# letsencrypt-controller.md "Ordering for the nodes", dashboard/backend/
# acme_order.go) with a real enforcing node and a native dashboardd,
# against Pebble on the host:
#   - the node's QEMU network is restricted with no way out at all: only
#     the Controller's calls in (gRPC) and the CA's validation (port 80);
#   - the Controller's account and DNS provider set through its API; a
#     node page then offers "Ordered by this Controller" with the
#     account's thumbprint, and HAProxy answers HTTP-01 for it;
#   - an HTTP-01 certificate and a wildcard over DNS-01 (the Controller's
#     httpreq provider) ordered by the Controller, installed on the node,
#     served - verified against Pebble's root; the node never ordered
#     anything itself, and no private key is in the Controller's data;
#   - a certificate naming a DNS provider the Controller doesn't have
#     fails, and the node's status says why;
#   - a renewal asked for on the node's page is ordered at once, swapped
#     in; no AVC denial on the node.
#
# Usage: hack/qemu-acme-ordered-test.sh <disk.img> <janusctl-bin> <pebble-bin-dir> <dashboardd-bin> <janus-acme-bin>
set -euo pipefail
# shellcheck source=state-read.sh
. "$(dirname "$0")/state-read.sh"

export PATH="$PATH:/usr/sbin:/sbin"

usage="usage: $0 <disk.img> <janusctl-bin> <pebble-bin-dir> <dashboardd-bin> <janus-acme-bin>"
SRC_DISK="${1:?$usage}"
CTL_BIN="${2:?$usage}"
PEBBLE_DIR="${3:?$usage}"
DASHBOARDD="${4:?$usage}"
ACME_CLIENT="${5:?$usage}"
TIMEOUT="${QEMU_ACME_TIMEOUT:-90}"
B="${QEMU_ACME_ORDERED_BASE_PORT:-$((18860 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
P_HEALTH=$B P_GRPC=$((B + 1)) P_80=$((B + 2)) P_443=$((B + 3))
P_ACME=$((B + 4)) P_ACME_MGMT=$((B + 5)) P_DNS=$((B + 6)) P_DNS_MGMT=$((B + 7))
P_DASH=$((B + 8)) P_REG=$((B + 9)) P_SHIM=$((B + 10))

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
  echo "ACME ordered test FAILED: $*" >&2
  for f in pebble.log shim.log dash.log; do
    [ -f "$WORKDIR/$f" ] && { echo "--- $f ---" >&2; tail -40 "$WORKDIR/$f" >&2; }
  done
  [ -f "$LOG" ] && { echo "--- console output ---" >&2; tail -150 "$LOG" >&2; }
  exit 1
}
check() { # check DESCRIPTION PATTERN TEXT
  grep -Eq -- "$2" <<<"$3" || fail "$1 (no match for $2 in: $3)"
}

# --- the CA, on the host, reached by the Controller only ------------------
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
"$PEBBLE_DIR/pebble-challtestsrv" -defaultIPv6 "" -dnsserver "127.0.0.1:$P_DNS" -http01 "" -https01 "" \
  -tlsalpn01 "" -doh "" -management "127.0.0.1:$P_DNS_MGMT" > "$WORKDIR/challtestsrv.log" 2>&1 &
PIDS+=($!)
PEBBLE_VA_NOSLEEP=1 PEBBLE_WFE_NONCEREJECT=0 "$PEBBLE_DIR/pebble" -config "$WORKDIR/pebble.json" \
  -dnsserver "127.0.0.1:$P_DNS" > "$WORKDIR/pebble.log" 2>&1 &
PIDS+=($!)
# The Controller's httpreq DNS provider -> challtestsrv's management API.
cat > "$WORKDIR/shim.py" <<'PY'
import json, sys, urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer
mgmt = sys.argv[2]
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        op = {"/present": "/set-txt", "/cleanup": "/clear-txt"}[self.path]
        data = {"host": body["fqdn"]}
        if op == "/set-txt":
            data["value"] = body["value"]
        urllib.request.urlopen(urllib.request.Request(mgmt + op, json.dumps(data).encode(), method="POST"))
        sys.stderr.write("%s %s\n" % (op, body["fqdn"]))
        sys.stderr.flush()
        self.send_response(200)
        self.end_headers()
    def log_message(self, *a):
        pass
HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY
python3 "$WORKDIR/shim.py" "$P_SHIM" "http://127.0.0.1:$P_DNS_MGMT" > "$WORKDIR/shim.log" 2>&1 &
PIDS+=($!)
deadline=$((SECONDS + 20))
until curl -sk -o /dev/null "https://127.0.0.1:$P_ACME/dir"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "Pebble never answered"
  sleep 0.5
done
echo "  ok: Pebble, challtestsrv and the DNS shim run"

# --- the node: no way out -------------------------------------------------
DISK="$WORKDIR/disk.img"
cp --sparse=always "$SRC_DISK" "$DISK"
cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/OVMF_VARS.fd"
fwd="restrict=on,hostfwd=tcp::${P_HEALTH}-:8080,hostfwd=tcp::${P_GRPC}-:9505"
fwd="$fwd,hostfwd=tcp:127.0.0.1:${P_80}-:80,hostfwd=tcp::${P_443}-:443"
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
check "the extension runs" "^State: +running" "$(ctl haproxy acme status)"
echo "  ok: the node is up"

# --- the Controller -------------------------------------------------------
"$DASHBOARDD" -addr "127.0.0.1:$P_DASH" -register-addr "127.0.0.1:$P_REG" -acme-client "$ACME_CLIENT" \
  -data-dir "$WORKDIR/data" > "$WORKDIR/dash.log" 2>&1 &
PIDS+=($!)
API="https://127.0.0.1:$P_DASH"
for _ in $(seq 30); do curl -sk -o /dev/null "$API/api/auth/status" && break; sleep 1; done
JAR="$WORKDIR/jar"
curl -sk -c "$JAR" -o /dev/null -X POST "$API/api/auth/setup" -H 'Content-Type: application/json' -d '{"password":"ordered-test-admin-pw"}'
curl -sk -b "$JAR" -X POST "$API/api/auth/mfa/totp/setup" -o "$WORKDIR/totp.json"
SECRET="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["secret"])' "$WORKDIR/totp.json")"
curl -sk -b "$JAR" -o /dev/null -X POST "$API/api/auth/mfa/totp/enable" -H 'Content-Type: application/json' \
  -d "{\"code\":\"$(python3 "$(dirname "$0")/totp.py" "$SECRET")\"}"
api() { curl -sk -b "$JAR" -H 'Content-Type: application/json' "$@"; }
ID="$(curl -sk -b "$JAR" -X POST "$API/api/nodes" -F "name=edge-ordered" -F "address=127.0.0.1:$P_GRPC" -F "pfx_password=t" \
  -F "pfx=@$WORKDIR/admin.pfx;type=application/x-pkcs12" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')" ||
  fail "add the node"
NODE="$API/nodes/$ID/api"
echo "  ok: the Controller runs, the node added"

out="$(api "$NODE/haproxy/acme/controller")"
check "not set up yet" '"ready": ?false' "$out"
api -X PUT "$API/api/acme" -d "{\"email\": \"ops@site.test\", \"accept_terms\": true,
  \"dns_providers\": [{\"name\": \"lab\", \"type\": \"httpreq\", \"settings\": {\"HTTPREQ_ENDPOINT\": \"http://127.0.0.1:$P_SHIM\"}, \"propagation_wait_seconds\": 2}]}" > "$WORKDIR/acme.json"
check "the account set up" '"ready": ?true' "$(cat "$WORKDIR/acme.json")"
if grep -q "127.0.0.1:$P_SHIM" "$WORKDIR/acme.json"; then fail "the provider's settings came back: $(cat "$WORKDIR/acme.json")"; fi
api "$NODE/haproxy/acme/controller" > "$WORKDIR/info.json"
THUMB="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["ordering"]["thumbprint"])' "$WORKDIR/info.json")"
[ "${#THUMB}" = 43 ] || fail "no account thumbprint for the node page: $(cat "$WORKDIR/info.json")"
echo "  ok: the Controller's account and DNS provider set; a node page gets the account's thumbprint"

# --- the node ordered by the Controller -------------------------------------
python3 - "$WORKDIR" "$P_ACME" "$THUMB" > "$WORKDIR/apply.json" <<'PY'
import json, sys
w, acme, thumb = sys.argv[1:4]
print(json.dumps({"config": {
  "account": {"directory": "https://127.0.0.1:%s/dir" % acme, "directory_ca": open(w + "/test-ca.pem").read()},
  "certificates": [
    {"name": "site", "domains": ["site.test", "www.site.test"]},
    {"name": "wild", "domains": ["*.wild.test", "wild.test"], "challenge": "dns-01", "dns_provider": "lab", "key_type": "ec384"},
    {"name": "orphan", "domains": ["orphan.test"], "challenge": "dns-01", "dns_provider": "nope"},
  ],
  "path": {"mode": "ACME_PATH_MODE_CONTROLLER_ORDERED", "account_thumbprint": thumb},
}}))
PY
out="$(api -X POST "$NODE/haproxy/acme/apply" -d @"$WORKDIR/apply.json")"
check "applied through the node's page" '"accepted": ?true' "$out"
check "janusctl names the path" "by the Controller, which orders the certificates" "$(ctl haproxy acme path)"
ctl haproxy get-config > "$WORKDIR/haproxy.cfg"
rule="$(ctl haproxy acme status | sed -n 's/^HTTP-01 rule: //p')"
cat >> "$WORKDIR/haproxy.cfg" <<EOF

frontend acme-http
    bind *:80
    $rule
    http-request redirect scheme https code 301

frontend tls-dir
    bind *:443 ssl crt /etc/haproxy/acme/
    http-request return status 200 content-type text/plain string "ordered\n"
EOF
out="$(ctl haproxy apply-config "$WORKDIR/haproxy.cfg" 2>&1)" || fail "haproxy.cfg: $out"
deadline=$((SECONDS + 30))
until [ "$(curl -s -m 3 "http://127.0.0.1:$P_80/.well-known/acme-challenge/tok")" = "tok.$THUMB" ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "HAProxy doesn't answer HTTP-01 for the Controller's account: $(curl -s -m 3 "http://127.0.0.1:$P_80/.well-known/acme-challenge/tok")"
  sleep 1
done
echo "  ok: HAProxy answers HTTP-01 for the Controller's account"

api -X POST "$API/api/acme/run" -o /dev/null
deadline=$((SECONDS + 180))
until out="$(ctl haproxy acme status)" && grep -Eq "^site +valid" <<<"$out" && grep -Eq "^wild +valid" <<<"$out"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the Controller didn't get the certificates installed: $out"
  sleep 3
done
curl -s "https://127.0.0.1:$P_ACME_MGMT/roots/0" -k > "$WORKDIR/pebble-root.pem"
served() { curl -s -m 5 --cacert "$WORKDIR/pebble-root.pem" --resolve "$1:$P_443:127.0.0.1" "https://$1:$P_443/"; }
[ "$(served www.site.test)" = "ordered" ] || fail "site isn't served (verified against Pebble's root)"
[ "$(served any.wild.test)" = "ordered" ] || fail "the wildcard isn't served"
grep -q "/set-txt _acme-challenge.wild.test." "$WORKDIR/shim.log" || fail "DNS-01 never went through the Controller's provider"
if grep -aq "letsencrypt: .*certificate .*: .*ordering it" "$LOG"; then fail "the node ordered a certificate itself"; fi
grep -aq "a request made for the Controller to order" "$LOG" || fail "the node made no request for the Controller"
grep -q "lets encrypt: node edge-ordered, certificate site: ordered and installed" "$WORKDIR/dash.log" || fail "the Controller's log doesn't say so"
# The ordering's data holds the account key sealed, and no certificate's
# key; the Controller's log never saw one either.
[ -f "$WORKDIR/data/acme/state.json" ] || fail "no ordering state in the Controller's data"
if grep -rlq "PRIVATE KEY" "$WORKDIR/data/acme" "$WORKDIR/dash.log"; then fail "a private key in the Controller's ordering data or log: $(grep -rl "PRIVATE KEY" "$WORKDIR/data/acme" "$WORKDIR/dash.log")"; fi
echo "  ok: both ordered by the Controller - HTTP-01 and a wildcard over its DNS provider - installed, served; no key left the node"

out="$(ctl haproxy acme status)"
check "the orphan's failure on the node" "no DNS provider named \"nope\"" "$out"
api "$API/api/acme" > "$WORKDIR/acme.json"
python3 - "$WORKDIR/acme.json" <<'PY' || fail "the Controller's view: $(cat "$WORKDIR/acme.json")"
import json, sys
v = json.load(open(sys.argv[1]))
n = [x for x in v["nodes"] if x["name"] == "edge-ordered"][0]
c = {x["name"]: x for x in n["certificates"]}
assert c["site"]["state"] == "valid" and c["site"]["order"].get("last_success"), c["site"]
assert c["wild"]["state"] == "valid", c["wild"]
assert "no DNS provider" in c["orphan"]["order"]["last_error"] and c["orphan"]["order"]["next_attempt"], c["orphan"]
PY
echo "  ok: a provider the Controller doesn't have fails, and both the node and the Controller say why"

serial_of() { ctl haproxy acme status | awk -v n="$1" '$1 == n {print}' ; }
before="$(openssl s_client -connect "127.0.0.1:$P_443" -servername www.site.test </dev/null 2>/dev/null | openssl x509 -noout -serial)"
api -X POST "$NODE/haproxy/acme/renew" -d '{"names": ["site"]}' -o /dev/null
deadline=$((SECONDS + 120))
until [ "$(openssl s_client -connect "127.0.0.1:$P_443" -servername www.site.test </dev/null 2>/dev/null | openssl x509 -noout -serial)" != "$before" ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the renewal asked for on the node's page wasn't ordered: $(serial_of site)"
  sleep 2
done
echo "  ok: a renewal asked for on the node's page ordered at once, swapped in"

denials="$(grep -a "avc:.*denied" "$LOG" || true)"
if [ -n "$denials" ]; then
  echo "$denials" >&2
  fail "AVC denials under enforcing"
fi
echo "ACME ordered test OK: a node that reaches nothing got its certificates from the Controller - HTTP-01 and a wildcard over DNS-01, renewed on request - its keys never left it, zero AVC denials"
