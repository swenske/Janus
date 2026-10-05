#!/usr/bin/env bash
# Proves a fleet without a Controller (janusctl fleet, phase 6) on two
# real nodes: the full disk image under real OVMF with SELinux enforcing,
# every call over mTLS.
#
# alice's machine makes the fleet (its root in a recovery kit); node A
# gets it from a NoCloud volume, node B from `janusctl image seed-fleet`
# - both trust it from their first boot, nobody copies an admin
# credential: alice adopts each on its CA's fingerprint, read from its
# console. Her certificates are signed on her machine, and the nodes log
# them as the fleet's. bob's machine (CI, an operator) gets an issuing
# CA of its own - requested there, signed where the kit is, synced to
# the nodes, limited by the bundle to operators and readers (an admin's
# or a Controller's certificate made with its key is refused) -, runs
# HAProxy, is refused the node's files, then revoked.
# carol's machine recovers the fleet from the kit alone - a lost
# Controller's case - and takes node A over: alice's machine is refused
# there, still let in on node B until it gets the new bundle. Fails on
# any AVC denial.
#
# Usage: hack/qemu-fleetctl-test.sh <disk.img> <janusctl-bin>
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

DISK="${1:?usage: $0 <disk.img> <janusctl-bin>}"
CTL_BIN="$(realpath "${2:?usage: $0 <disk.img> <janusctl-bin>}")"
REPO="$(pwd)"
BOOT_TIMEOUT_SECS="${QEMU_FLEETCTL_BOOT_TIMEOUT:-90}"
PORT_A="$((18240 + ${JANUS_TEST_PORT_OFFSET:-0}))"
PORT_B="$((18241 + ${JANUS_TEST_PORT_OFFSET:-0}))"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf)" >&2; exit 1; }

