#!/usr/bin/env bash
# Proves the rescue volume (docs/guide/recovery.md, internal/nocloud
# rescue.go, PairingService/Rescue) on a real node - the full disk image
# with the nftables extension, OVMF, SELinux enforcing:
#   - R trusts alice's fleet from its NoCloud volume, alice applies a
#     firewall that leaves only the API open; then alice's fleet is lost;
#   - a rescue ISO (a CD-ROM, R's cidata volume still attached): init
#     finds it, the console shows a rescue code - but the saved firewall
#     shuts the window's port;
#   - one made -skip-firewall: the window answers; a wrong code counts;
#     bob's janusctl access rescue gets an hour's admin certificate of R's
#     own CA (rescue:bob) - the API takes it, R logs and audits it, the
#     window closes; bob resets R's trust and adopts it into his fleet;
#   - the volume detached: no window, R trusts bob's fleet, its saved
#     firewall back;
#   - a FAT one (a disk): rescued again; janusd restarted in that boot
#     opens no second window;
#   - no AVC denial.
#
# Usage: hack/qemu-rescue-test.sh <disk.img> <janusctl-bin>
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

usage="usage: $0 <disk.img> <janusctl-bin>"
DISK="${1:?$usage}"
CTL_BIN="$(realpath "${2:?$usage}")"
TIMEOUT="${QEMU_RESCUE_BOOT_TIMEOUT:-90}"
B="$((19040 + ${JANUS_TEST_PORT_OFFSET:-0}))"
API="$((B + 1))"
PAIR="$((B + 2))"
OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf)" >&2; exit 1; }

WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && { kill "$QEMU_PID" 2>/dev/null || true; wait "$QEMU_PID" 2>/dev/null || true; }
  rm -rf "$WORKDIR"
}
trap cleanup EXIT
trap 'echo "qemu-rescue-test: line $LINENO: $BASH_COMMAND failed" >&2' ERR
LOGS=()
fail() {
  echo "Rescue test FAILED: $*" >&2
  for l in "${LOGS[@]}"; do
    [ -f "$l" ] && { echo "--- $(basename "$l") ---" >&2; grep -a "init: rescue\|rescue:\|pairing:\|firewall:\|access:\|avc:" "$l" | tail -30 >&2; }
  done
  [ -n "${LOG:-}" ] && [ -f "$LOG" ] && { echo "--- the end of $(basename "$LOG") ---" >&2; sed 's/\x1b\[[0-9;]*m//g' "$LOG" | tail -40 >&2; }
  exit 1
}
check() { # check DESCRIPTION PATTERN TEXT
  grep -Eq -- "$2" <<<"$3" || fail "$1 (no match for $2 in: $3)"
}

alice() { JANUSCONFIG="$WORKDIR/alice/janusctl.json" "$CTL_BIN" "$@"; }
bob() { JANUSCONFIG="$WORKDIR/bob/janusctl.json" "$CTL_BIN" "$@"; }
mkdir -p "$WORKDIR/alice" "$WORKDIR/bob"
alice -context lab fleet init -name lab -issuer alice-laptop -user alice -yes "$WORKDIR/alice-kit.age" >/dev/null 2>&1 || fail "alice's fleet"
alice fleet export "$WORKDIR/export" >/dev/null || fail "fleet export"
truncate -s 1M "$WORKDIR/cidata.img"
mkfs.vfat -F 12 -n cidata "$WORKDIR/cidata.img" >/dev/null
mcopy -i "$WORKDIR/cidata.img" "$WORKDIR/export/user-data.json" ::user-data

# The rescue volumes, made by janusctl: ISO (a CD-ROM), FAT (a disk).
"$CTL_BIN" image rescue-volume "$WORKDIR/rescue.iso" >/dev/null || fail "image rescue-volume"
"$CTL_BIN" image rescue-volume -skip-firewall "$WORKDIR/rescue-skip.iso" >/dev/null || fail "image rescue-volume -skip-firewall"
"$CTL_BIN" image rescue-volume -fat -skip-firewall "$WORKDIR/rescue-skip.img" >/dev/null || fail "image rescue-volume -fat"
if "$CTL_BIN" image rescue-volume "$WORKDIR/rescue.iso" >/dev/null 2>&1; then fail "rescue-volume wrote over a file"; fi

