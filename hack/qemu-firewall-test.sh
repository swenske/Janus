#!/usr/bin/env bash
# Proves the firewall (the nftables extension, docs/firewall.md) on a real
# node built with it: UEFI, SELinux enforcing. A ruleset is refused with
# its own line numbers, a valid one is applied on trial and confirmed over
# a new connection - and really filters (HAProxy's port becomes
# unreachable, the API stays reachable); a ruleset that locks the API out
# reverts by itself to the confirmed one; set elements are added live,
# kept across a reboot unless they have a timeout; the saved ruleset is
# back after the reboot; the exporter reports it; no AVC denial.
#
# Usage: hack/qemu-firewall-test.sh <disk.img> <janusctl-bin>
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

SRC_DISK="${1:?usage: $0 <disk.img> <janusctl-bin>}"
CTL_BIN="${2:?usage: $0 <disk.img> <janusctl-bin>}"
TIMEOUT="${QEMU_FIREWALL_TIMEOUT:-60}"
P_HTTP="${QEMU_FIREWALL_HTTP_PORT:-$((18401 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
P_GRPC="${QEMU_FIREWALL_GRPC_PORT:-$((18402 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
P_METRICS="${QEMU_FIREWALL_METRICS_PORT:-$((18403 + ${JANUS_TEST_PORT_OFFSET:-0}))}"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"

WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT
LOG="$WORKDIR/boot.log"
fail() {
  echo "Firewall test FAILED: $*" >&2
  [ -f "$LOG" ] && { echo "--- console output ---" >&2; cat "$LOG" >&2; }
  exit 1
}

DISK="$WORKDIR/disk.img"
cp "$SRC_DISK" "$DISK"
cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/OVMF_VARS.fd"
qemu-system-x86_64 -accel kvm -accel tcg \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$WORKDIR/OVMF_VARS.fd" \
  -drive file="$DISK",format=raw,if=virtio \
  -nographic -display none -m 512M \
  -netdev "user,id=net0,hostfwd=tcp::${P_HTTP}-:8080,hostfwd=tcp::${P_GRPC}-:9505,hostfwd=tcp::${P_METRICS}-:10056" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG" \
  &
QEMU_PID=$!

