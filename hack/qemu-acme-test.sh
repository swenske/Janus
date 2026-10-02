#!/usr/bin/env bash
# Proves Let's Encrypt (the letsencrypt extension, docs/letsencrypt.md) on
# a real node built with it - UEFI, SELinux enforcing - against Pebble,
# Let's Encrypt's own ACME test server, run on the host with its DNS test
# server (pebble-challtestsrv):
#   - the account key is created at first boot, its thumbprint is in
#     HAProxy's environment: the stateless HTTP-01 rule answers on :80;
#   - a configuration with two certificates writes self-signed stand-ins
#     at once, so a haproxy.cfg referencing them - one by file on :443,
#     both through the directory on :8443 - applies right away;
#   - "site" (two names) is obtained over HTTP-01, answered by HAProxy
#     itself, and "wild" (a wildcard) over DNS-01 through lego's httpreq
#     provider (a small shim to challtestsrv); both are served, verified
#     against Pebble's root;
#   - a renewal asked for is swapped into the running HAProxy without a
#     reload;
#   - after a reboot the same certificates are served, nothing is
#     ordered again; the exporter reports them; no AVC denial.
#
# Usage: hack/qemu-acme-test.sh <disk.img> <janusctl-bin> <pebble-bin-dir>
#   ACME_DISCOVERY=1: don't fail on AVC denials - list them (for writing
#   the acme_t rules, with a UKI built UKI_SELINUX_ENFORCING=0 and
#   UKI_EXTRA_CMDLINE=sysctl.kernel.printk_ratelimit=0).
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

usage="usage: $0 <disk.img> <janusctl-bin> <pebble-bin-dir>"
SRC_DISK="${1:?$usage}"
CTL_BIN="${2:?$usage}"
PEBBLE_DIR="${3:?$usage}"
TIMEOUT="${QEMU_ACME_TIMEOUT:-90}"
B="${QEMU_ACME_BASE_PORT:-$((18750 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
P_HEALTH=$B P_GRPC=$((B + 1)) P_METRICS=$((B + 2)) P_80=$((B + 3)) P_443=$((B + 4)) P_8443=$((B + 5))
P_ACME=$((B + 6)) P_ACME_MGMT=$((B + 7)) P_DNS=$((B + 8)) P_DNS_MGMT=$((B + 9)) P_SHIM=$((B + 10))

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"

WORKDIR="$(mktemp -d)"
PIDS=()
cleanup() {
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
  rm -rf "$WORKDIR"
}
trap cleanup EXIT
LOG="$WORKDIR/boot.log"
fail() {
  echo "ACME test FAILED: $*" >&2
  for f in pebble.log shim.log; do
    [ -f "$WORKDIR/$f" ] && { echo "--- $f ---" >&2; tail -40 "$WORKDIR/$f" >&2; }
  done
  [ -f "$LOG" ] && { echo "--- console output ---" >&2; tail -200 "$LOG" >&2; }
  exit 1
}
check() { # check DESCRIPTION PATTERN TEXT
  grep -Eq -- "$2" <<<"$3" || fail "$1 (no match for $2 in: $3)"
}

# --- the CA, on the host ------------------------------------------------
# The guest reaches the host at 10.0.2.2 (QEMU user networking): Pebble's
# HTTPS certificate must be valid for it, under a CA of the test's own -
# which the node trusts through directory_ca, as for a private ACME CA.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=ACME test CA" \
  -keyout "$WORKDIR/ca.key" -out "$WORKDIR/test-ca.pem" 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=pebble" \
  -keyout "$WORKDIR/pebble.key" -out "$WORKDIR/pebble.csr" 2>/dev/null
printf 'subjectAltName=IP:10.0.2.2,IP:127.0.0.1\n' > "$WORKDIR/san.ext"
openssl x509 -req -in "$WORKDIR/pebble.csr" -CA "$WORKDIR/test-ca.pem" -CAkey "$WORKDIR/ca.key" -CAcreateserial \
  -days 1 -extfile "$WORKDIR/san.ext" -out "$WORKDIR/pebble.pem" 2>/dev/null
