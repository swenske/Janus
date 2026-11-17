#!/usr/bin/env bash
# "Ordered by this Controller" set on a cluster (docs/guide/clusters.md
# "Let's Encrypt", docs/guide/letsencrypt-controller.md), on two real
# enforcing nodes with the letsencrypt extension, a native dashboardd and
# Pebble on the host.
#
# The nodes reach nothing outside (restricted user networks: the
# Controller's calls in, port 80 for the CA's validation) and each other
# on a multicast LAN; they take the Controller's fleet. The Controller's
# ordering set up (its account, a name rule, the test CA); a cluster made
# of them through its API, lb-1 the source. The cluster's Let's Encrypt
# set through PUT /api/clusters/{id}/letsencrypt, the way to the CA
# "ordered by this Controller": both members get the mode and the
# Controller's account (lb-2 follows lb-1, answering HTTP-01 for it); a
# member's own apply is refused. The Controller orders lb-1's
# certificate - Pebble validates it on lb-2 - and lb-2 pulls it. The way
# to the CA tried on both (check-path: ordered, nothing to reach). The
# orderer changed to lb-2: it keeps the mode, the Controller orders for
# it - a renewal asked through the cluster - and lb-1, following it now,
# pulls it. A certificate the Controller's name rules don't cover fails
# and waits; the rule added, the cluster's Renew has it ordered at once.
# No AVC denial on either node.
#
# Usage: hack/qemu-cluster-acme-ordered-test.sh <disk.img> <janusctl-bin> <pebble-bin-dir> <dashboardd-bin> <janus-acme-bin>
set -euo pipefail
# shellcheck source=state-read.sh
. "$(dirname "$0")/state-read.sh"
export PATH="$PATH:/usr/sbin:/sbin"

usage="usage: $0 <disk.img> <janusctl-bin> <pebble-bin-dir> <dashboardd-bin> <janus-acme-bin>"
SRC_DISK="${1:?$usage}"
CTL_BIN="$(realpath "${2:?$usage}")"
PEBBLE_DIR="$(realpath "${3:?$usage}")"
DASHBOARDD="$(realpath "${4:?$usage}")"
ACME_CLIENT="$(realpath "${5:?$usage}")"
TIMEOUT="${QEMU_CLUSTER_ORDERED_TIMEOUT:-150}"
BASE="${QEMU_CLUSTER_ORDERED_BASE_PORT:-$((19210 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
MCAST="${QEMU_CLUSTER_ORDERED_MCAST:-230.0.0.$((RANDOM % 200 + 20)):$((20000 + RANDOM % 20000))}"
OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
NODES="1 2"
# Node i: health BASE+10i, API +1, :80 +2, :443 +3. The Controller, Pebble
# and its DNS from BASE+40.
P_DASH=$((BASE + 40)) P_REG=$((BASE + 41))
P_ACME=$((BASE + 50)) P_ACME_MGMT=$((BASE + 51)) P_DNS=$((BASE + 52)) P_DNS_MGMT=$((BASE + 53))
API="https://127.0.0.1:$P_DASH"

WORKDIR="$(mktemp -d)"
declare -A PID
cleanup() {
  for n in "${!PID[@]}"; do kill "${PID[$n]}" 2>/dev/null || true; done
  for n in "${!PID[@]}"; do wait "${PID[$n]}" 2>/dev/null || true; done
  if [ "${QEMU_CLUSTER_ORDERED_KEEP:-}" = 1 ]; then echo "kept $WORKDIR" >&2; else rm -rf "$WORKDIR"; fi
}
trap cleanup EXIT