http() { curl -s -m 3 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${P_HTTP}/" || true; }
deadline=$((SECONDS + TIMEOUT))
until [ "$(http)" = "200" ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "HAProxy never answered"
  sleep 1
done

STATE_START_SECTOR="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
STATE_SIZE_SECTORS="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
dd if="$DISK" of="$WORKDIR/state.img" bs=512 skip="$STATE_START_SECTOR" count="$STATE_SIZE_SECTORS" status=none
for f in ca.crt admin.crt admin.key; do
  debugfs -R "dump pki/$f $WORKDIR/$f" "$WORKDIR/state.img" >/dev/null 2>&1
  [ -s "$WORKDIR/$f" ] || fail "couldn't extract pki/$f from STATE"
done
ctl() { "$CTL_BIN" -endpoint "127.0.0.1:${P_GRPC}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" "$@"; }

out="$(ctl network firewall status)"
echo "$out" | grep -q "State: *stopped" || fail "status before any ruleset: $out"
echo "$out" | grep -q "Saved: *no" || fail "status before any ruleset: $out"
echo "  ok: the module is there, no ruleset"

# A broken ruleset: refused, with the line it's on.
printf 'table inet filter {\n\tchain input {\n\t\tbogus\n\t}\n}\n' > "$WORKDIR/bad.nft"
if out="$(ctl network firewall check "$WORKDIR/bad.nft" 2>&1)"; then fail "a broken ruleset passed the check"; fi
echo "$out" | grep -q "ruleset:3:" || fail "the error doesn't point at line 3: $out"
echo "  ok: a broken ruleset is refused at its line"

# The real one: HAProxy's 8080 closed, the API and the exporter open.
cat > "$WORKDIR/ruleset.nft" <<'NFT'
table inet filter {
	set blocklist {
		type ipv4_addr
		flags interval, timeout
	}
	chain input {
		type filter hook input priority filter; policy drop;
		ct state established,related accept
		iif "lo" accept
		ip saddr @blocklist drop
		tcp dport { 9505, 10056 } accept
		ip protocol icmp accept
	}
}
NFT
out="$(ctl network firewall check "$WORKDIR/ruleset.nft")" || fail "the ruleset didn't pass the check: $out"
out="$(ctl network firewall apply -timeout 30s "$WORKDIR/ruleset.nft")" || fail "apply: $out"
echo "$out" | grep -q "confirmed over a new connection" || fail "apply wasn't confirmed: $out"
echo "  ok: applied and confirmed over a new connection"
[ "$(http)" = "000" ] || fail "HAProxy's port still answers through the firewall"
ctl version >/dev/null || fail "the API isn't reachable through the firewall"
echo "  ok: it filters - 8080 closed, the API open"

# Sets, live: one kept element, one with a timeout.
ctl network firewall set-add inet filter blocklist 192.0.2.7 198.51.100.0/24 >/dev/null || fail "set-add"
ctl network firewall set-add -timeout 1h inet filter blocklist 203.0.113.9 >/dev/null || fail "set-add -timeout"
ctl network firewall set-del inet filter blocklist 198.51.100.0/24 >/dev/null || fail "set-del"
if ctl network firewall set-add inet filter blocklist '1.2.3.4 } ; flush ruleset' >/dev/null 2>&1; then fail "an element breaking out of the list was accepted"; fi
out="$(ctl network firewall sets)"
echo "$out" | grep -q "^  192.0.2.7  kept$" || fail "kept element: $out"
echo "$out" | grep -q "^  203.0.113.9  expires in " || fail "timed element: $out"
if echo "$out" | grep -q "198.51.100.0/24"; then fail "a deleted element is still there: $out"; fi
echo "  ok: set elements added, kept or timed, deleted; injection refused"

# A ruleset that locks the API out reverts by itself to the confirmed one.
cat > "$WORKDIR/lockout.nft" <<'NFT'
table inet filter {
	chain input {
		type filter hook input priority filter; policy drop;
		iif "lo" accept
	}
}
NFT
ctl network firewall apply -timeout 10s -no-confirm "$WORKDIR/lockout.nft" >/dev/null || fail "applying the lockout ruleset"
if timeout 5 "$CTL_BIN" -endpoint "127.0.0.1:${P_GRPC}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" version >/dev/null 2>&1; then
  fail "the lockout ruleset didn't lock the API out"
fi
deadline=$((SECONDS + 40))
until ctl version >/dev/null 2>&1; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the lockout ruleset never reverted"
  sleep 2
done
grep -aq "firewall: not confirmed in time - reverted" "$LOG" || fail "no revert in the node's log"
out="$(ctl network firewall status)"
echo "$out" | grep -q "set blocklist" || fail "not back to the confirmed ruleset: $out"
echo "$out" | grep -q "192.0.2.7" || fail "the kept element didn't come back with the revert: $out"
if echo "$out" | grep -q "203.0.113.9"; then fail "the timed element survived re-applying the ruleset: $out"; fi
[ "$(http)" = "000" ] || fail "8080 open after the revert"
echo "  ok: a lockout reverts by itself to the confirmed ruleset (kept element back, timed one gone)"
ctl network firewall set-add -timeout 1h inet filter blocklist 203.0.113.9 >/dev/null || fail "set-add -timeout"

# The exporter reports it.
m="$(curl -fsS -m 5 "http://127.0.0.1:${P_METRICS}/metrics")" || fail "no metrics"
echo "$m" | grep -q '^janus_firewall_configured 1$' || fail "janus_firewall_configured"
echo "$m" | grep -q '^janus_firewall_set_elements{family="inet",table="filter",set="blocklist"} 2$' || fail "janus_firewall_set_elements: $(echo "$m" | grep firewall)"
echo "  ok: the exporter reports the firewall"

# After a reboot: the saved ruleset and the kept element, not the timed one.
boots="$(grep -ac "J A N U S" "$LOG" || true)"
ctl system reboot >/dev/null || fail "reboot"
deadline=$((SECONDS + TIMEOUT + 30))
until [ "$(grep -ac "J A N U S" "$LOG" || true)" -gt "$boots" ] && ctl version >/dev/null 2>&1; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the node didn't come back"
  sleep 2
done
grep -aq "firewall: applied the saved ruleset" "$LOG" || fail "the saved ruleset wasn't applied at boot"
out="$(ctl network firewall sets)"
echo "$out" | grep -q "^  192.0.2.7  kept$" || fail "the kept element is gone after the reboot: $out"
if echo "$out" | grep -q "203.0.113.9"; then fail "the timed element came back after the reboot: $out"; fi
[ "$(http)" = "000" ] || fail "8080 open after the reboot"
echo "  ok: after a reboot, the saved ruleset and its kept element"

# Removing the firewall: an empty ruleset.
: > "$WORKDIR/empty.nft"
out="$(ctl network firewall apply "$WORKDIR/empty.nft")" || fail "removing the firewall: $out"
grep -q "confirmed over a new connection" <<<"$out" || fail "removing the firewall: $out"
[ "$(http)" = "200" ] || fail "8080 still closed once the firewall is removed"
out="$(ctl network firewall status)"
grep -q "Saved: *no" <<<"$out" || fail "an empty ruleset still counts as a saved firewall: $out"
echo "  ok: an empty ruleset removes the firewall"

if grep -aq "avc:.*denied" "$LOG"; then
  grep -a "avc:.*denied" "$LOG" >&2
  fail "AVC denials under enforcing"
fi
echo "Firewall test OK: validated, applied on trial, confirmed over a new connection, filtering for real, a lockout reverted by itself, sets edited live and kept across a reboot, zero AVC denials"
