#!/usr/bin/env bash
# Proves the management dashboard's backend (dashboard/backend) works
# end to end against a real, running node - not a mock gRPC server, not
# just that the Go code compiles. Reuses the same real-boot + debugfs
# PKI extraction pattern every other lifecycle test in this project uses
# (see hack/qemu-lifecycle-rollback-test.sh) to get a real admin
# credential for a real booted node, then drives dashboardd's own REST
# API exactly the way the (still-to-come) frontend will:
#
#   1. boot disk.img for real, extract ca.crt/admin.crt/admin.key from
#      its STATE partition via debugfs.
#   2. start dashboardd against a scratch data directory.
#   3. POST /api/nodes with the node's real address + the extracted
#      admin credential as the one-time "bootstrap" credential - proves
#      the add-node flow genuinely calls GenerateClientConfiguration
#      against the real node and gets back a working, freshly-issued
#      service credential (not the bootstrap one relayed - see
#      dashboard/backend/internal/nodeproxy's own package doc for why
#      that's not just a design choice but a cryptographic
#      impossibility).
#   3b. tranche 6: add-node via a .pfx upload instead of pasted PEM
#      text (dashboard/backend/main.go's parseAddNodeRequest, the
#      frontend's default mode as of dashboard/frontend/src/App.jsx's
#      AddNodeForm) - a real openssl-built .pfx bundling the admin
#      cert+key plus the node's ca.crt (via -certfile, exactly how a
#      real user's would be built), added and immediately removed
#      again so it doesn't interfere with the main JSON-based flow
#      below. Also checks the two real error paths: a wrong password,
#      and a .pfx missing the bundled CA certificate - both need a
#      real, actionable message, not a generic parse failure.
#   4. GET /api/nodes lists it (name/address/port only, no credential).
#   5. hit the allocated per-node HTTPS port with the *bootstrap*
#      admin cert as the TLS client certificate (any cert issued by the
#      node's own CA satisfies the gate, not specifically the stored
#      service one - the gate and the relay credential are deliberately
#      different keys) and check the JSON response contains real values
#      relayed from the actual node (same values hack/
#      qemu-system-info-test.sh already proves are real).
#   6. confirm the mTLS gate actually rejects both no client cert at all
#      and a cert from an unrelated CA - a real security boundary, not
#      just "it happens to work with the right cert".
#   7. exercise the tranche 4 ops/config relay (dashboard/backend/
#      internal/nodeproxy/ops.go) against the node's own bootstrap
#      config - deliberately scoped to what's testable without a custom
#      boot fixture: ShowInfo and GetConfig against real values;
#      ApplyConfig with the exact same config read back (must be
#      accepted - proves the round trip) and with deliberately garbage
#      config (must be rejected, with a real validation error, and must
#      NOT disturb the running config - internal/haproxy.Manager.Apply's
#      own "never overwritten on a rejected apply" guarantee, now
#      proven through the dashboard's own relay too); BackendList
#      against its own real not-yet-implemented state (internal/api/
#      haproxy.go's own doc comment - falls through to
#      UnimplementedHAProxyServiceServer, so the relay must surface a
#      real error here, not fake a success); MapList/CertificateList
#      against the bootstrap config's genuinely empty map/cert list (a
#      correctly-relayed empty response, not a gap - protojson's
#      omitempty drops an empty repeated field entirely, confirmed
#      against a real boot before encoding this assertion); a real
#      CertificateUpload/List/Delete round trip (HAProxy's cert *store*
#      accepts uploads unconditionally, unlike file-backed maps/ACLs,
#      which only exist if the config already declares one - exercising
#      a real MapUpdate/ACLUpdate success case would need injecting a
#      whole custom haproxy.cfg + map/ACL files onto STATE before boot,
#      real effort for coverage `internal/haproxy`'s own CI step already
#      provides at the non-dashboard layer - so this test settles for
#      proving MapUpdate genuinely reaches the real node and surfaces
#      its real "no such map" error correctly, not a staged success).
#      Writing this step's assertions found two real, previously-
#      undiscovered bugs, both fixed alongside this test: (a) a missing
#      SELinux policy rule (selinux/policy.conf) denied the haproxy_t
#      domain write access to the fifo_file pipe internal/haproxy.
#      Manager.Validate's `haproxy -c` inherits from its janusd_t
#      parent (a genuinely different object from the supervised
#      process's own console-backed stdout/stderr, already covered) -
#      every validation error was being silently denied mid-write,
#      captured as an empty error list instead of haproxy's real
#      parse-error text, on a real boot only (a permissive/plain local
#      run never saw it - see manager.go's own comment); (b) internal/
#      haproxy.Manager.statsCommand had no read deadline at all - a
#      malformed multi-line payload (found via a test-harness bug of
#      this test's own early draft, not production code, but the gap
#      it exposed is real) leaves HAProxy's stats socket waiting for
#      more input forever, blocking the reading goroutine past any
#      caller's own timeout (fixed with a bounded conn.SetDeadline).
#   8. DELETE the node, confirm it's gone from the list AND its port
#      stops accepting connections entirely.
#   9. re-add it, restart dashboardd against the *same* data directory,
#      and confirm both the registry and the per-node listener come back
#      without needing to re-add anything - the whole point of
#      persisting to disk rather than keeping the registry in memory
#      only.
#   10. tranche 7: the main port's own admin auth (dashboard/backend/
#      internal/auth) - confirmed via a real cookie jar throughout this
#      whole script (every /api/nodes* call needs it, matching what the
#      real UI now enforces): /api/nodes with no session at all must be
#      refused (401), a first-run setup call establishes the one admin
#      password and logs the caller in via the returned session cookie,
#      and - after the dashboardd restart in step 9, since sessions are
#      deliberately in-memory-only and don't survive a restart - a fresh
#      login call is needed before the registry is reachable again.
#   11. Point 2 suite, tranche 2: node self-registration (dashboard/
#      backend/internal/pending + register.go's startRegistrationListener)
#      - a synthetic node CA + service credential (standing in for what
#      a real janusd would generate locally and send - tranche 2 is
#      the Controller side only, janusd doesn't self-register yet)
#      POSTed to the dedicated TLS registration port (separate from
#      both :8080 and the per-node pool - see startRegistrationListener's
#      own doc comment for why). Checks: a real registration is
#      accepted and shows up in GET /api/pending (auth-gated, like
#      everything else under /api/); missing required fields refused
#      with 400; a cert/key pair that doesn't actually parse together
#      refused with 400 and a real error, not a generic failure; the
#      pending entry survives a dashboardd restart (same reasoning as
#      the node registry itself - a node only announces once per boot,
#      so losing this to an in-memory store would strand it).
#   12. Point 2 suite, tranche 3: approve/reject the pending queue
#      (register.go's handlePendingAction/approvePending/rejectPending,
#      wired into the frontend's PendingList component). Two more
#      synthetic self-registrations (separate from the one used for the
#      restart-persistence check, which is left untouched): approving
#      one must move it into GET /api/nodes with a real allocated port
#      and a genuinely running mTLS-gated listener (any cert from its
#      own CA completes a real TLS handshake against it), and remove it
#      from /api/pending; rejecting the other must remove it from
#      /api/pending and it must never appear in /api/nodes at all.
#      Also checks that rejecting an already-rejected id is a real 404,
#      not a silent success. Writing this coverage surfaced a real bug
#      in approvePending itself, fixed alongside it: on a listener-start
#      failure (e.g. a port conflict) it left the pending entry in place
#      "to be retried" but had already called store.Add - a retry then
#      called store.Add a second time with a fresh id, permanently
#      orphaning the first, listener-less entry instead of actually
#      retrying it. Fixed by rolling the store entry back
#      (store.Remove) whenever startListener fails, verified by hand
#      against a live dashboardd instance (occupying the allocated port
#      to force the failure, confirming no orphan after the failed
#      attempt, then freeing the port and confirming a clean retry)
#      before encoding it here.
#   13. Point 2 suite, tranche 6: GET /api/controller-info (auth-gated
#      like everything else under /api/) - what an operator needs to
#      fill in janusctl lifecycle install's own -controller-address/
#      -controller-ca flags for a new node. Checks the gate itself and
#      that the returned CA certificate is a real, parseable
#      certificate (the dashboard's own TLS identity) - not the
#      suggested address's exact value, which depends on this test
#      host's own network interfaces (see suggestRegisterAddress's own
#      doc comment).
#   14. user-requested: the main port is HTTPS-only now (every
#      /api/nodes*/pending*/controller-info call in this script uses
#      https:// -k) - checks the session cookie set at setup is
#      genuinely marked Secure (curl's own cookie-jar file format,
#      not just that https:// happens to be the URL scheme used
#      throughout this script).
#
# Usage: hack/qemu-dashboard-test.sh <disk.img> <dashboardd-bin> <versitygw-bin>
set -euo pipefail
HACK="$(cd "$(dirname "$0")" && pwd)"

export PATH="$PATH:/usr/sbin:/sbin"

DISK="${1:?usage: $0 <disk.img> <dashboardd-bin>}"
DASHBOARDD="${2:?usage: $0 <disk.img> <dashboardd-bin> <versitygw-bin>}"
VERSITYGW="${3:?usage: $0 <disk.img> <dashboardd-bin> <versitygw-bin>}"
HTTP_TIMEOUT_SECS="${QEMU_DASHBOARD_HTTP_TIMEOUT:-40}"
HOST_HTTP_PORT="${QEMU_DASHBOARD_NODE_HTTP_PORT:-$((18120 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
HOST_GRPC_PORT="${QEMU_DASHBOARD_NODE_GRPC_PORT:-$((18121 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
DASHBOARD_ADDR_PORT="${QEMU_DASHBOARD_ADDR_PORT:-$((18122 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
DASHBOARD_REGISTER_PORT="${QEMU_DASHBOARD_REGISTER_PORT:-$((18123 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
S3_PORT="$((18124 + ${JANUS_TEST_PORT_OFFSET:-0}))"
VGW_PID=""

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf) - set \$OVMF_CODE to override" >&2; exit 1; }
[ -f "$OVMF_VARS_TEMPLATE" ] || { echo "OVMF vars template not found at $OVMF_VARS_TEMPLATE - set \$OVMF_VARS_TEMPLATE to override" >&2; exit 1; }

WORKDIR="$(mktemp -d)"
QEMU_PID=""
DASHBOARD_PID=""
cleanup() {
  [ -n "$DASHBOARD_PID" ] && kill "$DASHBOARD_PID" 2>/dev/null || true
  [ -n "$VGW_PID" ] && kill "$VGW_PID" 2>/dev/null || true
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

# --- boot the real node ---
OVMF_VARS="$WORKDIR/OVMF_VARS.fd"
cp "$OVMF_VARS_TEMPLATE" "$OVMF_VARS"
LOG="$WORKDIR/console.log"
qemu-system-x86_64 -accel kvm -accel tcg \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$OVMF_VARS" \
  -drive file="$DISK",format=raw,if=virtio \
  -nographic -no-reboot -display none -m 512M \
  -netdev "user,id=net0,hostfwd=tcp::${HOST_HTTP_PORT}-:8080,hostfwd=tcp::${HOST_GRPC_PORT}-:9505" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG" \
  &
QEMU_PID=$!

deadline=$((SECONDS + HTTP_TIMEOUT_SECS))
code=""
while [ "$SECONDS" -lt "$deadline" ]; do
  code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_HTTP_PORT}/" || true)"
  [ "$code" = "200" ] && break
  sleep 1
done
if [ "$code" != "200" ]; then
  echo "Dashboard test FAILED: node never answered HTTP 200 within ${HTTP_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
echo "Node OK: real UEFI boot, HTTP 200"

# --- extract PKI from STATE (same reasoning as every other lifecycle test) ---
STATE_START_SECTOR="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
STATE_SIZE_SECTORS="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
dd if="$DISK" of="$WORKDIR/state.img" bs=512 skip="$STATE_START_SECTOR" count="$STATE_SIZE_SECTORS" status=none
for f in ca.crt admin.crt admin.key; do
  debugfs -R "dump pki/$f $WORKDIR/$f" "$WORKDIR/state.img" >/dev/null 2>&1
  [ -s "$WORKDIR/$f" ] || { echo "Dashboard test FAILED: couldn't extract pki/$f from disk.img's STATE partition" >&2; exit 1; }
done

# --- start dashboardd ---
mkdir -p "$WORKDIR/data"
"$DASHBOARDD" -addr ":${DASHBOARD_ADDR_PORT}" -register-addr ":${DASHBOARD_REGISTER_PORT}" -data-dir "$WORKDIR/data" > "$WORKDIR/dashboardd.log" 2>&1 &
DASHBOARD_PID=$!
sleep 1
if ! kill -0 "$DASHBOARD_PID" 2>/dev/null; then
  echo "Dashboard test FAILED: dashboardd exited immediately" >&2
  cat "$WORKDIR/dashboardd.log" >&2
  exit 1
fi

# --- the main SPA (dashboard/frontend, go:embed'd) must actually be
# served, not just the REST API ---
MAIN_UI="$(curl -sk "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/")"
echo "$MAIN_UI" | grep -q '<div id="root">' || { echo "Dashboard test FAILED: main SPA index.html not served at /: $MAIN_UI" >&2; exit 1; }
echo "Main SPA OK: dashboard/frontend's built index.html is served at /"

# --- tranche 7: admin auth gates everything under /api/nodes* now -
# confirm the gate itself before doing anything it would block ---
COOKIE_JAR="$WORKDIR/cookies.txt"
unauth_code="$(curl -sk -o /dev/null -w '%{http_code}' "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes")"
[ "$unauth_code" = "401" ] || { echo "Dashboard test FAILED: /api/nodes with no session should be 401, got $unauth_code" >&2; exit 1; }

AUTH_STATUS="$(curl -sk "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/auth/status")"
echo "$AUTH_STATUS" | grep -q '"setup_required":true' || { echo "Dashboard test FAILED: a fresh data dir should report setup_required, got: $AUTH_STATUS" >&2; exit 1; }