fail() {
  echo "Cluster ACME ordered test FAILED: $*" >&2
  for f in pebble.log dash.log; do
    [ -f "$WORKDIR/$f" ] && { echo "--- $f ---" >&2; tail -40 "$WORKDIR/$f" >&2; }
  done
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

# --- Pebble, on the host: HTTP-01 validated on lb-2's port 80 -------------
not_before="$(date -u -d '-1 hour' +%Y%m%d%H%M%SZ)"
not_after="$(date -u -d '+1 day' +%Y%m%d%H%M%SZ)"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -not_before "$not_before" -not_after "$not_after" \
  -subj "/CN=Cluster ordered test CA" -keyout "$WORKDIR/ca.key" -out "$WORKDIR/test-ca.pem" 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=pebble" \
  -keyout "$WORKDIR/pebble.key" -out "$WORKDIR/pebble.csr" 2>/dev/null
printf 'subjectAltName=IP:127.0.0.1\n' > "$WORKDIR/san.ext"
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

# --- two nodes that reach nothing outside ----------------------------------
node() {
  local i=$1
  cp --sparse=always "$SRC_DISK" "$WORKDIR/$i.img"
  cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/$i.vars"
  qemu-system-x86_64 -accel kvm -accel tcg \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$WORKDIR/$i.vars" \
    -drive file="$WORKDIR/$i.img",format=raw,if=virtio \
    -nographic -display none -monitor none -m 512M \
    -netdev "user,id=net0,restrict=on,hostfwd=tcp::$((BASE + 10 * i))-:8080,hostfwd=tcp::$((BASE + 10 * i + 1))-:9505,hostfwd=tcp:127.0.0.1:$((BASE + 10 * i + 2))-:80,hostfwd=tcp::$((BASE + 10 * i + 3))-:443" \
    -device "virtio-net-pci,netdev=net0,mac=52:54:00:aa:03:0$i" \
    -netdev "socket,id=lan,mcast=$MCAST,localaddr=127.0.0.1" \
    -device "virtio-net-pci,netdev=lan,mac=52:54:00:bb:03:0$i" \
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
for n in $NODES; do state_creds "$WORKDIR/$n.img" "$WORKDIR/$n" || fail "node $n's pki/ from STATE"; done
ctl() { # ctl INDEX args... - the node's own first-boot admin, straight to it
  local i=$1
  shift
  "$CTL_BIN" -endpoint "127.0.0.1:$((BASE + 10 * i + 1))" -ca "$WORKDIR/$i-ca.crt" -cert "$WORKDIR/$i-admin.crt" -key "$WORKDIR/$i-admin.key" "$@"
}
for i in $NODES; do
  cat > "$WORKDIR/$i-net.json" <<EOF
{"hostname": "lb-$i", "interfaces": [
  {"name": "eth0", "mac": "52:54:00:aa:03:0$i", "mode": "ADDRESSING_MODE_DHCP"},
  {"name": "eth1", "mac": "52:54:00:bb:03:0$i", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.168.89.$i/24"]}
]}
EOF
  out="$(ctl "$i" network apply "$WORKDIR/$i-net.json" 2>&1)" || fail "network apply on $i: $out"
  check "lb-$i's letsencrypt extension runs" "^State: +running" "$(ctl "$i" haproxy acme status)"
done
echo "  ok: two nodes up, reaching nothing outside, on their LAN as lb-1 and lb-2"

# --- the Controller, its fleet, its ordering ---------------------------------
"$DASHBOARDD" -addr "127.0.0.1:$P_DASH" -register-addr "127.0.0.1:$P_REG" -acme-client "$ACME_CLIENT" \
  -data-dir "$WORKDIR/data" -image-factory "" > "$WORKDIR/dash.log" 2>&1 &
PID[dash]=$!
wait_for 30 "the Controller answering" curl -sk -o /dev/null "$API/api/auth/status"
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
api POST /api/auth/setup '{"password":"cluster-ordered-admin-pw","mfa_required":"nobody"}' >/dev/null || fail "setup"
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
  [ "$(api GET /api/fleet | python3 -c "import json,sys; d=json.load(sys.stdin); print(sum(1 for i in sys.argv[1:] if d.get('nodes',{}).get(i,{}).get('state')=='trusted'))" "${ID[@]}")" = 2 ]
}
wait_for 120 "the nodes to trust the fleet" trusted
api PUT /api/acme "{\"email\": \"ops@site.test\", \"accept_terms\": true,
  \"directories\": [\"https://127.0.0.1:$P_ACME/dir\"], \"name_policy\": [{\"suffixes\": [\"site.test\"]}]}" > "$WORKDIR/acme.json" || fail "the Controller's ordering"
check "the Controller's account set up" '"ready": ?true' "$(cat "$WORKDIR/acme.json")"
THUMB="$(api GET "/nodes/${ID[1]}/api/haproxy/acme/controller" | jq_ 'd["ordering"]["thumbprint"]')"
[ "${#THUMB}" = 43 ] || fail "no account thumbprint: $THUMB"
echo "  ok: the Controller's fleet on both nodes, its ordering set up"

# --- lb-1: a certificate (its stand-in first), HAProxy serving it ------------
python3 - "$WORKDIR" "$P_ACME" > "$WORKDIR/lb1-acme.json" <<'PY'
import json, sys
w, acme = sys.argv[1:3]
print(json.dumps({"account": {"directory": "https://127.0.0.1:%s/dir" % acme, "directory_ca": open(w + "/test-ca.pem").read()},
  "certificates": [{"name": "site", "domains": ["site.test"]}]}, indent=2))
PY
out="$(ctl 1 haproxy acme apply "$WORKDIR/lb1-acme.json" 2>&1)" || fail "lb-1's own Let's Encrypt: $out"
RULE="$(ctl 1 haproxy acme status | sed -n 's/^HTTP-01 rule: //p')"
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
out="$(ctl 1 haproxy apply-config "$WORKDIR/v1.cfg" 2>&1)" || fail "apply-config on lb-1: $out"

# --- the cluster, its Let's Encrypt ordered by the Controller ----------------
create="$(python3 -c 'import json,sys; ids=sys.argv[1:]; print(json.dumps({"name": "edge", "source": ids[0], "members": [{"node_id": ids[i], "name": "lb-%d" % (i+1), "address": "192.168.89.%d" % (i+1)} for i in range(2)]}))' "${ID[1]}" "${ID[2]}")"
CID="$(api POST /api/clusters "$create" | jq_ 'd["id"]')" || fail "create the cluster"
idle() { [ "$(api GET "/api/clusters/$CID" | jq_ 'd["busy"]')" = False ]; }
wait_for 180 "the cluster made" idle
err="$(api GET "/api/clusters/$CID" | jq_ 'd.get("error","")')"
[ -z "$err" ] || fail "the cluster's creation: $err"
python3 - "$WORKDIR" "$P_ACME" "$THUMB" > "$WORKDIR/cluster-acme.json" <<'PY'
import json, sys
w, acme, thumb = sys.argv[1:4]
print(json.dumps({"config": {"account": {"directory": "https://127.0.0.1:%s/dir" % acme, "directory_ca": open(w + "/test-ca.pem").read()},
  "certificates": [{"name": "site", "domains": ["site.test"]}],
  "path": {"mode": "ACME_PATH_MODE_CONTROLLER_ORDERED", "account_thumbprint": thumb}}, "note": "ordered by the Controller"}))
PY
api PUT "/api/clusters/$CID/letsencrypt" "$(cat "$WORKDIR/cluster-acme.json")" >/dev/null || fail "the cluster's Let's Encrypt"
wait_for 180 "the cluster's Let's Encrypt applied" idle
view="$(api GET "/api/clusters/$CID")"
[ -z "$(jq_ 'd.get("error","")' <<<"$view")" ] || fail "configuring the cluster's Let's Encrypt: $view"
check "the cluster manages Let's Encrypt" "\"acme\"" "$(jq_ 'json.dumps(d["managed"])' <<<"$view")"
for i in $NODES; do
  check "lb-$i ordered by the Controller" "ACME_PATH_MODE_CONTROLLER_ORDERED" "$(ctl "$i" haproxy acme get)"
done
check "lb-2 follows lb-1" "^Ordered by: +lb-1" "$(ctl 2 haproxy acme status)"
answer() { curl -s -m 3 "http://127.0.0.1:$((BASE + 22))/.well-known/acme-challenge/tok"; }
answers() { [ "$(answer)" = "tok.$THUMB" ]; }
wait_for 30 "lb-2 answering HTTP-01 for the Controller's account" answers
if out="$(ctl 2 haproxy acme apply "$WORKDIR/lb1-acme.json" 2>&1)"; then fail "lb-2's own apply accepted under the cluster's lock: $out"; fi
check "lb-2's own apply refused" "managed by cluster edge" "$out"
echo "  ok: the cluster's Let's Encrypt ordered by the Controller - both members in that mode, lb-2 answering for its account, locked"

# --- the Controller orders lb-1's certificate; lb-2 pulls it -------------------
api POST /api/acme/run >/dev/null || fail "ordering run"
valid() { ctl "$1" haproxy acme status | grep -Eq '^site +valid'; }
wait_for 180 "the Controller ordering lb-1's certificate (validated on lb-2)" valid 1
api POST "/api/clusters/$CID/check" >/dev/null || fail "check"
issued_on() { echo | openssl s_client -connect "127.0.0.1:$((BASE + 10 * $1 + 3))" -servername site.test 2>/dev/null | openssl x509 -noout -issuer -serial 2>/dev/null; }
pulled() { [ -n "$(issued_on 1)" ] && [ "$(issued_on 1)" = "$(issued_on 2)" ]; }
wait_for 60 "lb-2 serving lb-1's certificate" pulled
check "Pebble issued it" "issuer=.*Pebble" "$(issued_on 1)"
grep -q "lets encrypt: node lb-1, certificate site: ordered and installed" "$WORKDIR/dash.log" || fail "the Controller doesn't say it ordered for lb-1"
if grep -q "lets encrypt: node lb-2, certificate" "$WORKDIR/dash.log"; then fail "the Controller ordered for lb-2, a follower: $(grep 'node lb-2' "$WORKDIR/dash.log")"; fi
paths="$(api POST "/api/clusters/$CID/letsencrypt/check-path" '{}')" || fail "check-path"
check "the way tried on both members" '"member": ?"lb-2"' "$paths"
echo "  ok: the Controller ordered lb-1's certificate - validated on lb-2 -, lb-2 pulled it and serves it; the way tried on both"

# --- the orderer changes: lb-2 keeps the mode, the Controller orders for it ---
before="$(issued_on 2)"
api PATCH "/api/clusters/$CID" '{"secrets_from": "lb-2"}' >/dev/null || fail "lb-2 the orderer"
api POST "/api/clusters/$CID/check" >/dev/null || fail "check"
if ctl 2 haproxy acme status | grep -q '^Ordered by:'; then fail "lb-2 still follows: $(ctl 2 haproxy acme status)"; fi
check "lb-1 follows lb-2" "^Ordered by: +lb-2" "$(ctl 1 haproxy acme status)"
for i in $NODES; do
  check "lb-$i still ordered by the Controller" "ACME_PATH_MODE_CONTROLLER_ORDERED" "$(ctl "$i" haproxy acme get)"
done
renewed="$(api POST "/api/clusters/$CID/letsencrypt/renew" '{"names": ["site"]}')" || fail "renew through the cluster"
check "the renewal asked of lb-2" '"site"' "$renewed"
new_on_2() { [ -n "$(issued_on 2)" ] && [ "$(issued_on 2)" != "$before" ]; }
wait_for 180 "the Controller ordering for lb-2, the new orderer" new_on_2
grep -q "lets encrypt: node lb-2, certificate site: ordered and installed" "$WORKDIR/dash.log" || fail "the Controller doesn't say it ordered for lb-2"
api POST "/api/clusters/$CID/check" >/dev/null || fail "check"
wait_for 60 "lb-1 serving lb-2's certificate" pulled
echo "  ok: lb-2 the orderer - still ordered by the Controller, which ordered its renewal; lb-1 follows and pulled it"

# --- an order that failed, renewed through the cluster: ordered at once -----------
# A certificate the Controller's name rules don't cover fails and waits
# 5 minutes; the rule added, Renew on the cluster orders it at once - as
# "Order what's due now" does (v2026.11.17's lab, #660's finding 4: the
# cluster's Renew woke the loop alone, which waited for the next attempt).
python3 - "$WORKDIR/cluster-acme.json" > "$WORKDIR/cluster-acme-other.json" <<'PY'
import json, sys
c = json.load(open(sys.argv[1]))
c["config"]["certificates"].append({"name": "other", "domains": ["other.test"]})
c["note"] = "another certificate"
print(json.dumps(c))
PY
api PUT "/api/clusters/$CID/letsencrypt" "$(cat "$WORKDIR/cluster-acme-other.json")" >/dev/null || fail "the cluster's Let's Encrypt with another certificate"
wait_for 180 "the cluster's Let's Encrypt applied again" idle
api POST /api/acme/run >/dev/null || fail "ordering run"
refused() { grep -q "lets encrypt: node lb-2, certificate other: .*doesn't allow the name" "$WORKDIR/dash.log"; }
wait_for 60 "the Controller refusing other.test - no name rule covers it" refused
grep "lets encrypt: node lb-2, certificate other:" "$WORKDIR/dash.log" | tail -1 | grep -q "next attempt" || fail "no next attempt after the refusal: $(grep 'certificate other' "$WORKDIR/dash.log")"
api PUT /api/acme "{\"email\": \"ops@site.test\", \"accept_terms\": true,
  \"directories\": [\"https://127.0.0.1:$P_ACME/dir\"], \"name_policy\": [{\"suffixes\": [\"site.test\", \"other.test\"]}]}" >/dev/null || fail "the name rule for other.test"
renewed="$(api POST "/api/clusters/$CID/letsencrypt/renew" '{"names": ["other"]}')" || fail "renew other through the cluster"
check "the renewal asked of lb-2" '"other"' "$renewed"
other_valid() { ctl 2 haproxy acme status | grep -Eq '^other +valid'; }
wait_for 60 "the Controller ordering other at once after the cluster's Renew - not at its next attempt" other_valid
grep -q "lets encrypt: node lb-2, certificate other: ordered and installed" "$WORKDIR/dash.log" || fail "the Controller doesn't say it ordered other for lb-2"
echo "  ok: an order the policy refused waits; the rule added, the cluster's Renew had the Controller order it at once"

for i in $NODES; do
  if grep -a "avc:.*denied" "$WORKDIR/$i.log"; then fail "AVC denials on lb-$i"; fi
done
echo "Cluster ACME ordered test PASSED: Let's Encrypt ordered by the Controller, set on the cluster - both members in that mode, the follower validated and served what the Controller ordered for the orderer; a new orderer kept the mode and the Controller ordered for it; zero AVC denials"
