#!/usr/bin/env bash
# Let's Encrypt in a cluster (docs/guide/clusters.md "Let's Encrypt"), on
# two real enforcing nodes with the letsencrypt extension, against Pebble
# on the host - a cluster of a janusctl fleet (no Controller: the same
# engine).
#
# lb-1 has a certificate configured; Pebble validates HTTP-01 on lb-2's
# port 80 only. Before the cluster, lb-1 can't get it (lb-2 doesn't
# answer). The cluster made from lb-1: lb-2 pulls lb-1's configuration,
# is told lb-1 orders and answers HTTP-01 with lb-1's account. lb-1's
# order then goes through - validated on lb-2 -, lb-2 orders nothing
# (its renew refused, nothing obtained in its log), and a check has it
# pull the certificate: lb-2 serves it. A renewal on lb-1 reaches lb-2
# at the next check. lb-1 removed, lb-2 orders on its own with the
# configuration it pulled. No AVC denial.
#
# Usage: hack/qemu-cluster-acme-test.sh <disk.img> <janusctl-bin> <pebble-bin-dir>
set -euo pipefail
export PATH="$PATH:/usr/sbin:/sbin"

usage="usage: $0 <disk.img> <janusctl-bin> <pebble-bin-dir>"
SRC_DISK="${1:?$usage}"
CTL_BIN="$(realpath "${2:?$usage}")"
PEBBLE_DIR="$(realpath "${3:?$usage}")"
TIMEOUT="${QEMU_CLUSTER_ACME_TIMEOUT:-150}"
BASE="${QEMU_CLUSTER_ACME_BASE_PORT:-$((19110 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
MCAST="${QEMU_CLUSTER_ACME_MCAST:-230.0.0.$((RANDOM % 200 + 20)):$((20000 + RANDOM % 20000))}"
OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
NODES="1 2"
# Node i: health BASE+10i, API +1, :80 +2, :443 +3. Pebble and its DNS
# from BASE+50.
P_ACME=$((BASE + 50)) P_ACME_MGMT=$((BASE + 51)) P_DNS=$((BASE + 52)) P_DNS_MGMT=$((BASE + 53))

WORKDIR="$(mktemp -d)"
declare -A PID
cleanup() {
  for n in "${!PID[@]}"; do kill "${PID[$n]}" 2>/dev/null || true; done
  if [ "${QEMU_CLUSTER_ACME_KEEP:-}" = 1 ]; then echo "kept $WORKDIR" >&2; else rm -rf "$WORKDIR"; fi
}
trap cleanup EXIT

fail() {
  echo "Cluster ACME test FAILED: $*" >&2
  [ -f "$WORKDIR/pebble.log" ] && { echo "--- pebble ---" >&2; tail -30 "$WORKDIR/pebble.log" >&2; }
  for i in $NODES; do
    [ -f "$WORKDIR/$i.log" ] && { echo "--- console $i ---" >&2; tail -60 "$WORKDIR/$i.log" >&2; }
  done
  exit 1
}