short_pw_code="$(curl -sk -o /dev/null -w '%{http_code}' -X POST "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/auth/setup" -H "Content-Type: application/json" -d '{"password":"short"}')"
[ "$short_pw_code" = "400" ] || { echo "Dashboard test FAILED: a too-short setup password should be refused with 400, got $short_pw_code" >&2; exit 1; }

setup_code="$(curl -sk -c "$COOKIE_JAR" -o /dev/null -w '%{http_code}' -X POST "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/auth/setup" -H "Content-Type: application/json" -d '{"password":"dashboard-test-admin-pw"}')"
[ "$setup_code" = "204" ] || { echo "Dashboard test FAILED: first-run setup should return 204, got $setup_code" >&2; exit 1; }

second_setup_code="$(curl -sk -o /dev/null -w '%{http_code}' -X POST "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/auth/setup" -H "Content-Type: application/json" -d '{"password":"dashboard-test-admin-pw"}')"
[ "$second_setup_code" = "400" ] || { echo "Dashboard test FAILED: a second setup call should be refused, got $second_setup_code" >&2; exit 1; }

# The main port is HTTPS-only now (see dashboard/backend/main.go's own
# doc comment) - the session cookie must actually be marked Secure, not
# just work over the https:// this script already uses throughout.
# curl's cookie jar is the Netscape format: domain/flag/path/secure/
# expiry/name/value - the 4th field is TRUE/FALSE for Secure.
grep -P 'janus_session' "$COOKIE_JAR" | awk -F'\t' '{print $4}' | grep -qx TRUE || { echo "Dashboard test FAILED: session cookie isn't marked Secure: $(cat "$COOKIE_JAR")" >&2; exit 1; }
echo "Auth OK: /api/nodes refused with no session, setup validated (short password, second-setup refusal), session cookie established and marked Secure"

# The admin needs a second factor (the default policy): nothing but
# setting one up until then - an authenticator app here, its codes
# computed like the app would (hack/totp.py).
API="https://127.0.0.1:${DASHBOARD_ADDR_PORT}"
before_mfa="$(curl -sk -b "$COOKIE_JAR" -o /dev/null -w '%{http_code}' "$API/api/nodes")"
[ "$before_mfa" = 403 ] || { echo "Dashboard test FAILED: an admin without a second factor read /api/nodes ($before_mfa)" >&2; exit 1; }
curl -sk -b "$COOKIE_JAR" -X POST "$API/api/auth/mfa/totp/setup" -o "$WORKDIR/totp.json"
TOTP_SECRET="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["secret"])' "$WORKDIR/totp.json")"
curl -sk -b "$COOKIE_JAR" -X POST "$API/api/auth/mfa/totp/enable" -H 'Content-Type: application/json' -d "{\"code\":\"$(python3 "$HACK/totp.py" "$TOTP_SECRET")\"}" -o "$WORKDIR/recovery.json"
python3 -c 'import json,sys; c=json.load(open(sys.argv[1]))["recovery_codes"]; assert len(c) == 10, c' "$WORKDIR/recovery.json" || { echo "Dashboard test FAILED: setting the authenticator app up: $(cat "$WORKDIR/recovery.json")" >&2; exit 1; }
after_mfa="$(curl -sk -b "$COOKIE_JAR" -o /dev/null -w '%{http_code}' "$API/api/nodes")"
[ "$after_mfa" = 200 ] || { echo "Dashboard test FAILED: after setting a second factor up, /api/nodes answered $after_mfa" >&2; exit 1; }
if grep -q "$TOTP_SECRET" "$WORKDIR/data/users.json"; then echo "Dashboard test FAILED: the authenticator secret is in users.json in the clear" >&2; exit 1; fi
echo "Second factor OK: the admin set an authenticator app up before anything else, got 10 recovery codes, its secret sealed"

# Accounts: a reader made by the admin chooses their password, reads,
# and is refused every change - recorded in the audit with their name.
API="https://127.0.0.1:${DASHBOARD_ADDR_PORT}"
READER_JAR="$WORKDIR/reader-cookies.txt"
curl -sk -b "$COOKIE_JAR" -X POST "$API/api/users" -H 'Content-Type: application/json' -d '{"name":"rita","role":"reader"}' -o "$WORKDIR/rita.json"
rita_given="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["password"])' "$WORKDIR/rita.json")"
curl -sk -c "$READER_JAR" -o /dev/null -X POST "$API/api/auth/login" -H 'Content-Type: application/json' -d "{\"name\":\"rita\",\"password\":\"$rita_given\"}"
before_change="$(curl -sk -b "$READER_JAR" -o /dev/null -w '%{http_code}' "$API/api/nodes")"
[ "$before_change" = 403 ] || { echo "Dashboard test FAILED: a session with a given password read /api/nodes ($before_change)" >&2; exit 1; }
curl -sk -b "$READER_JAR" -c "$READER_JAR" -o /dev/null -X POST "$API/api/auth/password" -H 'Content-Type: application/json' -d "{\"current_password\":\"$rita_given\",\"new_password\":\"ritas-own-password\"}"
reader_read="$(curl -sk -b "$READER_JAR" -o /dev/null -w '%{http_code}' "$API/api/nodes")"
reader_add="$(curl -sk -b "$READER_JAR" -o /dev/null -w '%{http_code}' -X POST "$API/api/nodes" -H 'Content-Type: application/json' -d '{}')"
reader_users="$(curl -sk -b "$READER_JAR" -o /dev/null -w '%{http_code}' "$API/api/users")"
[ "$reader_read/$reader_add/$reader_users" = 200/403/403 ] || { echo "Dashboard test FAILED: a reader read /api/nodes $reader_read, added a node $reader_add, listed accounts $reader_users (want 200/403/403)" >&2; exit 1; }
curl -sk -b "$COOKIE_JAR" "$API/api/audit?user=rita" -o "$WORKDIR/audit-rita.json"
grep -q '"path":"/api/nodes","status":403' "$WORKDIR/audit-rita.json" || { echo "Dashboard test FAILED: the audit doesn't show rita's refused change: $(cat "$WORKDIR/audit-rita.json")" >&2; exit 1; }
echo "Accounts OK: a reader chose their password, read the nodes, was refused adding one and the accounts - the audit says so"

# --- Point 2 suite, tranche 6: GET /api/controller-info hands an
# operator what janusctl lifecycle install's own -controller-address/
# -controller-ca flags need - auth-gated like everything else under
# /api/, and its CA cert must be a real, parseable certificate (the
# dashboard's own TLS identity, see loadOrCreateDashboardIdentity). No
# -advertise-address is set for this dashboardd instance, so the
# address field is whatever suggestRegisterAddress's own pki.LocalIPs()
# fallback finds on this host (or empty, if that's empty too) - not
# asserted on exactly, just that the endpoint itself is well-formed and
# gated. ---
unauth_controller_info_code="$(curl -sk -o /dev/null -w '%{http_code}' "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/controller-info")"
[ "$unauth_controller_info_code" = "401" ] || { echo "Dashboard test FAILED: /api/controller-info with no session should be 401, got $unauth_controller_info_code" >&2; exit 1; }

CONTROLLER_INFO="$(curl -sk -b "$COOKIE_JAR" "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/controller-info")"
CONTROLLER_INFO_CA="$(echo "$CONTROLLER_INFO" | python3 -c 'import json,sys; print(json.load(sys.stdin)["ca_cert_pem"])')"
echo "$CONTROLLER_INFO_CA" | openssl x509 -noout -subject >/dev/null 2>&1 || { echo "Dashboard test FAILED: /api/controller-info's ca_cert_pem isn't a real, parseable certificate: $CONTROLLER_INFO" >&2; exit 1; }
echo "Controller-info OK: gated like the rest of /api/, ca_cert_pem is a real certificate: $CONTROLLER_INFO"

# --- Point 2 suite, tranche 2: node self-registration (dashboard/backend/
# internal/pending + register.go) - a synthetic node CA + service
# credential standing in for what a real janusd would generate locally
# and send (janusd itself doesn't self-register yet - this tranche is
# the Controller side only) ---
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
  -keyout "$WORKDIR/reg-node-ca.key" -out "$WORKDIR/reg-node-ca.crt" -days 1 -nodes -subj "/CN=synthetic node CA" >/dev/null 2>&1
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
  -keyout "$WORKDIR/reg-service.key" -out "$WORKDIR/reg-service.csr" -nodes -subj "/CN=service" >/dev/null 2>&1
openssl x509 -req -in "$WORKDIR/reg-service.csr" -CA "$WORKDIR/reg-node-ca.crt" -CAkey "$WORKDIR/reg-node-ca.key" \
  -CAcreateserial -out "$WORKDIR/reg-service.crt" -days 1 >/dev/null 2>&1
python3 -c '
import json, sys
ca, crt, key, out = sys.argv[1:5]
req = {
    "name": "self-registered-test-node",
    "address": "10.0.0.7:9505",
    "ca_cert_pem": open(ca).read(),
    "service_cert_pem": open(crt).read(),
    "service_key_pem": open(key).read(),
}
open(out, "w").write(json.dumps(req))
' "$WORKDIR/reg-node-ca.crt" "$WORKDIR/reg-service.crt" "$WORKDIR/reg-service.key" "$WORKDIR/register.json"

REGISTER_RESP="$(curl -sk -X POST "https://127.0.0.1:${DASHBOARD_REGISTER_PORT}/register" -H "Content-Type: application/json" -d @"$WORKDIR/register.json")"
PENDING_ID="$(echo "$REGISTER_RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])' 2>/dev/null)"
[ -n "$PENDING_ID" ] || { echo "Dashboard test FAILED: registration didn't return an id: $REGISTER_RESP" >&2; exit 1; }

PENDING_LIST="$(curl -sk -b "$COOKIE_JAR" "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/pending")"
echo "$PENDING_LIST" | grep -qF "\"id\":\"$PENDING_ID\"" || { echo "Dashboard test FAILED: GET /api/pending didn't list the self-registered node: $PENDING_LIST" >&2; exit 1; }
echo "$PENDING_LIST" | grep -q '"name":"self-registered-test-node"' || { echo "Dashboard test FAILED: pending entry missing its name: $PENDING_LIST" >&2; exit 1; }
echo "Node self-registration OK: $REGISTER_RESP, listed in GET /api/pending"

missing_fields_code="$(curl -sk -o /dev/null -w '%{http_code}' -X POST "https://127.0.0.1:${DASHBOARD_REGISTER_PORT}/register" -H "Content-Type: application/json" -d '{"name":"incomplete"}')"
[ "$missing_fields_code" = "400" ] || { echo "Dashboard test FAILED: registration with missing fields should be 400, got $missing_fields_code" >&2; exit 1; }

python3 -c '
import json, sys
ca, crt, out = sys.argv[1:4]
req = {"name": "bad-key", "address": "10.0.0.8:9505", "ca_cert_pem": open(ca).read(), "service_cert_pem": open(crt).read(), "service_key_pem": "not a real key"}
open(out, "w").write(json.dumps(req))
' "$WORKDIR/reg-node-ca.crt" "$WORKDIR/reg-service.crt" "$WORKDIR/register-bad-key.json"
bad_key_resp="$(curl -sk -X POST "https://127.0.0.1:${DASHBOARD_REGISTER_PORT}/register" -H "Content-Type: application/json" -d @"$WORKDIR/register-bad-key.json" -w '\n%{http_code}')"
bad_key_code="$(echo "$bad_key_resp" | tail -1)"
[ "$bad_key_code" = "400" ] || { echo "Dashboard test FAILED: registration with a malformed key should be 400, got $bad_key_code: $bad_key_resp" >&2; exit 1; }
echo "$bad_key_resp" | grep -q "don't form a valid certificate" || { echo "Dashboard test FAILED: malformed key should get a real, actionable error, got: $bad_key_resp" >&2; exit 1; }
echo "Registration error paths OK: missing fields and a malformed cert/key pair both refused with a real error"

# --- Point 2 suite, tranche 3: approve/reject the pending queue
# (dashboard/backend/register.go's handlePendingAction/approvePending/
# rejectPending, wired into the frontend's new PendingList component) -
# two more synthetic self-registrations, separate from $PENDING_ID above
# (which stays untouched here so the restart-persistence check further
# down still has a genuinely pending entry to find). One is approved -
# must land in GET /api/nodes with a real allocated port and a working
# mTLS-gated listener, and disappear from /api/pending. The other is
# rejected - must disappear from /api/pending and never appear in
# /api/nodes at all.
for n in approve-me reject-me; do
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
    -keyout "$WORKDIR/${n}-service.key" -out "$WORKDIR/${n}-service.csr" -nodes -subj "/CN=service-$n" >/dev/null 2>&1
  openssl x509 -req -in "$WORKDIR/${n}-service.csr" -CA "$WORKDIR/reg-node-ca.crt" -CAkey "$WORKDIR/reg-node-ca.key" \
    -CAcreateserial -out "$WORKDIR/${n}-service.crt" -days 1 >/dev/null 2>&1
  python3 -c '
import json, sys
ca, crt, key, name, out = sys.argv[1:6]
req = {
    "name": name,
    "address": "10.0.0.9:9505",
    "ca_cert_pem": open(ca).read(),
    "service_cert_pem": open(crt).read(),
    "service_key_pem": open(key).read(),
}
open(out, "w").write(json.dumps(req))
' "$WORKDIR/reg-node-ca.crt" "$WORKDIR/${n}-service.crt" "$WORKDIR/${n}-service.key" "$n" "$WORKDIR/register-${n}.json"
done

APPROVE_REGISTER_RESP="$(curl -sk -X POST "https://127.0.0.1:${DASHBOARD_REGISTER_PORT}/register" -H "Content-Type: application/json" -d @"$WORKDIR/register-approve-me.json")"
APPROVE_ID="$(echo "$APPROVE_REGISTER_RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])' 2>/dev/null)"
[ -n "$APPROVE_ID" ] || { echo "Dashboard test FAILED: approve-me registration didn't return an id: $APPROVE_REGISTER_RESP" >&2; exit 1; }