cat > "$WORKDIR/pebble.json" <<EOF
{"pebble": {"listenAddress": "127.0.0.1:$P_ACME", "managementListenAddress": "127.0.0.1:$P_ACME_MGMT",
  "certificate": "$WORKDIR/pebble.pem", "privateKey": "$WORKDIR/pebble.key",
  "httpPort": $P_80, "tlsPort": $((B + 11)), "ocspResponderURL": "", "externalAccountBindingRequired": false,
  "retryAfter": {"authz": 1, "order": 1}, "keyAlgorithm": "ecdsa",
  "profiles": {"default": {"description": "90 days", "validityPeriod": 7776000}}}}
EOF
# challtestsrv answers every name with 127.0.0.1: Pebble validates HTTP-01
# on the port forwarded to the guest's :80, and reads DNS-01 TXT records
# the shim sets.
"$PEBBLE_DIR/pebble-challtestsrv" -defaultIPv6 "" -dnsserver "127.0.0.1:$P_DNS" -http01 "" -https01 "" \
  -tlsalpn01 "" -doh "" -management "127.0.0.1:$P_DNS_MGMT" > "$WORKDIR/challtestsrv.log" 2>&1 &
PIDS+=($!)
PEBBLE_VA_NOSLEEP=1 PEBBLE_WFE_NONCEREJECT=0 "$PEBBLE_DIR/pebble" -config "$WORKDIR/pebble.json" \
  -dnsserver "127.0.0.1:$P_DNS" > "$WORKDIR/pebble.log" 2>&1 &
PIDS+=($!)
# lego's httpreq DNS provider -> challtestsrv's management API.
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