WORKDIR="$(mktemp -d)"
PIDS=()
cleanup() {
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

fail() {
  echo "Fleetctl test FAILED: $*" >&2
  for l in "$WORKDIR"/a.log "$WORKDIR"/b.log; do
    [ -f "$l" ] && { echo "--- $(basename "$l") ---" >&2; tail -40 "$l" >&2; }
  done
  exit 1
}

# One janusctl configuration per machine.
alice() { JANUSCONFIG="$WORKDIR/alice/janusctl.json" "$CTL_BIN" "$@"; }
bob() { JANUSCONFIG="$WORKDIR/bob/janusctl.json" "$CTL_BIN" "$@"; }
carol() { JANUSCONFIG="$WORKDIR/carol/janusctl.json" "$CTL_BIN" "$@"; }
mkdir -p "$WORKDIR/alice" "$WORKDIR/bob" "$WORKDIR/carol"

# --- the fleet, and two disks provisioned with it ---
alice -context lab fleet init -name lab -issuer alice-laptop -user alice -yes "$WORKDIR/kit.age" >"$WORKDIR/passphrase" 2>/dev/null \
  || fail "fleet init"
JANUS_KIT_PASSPHRASE="$(cat "$WORKDIR/passphrase")"
export JANUS_KIT_PASSPHRASE
alice fleet export "$WORKDIR/export" >/dev/null || fail "fleet export"

cp --sparse=always "$DISK" "$WORKDIR/a.img"
cp --sparse=always "$DISK" "$WORKDIR/b.img"
truncate -s 1M "$WORKDIR/cidata.img"
mkfs.vfat -F 12 -n cidata "$WORKDIR/cidata.img" >/dev/null
mcopy -i "$WORKDIR/cidata.img" "$WORKDIR/export/user-data.json" ::user-data
alice image seed-fleet "$WORKDIR/b.img" >/dev/null || fail "image seed-fleet"
echo "Provisioning OK: a kit, a NoCloud volume for node A, node B's disk seeded with the fleet"

boot() { # boot NAME PORT [extra drive]
  cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/$1.vars"
  qemu-system-x86_64 -accel kvm -accel tcg \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$WORKDIR/$1.vars" \
    -drive file="$WORKDIR/$1.img",format=raw,if=virtio \
    ${3:+-drive file="$3",format=raw,if=virtio} \
    -nographic -display none -m 512M \
    -netdev "user,id=net0,hostfwd=tcp::$2-:9505" \
    -device virtio-net-pci,netdev=net0 \
    -serial file:"$WORKDIR/$1.log" &
  PIDS+=($!)
}
boot a "$PORT_A" "$WORKDIR/cidata.img"
boot b "$PORT_B"
listening() { grep -aq "listening on" "$WORKDIR/$1.log" 2>/dev/null; }
deadline=$((SECONDS + BOOT_TIMEOUT_SECS))
until listening a && listening b; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the nodes didn't start within ${BOOT_TIMEOUT_SECS}s"
  sleep 1
done
grep -aq "init: nocloud: seeded the fleet from" "$WORKDIR/a.log" || fail "init didn't seed node A's fleet from the NoCloud volume"

# The node's CA's fingerprint, as its console shows it ("ca sha256").
fingerprint() {
  python3 - "$WORKDIR/$1.log" <<'PYEOF'
import re, sys
lines = [re.sub(r"\x1b\[[0-9;]*m", "", l) for l in open(sys.argv[1], errors="replace")]
for i, l in enumerate(lines):
    if "ca sha256" in l:
        groups = re.findall(r"\b[0-9a-f]{8}\b", l.split("ca sha256", 1)[1]) + re.findall(r"\b[0-9a-f]{8}\b", lines[i + 1])
        print("".join(groups[-8:]))
        break
PYEOF
}
FP_A="$(fingerprint a)"
FP_B="$(fingerprint b)"
[ "${#FP_A}" = 64 ] && [ "${#FP_B}" = 64 ] || fail "no CA fingerprint on the consoles: '$FP_A' '$FP_B'"

# --- alice adopts both on their fingerprints ---
if out="$(alice fleet adopt edge-a -endpoint "127.0.0.1:$PORT_A" -ca-fingerprint "$FP_B" 2>&1)"; then
  fail "node A adopted on node B's fingerprint: $out"
fi
grep -q "not adopted" <<<"$out" || fail "a wrong fingerprint, refused but: $out"
alice fleet adopt edge-a -endpoint "127.0.0.1:$PORT_A" -ca-fingerprint "$FP_A" >/dev/null || fail "adopting node A"
alice fleet adopt edge-b -endpoint "127.0.0.1:$PORT_B" -ca-fingerprint "$FP_B" >/dev/null || fail "adopting node B"
out="$(alice -all version 2>&1)" || fail "alice -all version: $out"
[ "$(grep -c "Image schematic" <<<"$out")" = 2 ] || fail "alice -all version: $out"
alice -n edge-a system service restart haproxy >/dev/null || fail "alice restarting HAProxy"
logs="$(alice -n edge-a system logs janusd 2>&1)"
grep -q 'api: SystemService/ServiceRestart: alice (os:admin, fleet)' <<<"$logs" || fail "node A didn't log alice's restart as the fleet's: $(grep ServiceRestart <<<"$logs")"
echo "Adoption OK: both nodes trusted the fleet from their first boot, adopted on their consoles' fingerprints (a wrong one refused); alice's certificates, signed on her machine, logged as the fleet's"

# --- bob's machine: an issuing CA of its own, as an operator ---
bob -context lab fleet issuer request -issuer bob-ci -user bob -role os:operator "$WORKDIR/request.json" >/dev/null || fail "issuer request"
if out="$(bob -n edge-a version 2>&1)"; then fail "bob before his grant: $out"; fi
alice fleet issuer sign -kit "$WORKDIR/kit.age" "$WORKDIR/request.json" "$WORKDIR/grant.json" >/dev/null || fail "issuer sign"
out="$(alice fleet sync 2>&1)" || fail "sync: $out"
[ "$(grep -c -- '->' <<<"$out")" = 2 ] || fail "the new bundle didn't reach both nodes: $out"
bob fleet issuer accept "$WORKDIR/grant.json" >/dev/null || fail "issuer accept"
out="$(bob -all haproxy show-info 2>&1)" || fail "bob -all haproxy show-info: $out"
if out="$(bob -n edge-b system cat /etc/haproxy/haproxy.cfg 2>&1)"; then fail "bob, an operator, read a file: $out"; fi
grep -q "PermissionDenied" <<<"$out" || fail "bob reading a file, refused but: $out"
bob -n edge-b system service restart haproxy >/dev/null || fail "bob restarting HAProxy"
logs="$(alice -n edge-b system logs janusd 2>&1)"
grep -q 'api: SystemService/ServiceRestart: bob (os:operator, fleet)' <<<"$logs" || fail "node B didn't log bob's restart"

# bob's issuing CA may sign operators and readers only - the bundle says
# so, and the nodes hold it to that: certificates made with its key by
# hand (hack/fleetca, not janusctl, which refuses) for an admin or a
# Controller - which acts for any role - are refused; an operator's,
# made the same way, lets in.
alice fleet issuer list | grep "bob-ci" | grep -q "os:operator, os:reader" || fail "bob-ci isn't limited in the bundle: $(alice fleet issuer list)"
python3 -c 'import json,sys; c=json.load(open(sys.argv[1]))["contexts"]["lab"]; print([n for n in c["nodes"] if n["name"]=="edge-a"][0]["ca_pem"])' "$WORKDIR/alice/janusctl.json" >"$WORKDIR/edge-a-ca.crt"
handmade() { # handmade NAME ROLE: a certificate bob's issuing CA signs, without janusctl
  (cd "$REPO" && go run ./hack/fleetca sign "$WORKDIR/bob/lab/issuing.crt" "$WORKDIR/bob/lab/issuing.key" "$2" "$1" "$WORKDIR/$1") || fail "fleetca sign"
}
by_hand() { "$CTL_BIN" -endpoint "127.0.0.1:$PORT_A" -ca "$WORKDIR/edge-a-ca.crt" -cert "$WORKDIR/$1.crt" -key "$WORKDIR/$1.key" "${@:2}"; }
handmade bob-op os:operator
handmade bob-admin os:admin
handmade bob-controller janus:controller
out="$(by_hand bob-op haproxy show-info 2>&1)" || fail "an operator's certificate made by hand with bob's issuing CA was refused - the test's certificate is wrong: $out"
if out="$(by_hand bob-admin version 2>&1)"; then fail "an admin's certificate from bob's operator-only issuing CA let in: $out"; fi
if out="$(by_hand bob-controller -as-user mallory -as-roles os:admin version 2>&1)"; then fail "a Controller's certificate from bob's operator-only issuing CA let in: $out"; fi
echo "Limits OK: bob's issuing CA may sign operators and readers only - the nodes refuse an admin's or a Controller's certificate made with its key"

alice fleet issuer revoke -kit "$WORKDIR/kit.age" bob-ci >/dev/null || fail "issuer revoke"
alice fleet sync >/dev/null || fail "sync after revoking"
if out="$(bob -n edge-a haproxy show-info 2>&1)"; then fail "bob after his revocation: $out"; fi
if out="$(bob fleet sync 2>&1)"; then fail "bob's sync after his revocation: $out"; fi
grep -q "revoked" <<<"$out" || fail "bob's sync, refused but: $out"
echo "Machines OK: bob's issuing CA requested on his machine, signed with the kit, synced to the nodes; an operator there - HAProxy yes, files no -; then revoked, refused everywhere"

# --- carol's machine recovers the fleet from the kit alone ---
carol -context lab fleet recover -kit "$WORKDIR/kit.age" -issuer carol-desk -user carol >/dev/null 2>&1 || fail "fleet recover"
if out="$(carol fleet adopt edge-a -endpoint "127.0.0.1:$PORT_A" -ca-fingerprint "$FP_A" 2>&1)"; then
  fail "carol adopted node A without the kit: $out"
fi
carol fleet adopt edge-a -endpoint "127.0.0.1:$PORT_A" -ca-fingerprint "$FP_A" -kit "$WORKDIR/kit.age" >/dev/null || fail "carol adopting node A with the kit"
carol -n edge-a version >/dev/null || fail "carol on node A"
if out="$(alice -n edge-a version 2>&1)"; then fail "alice on node A after carol's recovery: $out"; fi
alice -n edge-b version >/dev/null || fail "alice on node B, which carol didn't reach yet"
echo "Recovery OK: carol's machine took node A over with the kit alone - alice's machine refused there, let in on node B until it gets the new bundle"

for l in a b; do
  if grep -a "avc:.*denied" "$WORKDIR/$l.log"; then fail "AVC denials on node ${l^^}"; fi
done
echo "Fleetctl test OK: a fleet without a Controller - provisioned (NoCloud, seed), adopted on fingerprints, machines added, revoked and recovered from the kit - on two real enforcing nodes, zero AVC denials"
