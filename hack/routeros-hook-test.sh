#!/usr/bin/env bash
# A cluster's RouterOS balancer (docs/guide/clusters.md "RouterOS") on a
# real RouterOS: MikroTik's Cloud Hosted Router (versions.mk) boots
# under QEMU and is set up the way the guide says - a group with the
# rest-api, read, write and api policies, a user of it, a self-signed
# certificate for www-ssl - and a route of its own ("foreign"). The
# engine then keeps the router's routes through the Controller's code
# (TestRouterOSLive, fake members): a wrong certificate refused, a route
# per member to the service prefix, active (ECMP), a drained member's
# route removed before its drain and back after, a route deleted by hand
# put back by the check, a removed member's route gone, check-gateway=ping
# when asked - the foreign route never touched.
#
# Usage: hack/routeros-hook-test.sh <chr-<version>.img>
set -euo pipefail

CHR_IMG="${1:?usage: $0 <chr image>}"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
BASE="${ROUTEROS_TEST_BASE_PORT:-$((19680 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
HTTP="http://127.0.0.1:$BASE"
HTTPS_PORT=$((BASE + 1))
PASSWORD="janus-hook-$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"

WORKDIR="$(mktemp -d)"
cleanup() {
  [ -s "$WORKDIR/qemu.pid" ] && kill "$(cat "$WORKDIR/qemu.pid")" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

fail() {
  echo "routeros-hook test FAILED: $*" >&2
  [ -f "$WORKDIR/console.log" ] && tail -c 1500 "$WORKDIR/console.log" >&2
  exit 1
}

# rest METHOD PATH [JSON]: the router's REST API as admin (no password on
# a fresh CHR), over HTTP - the test's own access, not the hook's.
rest() {
  local method=$1 path=$2 body=${3:-}
  if [ -n "$body" ]; then
    curl -fsS --max-time 30 -u admin: -X "$method" -H 'Content-Type: application/json' -d "$body" "$HTTP/rest$path"
  else
    curl -fsS --max-time 30 -u admin: -X "$method" "$HTTP/rest$path"
  fi
}

cp "$CHR_IMG" "$WORKDIR/chr.img"
qemu-system-x86_64 -accel kvm -accel tcg -m 256 -smp 1 \
  -drive file="$WORKDIR/chr.img",format=raw,if=virtio \
  -netdev user,id=n0,hostfwd=tcp:127.0.0.1:"$BASE"-:80,hostfwd=tcp:127.0.0.1:"$HTTPS_PORT"-:443 \
  -device virtio-net-pci,netdev=n0 \
  -display none -serial file:"$WORKDIR/console.log" -pidfile "$WORKDIR/qemu.pid" -daemonize </dev/null
for _ in $(seq 1 90); do
  rest GET /system/resource >/dev/null 2>&1 && break
  sleep 2
done
version="$(rest GET /system/resource | python3 -c 'import json,sys; print(json.load(sys.stdin)["version"])')" || fail "the router's REST API never answered"
echo "Part 1 OK: RouterOS $version (CHR) up, its REST API answering"

# The hook's user: a group with what the guide asks, a user of it.
rest PUT /user/group '{"name":"janus-hook","policy":"read,write,api,rest-api"}' >/dev/null || fail "the group"
rest PUT /user "{\"name\":\"janus-hook\",\"group\":\"janus-hook\",\"password\":\"$PASSWORD\"}" >/dev/null || fail "the user"
# www-ssl with a self-signed certificate made on the router.
rest PUT /certificate '{"name":"www","common-name":"chr"}' >/dev/null || fail "the certificate"
rest POST /certificate/sign '{".id":"www"}' >/dev/null || fail "signing it"
for _ in $(seq 1 60); do
  rest GET '/certificate?name=www' | grep -q '"private-key":"true"' && break
  sleep 1
done
rest POST /ip/service/set '{"numbers":"www-ssl","certificate":"www","disabled":"false"}' >/dev/null || fail "www-ssl"
# The certificate's SHA-256, as the guide says to read it (/certificate
# print detail): the engine pins it - a mismatch would fail every call.
fp="$(rest GET '/certificate?name=www' | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["fingerprint"])')" || fail "its fingerprint"
for _ in $(seq 1 30); do
  code="$(curl -sk -o /dev/null -w '%{http_code}' -u "janus-hook:$PASSWORD" "https://127.0.0.1:$HTTPS_PORT/rest/ip/route")"
  [ "$code" = 200 ] && break
  sleep 1
done
[ "$code" = 200 ] || fail "the hook's user over https: $code"
# A route of the router's own, which the engine must never touch.
gwnet="$(rest GET /ip/address | python3 -c 'import json,sys; a=json.load(sys.stdin)[0]["address"]; print(".".join(a.split(".")[:3]))')"
rest PUT /ip/route '{"dst-address":"198.51.100.0/24","gateway":"'"$gwnet"'.2","comment":"foreign"}' >/dev/null || fail "the foreign route"
echo "Part 2 OK: the hook's user (rest-api, read, write, api), www-ssl with a self-signed certificate (SHA-256 $fp), a foreign route"

cd "$REPO"
out="$(JANUS_ROUTEROS_URL="https://127.0.0.1:$HTTPS_PORT" JANUS_ROUTEROS_FINGERPRINT="$fp" JANUS_ROUTEROS_USER=janus-hook \
  JANUS_ROUTEROS_PASSWORD="$PASSWORD" JANUS_ROUTEROS_CHECK_URL="http://admin:@127.0.0.1:$BASE" JANUS_ROUTEROS_GATEWAY_NET="$gwnet" \
  go test -count=1 -run '^TestRouterOSLive$' -v ./dashboard/backend 2>&1)" || fail "the engine on the router:
$out"
grep -q -- '--- PASS: TestRouterOSLive' <<<"$out" || fail "TestRouterOSLive didn't run: $out"
grep 'cluster_balancer_test.go' <<<"$out" | sed 's/^ *cluster_balancer_test.go:[0-9]*: /  /'
echo "Part 3 OK: the engine kept the router's routes - a wrong certificate refused, a route per member (active, ECMP), a drained member's route removed before its drain and back after, a route deleted by hand put back, a removed member's gone, check-gateway=ping set"

routes="$(rest GET /ip/route)"
python3 - "$routes" <<'PY' || fail "the router's routes at the end: $routes"
import json, sys
routes = json.loads(sys.argv[1])
foreign = [r for r in routes if r.get("comment") == "foreign"]
assert len(foreign) == 1 and foreign[0]["dst-address"] == "198.51.100.0/24", foreign
PY
echo "routeros-hook test PASSED: RouterOS $version kept by the engine, its own route untouched"