REJECT_REGISTER_RESP="$(curl -sk -X POST "https://127.0.0.1:${DASHBOARD_REGISTER_PORT}/register" -H "Content-Type: application/json" -d @"$WORKDIR/register-reject-me.json")"
REJECT_ID="$(echo "$REJECT_REGISTER_RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])' 2>/dev/null)"
[ -n "$REJECT_ID" ] || { echo "Dashboard test FAILED: reject-me registration didn't return an id: $REJECT_REGISTER_RESP" >&2; exit 1; }

APPROVE_RESP="$(curl -sk -b "$COOKIE_JAR" -w '\n%{http_code}' -X POST "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/pending/${APPROVE_ID}/approve")"
APPROVE_CODE="$(echo "$APPROVE_RESP" | tail -1)"
APPROVE_BODY="$(echo "$APPROVE_RESP" | sed '$d')"
[ "$APPROVE_CODE" = "201" ] || { echo "Dashboard test FAILED: approve should return 201, got $APPROVE_CODE: $APPROVE_BODY" >&2; exit 1; }
APPROVED_NODE_ID="$(echo "$APPROVE_BODY" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
[ -n "$APPROVED_NODE_ID" ] || { echo "Dashboard test FAILED: approve response missing the node's id: $APPROVE_BODY" >&2; exit 1; }

REJECT_CODE="$(curl -sk -b "$COOKIE_JAR" -o /dev/null -w '%{http_code}' -X POST "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/pending/${REJECT_ID}/reject")"
[ "$REJECT_CODE" = "204" ] || { echo "Dashboard test FAILED: reject should return 204, got $REJECT_CODE" >&2; exit 1; }

PENDING_AFTER_ACTIONS="$(curl -sk -b "$COOKIE_JAR" "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/pending")"
if echo "$PENDING_AFTER_ACTIONS" | grep -qF "\"id\":\"$APPROVE_ID\""; then
  echo "Dashboard test FAILED: approved entry still listed as pending: $PENDING_AFTER_ACTIONS" >&2
  exit 1
fi
if echo "$PENDING_AFTER_ACTIONS" | grep -qF "\"id\":\"$REJECT_ID\""; then
  echo "Dashboard test FAILED: rejected entry still listed as pending: $PENDING_AFTER_ACTIONS" >&2
  exit 1
fi
echo "$PENDING_AFTER_ACTIONS" | grep -qF "\"id\":\"$PENDING_ID\"" || { echo "Dashboard test FAILED: the untouched self-registered-test-node entry disappeared from pending: $PENDING_AFTER_ACTIONS" >&2; exit 1; }

NODES_AFTER_ACTIONS="$(curl -sk -b "$COOKIE_JAR" "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes")"
echo "$NODES_AFTER_ACTIONS" | grep -qF "\"id\":\"$APPROVED_NODE_ID\"" || { echo "Dashboard test FAILED: approved node not listed in /api/nodes: $NODES_AFTER_ACTIONS" >&2; exit 1; }
if echo "$NODES_AFTER_ACTIONS" | grep -q '"name":"reject-me"'; then
  echo "Dashboard test FAILED: rejected node appeared in /api/nodes: $NODES_AFTER_ACTIONS" >&2
  exit 1
fi

# The approved node's page is up under /nodes/<id>/ - this synthetic
# node's "address" (10.0.0.9:9505) isn't a real janusd, so the relay
# itself can't succeed: a 502 once nodeproxy tries to dial it.
approve_relay_code="$(curl -sk -b "$COOKIE_JAR" -o /dev/null -w '%{http_code}' -m 20 "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/nodes/${APPROVED_NODE_ID}/api/info" || true)"
[ "$approve_relay_code" = "502" ] || { echo "Dashboard test FAILED: the approved node's page answered $approve_relay_code, want 502 (relayed, the node unreachable)" >&2; exit 1; }

reject_unknown_id_code="$(curl -sk -b "$COOKIE_JAR" -o /dev/null -w '%{http_code}' -X POST "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/pending/${REJECT_ID}/reject")"
[ "$reject_unknown_id_code" = "404" ] || { echo "Dashboard test FAILED: rejecting an already-rejected id should be 404, got $reject_unknown_id_code" >&2; exit 1; }

# clean up the approved test node so it doesn't interfere with anything below
curl -sk -b "$COOKIE_JAR" -X DELETE "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes/${APPROVED_NODE_ID}" >/dev/null

echo "Pending approve/reject OK: approve moves the entry into /api/nodes with a real listener, reject discards it, a second reject on the same id is a real 404"

# --- add-node via .pfx upload (the frontend's default mode as of
# tranche 6 - see dashboard/backend/main.go's parseAddNodeRequest and
# dashboard/frontend/src/App.jsx's AddNodeForm) - a real .pfx built the
# same way a user's would be (openssl pkcs12 -export -certfile ca.crt,
# so the CA rides along inside the bundle, not just the leaf cert),
# added and immediately removed again so it doesn't interfere with the
# main JSON-based add-node flow below (still the primary path this
# script exercises end to end - the .pfx path only needs proving it
# reaches the same parseAddNodeRequest outcome, not a second full
# relay/mTLS-gate/restart-persistence pass). Also checks the two real
# error paths: a wrong password, and a .pfx missing the bundled CA.
openssl pkcs12 -export -inkey "$WORKDIR/admin.key" -in "$WORKDIR/admin.crt" \
  -certfile "$WORKDIR/ca.crt" -out "$WORKDIR/admin.pfx" -passout pass:dashboard-test-pfx >/dev/null 2>&1
PFX_ADD_RESP="$(curl -sk -b "$COOKIE_JAR" -X POST "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes" \
  -F "name=pfx-test-node" -F "address=127.0.0.1:${HOST_GRPC_PORT}" \
  -F "pfx_password=dashboard-test-pfx" -F "pfx=@$WORKDIR/admin.pfx;type=application/x-pkcs12")"
PFX_NODE_ID="$(echo "$PFX_ADD_RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])' 2>/dev/null)"
[ -n "$PFX_NODE_ID" ] || { echo "Dashboard test FAILED: .pfx add-node didn't return an id: $PFX_ADD_RESP" >&2; exit 1; }
curl -sk -b "$COOKIE_JAR" -X DELETE "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes/${PFX_NODE_ID}" >/dev/null
echo "Add-node via .pfx OK: $PFX_ADD_RESP"

WRONG_PW_RESP="$(curl -sk -b "$COOKIE_JAR" -o /dev/null -w '%{http_code}' -X POST "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes" \
  -F "name=wrong-pw" -F "address=127.0.0.1:${HOST_GRPC_PORT}" \
  -F "pfx_password=not-the-password" -F "pfx=@$WORKDIR/admin.pfx;type=application/x-pkcs12")"
[ "$WRONG_PW_RESP" = "400" ] || { echo "Dashboard test FAILED: wrong .pfx password should return 400, got $WRONG_PW_RESP" >&2; exit 1; }

openssl pkcs12 -export -inkey "$WORKDIR/admin.key" -in "$WORKDIR/admin.crt" \
  -out "$WORKDIR/admin-no-ca.pfx" -passout pass:dashboard-test-pfx >/dev/null 2>&1
NO_CA_RESP="$(curl -sk -b "$COOKIE_JAR" -X POST "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes" \
  -F "name=no-ca" -F "address=127.0.0.1:${HOST_GRPC_PORT}" \
  -F "pfx_password=dashboard-test-pfx" -F "pfx=@$WORKDIR/admin-no-ca.pfx;type=application/x-pkcs12")"
echo "$NO_CA_RESP" | grep -q "no CA certificate bundled" || { echo "Dashboard test FAILED: .pfx with no bundled CA should be refused with a clear message, got: $NO_CA_RESP" >&2; exit 1; }
echo ".pfx error paths OK: wrong password and missing-CA both refused with a real, actionable message"

# dashboardd allocates the first free port in its own pool - pin it low
# for this test by patching the request's own expectations rather than
# the binary: read back whatever port it actually picked.
python3 - "$WORKDIR/ca.crt" "$WORKDIR/admin.crt" "$WORKDIR/admin.key" "$HOST_GRPC_PORT" "$WORKDIR/add-node.json" <<'PYEOF'
import json, sys
ca, crt, key, grpc_port, out = sys.argv[1:6]
req = {
    "name": "test-node",
    "address": f"127.0.0.1:{grpc_port}",
    "ca_cert_pem": open(ca).read(),
    "bootstrap_cert_pem": open(crt).read(),
    "bootstrap_key_pem": open(key).read(),
}
open(out, "w").write(json.dumps(req))
PYEOF

ADD_RESP="$(curl -sk -b "$COOKIE_JAR" -X POST "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes" -H "Content-Type: application/json" -d @"$WORKDIR/add-node.json")"
echo "add-node response: $ADD_RESP"
NODE_ID="$(echo "$ADD_RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
if [ -z "$NODE_ID" ]; then
  echo "Dashboard test FAILED: add-node didn't return an id" >&2
  cat "$WORKDIR/dashboardd.log" >&2
  exit 1
fi
echo "Add-node OK: id=$NODE_ID"
NODE_BASE="https://127.0.0.1:${DASHBOARD_ADDR_PORT}/nodes/${NODE_ID}"

# --- list ---
LIST_RESP="$(curl -sk -b "$COOKIE_JAR" "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes")"
echo "$LIST_RESP" | grep -qF "\"id\":\"$NODE_ID\"" || { echo "Dashboard test FAILED: GET /api/nodes didn't list the registered node: $LIST_RESP" >&2; exit 1; }
if echo "$LIST_RESP" | grep -q "bootstrap\|service"; then
  echo "Dashboard test FAILED: GET /api/nodes leaked credential material: $LIST_RESP" >&2
  exit 1
fi
echo "List OK: node present, no credential material in the response"

# --- the node's page relays, for the signed-in admin ---
INFO="$(curl -sk -b "$COOKIE_JAR" "${NODE_BASE}/api/info")"
echo "relay response: $INFO"
echo "$INFO" | grep -q '"kernel_version"' || { echo "Dashboard test FAILED: relay response missing kernel_version: $INFO" >&2; exit 1; }
echo "$INFO" | grep -q '"active_slot":"A"' || { echo "Dashboard test FAILED: relay response's active_slot wasn't A: $INFO" >&2; exit 1; }
mem_total="$(echo "$INFO" | python3 -c 'import json,sys; print(json.load(sys.stdin)["memory"]["total_bytes"])')"
[ "$mem_total" -gt 0 ] || { echo "Dashboard test FAILED: relayed memory total_bytes was 0" >&2; exit 1; }
echo "Relay OK: real data (kernel_version, active_slot=A, memory=${mem_total} bytes) genuinely round-tripped through the dashboard to the real node and back"

# --- the node's page itself (nodeproxy's go:embed'd node app, not the
# main SPA), its assets relative to /nodes/<id>/ ---
NODE_UI="$(curl -sk "${NODE_BASE}/")"
echo "$NODE_UI" | grep -q '<title>Janus Node</title>' || { echo "Dashboard test FAILED: the node page isn't served at /nodes/<id>/: $NODE_UI" >&2; exit 1; }
node_asset="$(echo "$NODE_UI" | grep -o 'src="./assets/[^"]*"' | head -1 | sed 's/src="\.\///; s/"$//')"
[ "$(curl -sk -o /dev/null -w '%{http_code}' "${NODE_BASE}/${node_asset}")" = 200 ] || { echo "Dashboard test FAILED: the node page's asset ${node_asset} isn't served under its page" >&2; exit 1; }
echo "Node page OK: the node app is served at /nodes/<id>/ with its assets"

# --- tranche 4: ops/config relay (dashboard/backend/internal/nodeproxy/ops.go) ---
DASH_CERT=(-b "$COOKIE_JAR")

HAPROXY_INFO="$(curl -sk "${DASH_CERT[@]}" "${NODE_BASE}/api/haproxy/info")"
echo "$HAPROXY_INFO" | grep -q '"version"' || { echo "Dashboard test FAILED: /api/haproxy/info missing version: $HAPROXY_INFO" >&2; exit 1; }
echo "ShowInfo relay OK: $HAPROXY_INFO"

# PacketCapture relay (nodeproxy/pcap.go): a bounded capture comes back
# as a .pcap download holding the HTTP traffic generated meanwhile, and a
# bad request fails with a real HTTP error instead of a broken file.
( sleep 1; for _ in 1 2; do curl -s -o /dev/null "http://127.0.0.1:${HOST_HTTP_PORT}/"; done ) &
PCAP_CURL_PID=$!
pcap_code="$(curl -sk "${DASH_CERT[@]}" -D "$WORKDIR/pcap-headers.txt" -o "$WORKDIR/relay.pcap" -w '%{http_code}' "${NODE_BASE}/api/pcap?interface=eth0&filter=tcp%20port%208080&duration=3")"
wait "$PCAP_CURL_PID"
[ "$pcap_code" = "200" ] || { echo "Dashboard test FAILED: /api/pcap returned $pcap_code: $(cat "$WORKDIR/relay.pcap")" >&2; exit 1; }
grep -qi '^content-disposition: attachment; filename="janus-.*-eth0-.*\.pcap"' "$WORKDIR/pcap-headers.txt" || { echo "Dashboard test FAILED: /api/pcap isn't served as a .pcap attachment: $(cat "$WORKDIR/pcap-headers.txt")" >&2; exit 1; }
python3 - "$WORKDIR/relay.pcap" <<'PYEOF' || { echo "Dashboard test FAILED: the relayed pcap doesn't hold the captured HTTP traffic" >&2; exit 1; }
import struct, sys
data = open(sys.argv[1], "rb").read()
assert struct.unpack("<I", data[:4])[0] == 0xa1b2c3d4, "not a pcap file"
off, n, ok = 24, 0, 0
while off < len(data):
    incl = struct.unpack("<I", data[off + 8:off + 12])[0]
    pkt = data[off + 16:off + 16 + incl]
    off += 16 + incl
    n += 1
    l4 = 14 + (pkt[14] & 0x0f) * 4
    assert pkt[23] == 6 and 8080 in struct.unpack(">HH", pkt[l4:l4 + 4]), f"packet {n} doesn't match 'tcp port 8080'"
    ok += b"HTTP/1.1 200" in pkt
assert ok >= 2, f"expected 2 HTTP 200 responses, saw {ok}"
print(f"relayed pcap: {n} packets, all tcp port 8080, {ok} HTTP 200 responses")
PYEOF
bad_filter="$(curl -sk "${DASH_CERT[@]}" -w ' %{http_code}' "${NODE_BASE}/api/pcap?interface=eth0&filter=tcp%5B13%5D&duration=1")"
case "$bad_filter" in *"unsupported filter keyword"*" 400") ;; *) echo "Dashboard test FAILED: an unsupported filter should be a 400 with the node's message, got: $bad_filter" >&2; exit 1 ;; esac
no_duration="$(curl -sk "${DASH_CERT[@]}" -o /dev/null -w '%{http_code}' "${NODE_BASE}/api/pcap?interface=eth0")"
[ "$no_duration" = "400" ] || { echo "Dashboard test FAILED: a capture with no duration should be refused (400), got $no_duration" >&2; exit 1; }
# Unfiltered, the node leaves out its own connection to this dashboard.
curl -sk "${DASH_CERT[@]}" -o "$WORKDIR/relay-own.pcap" "${NODE_BASE}/api/pcap?interface=eth0&duration=2"
own_packets="$(python3 - "$WORKDIR/relay-own.pcap" <<'PYEOF'
import struct, sys
data = open(sys.argv[1], "rb").read()
off, n = 24, 0
while off < len(data):
    incl = struct.unpack("<I", data[off + 8:off + 12])[0]
    pkt = data[off + 16:off + 16 + incl]
    off += 16 + incl
    if struct.unpack(">H", pkt[12:14])[0] == 0x0800 and pkt[23] == 6:
        l4 = 14 + (pkt[14] & 0x0f) * 4
        n += 9505 in struct.unpack(">HH", pkt[l4:l4 + 4])