cp --sparse=always "$DISK" "$WORKDIR/r.img"
N=0
boot() { # boot [ISO|-fat IMG]: R, a fresh console log each time
  N=$((N + 1))
  LOG="$WORKDIR/r-$N.log"
  LOGS+=("$LOG")
  # Fresh firmware variables: the devices change from one boot to the
  # next, and boot entries kept for the previous ones may leave OVMF in
  # its shell.
  cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/r.vars"
  local extra=()
  case "${1:-}" in
    "") ;;
    -fat) extra=(-drive file="$2",format=raw,if=virtio,readonly=on) ;;
    *) extra=(-device virtio-scsi-pci,id=scsi0 -drive if=none,id=cd,file="$1",format=raw,media=cdrom,readonly=on -device scsi-cd,drive=cd,bus=scsi0.0) ;;
  esac
  qemu-system-x86_64 -accel kvm -accel tcg \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$WORKDIR/r.vars" \
    -drive file="$WORKDIR/r.img",format=raw,if=virtio \
    -drive file="$WORKDIR/cidata.img",format=raw,if=virtio "${extra[@]}" \
    -nographic -display none -m 512M \
    -netdev "user,id=net0,hostfwd=tcp::$API-:9505,hostfwd=tcp::$PAIR-:9506" \
    -device virtio-net-pci,netdev=net0 \
    -serial file:"$LOG" </dev/null &
  QEMU_PID=$!
  local deadline=$((SECONDS + TIMEOUT))
  until grep -aq "No shell, no SSH" "$LOG" 2>/dev/null; do
    [ "$SECONDS" -lt "$deadline" ] || fail "R's janusd never listened (boot $N)"
    sleep 1
  done
}
halt() { kill "$QEMU_PID" 2>/dev/null || true; wait "$QEMU_PID" 2>/dev/null || true; QEMU_PID=""; }
powered_off() { # powered_off: wait for R to power itself off (system shutdown)
  local deadline=$((SECONDS + 60))
  while kill -0 "$QEMU_PID" 2>/dev/null; do
    [ "$SECONDS" -lt "$deadline" ] || fail "R didn't power off"
    sleep 1
  done
  wait "$QEMU_PID" 2>/dev/null || true
  QEMU_PID=""
}
code_of() { { grep -ao 'rescue: code [0-9A-Z-]*' "$1" || true; } | tail -1 | awk '{print $3}'; }
window_open() { echo | timeout 5 openssl s_client -connect "127.0.0.1:$PAIR" 2>/dev/null | grep -q 'BEGIN CERTIFICATE'; }
rescue() { # rescue CODE DIR [NAME]
  bob access rescue -endpoint "127.0.0.1:$API" -pairing "127.0.0.1:$PAIR" -code "$1" -name "${3:-bob}" "$2" 2>&1
}
as_rescued() { # as_rescued DIR ARGS...: janusctl with the rescue certificate in DIR
  local d=$1
  shift
  "$CTL_BIN" -endpoint "127.0.0.1:$API" -ca "$d/ca.crt" -cert "$d/admin.crt" -key "$d/admin.key" "$@"
}

# --- R in alice's fleet, behind a firewall that leaves only the API ---
boot
grep -aq "init: nocloud: seeded the fleet from" "$LOG" || fail "R didn't take alice's fleet from its NoCloud volume"
if grep -aq "rescue: code\|pairing: code" "$LOG"; then fail "R, in a fleet, shows a code without a rescue volume"; fi
FP="$(grep -ao "this node's CA: SHA-256 [0-9a-f]*" "$LOG" | tail -1 | awk '{print $NF}')"
alice fleet adopt edge-r -endpoint "127.0.0.1:$API" -ca-fingerprint "$FP" >/dev/null || fail "alice adopting R"
cat >"$WORKDIR/ruleset.nft" <<'NFT'
table inet filter {
	chain input {
		type filter hook input priority filter; policy drop;
		ct state established,related accept
		iif "lo" accept
		tcp dport 9505 accept
	}
}
NFT
out="$(alice -n edge-r network firewall apply -timeout 30s "$WORKDIR/ruleset.nft")" || fail "the firewall: $out"
check "the firewall confirmed" "confirmed over a new connection" "$out"
alice -n edge-r system shutdown >/dev/null || fail "shutting R down"
powered_off
rm -rf "$WORKDIR/alice"
echo "  ok: R trusts alice's fleet, a firewall leaves only its API open - then alice's fleet is lost"

