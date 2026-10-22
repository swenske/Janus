#!/usr/bin/env bash
# Single sign-on through a real Keycloak (dashboard/backend/sso_login.go,
# docs/guide/sso.md): a Controller built from this tree and Keycloak
# (hack/keycloak, the realm in realm-janus.json) on a private Docker
# network, by their own names - controller.test and keycloak.test, two
# sites, as in real life - each with a certificate of a CA made here,
# which the Controller is given for Keycloak's. Headless Chromium (the
# pinned browser image) signs in through Keycloak's own pages:
# hack/browser/sso.mjs says what's checked - accounts made by the rules,
# refusals, the route kept across the round trip (janusctl's sign-in
# page), signing out of Keycloak too, linking an identity to a local
# account, a group changed in Keycloak, the rules changed on the
# Controller, the provider's second factor, disabling and removing the
# provider - with no page error, no CSP report, axe on the pages.
# Screenshots in build/controller-sso-test/out.
#
#   hack/controller-sso-test.sh <dashboardd, static> [browser-image]
#
# Nothing reaches the internet: the network is internal.
set -euo pipefail

DASHBOARDD="$(realpath "${1:?usage: $0 <dashboardd, static> [browser-image]}")"
BROWSER_IMAGE="${2:-janus-browser}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OFF="${JANUS_TEST_PORT_OFFSET:-0}"
P="janus-sso-test-$OFF"
KC_IMAGE=janus-keycloak-test
WORK="$ROOT/build/controller-sso-test"
KC_ADMIN_PASSWORD=sso-test-keycloak-admin

cleanup() {
  docker rm -f "$P-kc" "$P-controller" >/dev/null 2>&1 || true
  docker network rm "$P" >/dev/null 2>&1 || true
}
on_exit() {
  local status=$?
  if [ "$status" -ne 0 ]; then
    echo "controller-sso-test: FAILED - the Controller's log, then Keycloak's:" >&2
    docker logs --tail 60 "$P-controller" >&2 2>&1 || true
    docker logs --tail 40 "$P-kc" >&2 2>&1 || true
  fi
  cleanup
}
trap on_exit EXIT
cleanup
rm -rf "$WORK"
mkdir -p "$WORK/certs" "$WORK/data" "$WORK/out"

# A CA, and keycloak.test's and controller.test's certificates.
cd "$WORK/certs"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 -subj "/CN=janus sso test CA" \
  -addext basicConstraints=critical,CA:true -addext keyUsage=critical,keyCertSign -keyout ca.key -out ca.crt 2>/dev/null
for name in keycloak controller; do
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=$name.test" -keyout "$name.key" -out "$name.csr" 2>/dev/null
  printf 'subjectAltName=DNS:%s.test\nextendedKeyUsage=serverAuth\n' "$name" >"$name.ext"
  openssl x509 -req -in "$name.csr" -CA ca.crt -CAkey ca.key -CAcreateserial -days 2 -extfile "$name.ext" -out "$name.crt" 2>/dev/null
done
# Keycloak runs as its own user: its key readable (a throwaway).
chmod 0644 ./*.key
cd "$ROOT"

docker build -q -t "$KC_IMAGE" hack/keycloak >/dev/null
docker network create --internal "$P" >/dev/null

echo "controller-sso-test: starting Keycloak and the Controller"
docker run -d --name "$P-kc" --network "$P" --network-alias keycloak.test \
  -e KC_BOOTSTRAP_ADMIN_USERNAME=admin -e KC_BOOTSTRAP_ADMIN_PASSWORD="$KC_ADMIN_PASSWORD" \
  -v "$ROOT/hack/keycloak/realm-janus.json:/opt/keycloak/data/import/realm-janus.json:ro" -v "$WORK/certs:/certs:ro" \
  "$KC_IMAGE" start-dev --import-realm --hostname=https://keycloak.test:8443 --https-port=8443 \
  --https-certificate-file=/certs/keycloak.crt --https-certificate-key-file=/certs/keycloak.key >/dev/null
docker run -d --name "$P-controller" --network "$P" --network-alias controller.test --user "$(id -u):$(id -g)" \
  -v "$DASHBOARDD:/opt/janus/dashboardd:ro" -v "$WORK/data:/data" -v "$WORK/certs:/certs:ro" \
  --entrypoint /opt/janus/dashboardd "$BROWSER_IMAGE" \
  -addr :8443 -register-addr :8444 -data-dir /data -master-key-file /data/master.key \
  -tls-cert /certs/controller.crt -tls-key /certs/controller.key \
  -image-factory "" -updater-socket "" -releases-url https://127.0.0.1:1/releases >/dev/null

# wait_log CONTAINER TEXT SECONDS
wait_log() {
  local deadline=$((SECONDS + $3))
  until docker logs "$1" 2>&1 | grep -q "$2"; do
    if [ "$SECONDS" -ge "$deadline" ] || [ "$(docker inspect -f '{{.State.Running}}' "$1")" != true ]; then
      echo "controller-sso-test: $1 never logged \"$2\"" >&2
      return 1
    fi
    sleep 1
  done
}
wait_log "$P-controller" "listening on" 30
wait_log "$P-kc" "Listening on:" 180

echo "controller-sso-test: signing in through Keycloak in the browser"
docker run --rm --network "$P" --user "$(id -u):$(id -g)" -e HOME=/tmp -e NODE_TLS_REJECT_UNAUTHORIZED=0 \
  -e CONTROLLER=https://controller.test:8443 -e KEYCLOAK=https://keycloak.test:8443 \
  -e KC_ADMIN_PASSWORD="$KC_ADMIN_PASSWORD" -e OUT=/out -e CA=/ca.crt \
  -v "$ROOT/hack/browser/sso.mjs:/browser/sso.mjs:ro" -v "$WORK/certs/ca.crt:/ca.crt:ro" -v "$WORK/out:/out" \
  "$BROWSER_IMAGE" node --no-warnings sso.mjs

# What the Controller logged: a sign-in's claim names (never their
# values), and no secret.
logs="$(docker logs "$P-controller" 2>&1)"
grep -q "sso: kc: the ID token's claims: .*groups.*preferred_username" <<<"$logs" || { echo "controller-sso-test: no claim names logged" >&2; exit 1; }
if grep -q "janus-sso-test-client-secret\|alice-password" <<<"$logs"; then
  echo "controller-sso-test: a secret is in the Controller's log" >&2
  exit 1
fi
if grep -q "janus-sso-test-client-secret" "$WORK/data/sso.json"; then
  echo "controller-sso-test: the client secret is in sso.json in the clear" >&2
  exit 1
fi
echo "controller-sso-test: PASS"
