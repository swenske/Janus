#!/usr/bin/env bash
# A node replaced by another, its state carried over (SystemService.
# StateExport/StateImport, docs/guide/replacing-a-node.md) - the case of a
# node installed before the LOGS partition existed, replaced by one that
# has it.
#
# Node A (the old one) boots a disk whose LOGS partition was removed; it
# gets a hostname, a static address on a shared LAN, an HAProxy
# configuration binding that address, a map, an uploaded certificate, a
# kernel parameter. Node B (the new one) boots a fresh disk. A's state,
# with its identity, is piped from A into B (janusctl ... export - |
# janusctl ... import -); B restarts into it. Checked: an unused node
# only takes an import; B is A to its clients (A's credentials work on B,
# B's own no longer; the console names A's CA); the configuration, the
# map, the kernel parameter came over; B keeps its LOGS; B's network is
# still its own until the takeover - A shut down, A's network
# configuration applied on B, HAProxy then binds A's address and serves
# A's certificate - and no AVC denial on either.
#
# Node to node (StateImportFrom): node C, another fresh disk, pulls A's
# state from A itself - janusctl only carries C's signing request to A
# and A's certificate back. C reaches A through QEMU's gateway, at an
# address A's server certificate doesn't name: C checks A by its CA.
# Checked: a reader of A can't grant, B's credentials don't reach A, a
# node in use takes no pull; C restarts into A's state with A's
# identity, A's console logged the grant.
#
# Usage: hack/qemu-state-migrate-test.sh <disk.img> <janusctl-bin>
set -euo pipefail
export PATH="$PATH:/usr/sbin:/sbin"

SRC_DISK="${1:?usage: $0 <disk.img> <janusctl-bin>}"
CTL_BIN="${2:?usage: $0 <disk.img> <janusctl-bin>}"
TIMEOUT="${QEMU_STATE_TIMEOUT:-150}"
BASE="${QEMU_STATE_BASE_PORT:-$((18990 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
MCAST="${QEMU_STATE_MCAST:-230.0.0.$((RANDOM % 200 + 20)):$((20000 + RANDOM % 20000))}"
OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"

WORKDIR="$(mktemp -d)"
declare -A PID
cleanup() {
  for n in "${!PID[@]}"; do kill "${PID[$n]}" 2>/dev/null || true; done
  if [ "${QEMU_STATE_KEEP:-}" = 1 ]; then echo "kept $WORKDIR" >&2; else rm -rf "$WORKDIR"; fi
}
trap cleanup EXIT

fail() {
  echo "state migration test FAILED: $*" >&2
  for n in a b c; do
    [ -f "$WORKDIR/$n.log" ] && { echo "--- console $n ---" >&2; tail -80 "$WORKDIR/$n.log" >&2; }
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

check() { # check DESCRIPTION PATTERN TEXT
  grep -Eq -- "$2" <<<"$3" || fail "$1 (no match for $2 in: $3)"
}

idx() { case $1 in a) echo 1 ;; b) echo 2 ;; c) echo 3 ;; esac; }

# node NAME: boot it - http on BASE+10*i, API on +1.
node() {
  local n=$1 i
  i=$(idx "$n")
  cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/$n.vars"
  qemu-system-x86_64 -accel kvm -accel tcg \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$WORKDIR/$n.vars" \
    -drive file="$WORKDIR/$n.img",format=raw,if=virtio \
    -nographic -display none -monitor none -m 512M \
    -netdev "user,id=net0,hostfwd=tcp::$((BASE + 10 * i))-:8080,hostfwd=tcp::$((BASE + 10 * i + 1))-:9505" \
    -device "virtio-net-pci,netdev=net0,mac=52:54:00:aa:00:0$i" \
    -netdev "socket,id=lan,mcast=$MCAST,localaddr=127.0.0.1" \
    -device "virtio-net-pci,netdev=lan,mac=52:54:00:bb:00:0$i" \
    -serial file:"$WORKDIR/$n.log" </dev/null &
  PID[$n]=$!
}

# A: the disk without its LOGS partition - a node installed before it existed.
cp --sparse=always "$SRC_DISK" "$WORKDIR/a.img"
sgdisk -d 7 "$WORKDIR/a.img" >/dev/null
cp --sparse=always "$SRC_DISK" "$WORKDIR/b.img"
cp --sparse=always "$SRC_DISK" "$WORKDIR/c.img"
node a
node b
node c