print(n)
PYEOF
)"
[ "$own_packets" -eq 0 ] || { echo "Dashboard test FAILED: an unfiltered relayed capture holds $own_packets packets of the dashboard's own stream" >&2; exit 1; }
echo "PacketCapture relay OK: .pcap download with real HTTP traffic, own stream left out, bad filter and unbounded capture refused"

# --- the node page's relays (nodeproxy/system.go) against the real node ---
jget() { curl -sk "${DASH_CERT[@]}" -w '\n%{http_code}' "${NODE_BASE}$1"; }
expect_json() { # expect_json PATH PYTHON-ASSERTION-ON-d DESCRIPTION
  local out code body
  out="$(jget "$1")"; code="${out##*$'\n'}"; body="${out%$'\n'*}"
  [ "$code" = "200" ] || { echo "Dashboard test FAILED: $3: GET $1 -> $code: $body" >&2; exit 1; }
  python3 -c "import json,sys; d=json.loads(sys.argv[1]); assert $2, d" "$body" >/dev/null 2>&1 || { echo "Dashboard test FAILED: $3: GET $1 returned $body" >&2; exit 1; }
}
expect_json /api/metrics 'd["system"]["cpu_total_ticks"] > 0 and d["memory"]["total_bytes"] > 0 and d["haproxy"]["version"] and any(x["id"] == "haproxy" for x in d["services"]["processes"])' "metrics aggregate"
expect_json /api/system/overview 'd["hostname"] and d["version"]["active_slot"] == "A" and d["system"]["boot_time_unix"] > 0' "system overview"
expect_json /api/system/services 'any(s["id"] == "haproxy" and s["state"] == "running" and s["health"] == "healthy" for s in d["services"])' "services"
# A default-schematic node's update comes from GitHub Releases; whether
# GitHub answers from here isn't what's under test, the node's side is.
expect_json /api/update-check 'd["default_schematic"] and d["schematic_id"] == "a055fbb697e2d0abb0c5911e7702b07040f49eb71befeaf9b90495a905327f47" and d["arch"] == "amd64" and d["extensions"] == [] and d["source"] == "github" and d["state"] in ("ready", "unavailable") and d["version"]' "update check"
# Changing the extensions (nodeproxy/update.go). Only what starts no
# image-factory build: back to the default schematic (GitHub, like the
# check above), and an invalid name the Controller refuses itself. The
# catalog comes from the real factory - a 502 if it can't be reached.
jpost() { curl -sk "${DASH_CERT[@]}" -w '\n%{http_code}' -X POST -H 'Content-Type: application/json' -d "$2" "${NODE_BASE}$1"; }
out="$(jpost /api/factory/update '{"extensions":[]}')"; code="${out##*$'\n'}"; body="${out%$'\n'*}"
[ "$code" = "200" ] && python3 -c 'import json,sys; d=json.loads(sys.argv[1]); assert d["default_schematic"] and not d["schematic_change"] and d["extensions"] == [] and d["node_schematic_id"] == d["schematic_id"] and d["source"] == "github" and d["state"] in ("ready", "unavailable")' "$body" 2>/dev/null ||
  { echo "Dashboard test FAILED: extensions update for the default schematic -> $code: $body" >&2; exit 1; }
out="$(jpost /api/factory/update '{"extensions":["Not Valid!"]}')"; code="${out##*$'\n'}"
[ "$code" = "400" ] || { echo "Dashboard test FAILED: an invalid extension name should be refused with 400, got $code: ${out%$'\n'*}" >&2; exit 1; }
out="$(jget /api/factory/catalog)"; code="${out##*$'\n'}"; body="${out%$'\n'*}"
if [ "$code" = "200" ]; then
  python3 -c 'import json,sys; d=json.loads(sys.argv[1]); assert d["version"] and d["extensions"] and all(e["name"] and e["arches"] for e in d["extensions"])' "$body" 2>/dev/null ||
    { echo "Dashboard test FAILED: extension catalog: $body" >&2; exit 1; }
elif [ "$code" = "502" ] && grep -q "image factory" <<<"$body"; then
  echo "  (extension catalog: the image factory isn't reachable from here: $body)"
else
  echo "Dashboard test FAILED: extension catalog -> $code: $body" >&2; exit 1
fi
expect_json /api/system/metrics-config 'd["config"]["enabled"] and d["config"]["port"] == 10056 and d["listening"] and d["is_default"]' "exporter config"
expect_json /api/network/firewall 'd["state"] == "not_enabled"' "firewall module (not in the default image)"
expect_json /api/network/vrrp 'd["state"] == "not_enabled"' "VRRP module (not in the default image)"
expect_json /api/network/bgp 'd["state"] == "not_enabled"' "BGP module (not in the default image)"
expect_json /api/network/consul 'd["state"] == "not_enabled"' "Consul extension (not in the default image)"
expect_json /api/haproxy/acme 'd["state"] == "not_enabled"' "Let's Encrypt extension (not in the default image)"
expect_json /api/haproxy/files 'd["dir"] == "/etc/haproxy/files" and isinstance(d["files"], list)' "HAProxy files"
expect_json /api/system/processes 'any(p["pid"] == 1 for p in d["processes"])' "processes"
expect_json /api/system/mounts 'any(m["mounted_on"] == "/etc/.state" for m in d["mounts"])' "mounts"
expect_json /api/system/netstat 'any(c["local_address"].endswith(":9505") and c["state"] == "LISTEN" for c in d["connections"])' "netstat"
expect_json /api/network/modules 'd["bgp"]["state"] == "not_enabled"' "network modules"
expect_json '/api/files/list?path=/etc/haproxy' 'any(f["relative_name"] == "haproxy.cfg" for f in d)' "files list"
expect_json '/api/files/du?path=/etc/haproxy' 'int(d[0]["size_bytes"]) > 0' "disk usage"
expect_json /api/haproxy/stats '"pxname" in d["columns"] and len(d["rows"]) > 0' "haproxy stats"
curl -sk "${DASH_CERT[@]}" "${NODE_BASE}/api/files/read?path=/etc/haproxy/haproxy.cfg" | grep -q '^frontend ' || { echo "Dashboard test FAILED: files/read didn't return haproxy.cfg" >&2; exit 1; }
curl -sk "${DASH_CERT[@]}" -o "$WORKDIR/etc-haproxy.tar" "${NODE_BASE}/api/files/copy?path=/etc/haproxy"
tar tf "$WORKDIR/etc-haproxy.tar" | grep -qx 'haproxy/haproxy.cfg' || { echo "Dashboard test FAILED: files/copy isn't a tar holding haproxy/haproxy.cfg" >&2; exit 1; }
dev_code="$(curl -sk "${DASH_CERT[@]}" -o /dev/null -w '%{http_code}' "${NODE_BASE}/api/files/read?path=/dev/vda")"
[ "$dev_code" = "403" ] || { echo "Dashboard test FAILED: reading /dev/vda through the relay answered $dev_code, want 403" >&2; exit 1; }
valid="$(python3 -c 'import json,sys; print(json.dumps({"config": open(sys.argv[1]).read()}))' <(curl -sk "${DASH_CERT[@]}" "${NODE_BASE}/api/haproxy/config" | python3 -c 'import json,sys; print(json.load(sys.stdin)["config"], end="")'))"
curl -sk "${DASH_CERT[@]}" -H 'Content-Type: application/json' -d "$valid" "${NODE_BASE}/api/haproxy/validate" | grep -q '"valid":true' || { echo "Dashboard test FAILED: the running config didn't validate through the relay" >&2; exit 1; }
curl -sk "${DASH_CERT[@]}" -X POST "${NODE_BASE}/api/haproxy/reload" | grep -q '"success":true' || { echo "Dashboard test FAILED: reload through the relay" >&2; exit 1; }
curl -sk "${DASH_CERT[@]}" -H 'Content-Type: application/json' -d '{"role":"os:reader","format":"pfx","password":"pw","name":"t"}' -o "$WORKDIR/reader.pfx" "${NODE_BASE}/api/pki/client"
openssl pkcs12 -in "$WORKDIR/reader.pfx" -passin pass:pw -nokeys -clcerts 2>/dev/null | openssl x509 -noout -subject | grep -q 'O *= *os:reader' || { echo "Dashboard test FAILED: the issued .pfx isn't a reader certificate for this node" >&2; exit 1; }
# Live streams: each must deliver real content over Server-Sent Events.
timeout 5 curl -skN "${DASH_CERT[@]}" "${NODE_BASE}/api/stream/dmesg" > "$WORKDIR/dmesg.sse" || true
grep -q '^data: .*Linux version' "$WORKDIR/dmesg.sse" || { echo "Dashboard test FAILED: the dmesg stream didn't carry the kernel log: $(head -c 400 "$WORKDIR/dmesg.sse")" >&2; exit 1; }
timeout 4 curl -skN "${DASH_CERT[@]}" "${NODE_BASE}/api/stream/logs?id=janusd&tail=50" > "$WORKDIR/logs.sse" || true
grep -q '^data: .*listening on' "$WORKDIR/logs.sse" || { echo "Dashboard test FAILED: the janusd log stream: $(head -c 400 "$WORKDIR/logs.sse")" >&2; exit 1; }
timeout 4 curl -skN "${DASH_CERT[@]}" "${NODE_BASE}/api/stream/events" > "$WORKDIR/events.sse" || true
grep -q '"type":"haproxy.reloaded"' "$WORKDIR/events.sse" || { echo "Dashboard test FAILED: the event stream didn't show the reload just made: $(head -c 400 "$WORKDIR/events.sse")" >&2; exit 1; }
# Fleet status on the main port: the real node online, on slot A.
curl -sk -b "$COOKIE_JAR" "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes/status" | python3 -c 'import json,sys; d=json.load(sys.stdin); s=d[sys.argv[1]]; assert s["reachable"] and s["active_slot"] == "A" and s["haproxy_health"] == "healthy", s' "$NODE_ID" \
  || { echo "Dashboard test FAILED: /api/nodes/status for the real node" >&2; exit 1; }
# janusd restart through the relay: the shared gRPC connection must
# reconnect on its own once the node is back.
restart_code="$(curl -sk "${DASH_CERT[@]}" -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' -d '{"action":"restart"}' "${NODE_BASE}/api/system/power")"
[ "$restart_code" = "200" ] || { echo "Dashboard test FAILED: janusd restart through the relay answered $restart_code" >&2; exit 1; }
sleep 4
back=""
for _ in $(seq 1 20); do
  if curl -sk "${DASH_CERT[@]}" "${NODE_BASE}/api/system/overview" | grep -q '"hostname"'; then back=1; break; fi
  sleep 1
done
[ -n "$back" ] || { echo "Dashboard test FAILED: the relay never reconnected after janusd restarted" >&2; exit 1; }
echo "Node page relays OK: metrics, system views, files (read/list/du/tar, device refused), HAProxy stats/validate/reload, .pfx issuance, SSE streams (dmesg/logs/events), fleet status, and a janusd restart the shared connection recovered from"

GETCFG="$(curl -sk "${DASH_CERT[@]}" "${NODE_BASE}/api/haproxy/config")"
ORIG_SHA256="$(echo "$GETCFG" | python3 -c 'import json,sys; print(json.load(sys.stdin)["sha256"])')"
[ -n "$ORIG_SHA256" ] || { echo "Dashboard test FAILED: /api/haproxy/config missing sha256: $GETCFG" >&2; exit 1; }
echo "GetConfig relay OK: sha256=$ORIG_SHA256"

