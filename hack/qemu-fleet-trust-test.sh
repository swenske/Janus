#!/usr/bin/env bash
# Proves a node's fleet trust (internal/pki/fleet.go, AccessService) on
# a real node: the full disk image under real OVMF with SELinux enforcing,
# every call over mTLS with janusctl, a test fleet from hack/fleetca.
#
# Its own CA lets in from the first boot; a fleet's certificates only
# once the node pinned the fleet's root and applied a bundle the root
# signed - as long as their issuing CA is in the latest bundle (or the
# root signed them itself). An operator runs HAProxy, not the node's
# files; the Controller's certificate acts only for the user it names,
# and the node logs who did what. The trust survives a reboot; an older
# bundle is refused; only the node's own CA can make it forget its
# fleet. Fails on any AVC denial.
#
# Usage: hack/qemu-fleet-trust-test.sh <disk.img> <janusctl-bin>
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

DISK="${1:?usage: $0 <disk.img> <janusctl-bin>}"
CTL_BIN="${2:?usage: $0 <disk.img> <janusctl-bin>}"
BOOT_TIMEOUT_SECS="${QEMU_FLEET_BOOT_TIMEOUT:-60}"
HOST_PORT_8080="$((18210 + ${JANUS_TEST_PORT_OFFSET:-0}))"
HOST_GRPC_PORT="$((18211 + ${JANUS_TEST_PORT_OFFSET:-0}))"
MARKER="JANUS_INIT_BOOT_OK"
FIRST_BOOT_MSG="pki: first boot - generated a new CA"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf)" >&2; exit 1; }

WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

LOG="$WORKDIR/boot.log"
fail() {
  echo "Fleet trust test FAILED: $*" >&2
  [ -f "$LOG" ] && { echo "--- console output ---" >&2; cat "$LOG" >&2; }
  exit 1
}

go run ./hack/fleetca "$WORKDIR/fleet" || fail "hack/fleetca"
F="$WORKDIR/fleet"

cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/OVMF_VARS.fd"
qemu-system-x86_64 -accel kvm -accel tcg \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$WORKDIR/OVMF_VARS.fd" \
  -drive file="$DISK",format=raw,if=virtio \
  -nographic -display none -m 512M \
  -netdev "user,id=net0,hostfwd=tcp::${HOST_PORT_8080}-:8080,hostfwd=tcp::${HOST_GRPC_PORT}-:9505" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG" \
  &
QEMU_PID=$!

http_code() { curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_PORT_8080}/" || true; }
markers() { grep -ac "$MARKER" "$LOG" 2>/dev/null || true; }
first_boots() { grep -ac "$FIRST_BOOT_MSG" "$LOG" 2>/dev/null || true; }
wait_boot() { # wait_boot N: the Nth boot answers HTTP
  local deadline=$((SECONDS + BOOT_TIMEOUT_SECS))
  until [ "$(markers)" -ge "$1" ] && [ "$(http_code)" = "200" ]; do
    [ "$SECONDS" -lt "$deadline" ] || fail "boot #$1 never answered HTTP 200 within ${BOOT_TIMEOUT_SECS}s"
    sleep 1
  done
}
extract_pki() {
  local start size
  start="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
  size="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
  dd if="$DISK" of="$WORKDIR/state.img" bs=512 skip="$start" count="$size" status=none
  for f in ca.crt admin.crt admin.key; do
    rm -f "$WORKDIR/$f"
    debugfs -R "dump pki/$f $WORKDIR/$f" "$WORKDIR/state.img" >/dev/null 2>&1
    [ -s "$WORKDIR/$f" ] || return 1
  done
}
# as CERT [janusctl args]: a call with one of the test fleet's
# certificates ("local" = the node's own first-boot admin certificate).
as() {
  local who="$1"
  shift
  if [ "$who" = local ]; then
    "$CTL_BIN" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" "$@"
  else
    "$CTL_BIN" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/ca.crt" -cert "$F/$who.crt" -key "$F/$who.key" "$@"
  fi
}
lets_in() { # lets_in WHAT CERT [args]: the call succeeds
  local what="$1" out
  shift
  out="$(as "$@" 2>&1)" || fail "$what: refused: $out"
}
refuses() { # refuses WHAT PATTERN CERT [args]: the call fails, its error matching PATTERN
  local what="$1" pattern="$2" out
  shift 2
  if out="$(as "$@" 2>&1)"; then fail "$what: let in: $out"; fi
  grep -Eq "$pattern" <<<"$out" || fail "$what: refused, but not with /$pattern/: $out"
}

