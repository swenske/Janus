#!/usr/bin/env bash
# The custom orchestrator example (examples/orchestrator, docs/
# private-cloud/own-orchestrator.md) run as the docs show it, against
# real nodes booted under OVMF - no Controller, no janusctl: openssl, jq,
# grpcurl (hack/grpcurl, pinned) and a Python registration endpoint.
#
#   1. a fleet (fleet.sh), the orchestrator's certificates
#      (client-cert.sh: os:admin, and janus:controller acting for users),
#      the registration endpoint's (registration-cert.sh);
#   2. three nodes boot at once from the same image, each with its
#      NoCloud volume (user-data.sh, cidata.sh):
#      - edge-1 trusts the fleet from its first boot: the orchestrator
#        pins its CA from the serial console (first-contact.sh) - and a
#        node answering with another CA is refused;
#      - edge-2 announces itself with a token: admitted at once;
#      - edge-3 announces itself with no token: it waits, its CA is
#        checked against its console, it's approved, it takes the
#        fleet's trust at its next poll;
#   3. edge-1 is driven: HAProxy configurations applied and refused
#      (haproxy-apply.sh), a user's call refused by the node, a network
#      change on trial then confirmed (network-apply.sh);
#   4. edge-1 updates (upgrade.sh) from a bundle served over HTTP,
#      reboots into its other slot, confirms it healthy - and is still
#      reached with the CA pinned at first.
#
#   hack/qemu-orchestrator-test.sh <disk.img> <bzImage> <build-dir>
set -euo pipefail
export PATH="$PATH:/usr/sbin:/sbin"

DISK=${1:?usage: $0 <disk.img> <bzImage> <build-dir>}
KERNEL=${2:?usage: $0 <disk.img> <bzImage> <build-dir>}
BUILD_DIR=${3:?usage: $0 <disk.img> <bzImage> <build-dir>}
BOOT_TIMEOUT=${QEMU_ORCHESTRATOR_BOOT_TIMEOUT:-180}
OFFSET=${JANUS_TEST_PORT_OFFSET:-0}
REGISTRATION_PORT=$((18900 + OFFSET))
BUNDLE_PORT=$((18901 + OFFSET))
# edge-N: gRPC on 18900+2N, HTTP :80 on 18901+2N.
grpc_port() { echo $((18900 + 2 * $1 + OFFSET)); }
http_port() { echo $((18901 + 2 * $1 + OFFSET)); }

OVMF_CODE=${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}
OVMF_VARS_TEMPLATE=${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf)" >&2; exit 1; }

REPO=$(cd "$(dirname "$0")/.." && pwd)
EX=$REPO/examples/orchestrator
W=$(mktemp -d)
PIDS=()
cleanup() {
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
  # QEMU_ORCHESTRATOR_KEEP=1 keeps the consoles and the state to look at.
  if [ "${QEMU_ORCHESTRATOR_KEEP:-}" = 1 ]; then echo "kept $W" >&2; else rm -rf "$W"; fi
}
trap cleanup EXIT

fail() {
  echo "orchestrator test FAILED: $*" >&2
  for log in "$W"/edge-*.log "$W/registration.log"; do
    [ -f "$log" ] && { echo "--- $log ---" >&2; tail -60 "$log" >&2; }
  done
  exit 1
}

# wait_for SECONDS WHAT COMMAND...: polls COMMAND every second.
wait_for() {
  local timeout=$1 what=$2
  shift 2
  local deadline=$((SECONDS + timeout))
  until "$@"; do
    [ "$SECONDS" -lt "$deadline" ] || fail "$what - not within ${timeout}s"
    sleep 1
  done
}

# grpcurl from its pinned image, as this user (the keys are 0600).
docker build -q -t janus-grpcurl "$REPO/hack/grpcurl" >/dev/null
GRPCURL="docker run --rm -i --user $(id -u):$(id -g) --network host -v $W:$W:ro -v $REPO/api/proto:$REPO/api/proto:ro janus-grpcurl"
export GRPCURL
export JANUS_PROTO=$REPO/api/proto

# --- 1. the fleet and the orchestrator's certificates ---
"$EX/fleet.sh" "$W/fleet" >/dev/null 2>&1 || fail "fleet.sh"
"$EX/client-cert.sh" "$W/fleet" orchestrator os:admin "$W/orchestrator" >/dev/null 2>&1 || fail "client-cert.sh os:admin"
"$EX/client-cert.sh" "$W/fleet" portal janus:controller "$W/portal" >/dev/null 2>&1 || fail "client-cert.sh janus:controller"
"$EX/registration-cert.sh" "$W/fleet" "$W/registration" >/dev/null 2>&1 || fail "registration-cert.sh"
export JANUS_CERT=$W/orchestrator.crt JANUS_KEY=$W/orchestrator.key
echo "Part 1 OK: a fleet, its bundle and three certificates made with openssl and jq"