# Re-apply the exact same config read back - must be accepted (proves the
# round trip byte-for-byte). $(...) capturing GETCFG above only strips
# trailing newlines from the outer JSON document, not the config text's
# own embedded newlines (those survive as escaped "\n" inside the JSON
# string) - safe to re-extract from $GETCFG directly here. Re-encoding
# the raw config bytes through a shell variable instead, and stripping
# ITS trailing newline via $(...), is the mistake that was actually made
# once while writing this test - not this.
python3 -c '
import json, sys
cfg = json.loads(sys.argv[1])["config"]
json.dump({"config": cfg}, open(sys.argv[2], "w"))
' "$GETCFG" "$WORKDIR/apply-same.json"
APPLY_SAME="$(curl -sk "${DASH_CERT[@]}" -X POST -H "Content-Type: application/json" -d @"$WORKDIR/apply-same.json" "${NODE_BASE}/api/haproxy/config")"
echo "$APPLY_SAME" | grep -q '"accepted":true' || { echo "Dashboard test FAILED: re-applying the read-back config was rejected: $APPLY_SAME" >&2; exit 1; }
echo "ApplyConfig round-trip OK: $APPLY_SAME"

# Apply deliberately invalid config - must be rejected, with a real
# validation error (internal/haproxy.Manager.Validate's own fallback for
# an empty haproxy -c output - see its own comment), and must not
# disturb the running config.
python3 -c 'import json; print(json.dumps({"config": "this is not a valid haproxy config !!!"}))' > "$WORKDIR/apply-garbage.json"
APPLY_GARBAGE="$(curl -sk "${DASH_CERT[@]}" -X POST -H "Content-Type: application/json" -d @"$WORKDIR/apply-garbage.json" "${NODE_BASE}/api/haproxy/config")"
echo "$APPLY_GARBAGE" | grep -q '"accepted":false' || { echo "Dashboard test FAILED: garbage config wasn't rejected: $APPLY_GARBAGE" >&2; exit 1; }
echo "$APPLY_GARBAGE" | grep -q '"message":""' && { echo "Dashboard test FAILED: garbage config rejected with no error message: $APPLY_GARBAGE" >&2; exit 1; }
AFTER_GARBAGE_SHA256="$(curl -sk "${DASH_CERT[@]}" "${NODE_BASE}/api/haproxy/config" | python3 -c 'import json,sys; print(json.load(sys.stdin)["sha256"])')"
[ "$AFTER_GARBAGE_SHA256" = "$ORIG_SHA256" ] || { echo "Dashboard test FAILED: running config changed after a rejected apply (was $ORIG_SHA256, now $AFTER_GARBAGE_SHA256)" >&2; exit 1; }
echo "ApplyConfig rejection OK: garbage config refused with a real error message, running config untouched"

# BackendList: the bootstrap config has no backend, so a real success
# with an empty list (protojson drops the empty repeated field: {}).
BACKENDS_CODE="$(curl -sk "${DASH_CERT[@]}" -o "$WORKDIR/backends-resp.json" -w '%{http_code}' "${NODE_BASE}/api/haproxy/backends")"
[ "$BACKENDS_CODE" = "200" ] && python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert d.get("backends", []) == []' "$WORKDIR/backends-resp.json" || { echo "Dashboard test FAILED: BackendList relay (code=$BACKENDS_CODE): $(cat "$WORKDIR/backends-resp.json")" >&2; exit 1; }
echo "BackendList relay OK: real (empty) backend list for the bootstrap config"

# The bootstrap config declares no file-backed maps - MapList must
# relay a genuinely empty list (protojson's omitempty drops an empty
# repeated field entirely, so "{}" is the correct empty response, not a
# gap - confirmed against a real boot before encoding this assertion).
MAPS_RESP="$(curl -sk "${DASH_CERT[@]}" "${NODE_BASE}/api/haproxy/maps")"
[ "$MAPS_RESP" = "{}" ] || { echo "Dashboard test FAILED: expected an empty MapList response, got: $MAPS_RESP" >&2; exit 1; }
echo "MapList relay OK: genuinely empty (bootstrap config declares no file-backed maps)"

# MapUpdate against a map that doesn't exist must surface HAProxy's own
# real error through the relay, not a fake success - exercising a real
# success case needs a custom boot fixture with a file-backed map
# declared (already covered at the internal/haproxy layer by
# image-build.yml's own runtime map/ACL/certificate test).
MAPUPDATE_CODE="$(curl -sk "${DASH_CERT[@]}" -o /tmp/mapupdate-resp.$$ -w '%{http_code}' -X POST -H "Content-Type: application/json" -d '{"key":"k","value":"v","delete":false}' "${NODE_BASE}/api/haproxy/maps/nonexistent.map")"
[ "$MAPUPDATE_CODE" = "502" ] && grep -qi "map identifier\|no such\|unknown" /tmp/mapupdate-resp.$$ || { echo "Dashboard test FAILED: MapUpdate against a nonexistent map didn't surface a real error (code=$MAPUPDATE_CODE): $(cat /tmp/mapupdate-resp.$$)" >&2; rm -f /tmp/mapupdate-resp.$$; exit 1; }
rm -f /tmp/mapupdate-resp.$$
echo "MapUpdate relay OK: genuinely reaches the real node and surfaces its real error"

# CertificateList starts genuinely empty too (bootstrap config has no
# ssl bind/crt-list at all).
CERTS_RESP="$(curl -sk "${DASH_CERT[@]}" "${NODE_BASE}/api/haproxy/certs")"
[ "$CERTS_RESP" = "{}" ] || { echo "Dashboard test FAILED: expected an empty CertificateList response, got: $CERTS_RESP" >&2; exit 1; }

# Real CertificateUpload/List/Delete round trip - a throwaway
# self-signed test cert, no crt_list (the cert store accepts an upload
# unconditionally; binding it into a crt-list needs a config that
# declares one, out of scope here - see internal/haproxy/runtime_certs.go's
# own doc comment). Built via a real FILE, not a $(...) substitution -
# command substitution strips the bundle's own trailing newline, which
# left HAProxy's stats-socket "set ssl cert <<\n<bundle>" heredoc
# waiting forever for the rest of a line that never arrives (a real gap
# in internal/haproxy.Manager.statsCommand's missing read deadline,
# fixed alongside this test - see its own comment - but the bundle
# still needs to be well-formed here to exercise the real success path).
openssl req -x509 -newkey ed25519 -keyout "$WORKDIR/testcert.key" -out "$WORKDIR/testcert.crt" -days 1 -nodes -subj "/CN=dashboard-test.example.com" >/dev/null 2>&1
cat "$WORKDIR/testcert.crt" "$WORKDIR/testcert.key" > "$WORKDIR/testcert-bundle.pem"
python3 -c '
import json, sys
bundle = open(sys.argv[1]).read()
json.dump({"name": "dashboard-test.pem", "pem_bundle": bundle}, open(sys.argv[2], "w"))
' "$WORKDIR/testcert-bundle.pem" "$WORKDIR/cert-upload.json"
UPLOAD_RESP="$(curl -sk "${DASH_CERT[@]}" -m 15 -X POST -H "Content-Type: application/json" -d @"$WORKDIR/cert-upload.json" "${NODE_BASE}/api/haproxy/certs")"
[ "$UPLOAD_RESP" = "{}" ] || { echo "Dashboard test FAILED: CertificateUpload didn't return success: $UPLOAD_RESP" >&2; exit 1; }
LIST_AFTER_UPLOAD="$(curl -sk "${DASH_CERT[@]}" "${NODE_BASE}/api/haproxy/certs")"
echo "$LIST_AFTER_UPLOAD" | grep -qF '"name":"dashboard-test.pem"' || { echo "Dashboard test FAILED: uploaded cert not listed: $LIST_AFTER_UPLOAD" >&2; exit 1; }
DELETE_CODE="$(curl -sk "${DASH_CERT[@]}" -o /dev/null -w '%{http_code}' -X DELETE "${NODE_BASE}/api/haproxy/certs/dashboard-test.pem")"
[ "$DELETE_CODE" = "200" ] || { echo "Dashboard test FAILED: CertificateDelete returned $DELETE_CODE, want 200" >&2; exit 1; }
LIST_AFTER_DELETE_CERT="$(curl -sk "${DASH_CERT[@]}" "${NODE_BASE}/api/haproxy/certs")"
[ "$LIST_AFTER_DELETE_CERT" = "{}" ] || { echo "Dashboard test FAILED: cert still listed after delete: $LIST_AFTER_DELETE_CERT" >&2; exit 1; }
echo "Certificate relay OK: real upload/list/delete round trip against the real node's cert store"

# --- the node's API needs an account: none is 401; a node that doesn't
# trust the fleet yet - reached as admin - is refused a reader ---
no_session_code="$(curl -sk -o /dev/null -w '%{http_code}' -m 3 "${NODE_BASE}/api/info")"
[ "$no_session_code" = 401 ] || { echo "Dashboard test FAILED: the node's API with no session answered $no_session_code" >&2; exit 1; }
reader_old_code="$(curl -sk -b "$READER_JAR" -o /dev/null -w '%{http_code}' -m 3 "${NODE_BASE}/api/info")"
[ "$reader_old_code" = 403 ] || { echo "Dashboard test FAILED: a reader read a node that doesn't trust the fleet ($reader_old_code)" >&2; exit 1; }
echo "Node page gate OK: no session refused, a reader refused a node reached as admin"

# --- delete ---
del_code="$(curl -sk -b "$COOKIE_JAR" -X DELETE -o /dev/null -w '%{http_code}' "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes/${NODE_ID}")"
[ "$del_code" = "204" ] || { echo "Dashboard test FAILED: DELETE returned $del_code, want 204" >&2; exit 1; }
LIST_AFTER_DELETE="$(curl -sk -b "$COOKIE_JAR" "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes")"
if echo "$LIST_AFTER_DELETE" | grep -qF "\"id\":\"$NODE_ID\""; then
  echo "Dashboard test FAILED: node still listed after DELETE: $LIST_AFTER_DELETE" >&2
  exit 1
fi
after_delete_code="$(curl -sk -b "$COOKIE_JAR" -o /dev/null -w '%{http_code}' -m 3 "${NODE_BASE}/api/info")"
[ "$after_delete_code" = "404" ] || { echo "Dashboard test FAILED: the deleted node's page answered $after_delete_code" >&2; exit 1; }
echo "Delete OK: node unregistered, its page gone"

# --- persistence across a restart ---
curl -sk -b "$COOKIE_JAR" -X POST "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes" -H "Content-Type: application/json" -d @"$WORKDIR/add-node.json" > /dev/null
kill "$DASHBOARD_PID"
wait "$DASHBOARD_PID" 2>/dev/null || true
"$DASHBOARDD" -addr ":${DASHBOARD_ADDR_PORT}" -register-addr ":${DASHBOARD_REGISTER_PORT}" -data-dir "$WORKDIR/data" > "$WORKDIR/dashboardd-restart.log" 2>&1 &
DASHBOARD_PID=$!
sleep 1

# Sessions are deliberately in-memory only (internal/auth's own doc
# comment) - they don't survive a restart, so the old cookie must now
# be rejected, and a fresh login is needed before the registry is
# reachable again. The admin password itself (auth.json, on disk) does
# survive, same as the node registry.
stale_session_code="$(curl -sk -b "$COOKIE_JAR" -o /dev/null -w '%{http_code}' "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes")"
[ "$stale_session_code" = "401" ] || { echo "Dashboard test FAILED: a pre-restart session should not survive dashboardd restarting, got $stale_session_code" >&2; exit 1; }
relogin_code="$(curl -sk -c "$COOKIE_JAR" -o /dev/null -w '%{http_code}' -X POST "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/auth/login" -H "Content-Type: application/json" -d '{"password":"dashboard-test-admin-pw"}')"
[ "$relogin_code" = "204" ] || { echo "Dashboard test FAILED: re-login after restart with the same (persisted) password should succeed, got $relogin_code" >&2; exit 1; }
# Its second factor: the next code (the current one may be the one used).
code_code="$(curl -sk -b "$COOKIE_JAR" -o /dev/null -w '%{http_code}' -X POST "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/auth/mfa/totp" -H 'Content-Type: application/json' -d "{\"code\":\"$(python3 "$HACK/totp.py" "$TOTP_SECRET" 1)\"}")"
[ "$code_code" = "204" ] || { echo "Dashboard test FAILED: the second factor after the restart (the master key opening the secret) answered $code_code" >&2; exit 1; }
echo "Auth persistence OK: the admin password survives a restart, the session doesn't - a fresh login is required"

PENDING_AFTER_RESTART="$(curl -sk -b "$COOKIE_JAR" "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/pending")"
echo "$PENDING_AFTER_RESTART" | grep -qF "\"id\":\"$PENDING_ID\"" || { echo "Dashboard test FAILED: the pending self-registration didn't survive a dashboardd restart: $PENDING_AFTER_RESTART" >&2; exit 1; }
echo "Pending persistence OK: the self-registered node's pending entry survived a dashboardd restart"

RESTART_LIST="$(curl -sk -b "$COOKIE_JAR" "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/nodes")"
echo "$RESTART_LIST" | grep -q '"name":"test-node"' || { echo "Dashboard test FAILED: node registry didn't survive a restart: $RESTART_LIST" >&2; cat "$WORKDIR/dashboardd-restart.log" >&2; exit 1; }
RESTART_ID="$(echo "$RESTART_LIST" | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["id"])')"
NODE_BASE="https://127.0.0.1:${DASHBOARD_ADDR_PORT}/nodes/${RESTART_ID}"
restart_relay_code="$(curl -sk -b "$COOKIE_JAR" -o /dev/null -w '%{http_code}' "${NODE_BASE}/api/info")"
[ "$restart_relay_code" = "200" ] || { echo "Dashboard test FAILED: the node's page didn't relay after restart (got $restart_relay_code)" >&2; exit 1; }
echo "Restart persistence OK: node registry and its page both survived a dashboardd restart against the same data directory"