# --- a rescue volume: a code on the console, the window shut by the firewall ---
boot "$WORKDIR/rescue.iso"
check "init found the volume" "init: rescue: a rescue volume on /dev/sr0" "$(cat "$LOG")"
CODE="$(code_of "$LOG")"
[ "${#CODE}" = 24 ] || fail "no rescue code on the console"
check "the banner shows the rescue code" "rescue    .*$CODE" "$(sed 's/\x1b\[[0-9;]*m//g' "$LOG")"
if window_open; then fail "the window answers through a firewall that shuts 9506"; fi
halt
echo "  ok: a rescue volume - the console shows a rescue code; the saved firewall shuts the window"
# The Controller relays a machine's console to the machine's operators,
# the code hidden (security review 2026-11-05 #12): this very console,
# through the Controller's redactors.
JANUS_CONSOLE_LOG="$LOG" JANUS_CONSOLE_CODE="$CODE" go test ./dashboard/backend -run '^TestRealConsoleCodeHidden$' -count=1 -v >"$WORKDIR/redact.out" 2>&1 || { cat "$WORKDIR/redact.out" >&2; fail "the Controller relays this console with its rescue code"; }
grep -q -- "--- PASS: TestRealConsoleCodeHidden" "$WORKDIR/redact.out" || { cat "$WORKDIR/redact.out" >&2; fail "the console redaction test didn't run"; }
echo "  ok: the Controller relays this console without its rescue code ($(grep -o 'hidden [0-9]* times' "$WORKDIR/redact.out"))"

# --- -skip-firewall: rescued, then bob's fleet ---
boot "$WORKDIR/rescue-skip.iso"
check "the firewall skipped" "firewall: the rescue volume says skip" "$(cat "$LOG")"
CODE="$(code_of "$LOG")"
[ "${#CODE}" = 24 ] || fail "no rescue code on the console"
window_open || fail "the rescue window doesn't answer"
WRONG="$(go run ./hack/pairingcode wrong "$CODE")" || fail "make a wrong code"
if out="$(rescue "$WRONG" "$WORKDIR/wrong")"; then fail "rescued with a wrong code: $out"; fi
check "a wrong code counted" "wrong pairing code - 4 tries left" "$out"
out="$(rescue "$CODE" "$WORKDIR/r1")" || fail "access rescue: $out"
check "rescued" "Rescued: an admin certificate of the node's own CA \(SHA-256 $FP\) for rescue:bob" "$out"
[ "$(stat -c %a "$WORKDIR/r1/admin.key")" = 600 ] || fail "the rescue key isn't 0600"
openssl x509 -in "$WORKDIR/r1/admin.crt" -noout -subject | grep -q "CN *= *rescue:bob" || fail "the certificate isn't rescue:bob"
as_rescued "$WORKDIR/r1" version >/dev/null || fail "R's API refuses the rescue certificate"
out="$(as_rescued "$WORKDIR/r1" access trust)" || fail "access trust: $out"
logs="$(as_rescued "$WORKDIR/r1" system logs janusd 2>&1)"
check "R logs the rescue" "rescue: an admin certificate for rescue:bob, an hour, issued to" "$logs"
check "R logs who called" "rescue:bob \(os:admin\)" "$logs"
if grep -q "$CODE" <<<"$logs"; then fail "the rescue code is in janusd's log"; fi
audit="$(timeout 5 "$CTL_BIN" -endpoint "127.0.0.1:$API" -ca "$WORKDIR/r1/ca.crt" -cert "$WORKDIR/r1/admin.crt" -key "$WORKDIR/r1/admin.key" system audit 2>/dev/null || true)"
check "the wrong code audited" $'PairingService/Rescue\tpairing code\t[0-9.:]+\trefused: .*wrong pairing code' "$audit"
check "the rescue audited" $'PairingService/Rescue\tpairing code\t[0-9.:]+\tOK' "$audit"
sleep 35 # the window lingers for a lost answer (api.PairReplay)
if window_open; then fail "the rescue window still answers once used"; fi
# #18: the skipped firewall must not outlive the window - it's applied
# once the window closes, in this same boot.
check "the skipped firewall is applied when the window closes" "firewall: the rescue window closed - the saved ruleset is applied now" "$(cat "$LOG")"
as_rescued "$WORKDIR/r1" access trust-reset >/dev/null || fail "trust-reset with the rescue certificate"
bob -context home fleet init -name home -issuer bob-laptop -user bob -yes "$WORKDIR/bob-kit.age" >/dev/null 2>&1 || fail "bob's fleet"
bob fleet adopt edge-r -endpoint "127.0.0.1:$API" -ca "$WORKDIR/r1/ca.crt" -cert "$WORKDIR/r1/admin.crt" -key "$WORKDIR/r1/admin.key" >/dev/null || fail "bob adopting R"
bob -n edge-r version >/dev/null || fail "bob can't reach R with his fleet's certificate"
halt
echo "  ok: -skip-firewall - the window answers; a wrong code counted; bob rescued R (rescue:bob, an hour, R's own CA): its API took it, it logged and audited it, the window closed; R reset and adopted into bob's fleet"