# --- the node -------------------------------------------------------------
DISK="$WORKDIR/disk.img"
cp "$SRC_DISK" "$DISK"
cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/OVMF_VARS.fd"
fwd="hostfwd=tcp::${P_HEALTH}-:8080,hostfwd=tcp::${P_GRPC}-:9505,hostfwd=tcp::${P_METRICS}-:10056"
fwd="$fwd,hostfwd=tcp:127.0.0.1:${P_80}-:80,hostfwd=tcp::${P_443}-:443,hostfwd=tcp::${P_8443}-:8443"
boot() {
  qemu-system-x86_64 -accel kvm -accel tcg \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$WORKDIR/OVMF_VARS.fd" \
    -drive file="$DISK",format=raw,if=virtio \
    -nographic -display none -m 512M \
    -netdev "user,id=net0,$fwd" -device virtio-net-pci,netdev=net0 \
    -serial "file:$LOG.$1" &
  QEMU_PID=$!
  PIDS+=("$QEMU_PID")
  ln -sf "$LOG.$1" "$LOG"
}
health() { curl -s -m 3 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${P_HEALTH}/" || true; }
wait_up() {
  local deadline=$((SECONDS + TIMEOUT))
  until [ "$(health)" = "200" ] && grep -aq "listening on .* (mTLS required)" "$LOG"; do
    [ "$SECONDS" -lt "$deadline" ] || fail "the node never came up"
    sleep 1
  done
}
boot 1
wait_up

STATE_START_SECTOR="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
STATE_SIZE_SECTORS="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
dd if="$DISK" of="$WORKDIR/state.img" bs=512 skip="$STATE_START_SECTOR" count="$STATE_SIZE_SECTORS" status=none
for f in ca.crt admin.crt admin.key; do
  debugfs -R "dump pki/$f $WORKDIR/$f" "$WORKDIR/state.img" >/dev/null 2>&1
  [ -s "$WORKDIR/$f" ] || fail "couldn't extract pki/$f from STATE"
done
ctl() { "$CTL_BIN" -endpoint "127.0.0.1:${P_GRPC}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" "$@"; }

out="$(ctl haproxy acme status)"
check "the extension runs" "^State: +running" "$out"
THUMB="$(awk '/^Thumbprint:/ {print $2}' <<<"$out")"
[ -n "$THUMB" ] || fail "no account thumbprint: $out"
check "no configuration yet" "No configuration saved" "$out"
echo "  ok: the account key exists (thumbprint $THUMB)"

# --- the configuration, then HAProxy's ---------------------------------
python3 - "$WORKDIR" "$P_ACME" "$P_SHIM" > "$WORKDIR/acme.json" <<'PY'
import json, sys
w, acme, shim = sys.argv[1:4]
print(json.dumps({
  "account": {"directory": "https://10.0.2.2:%s/dir" % acme, "directory_ca": open(w + "/test-ca.pem").read(),
              "email": "ops@site.test", "accept_terms": True},
  "certificates": [
    {"name": "site", "domains": ["site.test", "www.site.test"]},
    {"name": "wild", "domains": ["*.wild.test", "wild.test"], "challenge": "dns-01", "dns_provider": "lab", "key_type": "ec384"},
  ],
  "dns_providers": [{"name": "lab", "type": "httpreq", "settings": {"HTTPREQ_ENDPOINT": "http://10.0.2.2:%s" % shim},
                     "propagation_wait_seconds": 2}],
}, indent=2))
PY
if ctl haproxy acme check <(sed 's/"httpreq"/"nope"/' "$WORKDIR/acme.json") >/dev/null 2>&1; then
  fail "an unknown DNS provider type passed the check"
fi
out="$(ctl haproxy acme apply "$WORKDIR/acme.json")" || fail "apply: $out"
echo "  ok: configuration applied"

ctl haproxy get-config > "$WORKDIR/haproxy.cfg"
rule="$(ctl haproxy acme status | sed -n 's/^HTTP-01 rule: //p')"
cat >> "$WORKDIR/haproxy.cfg" <<EOF

frontend acme-http
    bind *:80
    $rule
    http-request redirect scheme https code 301

frontend tls-file
    bind *:443 ssl crt /etc/haproxy/acme/site.pem
    http-request return status 200 content-type text/plain string "tls-file\n"

frontend tls-dir
    bind *:8443 ssl crt /etc/haproxy/acme/
    http-request return status 200 content-type text/plain string "tls-dir\n"
EOF
out="$(ctl haproxy apply-config "$WORKDIR/haproxy.cfg" 2>&1)" || fail "a configuration using the stand-ins was refused: $out"
echo "  ok: haproxy.cfg referencing the certificates applies before they're obtained"
got="$(curl -s -m 3 "http://127.0.0.1:$P_80/.well-known/acme-challenge/tok123")"
[ "$got" = "tok123.$THUMB" ] || fail "the HTTP-01 rule answered '$got', want tok123.$THUMB"
echo "  ok: HAProxy answers HTTP-01 challenges with the account's thumbprint"

# --- obtained -------------------------------------------------------------
deadline=$((SECONDS + 120))
until out="$(ctl haproxy acme status)" && grep -Eq "^site +valid" <<<"$out" && grep -Eq "^wild +valid" <<<"$out"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the certificates weren't obtained: $out"
  sleep 2
done
check "the account is known" "^Account: +https://10.0.2.2:$P_ACME/" "$out"
echo "  ok: both certificates obtained"

curl -s "https://127.0.0.1:$P_ACME_MGMT/roots/0" -k > "$WORKDIR/pebble-root.pem"
grep -q "BEGIN CERTIFICATE" "$WORKDIR/pebble-root.pem" || fail "no Pebble root"
served() { # served PORT NAME -> the body, verified against Pebble's root
  curl -s -m 5 --cacert "$WORKDIR/pebble-root.pem" --resolve "$2:$1:127.0.0.1" "https://$2:$1/"
}
serial() { # serial PORT NAME
  openssl s_client -connect "127.0.0.1:$1" -servername "$2" </dev/null 2>/dev/null | openssl x509 -noout -serial
}
[ "$(served "$P_443" www.site.test)" = "tls-file" ] || fail "site isn't served on :443 (verified against Pebble's root)"
[ "$(served "$P_8443" site.test)" = "tls-dir" ] || fail "site isn't served through the directory on :8443"
[ "$(served "$P_8443" any.wild.test)" = "tls-dir" ] || fail "the wildcard isn't served through the directory on :8443"
grep -q "/set-txt _acme-challenge.wild.test." "$WORKDIR/shim.log" || fail "DNS-01 never went through the provider"
echo "  ok: both served, valid under Pebble's root - HTTP-01 by HAProxy, the wildcard over DNS-01"

# --- a renewal, without a reload -------------------------------------------
reloads() { curl -s "http://127.0.0.1:$P_METRICS/metrics" | awk '/^janus_haproxy_reloads_total / {print $2}'; }
before_reloads="$(reloads)"
before_serial="$(serial "$P_443" site.test)"
ctl haproxy acme renew site >/dev/null || fail "renew"
deadline=$((SECONDS + 90))
until [ "$(serial "$P_443" site.test)" != "$before_serial" ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the renewed certificate is never served"
  sleep 2
done
[ "$(reloads)" = "$before_reloads" ] || fail "HAProxy was reloaded for a renewal ($before_reloads -> $(reloads))"
grep -aq "certificate site swapped into HAProxy" "$LOG" || fail "no swap in the node's log"
[ "$(served "$P_8443" site.test)" = "tls-dir" ] || fail "the renewed certificate isn't served on the directory bind"
echo "  ok: renewed and swapped into the running HAProxy, no reload"

m="$(curl -s "http://127.0.0.1:$P_METRICS/metrics")"
check "exporter: the account" '^janus_acme_account_registered 1$' "$m"
check "exporter: site valid" '^janus_acme_certificate_state\{name="site",state="valid"\} 1$' "$m"
check "exporter: wild never failed" '^janus_acme_certificate_failures\{name="wild"\} 0$' "$m"
echo "  ok: the exporter reports them"

# --- after a reboot ---------------------------------------------------------
serial_now="$(serial "$P_443" site.test)"
orders="$(grep -c "certificate .* ordering it" "$LOG" || true)"
ctl system reboot >/dev/null 2>&1 || true
sleep 3
deadline=$((SECONDS + TIMEOUT))
until grep -aq "listening on .* (mTLS required)" "$LOG" && [ "$(grep -ac "listening on .* (mTLS required)" "$LOG")" -ge 2 ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the node didn't come back from its reboot"
  sleep 1
done
wait_up
[ "$(serial "$P_443" site.test)" = "$serial_now" ] || fail "not the same certificate after the reboot"
[ "$(served "$P_8443" x.wild.test)" = "tls-dir" ] || fail "the wildcard isn't served after the reboot"
got="$(curl -s -m 3 "http://127.0.0.1:$P_80/.well-known/acme-challenge/again")"
[ "$got" = "again.$THUMB" ] || fail "after the reboot the HTTP-01 rule answers '$got'"
sleep 5
[ "$(grep -c "certificate .* ordering it" "$LOG" || true)" = "$orders" ] || fail "certificates ordered again after the reboot"
echo "  ok: after a reboot - same certificates, same thumbprint, nothing ordered"

# --- the node's trust store ---------------------------------------------
# Without directory_ca, janus-acme checks the CA against the node's trust
# store, as for Let's Encrypt - Pebble's test CA isn't in it. Go reads
# /etc/ssl/certs for it: a denial a real node hit, not this test.
python3 - "$WORKDIR/acme.json" > "$WORKDIR/acme-system.json" <<'PY'
import json, sys
c = json.load(open(sys.argv[1]))
del c["account"]["directory_ca"]
print(json.dumps(c))
PY
out="$(ctl haproxy acme apply "$WORKDIR/acme-system.json")" || fail "apply without directory_ca: $out"
ctl haproxy acme renew site >/dev/null || fail "renew without directory_ca"
deadline=$((SECONDS + 90))
until out="$(ctl haproxy acme status)" && grep -q "x509\|unknown authority" <<<"$out"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "Pebble's CA wasn't refused by the node's trust store: $out"
  sleep 2
done
echo "  ok: without directory_ca the node's trust store applies - Pebble's test CA refused"

denials="$(grep -a "avc:.*denied" "$LOG".* || true)"
if [ -n "$denials" ]; then
  if [ "${ACME_DISCOVERY:-}" = 1 ]; then
    echo "--- AVC denials (discovery) ---"
    sed -E 's/.*avc:  denied  //; s/pid=[0-9]+ //; s/ino=[0-9]+ //; s/ permissive=[01]//' <<<"$denials" | sort | uniq -c
  else
    echo "$denials" >&2
    fail "AVC denials under enforcing"
  fi
fi
echo "ACME test OK: HTTP-01 answered by HAProxy and a wildcard over DNS-01 from Pebble, stand-ins first, a renewal swapped in without a reload, kept across a reboot, zero AVC denials"