# signin signs the admin in again after a restart (sessions are in
# memory) - its second factor a recovery code, one per sign-in (a code
# from the app is good once per 30 s).
RECOVERY_USED=0
signin() {
  curl -sk -c "$COOKIE_JAR" -o /dev/null -X POST "$API/api/auth/login" -H 'Content-Type: application/json' -d '{"password":"dashboard-test-admin-pw"}'
  local code
  code="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["recovery_codes"][int(sys.argv[2])])' "$WORKDIR/recovery.json" "$RECOVERY_USED")"
  RECOVERY_USED=$((RECOVERY_USED + 1))
  [ "$(curl -sk -b "$COOKIE_JAR" -o /dev/null -w '%{http_code}' -X POST "$API/api/auth/mfa/recovery" -H 'Content-Type: application/json' -d "{\"code\":\"$code\"}")" = 204 ]
}

# --- the fleet (dashboard/backend/fleet.go): set up, its recovery kit
# given back, then the node brought to trust it - the Controller reaches
# it with its fleet certificate, acting for the browser's user, and no
# longer keeps a credential for it ---
API="https://127.0.0.1:${DASHBOARD_ADDR_PORT}"
FLEET_NODE_ID="$(echo "$RESTART_LIST" | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["id"])')"
curl -sk -b "$COOKIE_JAR" "$API/api/fleet" | grep -q '"state":"none"' || { echo "Dashboard test FAILED: a fresh Controller has a fleet: $(curl -sk -b "$COOKIE_JAR" "$API/api/fleet")" >&2; exit 1; }
curl -sk -b "$COOKIE_JAR" -X POST "$API/api/fleet/setup" -o "$WORKDIR/fleet-setup.json"
PASSPHRASE="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["passphrase"])' "$WORKDIR/fleet-setup.json")"
curl -sk -b "$COOKIE_JAR" "$API/api/fleet/recovery-kit" -o "$WORKDIR/recovery-kit.age"
head -1 "$WORKDIR/recovery-kit.age" | grep -q 'BEGIN AGE ENCRYPTED FILE' || { echo "Dashboard test FAILED: the recovery kit isn't an armored age file" >&2; exit 1; }
confirm_body() { python3 -c 'import json,sys; print(json.dumps({"kit": open(sys.argv[1]).read(), "passphrase": sys.argv[2]}))' "$WORKDIR/recovery-kit.age" "$1" >"$WORKDIR/confirm.json"; }
confirm_body "0000-0000-0000-0000-0000-0000"
wrong_code="$(curl -sk -b "$COOKIE_JAR" -o /dev/null -w '%{http_code}' -X POST "$API/api/fleet/confirm" -H 'Content-Type: application/json' --data-binary @"$WORKDIR/confirm.json")"
[ "$wrong_code" = "400" ] || { echo "Dashboard test FAILED: a wrong passphrase should be refused with 400, got $wrong_code" >&2; exit 1; }
curl -sk -b "$COOKIE_JAR" "$API/api/fleet" | grep -q '"state":"pending"' || { echo "Dashboard test FAILED: a wrong passphrase changed the fleet's state" >&2; exit 1; }
confirm_body "$PASSPHRASE"
ok_code="$(curl -sk -b "$COOKIE_JAR" -o /dev/null -w '%{http_code}' -X POST "$API/api/fleet/confirm" -H 'Content-Type: application/json' --data-binary @"$WORKDIR/confirm.json")"
[ "$ok_code" = "200" ] || { echo "Dashboard test FAILED: confirming the kit with its passphrase: $ok_code" >&2; exit 1; }
for f in root.key.sealed recovery-kit.age; do
  [ ! -e "$WORKDIR/data/fleet/$f" ] || { echo "Dashboard test FAILED: $f still on the Controller after the kit was confirmed" >&2; exit 1; }
done
trust_state() { curl -sk -b "$COOKIE_JAR" "$API/api/fleet" | python3 -c 'import json,sys; print(json.load(sys.stdin)["nodes"].get(sys.argv[1], {}).get("state", ""))' "$FLEET_NODE_ID"; }
deadline=$((SECONDS + 60))
until [ "$(trust_state)" = "trusted" ]; do
  [ "$SECONDS" -lt "$deadline" ] || { echo "Dashboard test FAILED: the node never came to trust the fleet: $(curl -sk -b "$COOKIE_JAR" "$API/api/fleet")" >&2; cat "$WORKDIR/dashboardd-restart.log" >&2; exit 1; }
  sleep 1
done
[ ! -e "$WORKDIR/data/nodes/$FLEET_NODE_ID/service.key" ] || { echo "Dashboard test FAILED: the node's service key is still kept once it trusts the fleet" >&2; exit 1; }
grep -q '"fleet":true' "$WORKDIR/data/nodes/$FLEET_NODE_ID/meta.json" || { echo "Dashboard test FAILED: the node isn't recorded as trusting the fleet" >&2; exit 1; }
echo "Fleet setup OK: a wrong passphrase refused, the kit confirmed, the root key gone, the node trusts the fleet and its service key is deleted"

fleet_relay() { # the node's page, now relayed with the fleet certificate
  curl -sk -b "$COOKIE_JAR" -o /dev/null -w '%{http_code}' -X POST "${NODE_BASE}/api/system/services/haproxy/restart"
}
[ "$(fleet_relay)" = "200" ] || { echo "Dashboard test FAILED: restarting HAProxy through the fleet relay" >&2; exit 1; }

# The accounts' roles on the node itself: an operator restarts HAProxy,
# and the node refuses it what only an admin does; a reader reads now
# (the Controller acts for it as os:reader) and changes nothing.
OPERATOR_JAR="$WORKDIR/operator-cookies.txt"
curl -sk -b "$COOKIE_JAR" -o /dev/null -X POST "$API/api/users" -H 'Content-Type: application/json' -d '{"name":"olga","role":"operator","password":"olga-given-pw"}'
curl -sk -c "$OPERATOR_JAR" -o /dev/null -X POST "$API/api/auth/login" -H 'Content-Type: application/json' -d '{"name":"olga","password":"olga-given-pw"}'
curl -sk -b "$OPERATOR_JAR" -c "$OPERATOR_JAR" -o /dev/null -X POST "$API/api/auth/password" -H 'Content-Type: application/json' -d '{"current_password":"olga-given-pw","new_password":"olgas-own-password"}'
op_restart="$(curl -sk -b "$OPERATOR_JAR" -o /dev/null -w '%{http_code}' -X POST "${NODE_BASE}/api/system/services/haproxy/restart")"
op_issue="$(curl -sk -b "$OPERATOR_JAR" -o "$WORKDIR/op-issue.txt" -w '%{http_code}' -X POST "${NODE_BASE}/api/pki/client" -H 'Content-Type: application/json' -d '{"role":"os:reader","name":"x","ttl_seconds":3600,"format":"pem"}')"
curl -sk -c "$READER_JAR" -o /dev/null -X POST "$API/api/auth/login" -H 'Content-Type: application/json' -d '{"name":"rita","password":"ritas-own-password"}' # after the restart
reader_read="$(curl -sk -b "$READER_JAR" -o /dev/null -w '%{http_code}' "${NODE_BASE}/api/info")"
reader_change="$(curl -sk -b "$READER_JAR" -o /dev/null -w '%{http_code}' -X POST "${NODE_BASE}/api/system/services/haproxy/restart")"
[ "$op_restart/$op_issue/$reader_read/$reader_change" = 200/403/200/403 ] || { echo "Dashboard test FAILED: operator restart $op_restart, operator issuing a certificate $op_issue ($(cat "$WORKDIR/op-issue.txt")), reader read $reader_read, reader restart $reader_change (want 200/403/200/403)" >&2; exit 1; }
grep -q 'requires role \[os:admin\], olga has \[os:operator\]' "$WORKDIR/op-issue.txt" || { echo "Dashboard test FAILED: the operator's certificate refused by someone else than the node: $(cat "$WORKDIR/op-issue.txt")" >&2; exit 1; }

# A scoped account (no role over everything; operator on team=web,
# HAProxy only): it reaches the node once labelled, reloads HAProxy - the
# node logs it with its domain, having checked it -, and the Controller
# refuses the rest before it leaves; none of the Controller's own routes.
SCOPED_NODE="${NODE_BASE##*/}"
lab_code="$(curl -sk -b "$COOKIE_JAR" -o /dev/null -w '%{http_code}' -X PATCH "$API/api/nodes/$SCOPED_NODE" -H 'Content-Type: application/json' -d '{"labels":{"team":"web"}}')"
[ "$lab_code" = 200 ] || { echo "Dashboard test FAILED: labelling the node: $lab_code" >&2; exit 1; }
TF_JAR="$WORKDIR/tf-cookies.txt"
curl -sk -b "$COOKIE_JAR" -o /dev/null -X POST "$API/api/users" -H 'Content-Type: application/json' \
  -d '{"name":"tf","role":"none","password":"tf-given-password","grants":[{"role":"operator","selector":{"team":"web"},"domains":["haproxy"]}]}'
curl -sk -c "$TF_JAR" -o /dev/null -X POST "$API/api/auth/login" -H 'Content-Type: application/json' -d '{"name":"tf","password":"tf-given-password"}'
curl -sk -b "$TF_JAR" -c "$TF_JAR" -o /dev/null -X POST "$API/api/auth/password" -H 'Content-Type: application/json' -d '{"current_password":"tf-given-password","new_password":"tfs-own-password"}'
tf_nodes="$(curl -sk -b "$TF_JAR" "$API/api/nodes" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')"
tf_reload="$(curl -sk -b "$TF_JAR" -o /dev/null -w '%{http_code}' -X POST "${NODE_BASE}/api/haproxy/reload")"
tf_restart="$(curl -sk -b "$TF_JAR" -o "$WORKDIR/tf-restart.txt" -w '%{http_code}' -X POST "${NODE_BASE}/api/system/services/haproxy/restart")"
tf_users="$(curl -sk -b "$TF_JAR" -o /dev/null -w '%{http_code}' "$API/api/users")"
[ "$tf_nodes/$tf_reload/$tf_restart/$tf_users" = 1/200/403/403 ] || { echo "Dashboard test FAILED: the scoped account: nodes $tf_nodes, reload $tf_reload, restart $tf_restart ($(cat "$WORKDIR/tf-restart.txt")), accounts $tf_users (want 1/200/403/403)" >&2; exit 1; }
grep -q 'may not call SystemService/ServiceRestart' "$WORKDIR/tf-restart.txt" || { echo "Dashboard test FAILED: the scoped restart refused by someone else than the Controller: $(cat "$WORKDIR/tf-restart.txt")" >&2; exit 1; }
curl -sk -m 4 -b "$COOKIE_JAR" "${NODE_BASE}/api/stream/logs?id=janusd&tail=100" >"$WORKDIR/node-janusd-tf.log" || true
grep -q 'api: HAProxyService/Reload: tf (os:operator; haproxy) via janus-controller' "$WORKDIR/node-janusd-tf.log" || { echo "Dashboard test FAILED: the node didn't log the scoped reload with its domain: $(grep Reload "$WORKDIR/node-janusd-tf.log")" >&2; exit 1; }
echo "Scopes OK: an account with a grant on team=web over HAProxy reloads it - the node checked and logged its domain -, its restart refused before leaving, the Controller's routes closed"
curl -sk -m 4 -b "$COOKIE_JAR" "${NODE_BASE}/api/stream/logs?id=janusd&tail=300" >"$WORKDIR/node-janusd.log" || true
grep -q 'api: SystemService/ServiceRestart: admin (os:admin) via janus-controller' "$WORKDIR/node-janusd.log" || { echo "Dashboard test FAILED: the node didn't log the admin's restart via the Controller: $(cat "$WORKDIR/node-janusd.log")" >&2; exit 1; }
grep -q 'api: SystemService/ServiceRestart: olga (os:operator) via janus-controller' "$WORKDIR/node-janusd.log" || { echo "Dashboard test FAILED: the node didn't log the operator's restart as hers" >&2; exit 1; }
grep -q 'access: fleet root' "$WORKDIR/node-janusd.log" || { echo "Dashboard test FAILED: the node didn't log taking the fleet" >&2; exit 1; }
echo "Fleet relay OK: the node logs each account acting through the Controller - an operator restarted HAProxy and was refused an admin's call by the node itself, a reader read"

# janusctl signed in to the Controller with an API token: a certificate
# of the fleet for the account - an hour, for a key janusctl made - and
# the nodes; it reaches the node directly, which applies the role.
go build -o "$WORKDIR/janusctl" ./cmd/janusctl
export JANUSCONFIG="$WORKDIR/janusctl-config/config.json"
CONTROLLER_FP="$(openssl x509 -in "$WORKDIR/data/dashboard-identity.crt" -noout -fingerprint -sha256 | cut -d= -f2)"
cli_token() { # cli_token JAR ROLE
  curl -sk -b "$1" -X POST "$API/api/tokens" -H 'Content-Type: application/json' -d "{\"name\":\"janusctl-$2\",\"role\":\"$2\"}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])'
}
ADMIN_TOKEN="$(cli_token "$COOKIE_JAR" admin)"
READER_TOKEN="$(cli_token "$READER_JAR" reader)"
JANUS_TOKEN="$ADMIN_TOKEN" "$WORKDIR/janusctl" login -context admin -controller "127.0.0.1:${DASHBOARD_ADDR_PORT}" -controller-fingerprint "$CONTROLLER_FP" >"$WORKDIR/janusctl-login.txt" 2>&1 \
  || { echo "Dashboard test FAILED: janusctl login: $(cat "$WORKDIR/janusctl-login.txt")" >&2; exit 1; }
grep -q 'as admin (os:admin)' "$WORKDIR/janusctl-login.txt" || { echo "Dashboard test FAILED: janusctl login: $(cat "$WORKDIR/janusctl-login.txt")" >&2; exit 1; }
"$WORKDIR/janusctl" -context admin -n test-node system service restart haproxy >/dev/null \
  || { echo "Dashboard test FAILED: janusctl, signed in as admin, restarting HAProxy on the node" >&2; exit 1; }
JANUS_TOKEN="$READER_TOKEN" "$WORKDIR/janusctl" login -context reader -controller "127.0.0.1:${DASHBOARD_ADDR_PORT}" -controller-fingerprint "$CONTROLLER_FP" >/dev/null 2>&1 \
  || { echo "Dashboard test FAILED: janusctl login as a reader" >&2; exit 1; }
"$WORKDIR/janusctl" -context reader -n test-node version >/dev/null || { echo "Dashboard test FAILED: janusctl as a reader can't read the node" >&2; exit 1; }
if "$WORKDIR/janusctl" -context reader -n test-node system service restart haproxy >"$WORKDIR/janusctl-reader.txt" 2>&1; then
  echo "Dashboard test FAILED: janusctl as a reader restarted HAProxy" >&2; exit 1
fi
grep -q 'requires role .*rita has \[os:reader\]' "$WORKDIR/janusctl-reader.txt" || { echo "Dashboard test FAILED: the reader refused by someone else than the node: $(cat "$WORKDIR/janusctl-reader.txt")" >&2; exit 1; }
wrong_pin="$(JANUS_TOKEN="$ADMIN_TOKEN" "$WORKDIR/janusctl" login -context x -controller "127.0.0.1:${DASHBOARD_ADDR_PORT}" -controller-fingerprint "00:11" 2>&1 || true)"
grep -q 'not trusted' <<<"$wrong_pin" || { echo "Dashboard test FAILED: janusctl trusted a Controller with another fingerprint: $wrong_pin" >&2; exit 1; }
curl -sk -m 4 -b "$COOKIE_JAR" "${NODE_BASE}/api/stream/logs?id=janusd&tail=300" >"$WORKDIR/node-janusd.log" || true
grep -qE 'api: SystemService/ServiceRestart: admin \(os:admin, fleet\)$' "$WORKDIR/node-janusd.log" || { echo "Dashboard test FAILED: the node didn't log janusctl's restart as the account's, directly: $(grep ServiceRestart "$WORKDIR/node-janusd.log")" >&2; exit 1; }
echo "janusctl OK: signed in with an API token (the Controller pinned), it reached the node directly with the fleet's certificate as admin; a reader read and was refused a restart by the node"

# janusctl signed in with an SSH key of the account - added from the
# admin's sign-in that gave its second factor -, from ssh-agent: the
# certificate is for the key itself, which signs the TLS handshakes with
# the node too.
ssh-keygen -q -t ed25519 -N '' -C dashboard-test -f "$WORKDIR/id_ed25519"
python3 -c 'import json,sys; print(json.dumps({"name": "test laptop", "public_key": open(sys.argv[1]).read()}))' "$WORKDIR/id_ed25519.pub" >"$WORKDIR/ssh-key.json"
ssh_add_code="$(curl -sk -b "$COOKIE_JAR" -o "$WORKDIR/ssh-key-out.json" -w '%{http_code}' -X POST "$API/api/auth/ssh-keys" -H 'Content-Type: application/json' --data-binary @"$WORKDIR/ssh-key.json")"
[ "$ssh_add_code" = 201 ] || { echo "Dashboard test FAILED: adding an SSH key: $ssh_add_code $(cat "$WORKDIR/ssh-key-out.json")" >&2; exit 1; }
eval "$(ssh-agent -s)" >/dev/null
trap 'ssh-agent -k >/dev/null 2>&1 || true; cleanup' EXIT
ssh-add -q "$WORKDIR/id_ed25519"
"$WORKDIR/janusctl" login -context ssh -controller "127.0.0.1:${DASHBOARD_ADDR_PORT}" -controller-fingerprint "$CONTROLLER_FP" -user admin -ssh-key "$WORKDIR/id_ed25519.pub" >"$WORKDIR/janusctl-ssh.txt" 2>&1 \
  || { echo "Dashboard test FAILED: janusctl login with an SSH key from ssh-agent: $(cat "$WORKDIR/janusctl-ssh.txt")" >&2; exit 1; }
grep -q 'as admin (os:admin)' "$WORKDIR/janusctl-ssh.txt" || { echo "Dashboard test FAILED: janusctl SSH login: $(cat "$WORKDIR/janusctl-ssh.txt")" >&2; exit 1; }
[ ! -e "$WORKDIR/janusctl-config/ssh/key.pem" ] || { echo "Dashboard test FAILED: an SSH context keeps a private key of its own" >&2; exit 1; }
"$WORKDIR/janusctl" -context ssh -n test-node system service restart haproxy >/dev/null \
  || { echo "Dashboard test FAILED: janusctl with the agent's key restarting HAProxy on the node" >&2; exit 1; }
ssh-add -q -D
if "$WORKDIR/janusctl" -context ssh -n test-node version >/dev/null 2>&1; then
  echo "Dashboard test FAILED: janusctl reached the node with the key gone from ssh-agent" >&2; exit 1
fi
echo "janusctl SSH OK: signed in with the account's Ed25519 key from ssh-agent, the agent signed the TLS handshake with the node, which took the certificate; nothing reached it once the key left the agent"

# janusctl login -device: its code approved on the Controller's page -
# here through the page's API, with the admin's session -, as a reader.
"$WORKDIR/janusctl" login -context device -controller "127.0.0.1:${DASHBOARD_ADDR_PORT}" -controller-fingerprint "$CONTROLLER_FP" -device >"$WORKDIR/janusctl-device.txt" 2>&1 &
DEVICE_PID=$!
deadline=$((SECONDS + 20))
until DEVICE_CODE="$(grep -oE '\b[0-9A-Z]{4}-[0-9A-Z]{4}\b' "$WORKDIR/janusctl-device.txt" | head -1)" && [ -n "$DEVICE_CODE" ]; do
  [ "$SECONDS" -lt "$deadline" ] || { echo "Dashboard test FAILED: janusctl login -device showed no code: $(cat "$WORKDIR/janusctl-device.txt")" >&2; exit 1; }
  sleep 0.5
done
approve_code="$(curl -sk -b "$COOKIE_JAR" -o /dev/null -w '%{http_code}' -X POST "$API/api/cli/device/$DEVICE_CODE/approve" -H 'Content-Type: application/json' -d '{"role":"reader"}')"
[ "$approve_code" = 204 ] || { echo "Dashboard test FAILED: approving janusctl's device code: $approve_code" >&2; exit 1; }
wait "$DEVICE_PID" || { echo "Dashboard test FAILED: janusctl login -device: $(cat "$WORKDIR/janusctl-device.txt")" >&2; exit 1; }
grep -q 'as admin (os:reader)' "$WORKDIR/janusctl-device.txt" || { echo "Dashboard test FAILED: janusctl login -device: $(cat "$WORKDIR/janusctl-device.txt")" >&2; exit 1; }
"$WORKDIR/janusctl" -context device -n test-node version >/dev/null || { echo "Dashboard test FAILED: janusctl signed in with -device can't read the node" >&2; exit 1; }
echo "janusctl device OK: its code approved for the account as a reader, janusctl got its certificate and read the node"

# janusctl for an account without a role over everything: a scoped
# certificate - operator over HAProxy on the nodes labelled team=web,
# named by their CA's key. The node itself holds it to that (janusctl
# reaches it directly, the Controller isn't on the way): HAProxy yes, a
# service restart no; and a certificate naming another node only opens
# nothing here.
TF_TOKEN="$(curl -sk -b "$TF_JAR" -X POST "$API/api/tokens" -H 'Content-Type: application/json' -d '{"name":"janusctl-tf","expires_in_days":1}' | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')"
[ -n "$TF_TOKEN" ] || { echo "Dashboard test FAILED: no token for the scoped account" >&2; exit 1; }
tf_login() {
  JANUS_TOKEN="$TF_TOKEN" "$WORKDIR/janusctl" login -context "$1" -controller "127.0.0.1:${DASHBOARD_ADDR_PORT}" -controller-fingerprint "$CONTROLLER_FP" >"$WORKDIR/janusctl-$1.txt" 2>&1 \
    || { echo "Dashboard test FAILED: janusctl login as the scoped account: $(cat "$WORKDIR/janusctl-$1.txt")" >&2; exit 1; }
}
tf_login tf
grep -q 'as tf (scoped: os:operator (haproxy) on 1 node)' "$WORKDIR/janusctl-tf.txt" || { echo "Dashboard test FAILED: the scoped account's certificate: $(cat "$WORKDIR/janusctl-tf.txt")" >&2; exit 1; }
"$WORKDIR/janusctl" -context tf -n test-node haproxy get-config >"$WORKDIR/tf-haproxy.cfg" || { echo "Dashboard test FAILED: the scoped certificate reading haproxy.cfg" >&2; exit 1; }
"$WORKDIR/janusctl" -context tf -n test-node haproxy apply-config "$WORKDIR/tf-haproxy.cfg" >"$WORKDIR/tf-apply.txt" 2>&1 \
  || { echo "Dashboard test FAILED: the scoped certificate applying haproxy.cfg: $(cat "$WORKDIR/tf-apply.txt")" >&2; exit 1; }
if "$WORKDIR/janusctl" -context tf -n test-node system service restart haproxy >"$WORKDIR/tf-cli-restart.txt" 2>&1; then
  echo "Dashboard test FAILED: the scoped certificate restarted a service" >&2; exit 1
fi
grep -q 'is in the services domain: tf may only haproxy on this node' "$WORKDIR/tf-cli-restart.txt" || { echo "Dashboard test FAILED: the scoped restart refused by someone else than the node: $(cat "$WORKDIR/tf-cli-restart.txt")" >&2; exit 1; }
curl -sk -m 4 -b "$COOKIE_JAR" "${NODE_BASE}/api/stream/logs?id=janusd&tail=200" >"$WORKDIR/node-janusd-tf-cli.log" || true
grep -qE 'api: HAProxyService/ApplyConfig: tf \(os:operator, fleet, scoped; haproxy\)$' "$WORKDIR/node-janusd-tf-cli.log" || { echo "Dashboard test FAILED: the node didn't log the scoped apply as its certificate's: $(grep ApplyConfig "$WORKDIR/node-janusd-tf-cli.log")" >&2; exit 1; }
# Another node labelled team=web (a synthetic one), this one team=db: a
# new certificate names the other only.
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -keyout "$WORKDIR/elsewhere-service.key" -out "$WORKDIR/elsewhere-service.csr" -nodes -subj "/CN=service-elsewhere" >/dev/null 2>&1
openssl x509 -req -in "$WORKDIR/elsewhere-service.csr" -CA "$WORKDIR/reg-node-ca.crt" -CAkey "$WORKDIR/reg-node-ca.key" -CAcreateserial -out "$WORKDIR/elsewhere-service.crt" -days 1 >/dev/null 2>&1
python3 -c '
import json, sys
ca, crt, key, out = sys.argv[1:5]
open(out, "w").write(json.dumps({"name": "scope-elsewhere", "address": "10.0.0.10:9505", "ca_cert_pem": open(ca).read(), "service_cert_pem": open(crt).read(), "service_key_pem": open(key).read()}))
' "$WORKDIR/reg-node-ca.crt" "$WORKDIR/elsewhere-service.crt" "$WORKDIR/elsewhere-service.key" "$WORKDIR/register-elsewhere.json"
ELSEWHERE_PENDING="$(curl -sk -X POST "https://127.0.0.1:${DASHBOARD_REGISTER_PORT}/register" -H "Content-Type: application/json" -d @"$WORKDIR/register-elsewhere.json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
ELSEWHERE_ID="$(curl -sk -b "$COOKIE_JAR" -X POST "$API/api/pending/${ELSEWHERE_PENDING}/approve" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
[ -n "$ELSEWHERE_ID" ] || { echo "Dashboard test FAILED: the synthetic node for the scope" >&2; exit 1; }
relabel() {
  [ "$(curl -sk -b "$COOKIE_JAR" -o /dev/null -w '%{http_code}' -X PATCH "$API/api/nodes/$1" -H 'Content-Type: application/json' -d "{\"labels\":{\"team\":\"$2\"}}")" = 200 ] \
    || { echo "Dashboard test FAILED: labelling node $1 team=$2" >&2; exit 1; }
}
relabel "$ELSEWHERE_ID" web
relabel "$SCOPED_NODE" db
tf_login tf-elsewhere
grep -q 'as tf (scoped: os:operator (haproxy) on 1 node)' "$WORKDIR/janusctl-tf-elsewhere.txt" || { echo "Dashboard test FAILED: the certificate naming the other node: $(cat "$WORKDIR/janusctl-tf-elsewhere.txt")" >&2; exit 1; }
TF_ELSEWHERE_DIR="$WORKDIR/janusctl-config/tf-elsewhere"
if "$WORKDIR/janusctl" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/ca.crt" -cert "$TF_ELSEWHERE_DIR/cert.pem" -key "$TF_ELSEWHERE_DIR/key.pem" version >"$WORKDIR/tf-elsewhere.txt" 2>&1; then
  echo "Dashboard test FAILED: a certificate naming another node opened this one" >&2; exit 1
fi
grep -q "tf's certificate doesn't open this node: its scope names other nodes" "$WORKDIR/tf-elsewhere.txt" || { echo "Dashboard test FAILED: the other node's certificate refused by someone else than the node: $(cat "$WORKDIR/tf-elsewhere.txt")" >&2; exit 1; }
relabel "$SCOPED_NODE" web
curl -sk -b "$COOKIE_JAR" -o /dev/null -X DELETE "$API/api/nodes/$ELSEWHERE_ID"
echo "janusctl scoped OK: an account without a role over everything got a certificate naming its nodes by their CA's key; the node applied haproxy.cfg with it, refused it a service restart itself, and refused one naming another node"

kill "$DASHBOARD_PID"
wait "$DASHBOARD_PID" 2>/dev/null || true
"$DASHBOARDD" -addr ":${DASHBOARD_ADDR_PORT}" -register-addr ":${DASHBOARD_REGISTER_PORT}" -data-dir "$WORKDIR/data" > "$WORKDIR/dashboardd-restart2.log" 2>&1 &
DASHBOARD_PID=$!
deadline=$((SECONDS + 20))
until signin 2>/dev/null; do
  [ "$SECONDS" -lt "$deadline" ] || { echo "Dashboard test FAILED: signing in after the restart" >&2; exit 1; }
  sleep 1
done
until [ "$(fleet_relay)" = "200" ]; do
  [ "$SECONDS" -lt "$deadline" ] || { echo "Dashboard test FAILED: the fleet relay didn't come back after a restart" >&2; cat "$WORKDIR/dashboardd-restart2.log" >&2; exit 1; }
  sleep 1
done
echo "Fleet restart OK: the issuing CA unsealed with the master key, the node reached again with a fresh fleet certificate"

# --- the node replaces its own CA (janusctl access rotate-ca): the
# Controller follows it - the new CA comes cross-signed by the one it
# pinned - and the new admin certificate opens the node's page ---
"$WORKDIR/janusctl" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" access rotate-ca "$WORKDIR/rotated" >/dev/null \
  || { echo "Dashboard test FAILED: janusctl access rotate-ca" >&2; exit 1; }
cp "$WORKDIR/data/nodes/$FLEET_NODE_ID/ca.crt" "$WORKDIR/pinned-before.crt"
kill "$DASHBOARD_PID"
wait "$DASHBOARD_PID" 2>/dev/null || true
"$DASHBOARDD" -addr ":${DASHBOARD_ADDR_PORT}" -register-addr ":${DASHBOARD_REGISTER_PORT}" -data-dir "$WORKDIR/data" > "$WORKDIR/dashboardd-restart3.log" 2>&1 &
DASHBOARD_PID=$!
rotated_relay() {
  curl -sk -b "$COOKIE_JAR" -o /dev/null -w '%{http_code}' "${NODE_BASE}/api/info"
}
deadline=$((SECONDS + 30))
until signin 2>/dev/null; do
  [ "$SECONDS" -lt "$deadline" ] || { echo "Dashboard test FAILED: signing in after the restart" >&2; exit 1; }
  sleep 1
done
until [ "$(rotated_relay)" = "200" ]; do
  [ "$SECONDS" -lt "$deadline" ] || { echo "Dashboard test FAILED: the node isn't reached after it replaced its CA" >&2; cat "$WORKDIR/dashboardd-restart3.log" >&2; exit 1; }
  sleep 1
done
if cmp -s "$WORKDIR/pinned-before.crt" "$WORKDIR/data/nodes/$FLEET_NODE_ID/ca.crt"; then
  echo "Dashboard test FAILED: the Controller still pins the node's old CA" >&2; exit 1
fi
grep -q 'replaced its CA - the Controller now pins the new one' "$WORKDIR/dashboardd-restart3.log" || { echo "Dashboard test FAILED: the Controller didn't say it follows the node's new CA" >&2; exit 1; }
"$WORKDIR/janusctl" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/rotated/ca.crt" -cert "$WORKDIR/rotated/admin.crt" -key "$WORKDIR/rotated/admin.key" version >/dev/null \
  || { echo "Dashboard test FAILED: the new admin certificate doesn't reach the node" >&2; exit 1; }
echo "CA rotation OK: the Controller follows the node's new CA, cross-signed by the old one, and the new admin certificate reaches the node"
# A scoped certificate names the node by its CA's key: the key of the
# cross-signed certificate the Controller pins now - a new sign-in opens
# the node again.
tf_login tf
"$WORKDIR/janusctl" -context tf -n test-node haproxy get-config >/dev/null 2>"$WORKDIR/tf-rotated.txt" \
  || { echo "Dashboard test FAILED: a scoped certificate doesn't open the node after it replaced its CA: $(cat "$WORKDIR/tf-rotated.txt")" >&2; exit 1; }
echo "Scoped after the CA rotation OK: a new certificate names the node by its new CA's key, and opens it"

# --- the same from the node's page (Access, nodeproxy/access.go): the
# admin's key made by the browser - openssl here -, only its public half
# sent; the page offers it because the account's role may call it ---
curl -sk -b "$COOKIE_JAR" "${NODE_BASE}/api/me" -o "$WORKDIR/me.json"
python3 -c 'import json,sys; m=json.load(open(sys.argv[1]))["may"]; assert "AccessService/LocalCARotate" in m and "LifecycleService/Upgrade" in m, m' "$WORKDIR/me.json" \
  || { echo "Dashboard test FAILED: the admin's node page isn't offered what an admin may do: $(cat "$WORKDIR/me.json")" >&2; exit 1; }
mkdir -p "$WORKDIR/relayed"
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$WORKDIR/relayed/admin.key" 2>/dev/null
openssl pkey -in "$WORKDIR/relayed/admin.key" -pubout -out "$WORKDIR/relayed/admin.pub"
python3 -c 'import json,sys; print(json.dumps({"admin_public_key": open(sys.argv[1]).read()}))' "$WORKDIR/relayed/admin.pub" >"$WORKDIR/rotate.json"
rotate_code="$(curl -sk -b "$COOKIE_JAR" -o "$WORKDIR/rotate-out.json" -w '%{http_code}' -X POST "${NODE_BASE}/api/access/rotate-ca" -H 'Content-Type: application/json' --data-binary @"$WORKDIR/rotate.json")"
[ "$rotate_code" = 200 ] || { echo "Dashboard test FAILED: replacing the CA from the node's page: $rotate_code $(cat "$WORKDIR/rotate-out.json")" >&2; exit 1; }
python3 - "$WORKDIR/rotate-out.json" "$WORKDIR/relayed" <<'PYEOF'
import json, sys
out = json.load(open(sys.argv[1]))
open(sys.argv[2] + "/ca.crt", "w").write(out["ca_cert"])
open(sys.argv[2] + "/admin.crt", "w").write(out["admin_cert"])
PYEOF
"$WORKDIR/janusctl" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/relayed/ca.crt" -cert "$WORKDIR/relayed/admin.crt" -key "$WORKDIR/relayed/admin.key" version >/dev/null \
  || { echo "Dashboard test FAILED: the admin certificate from the node's page doesn't reach the node" >&2; exit 1; }
if "$WORKDIR/janusctl" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/relayed/ca.crt" -cert "$WORKDIR/rotated/admin.crt" -key "$WORKDIR/rotated/admin.key" version >/dev/null 2>&1; then
  echo "Dashboard test FAILED: the previous CA's admin certificate still reaches the node" >&2; exit 1
fi
[ "$(rotated_relay)" = 200 ] || { echo "Dashboard test FAILED: the Controller lost the node after replacing its CA from the page" >&2; exit 1; }
curl -sk -m 4 -b "$COOKIE_JAR" "${NODE_BASE}/api/stream/logs?id=janusd&tail=50" >"$WORKDIR/node-janusd-rotate.log" || true
grep -q "access: the node's own CA replaced .* by admin (os:admin) via janus-controller" "$WORKDIR/node-janusd-rotate.log" \
  || { echo "Dashboard test FAILED: the node didn't log who replaced its CA: $(cat "$WORKDIR/node-janusd-rotate.log")" >&2; exit 1; }
echo "CA rotation from the node's page OK: the browser's key got the new admin certificate, the previous one stopped working, the Controller kept the node"

# --- backups (dashboard/backend/backups.go): the Controller backs itself
# up to a real S3 bucket - versitygw, which checks S3's signatures -,
# encrypted to its backup kit and signed; a new Controller restores it
# from its first page, and reaches the node through the restored fleet ---
mkdir -p "$WORKDIR/s3/janus-backups"
"$VERSITYGW" --access janus-test --secret janus-test-secret-1234 --port "127.0.0.1:$S3_PORT" posix "$WORKDIR/s3" >"$WORKDIR/versitygw.log" 2>&1 &
VGW_PID=$!
S3_URL="http://127.0.0.1:$S3_PORT"
JANUS_TEST_S3="http://janus-test:janus-test-secret-1234@127.0.0.1:$S3_PORT/janus-backups" go test ./dashboard/backend/internal/s3 -run TestLive -count=1 >/dev/null \
  || { echo "Dashboard test FAILED: the S3 client against versitygw" >&2; exit 1; }
BACKUP_PASS="$(curl -sk -b "$COOKIE_JAR" -X POST "$API/api/backups/kit" | python3 -c 'import json,sys; print(json.load(sys.stdin)["passphrase"])')"
curl -sk -b "$COOKIE_JAR" "$API/api/backups/kit" -o "$WORKDIR/backup-kit.age"
python3 -c 'import json,sys; print(json.dumps({"kit": open(sys.argv[1]).read(), "passphrase": sys.argv[2]}))' "$WORKDIR/backup-kit.age" "$BACKUP_PASS" >"$WORKDIR/backup-confirm.json"
kit_code="$(curl -sk -b "$COOKIE_JAR" -o /dev/null -w '%{http_code}' -X POST "$API/api/backups/kit/confirm" -H 'Content-Type: application/json' --data-binary @"$WORKDIR/backup-confirm.json")"
[ "$kit_code" = 200 ] || { echo "Dashboard test FAILED: confirming the backup kit: $kit_code" >&2; exit 1; }
S3_SETTINGS="{\"enabled\":true,\"endpoint\":\"$S3_URL\",\"region\":\"us-east-1\",\"bucket\":\"janus-backups\",\"prefix\":\"janus/\",\"access_key\":\"janus-test\",\"path_style\":true,\"interval_hours\":24,\"keep\":30,\"recipients\":[]}"
set_code="$(curl -sk -b "$COOKIE_JAR" -o "$WORKDIR/backup-set.json" -w '%{http_code}' -X PUT "$API/api/backups/settings" -H 'Content-Type: application/json' -d "${S3_SETTINGS%\}},\"secret_key\":\"janus-test-secret-1234\"}")"
[ "$set_code" = 200 ] || { echo "Dashboard test FAILED: the backup settings: $set_code $(cat "$WORKDIR/backup-set.json")" >&2; exit 1; }
curl -sk -b "$COOKIE_JAR" -X POST "$API/api/backups/run" -o "$WORKDIR/backup-run.json"
BACKUP_KEY="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["key"])' "$WORKDIR/backup-run.json" 2>/dev/null)" \
  || { echo "Dashboard test FAILED: backing up: $(cat "$WORKDIR/backup-run.json")" >&2; exit 1; }