listening() { [ "$(grep -ac "listening on .* (mTLS required)" "$WORKDIR/$1.log" 2>/dev/null)" -ge "$2" ]; }
for n in a b c; do wait_for "$TIMEOUT" "node $n's janusd listening" listening "$n" 1; done
sleep 2
python3 - "$WORKDIR" <<'PY'
import re, sys
w = sys.argv[1]
for n in "abc":
    log = open(f"{w}/{n}.log", errors="replace").read().replace("\r", "")
    pems = re.findall(r"(-----BEGIN ([A-Z ]+)-----.*?-----END \2-----)", log, re.S)
    for (pem, _), name in zip(pems, ["ca.crt", "admin.crt", "admin.key"]):
        open(f"{w}/{n}-{name}", "w").write(pem + "\n")
PY
ctl() { # ctl ENDPOINT-NODE CREDENTIALS-NODE args...
  local i=$(idx "$1") c=$2
  shift 2
  "$CTL_BIN" -endpoint "127.0.0.1:$((BASE + 10 * i + 1))" -ca "$WORKDIR/$c-ca.crt" -cert "$WORKDIR/$c-admin.crt" -key "$WORKDIR/$c-admin.key" "$@"
}
for n in a b c; do ctl "$n" "$n" version >/dev/null || fail "no API on node $n"; done
out="$(ctl a a system boots 2>&1 || true)"
check "node A has no LOGS partition" "no LOGS partition" "$out"
echo "  ok: three nodes up, A without LOGS"