# --- the volume detached: nothing open, bob's fleet, the firewall back ---
boot
if grep -aq "init: rescue\|rescue: code" "$LOG"; then fail "a rescue window without the volume"; fi
bob -n edge-r version >/dev/null || fail "R, the volume detached, doesn't let bob's fleet in"
out="$(bob -n edge-r network firewall status)" || fail "firewall status: $out"
check "the saved firewall is back" "tcp dport 9505 accept" "$out"
if window_open; then fail "a window answers without the volume"; fi
halt
echo "  ok: the volume detached - no window, R trusts bob's fleet, its firewall is back"

# --- a FAT rescue volume (a disk); once a boot ---
boot -fat "$WORKDIR/rescue-skip.img"
check "init found the FAT volume" "init: rescue: a rescue volume on /dev/vd" "$(cat "$LOG")"
CODE="$(code_of "$LOG")"
out="$(rescue "$CODE" "$WORKDIR/r2" carol)" || fail "access rescue from a FAT volume: $out"
as_rescued "$WORKDIR/r2" version >/dev/null || fail "R's API refuses carol's rescue certificate"
before="$(grep -ac "rescue: code" "$LOG")"
bob -n edge-r system restart >/dev/null 2>&1 || true
deadline=$((SECONDS + 60))
until [ "$(grep -ac "No shell, no SSH" "$LOG")" -ge 2 ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "janusd didn't come back after its restart"
  sleep 1
done
check "a restarted janusd opens no second window" "rescue: this boot's rescue certificate was issued already" "$(cat "$LOG")"
[ "$(grep -ac "rescue: code" "$LOG")" = "$before" ] || fail "a restarted janusd showed another rescue code"
halt
echo "  ok: a FAT rescue volume rescued R too; a janusd restarted in that boot opened no second window"

for l in "${LOGS[@]}"; do
  denials="$(grep -a "avc:.*denied" "$l" || true)"
  [ -z "$denials" ] || { echo "$denials" >&2; fail "AVC denials ($(basename "$l"))"; }
done
echo "Rescue test OK: a rescue volume opened a window on a node whose fleet was lost - its firewall skipped when asked -, the code its console showed gave an hour's admin certificate of its own CA, once a boot; the node reset and adopted into another fleet; zero AVC denials"
