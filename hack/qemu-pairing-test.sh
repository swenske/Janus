#!/usr/bin/env bash
# Proves the pairing window (internal/pairing, docs/internals/trust.md)
# on real nodes - the full disk image under OVMF, SELinux enforcing:
#   - P, provisioned with nothing: its console shows a pairing code (the
#     banner and a pairing: line), janusd's own log never does; a code
#     naming another certificate is refused by janusctl itself - nothing
#     sent; a wrong secret is refused by the node and audited; the right
#     code pairs it - it trusts the fleet, janusctl reaches its API with
#     the fleet's certificate, its CA pinned -; the window's port closes;
#     restarted, it opens no window;
#   - Q: five wrong secrets from one source pause that source - the right
#     code from it is refused then, the window stays open -; restarted, a
#     new code pairs it;
#   - N (the fleet from a NoCloud volume) and C (a Controller from one)
#     open no window;
#   - no AVC denial on any of them.
#
# Usage: hack/qemu-pairing-test.sh <disk.img> <janusctl-bin>
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

usage="usage: $0 <disk.img> <janusctl-bin>"
DISK="${1:?$usage}"
CTL_BIN="$(realpath "${2:?$usage}")"
TIMEOUT="${QEMU_PAIRING_BOOT_TIMEOUT:-90}"
B="$((18990 + ${JANUS_TEST_PORT_OFFSET:-0}))"
OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf)" >&2; exit 1; }