# --- 2. three nodes boot ---
mkdir -p "$W/registration-state"
TOKEN=$(openssl rand -hex 16)
echo "$TOKEN" > "$W/registration-state/tokens"
python3 "$EX/registration-server.py" --listen "0.0.0.0:$REGISTRATION_PORT" --fleet "$W/fleet" \
  --cert "$W/registration.crt" --key "$W/registration.key" --state "$W/registration-state" \
  > "$W/registration.log" 2>&1 &
PIDS+=($!)

# The guest reaches the host at 10.0.2.2 (QEMU's user networking).
"$EX/user-data.sh" fleet "$W/fleet" edge-1 | "$EX/cidata.sh" "$W/edge-1.iso"
"$EX/user-data.sh" announce "$W/fleet" edge-2 "10.0.2.2:$REGISTRATION_PORT" "$TOKEN" | "$EX/cidata.sh" "$W/edge-2.iso"
"$EX/user-data.sh" announce "$W/fleet" edge-3 "10.0.2.2:$REGISTRATION_PORT" "not-a-token" | "$EX/cidata.sh" "$W/edge-3.iso"

boot() {
  local n=$1
  cp --sparse=always "$DISK" "$W/edge-$n.img"
  cp "$OVMF_VARS_TEMPLATE" "$W/edge-$n.vars"
  qemu-system-x86_64 -accel kvm -accel tcg -m 512M -display none -monitor none \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$W/edge-$n.vars" \
    -drive file="$W/edge-$n.img",format=raw,if=virtio \
    -device virtio-scsi-pci,id=scsi0 \
    -drive if=none,id=cd,file="$W/edge-$n.iso",format=raw,media=cdrom,readonly=on -device scsi-cd,drive=cd,bus=scsi0.0 \
    -netdev "user,id=net0,hostfwd=tcp::$(grpc_port "$n")-:9505,hostfwd=tcp::$(http_port "$n")-:80" \
    -device virtio-net-pci,netdev=net0 \
    -serial file:"$W/edge-$n.log" </dev/null 2>"$W/edge-$n.qemu.err" &
  PIDS+=($!)
}
for n in 1 2 3; do boot "$n"; done
listening() {
  local n
  n=$(grep -ac "listening on .* (mTLS required)" "$W/edge-$1.log" 2>/dev/null || true)
  [ "${n:-0}" -ge "${2:-1}" ]
}
for n in 1 2 3; do wait_for "$BOOT_TIMEOUT" "edge-$n's janusd listening" listening "$n"; done
echo "Part 2 OK: three nodes booted, each with its NoCloud volume"

# edge-1: its CA from its console.
EDGE1=127.0.0.1:$(grpc_port 1)
console_ca() { grep -ao "pki: this node's CA: SHA-256 [0-9a-f]\{64\}" "$W/edge-$1.log" | tail -1 | awk '{print $NF}'; }
"$EX/first-contact.sh" "$EDGE1" "$W/edge-1.log" "$W/edge-1-ca.pem" || fail "first-contact.sh on edge-1"
# A console showing another CA: the node isn't trusted.
sed "s/\(pki: this node's CA: SHA-256 \)[0-9a-f]*/\1$(printf '0%.0s' {1..64})/" "$W/edge-1.log" > "$W/forged.log"
if "$EX/first-contact.sh" "$EDGE1" "$W/forged.log" "$W/forged-ca.pem" 2>"$W/forged.err"; then
  fail "first-contact.sh trusted a node whose CA isn't the console's"
fi
grep -q "not the node" "$W/forged.err" || fail "first-contact.sh refused for another reason: $(cat "$W/forged.err")"
[ ! -e "$W/forged-ca.pem" ] || fail "first-contact.sh kept a CA it refused"
export JANUS_NODE_CA=$W/edge-1-ca.pem
[ "$("$EX/janus.sh" "$EDGE1" NetworkService/NetworkStatus | jq -r .hostname)" = edge-1 ] || fail "edge-1 doesn't have the hostname its user-data gave"
echo "Part 2a OK: edge-1's CA pinned from its console ($(console_ca 1)); a mismatching console refused"