wait_for() { # wait_for SECONDS WHAT COMMAND...: polls COMMAND every second
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

# --- Pebble, on the host: HTTP-01 validated on lb-2's :80 only -----------
not_before="$(date -u -d '-1 hour' +%Y%m%d%H%M%SZ)"
not_after="$(date -u -d '+1 day' +%Y%m%d%H%M%SZ)"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -not_before "$not_before" -not_after "$not_after" \
  -subj "/CN=Cluster ACME test CA" -keyout "$WORKDIR/ca.key" -out "$WORKDIR/test-ca.pem" 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=pebble" \
  -keyout "$WORKDIR/pebble.key" -out "$WORKDIR/pebble.csr" 2>/dev/null
printf 'subjectAltName=IP:10.0.2.2,IP:127.0.0.1\n' > "$WORKDIR/san.ext"
openssl x509 -req -in "$WORKDIR/pebble.csr" -CA "$WORKDIR/test-ca.pem" -CAkey "$WORKDIR/ca.key" -CAcreateserial \
  -not_before "$not_before" -not_after "$not_after" -extfile "$WORKDIR/san.ext" -out "$WORKDIR/pebble.pem" 2>/dev/null
cat > "$WORKDIR/pebble.json" <<EOF
{"pebble": {"listenAddress": "127.0.0.1:$P_ACME", "managementListenAddress": "127.0.0.1:$P_ACME_MGMT",
  "certificate": "$WORKDIR/pebble.pem", "privateKey": "$WORKDIR/pebble.key",
  "httpPort": $((BASE + 22)), "tlsPort": $((BASE + 54)), "ocspResponderURL": "", "externalAccountBindingRequired": false,
  "retryAfter": {"authz": 1, "order": 1}, "keyAlgorithm": "ecdsa",
  "profiles": {"default": {"description": "90 days", "validityPeriod": 7776000}}}}
EOF
"$PEBBLE_DIR/pebble-challtestsrv" -defaultIPv6 "" -dnsserver "127.0.0.1:$P_DNS" -http01 "" -https01 "" \
  -tlsalpn01 "" -doh "" -management "127.0.0.1:$P_DNS_MGMT" > "$WORKDIR/challtestsrv.log" 2>&1 &
PID[challtestsrv]=$!
PEBBLE_VA_NOSLEEP=1 PEBBLE_WFE_NONCEREJECT=0 "$PEBBLE_DIR/pebble" -config "$WORKDIR/pebble.json" \
  -dnsserver "127.0.0.1:$P_DNS" > "$WORKDIR/pebble.log" 2>&1 &
PID[pebble]=$!
wait_for 20 "Pebble answering" curl -sk -o /dev/null "https://127.0.0.1:$P_ACME/dir"
echo "  ok: Pebble runs, validating HTTP-01 on lb-2's port 80"

# --- two nodes ------------------------------------------------------------
node() {
  local i=$1
  cp --sparse=always "$SRC_DISK" "$WORKDIR/$i.img"
  cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/$i.vars"
  qemu-system-x86_64 -accel kvm -accel tcg \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$WORKDIR/$i.vars" \
    -drive file="$WORKDIR/$i.img",format=raw,if=virtio \
    -nographic -display none -monitor none -m 512M \
    -netdev "user,id=net0,hostfwd=tcp::$((BASE + 10 * i))-:8080,hostfwd=tcp::$((BASE + 10 * i + 1))-:9505,hostfwd=tcp:127.0.0.1:$((BASE + 10 * i + 2))-:80,hostfwd=tcp::$((BASE + 10 * i + 3))-:443" \
    -device "virtio-net-pci,netdev=net0,mac=52:54:00:aa:02:0$i" \
    -netdev "socket,id=lan,mcast=$MCAST,localaddr=127.0.0.1" \
    -device "virtio-net-pci,netdev=lan,mac=52:54:00:bb:02:0$i" \
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
  {"name": "eth0", "mac": "52:54:00:aa:02:0$i", "mode": "ADDRESSING_MODE_DHCP"},
  {"name": "eth1", "mac": "52:54:00:bb:02:0$i", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.168.88.$i/24"]}
]}
EOF
  out="$(own "$i" network apply "$WORKDIR/$i-net.json" 2>&1)" || fail "network apply on $i: $out"
done
alice() { JANUSCONFIG="$WORKDIR/alice/janusctl.json" "$CTL_BIN" "$@"; }
mkdir -p "$WORKDIR/alice"
alice -context lab fleet init -name lab -issuer alice-laptop -user alice -yes "$WORKDIR/kit.age" >"$WORKDIR/passphrase" 2>/dev/null || fail "fleet init"
for i in $NODES; do
  out="$(alice fleet adopt "lb-$i" -endpoint "127.0.0.1:$((BASE + 10 * i + 1))" -ca "$WORKDIR/$i-ca.crt" -cert "$WORKDIR/$i-admin.crt" -key "$WORKDIR/$i-admin.key" 2>&1)" || fail "adopting lb-$i: $out"
done
THUMB1="$(alice -n lb-1 haproxy acme status | awk '/^Thumbprint:/ {print $2}')"
THUMB2="$(alice -n lb-2 haproxy acme status | awk '/^Thumbprint:/ {print $2}')"
[ -n "$THUMB1" ] && [ -n "$THUMB2" ] && [ "$THUMB1" != "$THUMB2" ] || fail "each node's own account: '$THUMB1' '$THUMB2'"
echo "  ok: two nodes, adopted by alice's fleet, each with its own ACME account"

# --- lb-1: a certificate, HAProxy serving it ------------------------------
python3 - "$WORKDIR" "$P_ACME" > "$WORKDIR/acme.json" <<'PY'
import json, sys
w, acme = sys.argv[1:3]
print(json.dumps({
  "account": {"directory": "https://10.0.2.2:%s/dir" % acme, "directory_ca": open(w + "/test-ca.pem").read(),
              "email": "ops@site.test", "accept_terms": True},
  "certificates": [{"name": "site", "domains": ["site.test"]}],
}, indent=2))
PY
out="$(alice -n lb-1 haproxy acme apply "$WORKDIR/acme.json" 2>&1)" || fail "lb-1's ACME configuration: $out"
RULE="$(alice -n lb-1 haproxy acme status | sed -n 's/^HTTP-01 rule: //p')"
[ -n "$RULE" ] || fail "no HTTP-01 rule"
cat > "$WORKDIR/v1.cfg" <<EOF
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

frontend fe_health
    bind :8080
    http-request return status 200 content-type text/plain lf-string "%[var(proc.janus_member)]\n"

frontend fe_http
    bind :80
    $RULE
    http-request return status 404

frontend fe_https
    bind :443 ssl crt /etc/haproxy/acme/site.pem
    http-request return status 200 content-type text/plain lf-string "%[var(proc.janus_member)]\n"
EOF
out="$(alice -n lb-1 haproxy apply-config "$WORKDIR/v1.cfg" 2>&1)" || fail "apply-config on lb-1: $out"
# Pebble validates on lb-2, which doesn't answer yet: no certificate.
sleep 5
check "lb-1's certificate not obtained before the cluster" "site +pending|site .*fail|pending" "$(alice -n lb-1 haproxy acme status)"
echo "  ok: lb-1's certificate configured, its HTTP-01 challenge validated on lb-2 - which doesn't answer it yet"

# --- the cluster -------------------------------------------------------------
out="$(alice cluster create edge -source lb-1 -address lb-1=192.168.88.1 -address lb-2=192.168.88.2 lb-1 lb-2 2>&1)" || fail "cluster create: $out"
check "the journal says lb-1 orders" "ordered by lb-1" "$out"
st2="$(alice -n lb-2 haproxy acme status)"
check "lb-2 is told lb-1 orders" "^Ordered by: +lb-1" "$st2"
check "lb-2 answers HTTP-01 with lb-1's account" "^Thumbprint: +$THUMB1" "$st2"
check "lb-2 holds lb-1's configuration" "site" "$st2"
token="test-token-$RANDOM"
[ "$(curl -s "http://127.0.0.1:$((BASE + 22))/.well-known/acme-challenge/$token")" = "$token.$THUMB1" ] || fail "lb-2's HTTP-01 answer isn't lb-1's account's"
if out="$(alice -n lb-2 haproxy acme renew site 2>&1)"; then fail "lb-2 renewed what lb-1 orders: $out"; fi
check "lb-2's renew refused" "lb-1 orders this cluster's certificates" "$out"
echo "  ok: cluster edge - lb-2 pulled lb-1's configuration, orders nothing, answers HTTP-01 with lb-1's account"

# --- lb-1 orders, validated on lb-2; lb-2 pulls ------------------------------
alice -n lb-1 haproxy acme renew site >/dev/null || fail "renew site on lb-1"
valid1() { alice -n lb-1 haproxy acme status | grep -Eq '^ *site +valid'; }
wait_for 90 "lb-1's certificate obtained (validated on lb-2)" valid1
out="$(alice cluster check edge 2>&1 || true)"
issued_on() { echo | openssl s_client -connect "127.0.0.1:$((BASE + 10 * $1 + 3))" -servername site.test 2>/dev/null | openssl x509 -noout -issuer -serial 2>/dev/null; }
i1="$(issued_on 1)"
i2="$(issued_on 2)"
check "lb-1 serves Pebble's certificate" "issuer=.*Pebble" "$i1"
[ "$i1" = "$i2" ] || fail "lb-2 doesn't serve lb-1's certificate after the check: lb-1 $i1, lb-2 $i2 ($out)"
if alice -n lb-2 system logs letsencrypt 2>/dev/null | grep -Eiq 'obtained|ordering'; then fail "lb-2 ordered: $(alice -n lb-2 system logs letsencrypt)"; fi
echo "  ok: lb-1 obtained the certificate - its challenge answered by lb-2 -, lb-2 pulled it at the check and serves it, having ordered nothing"

# --- a renewal on lb-1 reaches lb-2 --------------------------------------------
before="$(issued_on 1)"
alice -n lb-1 haproxy acme renew site >/dev/null || fail "renew site again on lb-1"
renewed() { [ "$(issued_on 1)" != "$before" ] && [ -n "$(issued_on 1)" ]; }
wait_for 90 "lb-1's renewal" renewed
out="$(alice cluster check edge 2>&1 || true)"
[ "$(issued_on 2)" = "$(issued_on 1)" ] || fail "lb-2 after the renewal's check: $(issued_on 2), lb-1 $(issued_on 1) ($out)"
check "the journal says lb-2 pulled the renewal" "lb-2 pulled lb-1's renewed certificate\(s\): site" "$(alice cluster journal edge)"
echo "  ok: lb-1's renewal reached lb-2 at the next check"

# --- lb-1 leaves: lb-2 orders on its own -----------------------------------------
out="$(alice cluster remove edge lb-1 2>&1)" || fail "remove lb-1: $out"
st2="$(alice -n lb-2 haproxy acme status)"
if grep -q '^Ordered by:' <<<"$st2"; then fail "lb-2 still follows after lb-1 left: $st2"; fi
check "lb-2 answers with its own account again" "^Thumbprint: +$THUMB2" "$st2"
before="$(issued_on 2)"
alice -n lb-2 haproxy acme renew site >/dev/null || fail "renew site on lb-2"
own_order() { [ "$(issued_on 2)" != "$before" ] && [ -n "$(issued_on 2)" ]; }
wait_for 90 "lb-2's own order (with the configuration it pulled)" own_order
echo "  ok: lb-1 out, lb-2 orders on its own - with the configuration it pulled - and its challenge is answered by itself"

for i in $NODES; do
  if grep -a "avc:.*denied" "$WORKDIR/$i.log"; then fail "AVC denials on node $i"; fi
done
echo "Cluster ACME test PASSED: one member orders, validated on another; the other orders nothing, pulls the certificate and its renewal; it takes over when the orderer leaves"