[ -s "$WORKDIR/s3/janus-backups/$BACKUP_KEY" ] || { echo "Dashboard test FAILED: no $BACKUP_KEY in the bucket" >&2; exit 1; }
sed -n 3p "$WORKDIR/s3/janus-backups/$BACKUP_KEY" | grep -q '^age-encryption.org/v1' || { echo "Dashboard test FAILED: the backup isn't age-encrypted after its manifest" >&2; exit 1; }
python3 -c 'import json,sys; m=json.load(open(sys.argv[1]))["manifest"]; assert m["nodes"] and all(v == "" for v in m["nodes"].values()), m' "$WORKDIR/backup-run.json" \
  || { echo "Dashboard test FAILED: the backup didn't take every node's configuration: $(cat "$WORKDIR/backup-run.json")" >&2; exit 1; }
echo "Backup OK: the Controller backed itself and the node's configuration up to versitygw, encrypted to its backup kit, signed"

kill "$DASHBOARD_PID"
wait "$DASHBOARD_PID" 2>/dev/null || true
"$DASHBOARDD" -addr ":${DASHBOARD_ADDR_PORT}" -register-addr ":${DASHBOARD_REGISTER_PORT}" -data-dir "$WORKDIR/data2" > "$WORKDIR/dashboardd-new.log" 2>&1 &
DASHBOARD_PID=$!
deadline=$((SECONDS + 20))
until curl -sk "$API/api/auth/status" | grep -q '"setup_required":true'; do
  [ "$SECONDS" -lt "$deadline" ] || { echo "Dashboard test FAILED: the new Controller didn't come up" >&2; exit 1; }
  sleep 0.5