wait_boot 1
deadline=$((SECONDS + 60))
until [ "$(first_boots)" -ge 1 ] && extract_pki && as local version >/dev/null 2>&1; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the first boot's PKI never answered over mTLS"
  sleep 1
done
echo "Boot 1 OK"

# --- no fleet: only the node's own CA ---
grep -q '^Fleet: *none' <<<"$(as local access trust)" || fail "a fresh node trusts a fleet: $(as local access trust)"
refuses "a fleet certificate before the node trusts the fleet" 'certificate|handshake|EOF|Unavailable' admin system hostname
refuses "a root-signed certificate before the node trusts the fleet" 'certificate|handshake|EOF|Unavailable' admin-root system hostname
echo "  ok: a fresh node lets in only its own CA's certificates"

# --- pin the root, bundle 1 (issuing CA) ---
refuses "a bundle without the root" 'trusts no fleet yet' local access trust-set "$F/bundle-1.json"
out="$(as local access trust-set -root "$F/root.crt" "$F/bundle-1.json")" || fail "trust-set: $out"
grep -q 'version 1' <<<"$out" || fail "trust-set didn't apply bundle 1: $out"
lets_in "an admin of the issuing CA" admin system hostname
lets_in "a certificate the root signed itself" admin-root system hostname
lets_in "the node's own CA, still" local system hostname
refuses "an issuing CA the bundle doesn't list" 'certificate|handshake|EOF|Unavailable' admin-other system hostname
refuses "a CA of no fleet" 'certificate|handshake|EOF|Unavailable' admin-stranger system hostname
echo "  ok: bundle 1 lets the issuing CA's certificates in, and only those (and the root's)"

# --- roles ---
lets_in "an operator reads HAProxy's state" operator haproxy show-info
as local haproxy get-config >"$WORKDIR/haproxy.cfg" || fail "get-config"
lets_in "an operator applies HAProxy's configuration" operator haproxy apply-config "$WORKDIR/haproxy.cfg"
refuses "an operator reads a file" PermissionDenied operator system cat /etc/haproxy/haproxy.cfg
as local network get >"$WORKDIR/net.json" || fail "network get"
refuses "an operator applies a network configuration" PermissionDenied operator network apply "$WORKDIR/net.json"
refuses "an operator issues a certificate" PermissionDenied operator pki generate-client-config "$WORKDIR/x"
echo "  ok: an operator runs HAProxy, not the node's files, network or credentials"

refuses "the Controller's certificate for nobody" 'acts for a user' controller system hostname
lets_in "the Controller for an operator, applying HAProxy's configuration" controller -as-user alice -as-roles os:operator haproxy apply-config "$WORKDIR/haproxy.cfg"
refuses "the Controller for an operator, reading a file" PermissionDenied controller -as-user alice -as-roles os:operator system cat /etc/haproxy/haproxy.cfg
refuses "the Controller with an unknown role" PermissionDenied controller -as-user mallory -as-roles os:root system hostname
logs="$(as local system logs janusd)"
grep -q 'api: HAProxyService/ApplyConfig: alice (os:operator) via controller' <<<"$logs" || fail "janusd didn't log the Controller's call for alice: $logs"
grep -q 'api: HAProxyService/ApplyConfig: operator (os:operator)' <<<"$logs" || fail "janusd didn't log the operator's call: $logs"
echo "  ok: the Controller acts only for the user it names, and the node logs who did what"

# --- replacing the node's own CA: the old CA's certificates stop working ---
mkdir -p "$WORKDIR/old"
cp "$WORKDIR/ca.crt" "$WORKDIR/admin.crt" "$WORKDIR/admin.key" "$WORKDIR/old/"
out="$(as local access rotate-ca "$WORKDIR/rot")" || fail "rotate-ca: $out"
cp "$WORKDIR/rot/ca.crt" "$WORKDIR/rot/admin.crt" "$WORKDIR/rot/admin.key" "$WORKDIR/"
lets_in "the new admin certificate" local system hostname
if "$CTL_BIN" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/old/admin.crt" -key "$WORKDIR/old/admin.key" system hostname >/dev/null 2>&1; then
  fail "the old CA's admin certificate still works after rotate-ca"
fi
if "$CTL_BIN" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/old/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" system hostname >/dev/null 2>&1; then
  fail "the node still presents a server certificate of its old CA"
fi
lets_in "the fleet's certificates, untouched by the rotation" admin system hostname
echo "  ok: rotate-ca - the new admin certificate's key made by janusctl, the old CA's certificates refused, the fleet's kept"