WORKDIR="$(mktemp -d)"
declare -A PID
cleanup() {
  for p in "${PID[@]}"; do kill "$p" 2>/dev/null || true; done
  for p in "${PID[@]}"; do wait "$p" 2>/dev/null || true; done
  rm -rf "$WORKDIR"
}
trap cleanup EXIT
trap 'echo "qemu-pairing-test: line $LINENO: $BASH_COMMAND failed" >&2' ERR
fail() {
  echo "Pairing test FAILED: $*" >&2
  for l in "$WORKDIR"/*.log; do
    [ -f "$l" ] && { echo "--- $(basename "$l") ---" >&2; grep -a "pki:\|pairing:\|nocloud\|access:\|avc:" "$l" | tail -30 >&2; }
  done
  exit 1
}
check() { # check DESCRIPTION PATTERN TEXT
  grep -Eq -- "$2" <<<"$3" || fail "$1 (no match for $2 in: $3)"
}

alice() { JANUSCONFIG="$WORKDIR/alice/janusctl.json" "$CTL_BIN" "$@"; }
mkdir -p "$WORKDIR/alice"
alice -context lab fleet init -name lab -issuer alice-laptop -user alice -yes "$WORKDIR/kit.age" >/dev/null 2>&1 || fail "fleet init"
alice fleet export "$WORKDIR/export" >/dev/null || fail "fleet export"

# The nodes: index i, API on B+10i+1, pairing window on B+10i+2.
idx() { case "$1" in p) echo 0 ;; q) echo 1 ;; n) echo 2 ;; c) echo 3 ;; esac; }
api_port() { echo $((B + 10 * $(idx "$1") + 1)); }
pair_port() { echo $((B + 10 * $(idx "$1") + 2)); }
for n in p q n c; do cp --sparse=always "$DISK" "$WORKDIR/$n.img"; done
cidata() { # cidata NAME USER-DATA-FILE
  truncate -s 1M "$WORKDIR/$1-cidata.img"
  mkfs.vfat -F 12 -n cidata "$WORKDIR/$1-cidata.img" >/dev/null
  mcopy -i "$WORKDIR/$1-cidata.img" "$2" ::user-data
}
cidata n "$WORKDIR/export/user-data.json"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=nobody" -days 2 \
  -keyout "$WORKDIR/ctl.key" -out "$WORKDIR/ctl.crt" 2>/dev/null
python3 -c 'import json,sys; print(json.dumps({"controller_address": "10.0.2.2:1", "controller_ca_cert": open(sys.argv[1]).read()}))' "$WORKDIR/ctl.crt" >"$WORKDIR/c-user-data.json"
cidata c "$WORKDIR/c-user-data.json"

boot() { # boot NAME: a fresh console log each start (the previous one kept as NAME.prev)
  local n=$1 extra=()
  [ -f "$WORKDIR/$n.log" ] && mv "$WORKDIR/$n.log" "$WORKDIR/$n.prev"
  [ -f "$WORKDIR/$n-cidata.img" ] && extra=(-drive file="$WORKDIR/$n-cidata.img",format=raw,if=virtio)
  [ -f "$WORKDIR/$n.vars" ] || cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/$n.vars"
  qemu-system-x86_64 -accel kvm -accel tcg \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$WORKDIR/$n.vars" \
    -drive file="$WORKDIR/$n.img",format=raw,if=virtio "${extra[@]}" \
    -nographic -display none -m 512M \
    -netdev "user,id=net0,hostfwd=tcp::$(api_port "$n")-:9505,hostfwd=tcp::$(pair_port "$n")-:9506" \
    -device virtio-net-pci,netdev=net0 \
    -serial file:"$WORKDIR/$n.log" </dev/null &
  PID[$n]=$!
}
halt() { kill "${PID[$1]}" 2>/dev/null || true; wait "${PID[$1]}" 2>/dev/null || true; unset "PID[$1]"; }
up() { # up NAME: janusd listening, its banner printed
  local deadline=$((SECONDS + TIMEOUT))
  until grep -aq "No shell, no SSH" "$WORKDIR/$1.log" 2>/dev/null; do
    [ "$SECONDS" -lt "$deadline" ] || fail "node $1's janusd never listened"
    sleep 1
  done
}
code_of() { { grep -ao 'pairing: code [0-9A-Z-]*' "$WORKDIR/$1.log" || true; } | tail -1 | awk '{print $3}'; }
window_open() { echo | timeout 5 openssl s_client -connect "127.0.0.1:$(pair_port "$1")" 2>/dev/null | grep -q 'BEGIN CERTIFICATE'; }
adopt() { # adopt NODE CODE
  alice fleet adopt "edge-$1" -endpoint "127.0.0.1:$(api_port "$1")" -pairing "127.0.0.1:$(pair_port "$1")" -code "$2" 2>&1
}

for n in p q n c; do boot "$n"; done
for n in p q n c; do up "$n"; done
echo "  ok: four nodes up"

# --- P: the window, its code on the console only ---
CODE_P="$(code_of p)"
[ "${#CODE_P}" = 24 ] || fail "node P shows no pairing code"
check "P's banner shows the code" "pairing   .*$CODE_P" "$(sed 's/\x1b\[[0-9;]*m//g' "$WORKDIR/p.log")"
window_open p || fail "node P's pairing window doesn't answer"
for n in n c; do
  if grep -aq "pairing: code" "$WORKDIR/$n.log"; then fail "node $n, provisioned, shows a pairing code"; fi
  if window_open "$n"; then fail "node $n, provisioned, has a pairing window"; fi
done
grep -aq "init: nocloud: seeded the fleet from" "$WORKDIR/n.log" || fail "node N didn't take the fleet from its NoCloud volume"
echo "  ok: P shows its code and opens its window; N (a fleet) and C (a Controller) neither"

# Another certificate's code - P's own with its first symbol changed and
# the check recomputed: janusctl refuses the node itself, sends nothing.
OTHER="$(go run ./hack/pairingcode flip "$CODE_P")" || fail "make another code"
if out="$(adopt p "$OTHER")"; then fail "adopted with another certificate's code: $out"; fi
check "another certificate's code refused by janusctl" "isn't the one its code names" "$out"
WRONG="$(go run ./hack/pairingcode wrong "$CODE_P")" || fail "make a wrong code"
if out="$(adopt p "$WRONG")"; then fail "adopted with a wrong secret: $out"; fi
check "a wrong secret refused by the node" "wrong pairing code - 4 tries left" "$out"
out="$(adopt p "$CODE_P")" || fail "pairing P: $out"
check "P paired" "Paired with 127.0.0.1:$(pair_port p)" "$out"
check "P adopted" "edge-p .* is in fleet \"lab\"" "$out"
alice -n edge-p version >/dev/null || fail "alice can't reach P's API with the fleet's certificate"
logs="$(alice -n edge-p system logs janusd 2>&1)"
check "P logs its pairing" "pairing: paired from .* the node trusts the fleet of root" "$logs"
if grep -q "$CODE_P" <<<"$logs"; then fail "the code is in janusd's log"; fi
audit="$(JANUSCONFIG="$WORKDIR/alice/janusctl.json" timeout 5 "$CTL_BIN" -n edge-p system audit 2>/dev/null || true)"
check "the wrong secret audited" $'PairingService/Pair\tpairing code\t[0-9.:]+\trefused: .*wrong pairing code' "$audit"
check "the pairing audited" $'PairingService/Pair\tpairing code\t[0-9.:]+\tOK' "$audit"
sleep 35 # the window lingers for a lost answer (api.PairReplay)
if window_open p; then fail "P's pairing window still answers once paired"; fi
echo "  ok: another certificate's code refused by janusctl, a wrong secret by the node; the right one paired P - its API reached with the fleet's certificate, the call audited, the code in no log -, its window closed"

halt p
boot p
up p
if grep -aq "pairing: code" "$WORKDIR/p.log"; then fail "P, paired, shows a code again after a restart"; fi
alice -n edge-p version >/dev/null || fail "P, restarted, doesn't let the fleet in"
echo "  ok: P restarted trusts the fleet, opens no window"

# --- Q: five wrong codes from one source pause it, not the window ---
CODE_Q="$(code_of q)"
[ "${#CODE_Q}" = 24 ] || fail "node Q shows no pairing code"
for i in 1 2 3 4 5; do
  WRONG="$(go run ./hack/pairingcode wrong "$CODE_Q" "$i")"
  if out="$(adopt q "$WRONG")"; then fail "Q adopted with a wrong secret: $out"; fi
done
check "the fifth wrong secret pauses the source" "wrong pairing codes from your address" "$out"
if out="$(adopt q "$CODE_Q")"; then fail "Q paired from a paused source: $out"; fi
check "the right code from the paused source is refused" "from your address" "$out"
window_open q || fail "Q's window stopped answering - a source's failures must not close it for everyone"
halt q
boot q
up q
NEW_Q="$(code_of q)"
[ "${#NEW_Q}" = 24 ] && [ "$NEW_Q" != "$CODE_Q" ] || fail "Q restarted: no new code ($NEW_Q)"
out="$(adopt q "$NEW_Q")" || fail "pairing Q with its new code: $out"
alice -n edge-q version >/dev/null || fail "alice can't reach Q's API"
echo "  ok: five wrong codes paused the source (the window stayed open), the right one refused then; restarted, a new code paired it"

for n in p q n c; do
  denials="$(grep -a "avc:.*denied" "$WORKDIR/$n.log" || true)"
  [ -z "$denials" ] || { echo "$denials" >&2; fail "AVC denials on node $n"; }
done
echo "Pairing test OK: a node that trusts nothing paired with the code its console shows - checked by janusctl before anything was sent, a wrong one counted, five pausing the source (the window stayed open) -, provisioned nodes open none, zero AVC denials"