# A's setup: a hostname and an address on the LAN, name-matched (the
# replacement's NICs have other MACs), an HAProxy configuration binding
# that address, a map, a certificate, a kernel parameter.
cat > "$WORKDIR/a-net.json" <<'EOF'
{"hostname": "old-lb", "interfaces": [
  {"name": "eth0", "mode": "ADDRESSING_MODE_DHCP"},
  {"name": "eth1", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.168.83.1/24"]}
]}
EOF
out="$(ctl a a network apply "$WORKDIR/a-net.json" 2>&1)" || fail "network apply on A: $out"
cat > "$WORKDIR/haproxy.cfg" <<'EOF'
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

frontend fe
    bind :8080
    http-request return status 200 content-type text/plain lf-string "served\n"

frontend public
    bind 192.168.83.1:8081
    http-request return status 200 content-type text/plain lf-string "public\n"
EOF
out="$(ctl a a haproxy apply-config "$WORKDIR/haproxy.cfg" 2>&1)" || fail "apply-config on A: $out"
printf 'example.test be_example\n' > "$WORKDIR/hosts.map"
out="$(ctl a a haproxy file-put maps/hosts.map "$WORKDIR/hosts.map" 2>&1)" || fail "file-put on A: $out"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -subj "/CN=www.example" -days 2 -keyout "$WORKDIR/www.key" -out "$WORKDIR/www.crt" 2>/dev/null
cat "$WORKDIR/www.crt" "$WORKDIR/www.key" > "$WORKDIR/www.pem"
out="$(ctl a a haproxy cert-upload www "$WORKDIR/www.pem" 2>&1)" || fail "cert-upload on A: $out"
out="$(ctl a a system sysctl set net.core.somaxconn=8192 2>&1)" || fail "sysctl on A: $out"
check "A's kernel parameter" "8192" "$(ctl a a system sysctl get net.core.somaxconn)"
cfg_sum() { ctl "$1" "$2" haproxy get-config | sha256sum | cut -d' ' -f1; }
A_CFG="$(cfg_sum a a)"
A_CA="$(grep -a "pki: this node's CA: SHA-256" "$WORKDIR/a.log" | tail -1 | sed 's/.*SHA-256 //')"
[ -n "$A_CA" ] || fail "no CA line on A's console"
echo "  ok: A set up (old-lb, 192.168.83.1, a configuration, a map, a certificate, a kernel parameter)"

# An export to a file: a tar, the manifest first, A's keys in it.
ctl a a system state export -identity "$WORKDIR/a-state.tar" 2>/dev/null || fail "export to a file"
[ "$(stat -c %a "$WORKDIR/a-state.tar")" = 600 ] || fail "the export isn't 0600"
python3 - "$WORKDIR/a-state.tar" <<'PY' || fail "the export's layout"
import sys, tarfile, json
t = tarfile.open(sys.argv[1])
names = t.getnames()
assert names[0] == "MANIFEST.json", names[:3]
m = json.load(t.extractfile("MANIFEST.json"))
assert m["identity"] and m["hostname"] == "old-lb", m
for want in ("pki/ca.key", "haproxy/haproxy.cfg", "haproxy/files/maps/hosts.map", "network/config.json", "config/sysctl.d/90-haproxy-tuning.conf"):
    assert want in names, want
for never in ("controller/token", "boot/pending.json", "haproxy/cluster/member.pem"):
    assert never not in names, never
assert not any(n.startswith("events/") for n in names)
PY
# Never over a file already there.
if ctl a a system state export "$WORKDIR/a-state.tar" 2>/dev/null; then fail "an export overwrote a file"; fi
# A node in use takes no import.
out="$(ctl a a system state import "$WORKDIR/a-state.tar" 2>&1 || true)"
check "a node in use refuses an import" "in use" "$out"
echo "  ok: an export to a file, refused over a file and onto a node in use"

# The replacement: A's state piped into B, A's identity with it.
out="$(ctl a a system state export -identity - | ctl b b system state import -identity -save-network "$WORKDIR/held-net.json" - 2>&1)" || fail "export | import: $out"
check "the import names A" "Imported: +old-lb's state" "$out"
check "the identity reused" "Identity: +old-lb's" "$out"
check "the network held and saved" "saved to $WORKDIR/held-net.json" "$out"
check "the node restarts into it" "Rebooting:" "$out"
check "the held network is A's" '"old-lb"' "$(cat "$WORKDIR/held-net.json")"
wait_for "$TIMEOUT" "node B restarted" listening b 2
sleep 2
check "B put the state in place" "state: old-lb's state put in place" "$(cat "$WORKDIR/b.log")"
echo "  ok: B restarted into A's state"

# B is A to its clients.
ctl b a version >/dev/null || fail "A's credentials don't work on B"
if ctl b b version >/dev/null 2>&1; then fail "B's own credentials still work"; fi
B_CA="$(grep -a "pki: this node's CA: SHA-256" "$WORKDIR/b.log" | tail -1 | sed 's/.*SHA-256 //')"
[ "$B_CA" = "$A_CA" ] || fail "B's CA ($B_CA) isn't A's ($A_CA)"
echo "  ok: B has A's identity"

# The configuration came over; B keeps its LOGS; its network is its own.
[ "$(cfg_sum b a)" = "$A_CFG" ] || fail "B's haproxy.cfg isn't A's"
check "the map came over" "maps/hosts.map" "$(ctl b a haproxy files)"
check "the kernel parameter came over" "8192" "$(ctl b a system sysctl get net.core.somaxconn)"
check "B keeps its LOGS" "LOGS partition /dev/vda7" "$(ctl b a system boots 2>&1)"
if grep -q "old-lb" <<<"$(ctl b a system hostname)"; then fail "B took A's hostname before the takeover"; fi
echo "  ok: configuration, map, kernel parameter on B; its LOGS kept; its network its own"

# Node to node: C pulls A's state from A itself. janusctl reaches A on
# its forwarded port; C reaches A through QEMU's gateway (10.0.2.2), an
# address A's certificate doesn't name.
A_API="127.0.0.1:$((BASE + 11))" A_FROM_C="10.0.2.2:$((BASE + 11))"
pull() { # pull CREDENTIALS-NODE-FOR-C FROM-CA FROM-CERT FROM-KEY args...
  local c=$1 ca=$2 crt=$3 key=$4
  shift 4
  ctl c "$c" system state import -from "$A_API" -from-ca "$ca" -from-cert "$crt" -from-key "$key" -from-address "$A_FROM_C" "$@"
}
ctl a a pki generate-client-config -role os:reader -name a-reader "$WORKDIR/a-reader" >/dev/null || fail "a reader certificate on A"
out="$(pull c "$WORKDIR/a-ca.crt" "$WORKDIR/a-reader/client.crt" "$WORKDIR/a-reader/client.key" -identity 2>&1 || true)"
check "a reader of A can't grant its state" "StateExportGrant.*requires role" "$out"
out="$(pull c "$WORKDIR/a-ca.crt" "$WORKDIR/b-admin.crt" "$WORKDIR/b-admin.key" -identity 2>&1 || true)"
check "B's credentials don't reach A" "StateExportGrant|certificate" "$out"
if grep -q "Imported:" <<<"$out"; then fail "a pull with B's credentials went through: $out"; fi
out="$(ctl b a system state import -from "$A_API" -from-ca "$WORKDIR/a-ca.crt" -from-cert "$WORKDIR/a-admin.crt" -from-key "$WORKDIR/a-admin.key" -from-address "$A_FROM_C" 2>&1 || true)"
check "a node in use takes no pull" "in use" "$out"
echo "  ok: refused - a reader of A, B's credentials on A, B in use"

out="$(pull c "$WORKDIR/a-ca.crt" "$WORKDIR/a-admin.crt" "$WORKDIR/a-admin.key" -identity -save-network "$WORKDIR/c-held-net.json" 2>&1)" || fail "C pulling from A: $out"
check "C names A" "Imported: +old-lb's state" "$out"
check "C took A's identity" "Identity: +old-lb's" "$out"
check "C restarts into it" "Rebooting:" "$out"
check "A logged the grant" "state: .* may pull this node's state once, until .* \(identity true\)" "$(cat "$WORKDIR/a.log")"
check "A's export went to C's transfer certificate" "state: exported .* - by .*state transfer" "$(cat "$WORKDIR/a.log")"
wait_for "$TIMEOUT" "node C restarted" listening c 2
sleep 2
check "C staged what it pulled" "state: old-lb's state staged .*pulled from $A_FROM_C" "$(cat "$WORKDIR/c.log")"
check "C put the state in place" "state: old-lb's state put in place" "$(cat "$WORKDIR/c.log")"
ctl c a version >/dev/null || fail "A's credentials don't work on C"
if ctl c c version >/dev/null 2>&1; then fail "C's own credentials still work"; fi
[ "$(cfg_sum c a)" = "$A_CFG" ] || fail "C's haproxy.cfg isn't A's"
check "the map came over to C" "maps/hosts.map" "$(ctl c a haproxy files)"
check "the kernel parameter came over to C" "8192" "$(ctl c a system sysctl get net.core.somaxconn)"
check "C's held network is A's" '"old-lb"' "$(cat "$WORKDIR/c-held-net.json")"
ctl c a system shutdown >/dev/null || fail "shutdown on C"
echo "  ok: C pulled A's state from A itself - identity, configuration, map, kernel parameter"

# The takeover: A down, A's network on B, HAProxy on A's address.
ctl a a system shutdown >/dev/null || fail "shutdown on A"
deadline=$((SECONDS + 60))
while kill -0 "${PID[a]}" 2>/dev/null; do
  [ "$SECONDS" -lt "$deadline" ] || fail "A didn't power off"
  sleep 1
done
unset 'PID[a]'
out="$(ctl b a network apply "$WORKDIR/held-net.json" 2>&1)" || fail "network apply on B: $out"
check "B named old-lb" "old-lb" "$(ctl b a system hostname)"
ctl b a haproxy reload >/dev/null 2>&1 || true
listens() { grep -Eq '192\.168\.83\.1:8081 +[^ ]+ +LISTEN' <<<"$(ctl b a system netstat 2>/dev/null)"; }
wait_for 40 "HAProxy on B listening on A's address" listens
check "A's certificate served by B" "www" "$(ctl b a haproxy cert-list)"
check "B serves" "served" "$(curl -s --max-time 5 "http://127.0.0.1:$((BASE + 20))/")"
echo "  ok: B took A's address over and serves A's configuration and certificate"

for n in a b c; do
  if grep -a "avc:.*denied" "$WORKDIR/$n.log"; then fail "AVC denials on node $n"; fi
done
echo "state migration test PASSED: A (no LOGS) replaced by B (LOGS) - identity, configuration, certificate, kernel parameter carried, address taken over; C pulled A's state node to node"