done
restore_code="$(curl -sk -o "$WORKDIR/restore.json" -w '%{http_code}' -X POST "$API/api/restore" -F "kit=@$WORKDIR/backup-kit.age" -F "passphrase=$BACKUP_PASS" \
  -F "s3=${S3_SETTINGS%\}},\"secret_key\":\"janus-test-secret-1234\"}" -F "key=$BACKUP_KEY")"
[ "$restore_code" = 200 ] || { echo "Dashboard test FAILED: restoring on the new Controller: $restore_code $(cat "$WORKDIR/restore.json")" >&2; exit 1; }
deadline=$((SECONDS + 30))
until curl -sk "$API/api/auth/status" 2>/dev/null | grep -q '"setup_required":false'; do
  [ "$SECONDS" -lt "$deadline" ] || { echo "Dashboard test FAILED: the restored Controller didn't start again: $(tail -5 "$WORKDIR/dashboardd-new.log")" >&2; exit 1; }
  sleep 0.5
done
signin || { echo "Dashboard test FAILED: signing in to the restored Controller" >&2; exit 1; }
restored_relay="$(curl -sk -b "$COOKIE_JAR" -o /dev/null -w '%{http_code}' "${NODE_BASE}/api/info")"
[ "$restored_relay" = 200 ] || { echo "Dashboard test FAILED: the restored Controller doesn't reach the node: $restored_relay $(tail -5 "$WORKDIR/dashboardd-new.log")" >&2; exit 1; }
echo "Restore OK: a new Controller restored the backup from the bucket with the kit, started again as the one backed up - its accounts, its fleet - and reached the node"

echo "Dashboard test OK: add/list/relay/accounts on node pages/delete/restart-persistence/fleet/backup and restore all verified against a real running node"