# edge-2: admitted on its token. The inventory is the registration
# endpoint's state.
inventory() { jq -r --arg name "$1" "select(.name == \$name) | $2" "$W"/registration-state/nodes/*.json 2>/dev/null; }
announced() { [ -n "$(inventory "$1" .id)" ]; }
wait_for 120 "edge-2 announced" announced edge-2
[ "$(inventory edge-2 .admitted)" = true ] || fail "edge-2's token didn't admit it"
[ ! -s "$W/registration-state/tokens" ] || fail "edge-2's token can be used again"
inventory edge-2 .ca_cert_pem > "$W/edge-2-ca.pem"
responds() { JANUS_NODE_CA=$W/edge-$1-ca.pem "$EX/janus.sh" "127.0.0.1:$(grpc_port "$1")" SystemService/Version >/dev/null 2>&1; }
wait_for 60 "edge-2 answering the orchestrator's fleet certificate" responds 2
grep -aq "selfregister: the Controller at 10.0.2.2:$REGISTRATION_PORT checked through its fleet's root" "$W/edge-2.log" ||
  fail "edge-2 didn't check the registration endpoint through the fleet's root"
echo "Part 2b OK: edge-2 announced itself as $(inventory edge-2 .address), admitted on its token, answers the fleet"

# edge-3: waits for an approval - given once its CA is the console's.
wait_for 120 "edge-3 announced" announced edge-3
[ "$(inventory edge-3 .admitted)" = false ] || fail "edge-3 was admitted without a token"
inventory edge-3 .ca_cert_pem > "$W/edge-3-ca.pem"
if responds 3; then fail "edge-3 answers the fleet before it was approved"; fi
[ "$(openssl x509 -in "$W/edge-3-ca.pem" -outform DER | sha256sum | awk '{print $1}')" = "$(console_ca 3)" ] ||
  fail "edge-3 announced another CA than its console shows"
touch "$W/registration-state/approved/$(inventory edge-3 .id)"
wait_for 60 "edge-3 answering once approved" responds 3
echo "Part 2c OK: edge-3 waited, was approved, took the fleet's trust at its next poll"

# --- 3. driving edge-1 ---
"$EX/haproxy-apply.sh" "$EDGE1" "$REPO/examples/haproxy/web.cfg" >/dev/null || fail "haproxy-apply.sh with examples/haproxy/web.cfg"
[ "$(curl -s -m 5 "http://127.0.0.1:$(http_port 1)/healthz")" = ok ] || fail "edge-1 doesn't serve web.cfg's /healthz"
printf 'global\n    bogus-keyword\n' > "$W/bad.cfg"
if "$EX/haproxy-apply.sh" "$EDGE1" "$W/bad.cfg" >/dev/null 2>&1; then fail "edge-1 accepted a broken configuration"; fi
[ "$(curl -s -m 5 "http://127.0.0.1:$(http_port 1)/healthz")" = ok ] || fail "a refused configuration changed edge-1's HAProxy"
# A user's call, through a janus:controller certificate: the node checks
# the user's role.
as_reader() { JANUS_CERT=$W/portal.crt JANUS_KEY=$W/portal.key JANUS_AS_USER=alice JANUS_AS_ROLES=os:reader "$EX/janus.sh" "$EDGE1" "$@"; }
as_reader HAProxyService/ShowInfo >/dev/null || fail "alice (os:reader) can't read edge-1's HAProxy"
if as_reader HAProxyService/Reload >"$W/reload.err" 2>&1; then fail "alice (os:reader) reloaded HAProxy"; fi
grep -q "alice has \[os:reader\]" "$W/reload.err" || fail "the refusal doesn't name alice: $(cat "$W/reload.err")"
grep -aq "api: HAProxyService/ApplyConfig: orchestrator (os:admin, fleet)" "$W/edge-1.log" || fail "edge-1 didn't log who applied its configuration"
echo '{"hostname": "edge-1b"}' > "$W/network.json"
"$EX/network-apply.sh" "$EDGE1" "$W/network.json" >/dev/null || fail "network-apply.sh"
[ "$("$EX/janus.sh" "$EDGE1" NetworkService/NetworkStatus | jq -r '.hostname, (.trialPending // false)' | paste -sd' ')" = "edge-1b false" ] ||
  fail "edge-1's network change isn't confirmed"
echo "Part 3 OK: edge-1 driven - configurations applied and refused, a user's role checked by the node, a network change confirmed"

# --- 4. edge-1 updates ---
"$REPO/image/release/assemble.sh" "$W/bundle" "$KERNEL" "$BUILD_DIR/rootfs" >/dev/null 2>&1 || fail "assembling a release bundle"
(cd "$W/bundle" && exec python3 -m http.server "$BUNDLE_PORT" --bind 0.0.0.0) > "$W/bundle-http.log" 2>&1 &
PIDS+=($!)
JANUS_INSECURE_SKIP_SIGNATURE_CHECK=1 "$EX/upgrade.sh" "$EDGE1" "http://10.0.2.2:$BUNDLE_PORT" "$(cat "$W/bundle/rootfs.squashfs.sha256")" > "$W/upgrade.out" ||
  fail "upgrade.sh: $(cat "$W/upgrade.out")"
[ "$(tail -1 "$W/upgrade.out" | jq -r .stage)" = rebooting ] || fail "upgrade.sh didn't end on rebooting: $(cat "$W/upgrade.out")"
wait_for "$BOOT_TIMEOUT" "edge-1 back after its update" listening 1 2
confirmed() { grep -aq "bootcommit: confirmed healthy for slot B" "$W/edge-1.log"; }
wait_for 120 "edge-1's new slot confirmed healthy" confirmed
[ "$("$EX/janus.sh" "$EDGE1" SystemService/Version | jq -r .activeSlot)" = B ] || fail "edge-1 doesn't run slot B"
[ "$(curl -s -m 5 "http://127.0.0.1:$(http_port 1)/healthz")" = ok ] || fail "edge-1 lost its HAProxy configuration across the update"
echo "Part 4 OK: edge-1 updated to slot B, confirmed healthy, still reached with the CA pinned at first"

echo "orchestrator test OK: the example drove three real nodes - no Controller, no janusctl"