out="$(as local access rotate-ca -console "$WORKDIR/rot2")" || fail "rotate-ca -console: $out"
deadline=$((SECONDS + 10))
until python3 - "$LOG" "$WORKDIR" <<'PY'
import re, sys
log = open(sys.argv[1], errors="replace").read()
i = log.rfind("pki: the node's own CA was replaced")
if i < 0:
    sys.exit(1)
blocks = [m.group(0) for m in re.finditer(r"-----BEGIN ([A-Z ]+)-----.*?-----END \1-----", log[i:], re.S)]
if len(blocks) < 3:
    sys.exit(1)
for name, b in zip(["ca.crt", "admin.crt", "admin.key"], blocks):
    open(sys.argv[2] + "/" + name, "w").write(b.replace("\r", "") + "\n")
PY
do
  [ "$SECONDS" -lt "$deadline" ] || fail "rotate-ca -console: nothing printed on the console"
  sleep 1
done
cmp -s "$WORKDIR/ca.crt" "$WORKDIR/rot2/ca.crt" || fail "the console's CA isn't the one rotate-ca -console returned"
lets_in "the admin certificate printed on the console" local system hostname
if "$CTL_BIN" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/rot/admin.crt" -key "$WORKDIR/rot/admin.key" system hostname >/dev/null 2>&1; then
  fail "the previous admin certificate still works after the second rotation"
fi
if grep -q 'PRIVATE KEY' <<<"$(as local system logs janusd)"; then fail "the printed admin key is in janusd's log, which Logs serves"; fi
echo "  ok: rotate-ca -console - the node's key printed on the console only, like at first boot"

# --- the trust survives a reboot ---
as local system reboot >/dev/null || fail "Reboot failed"
wait_boot 2
deadline=$((SECONDS + 60))
until as admin version >/dev/null 2>&1; do
  [ "$SECONDS" -lt "$deadline" ] || fail "after the reboot, the fleet's admin certificate is refused: $(as admin version 2>&1)"
  sleep 1
done
[ "$(first_boots)" -eq 1 ] || fail "PKI was regenerated by a plain Reboot"
lets_in "the rotated CA's admin certificate after the reboot" local system hostname
grep -q 'pki: trusts the fleet of root "test fleet root", bundle version 1' <<<"$(as local system logs janusd)" || fail "janusd didn't say which fleet it trusts after the reboot: $(as local system logs janusd)"
echo "  ok: the fleet trust and the rotated CA survive a reboot"

# --- bundle 2: the first issuing CA leaves, the other comes in ---
out="$(as admin access trust-set "$F/bundle-2.json")" || fail "a fleet admin applying bundle 2: $out"
grep -q 'version 2' <<<"$out" || fail "bundle 2 not applied: $out"
refuses "an issuing CA bundle 2 dropped" 'certificate|handshake|EOF|Unavailable' admin system hostname
lets_in "the issuing CA bundle 2 lists" admin-other system hostname
refuses "an older bundle" 'older than the node' admin-other access trust-set "$F/bundle-1.json"
refuses "another fleet's root" 'another fleet root' admin-other access trust-set -root "$F/stranger-root.crt" "$F/bundle-2.json"
echo "  ok: bundle 2 drops the first issuing CA without touching the node; an older bundle is refused"

# The file API never serves the fleet trust's files either.
refuses "Read of the pinned root" PermissionDenied local system cat /etc/janus/pki/fleet/root.crt

# --- forgetting the fleet: only through the node's own CA ---
refuses "a fleet admin making the node forget its fleet" "node's own CA" admin-other access trust-reset
out="$(as local access trust-reset)" || fail "trust-reset with the node's own CA: $out"
grep -q '^Fleet: *none' <<<"$out" || fail "trust-reset didn't forget the fleet: $out"
refuses "the fleet after trust-reset" 'certificate|handshake|EOF|Unavailable' admin-other system hostname
lets_in "the node's own CA after trust-reset" local system hostname
echo "  ok: only the node's own CA makes it forget its fleet"

as local system shutdown >/dev/null || fail "Shutdown failed"
deadline=$((SECONDS + 30))
while kill -0 "$QEMU_PID" 2>/dev/null; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the VM didn't power off after Shutdown"
  sleep 1
done
QEMU_PID=""

if grep -a "avc:.*denied" "$LOG"; then
  fail "SELinux denials"
fi
echo "Fleet trust test OK: own CA always, fleet certificates by bundle, roles and the Controller's users enforced, on a real enforcing node, zero AVC denials"
