#!/usr/bin/env bash
# Proves SystemService.PacketCapture on a real node: the full disk image
# under real OVMF with SELinux enforcing (baked into the UKI), a real
# `janusctl system pcap` over mTLS while HTTP requests hit HAProxy, and
# the resulting pcap parsed independently - every packet must match the
# filter, the HTTP exchange must be in it, and no AVC denial may appear.
# Also checks janusd's console banner (cmd/janusd/motd.go).
#
# Usage: hack/qemu-packet-capture-test.sh <disk.img> <janusctl-bin>
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

DISK="${1:?usage: $0 <disk.img> <janusctl-bin>}"
CTL_BIN="${2:?usage: $0 <disk.img> <janusctl-bin>}"
HTTP_TIMEOUT_SECS="${QEMU_PCAP_HTTP_TIMEOUT:-40}"
HOST_PORT_8080="${QEMU_PCAP_TEST_PORT:-18198}"
HOST_GRPC_PORT="${QEMU_PCAP_GRPC_PORT:-18199}"

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
  echo "Packet capture test FAILED: $*" >&2
  [ -f "$LOG" ] && { echo "--- console output ---" >&2; cat "$LOG" >&2; }
  exit 1
}

cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/OVMF_VARS.fd"
qemu-system-x86_64 \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$WORKDIR/OVMF_VARS.fd" \
  -drive file="$DISK",format=raw,if=virtio \
  -nographic -display none -m 512M \
  -netdev "user,id=net0,hostfwd=tcp::${HOST_PORT_8080}-:8080,hostfwd=tcp::${HOST_GRPC_PORT}-:9505" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG" \
  &
QEMU_PID=$!

deadline=$((SECONDS + HTTP_TIMEOUT_SECS))
until [ "$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_PORT_8080}/" || true)" = "200" ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "HAProxy never answered HTTP 200 within ${HTTP_TIMEOUT_SECS}s"
  sleep 1
done
until grep -qa "J A N U S" "$LOG"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "janusd's console banner never appeared"
  sleep 1
done
grep -qa "api       10.0.2.15:9505" "$LOG" || fail "the console banner doesn't show the node's API address"
grep -qa "boot slot A" "$LOG" || fail "the console banner doesn't show the active boot slot"
echo "Console banner OK"

# PKI from STATE (see hack/qemu-lifecycle-rollback-test.sh)
STATE_START_SECTOR="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
STATE_SIZE_SECTORS="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
dd if="$DISK" of="$WORKDIR/state.img" bs=512 skip="$STATE_START_SECTOR" count="$STATE_SIZE_SECTORS" status=none
for f in ca.crt admin.crt admin.key; do
  debugfs -R "dump pki/$f $WORKDIR/$f" "$WORKDIR/state.img" >/dev/null 2>&1
  [ -s "$WORKDIR/$f" ] || fail "couldn't extract pki/$f from STATE"
done
ctl() { "$CTL_BIN" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" "$@"; }

# An unsupported filter must be refused, not approximated.
if out="$(ctl system pcap -i eth0 -f 'tcp[13] & 2 != 0' -duration 1s -o /dev/null 2>&1)"; then
  fail "an unsupported filter expression was accepted"
fi
echo "$out" | grep -q "InvalidArgument" || fail "unsupported filter didn't return InvalidArgument: $out"

# The real capture: 3 HTTP requests while capturing for 5s.
( sleep 1.5; for _ in 1 2 3; do curl -s -o /dev/null "http://127.0.0.1:${HOST_PORT_8080}/"; sleep 0.3; done ) &
CURL_PID=$!
ctl system pcap -i eth0 -f 'tcp port 8080' -duration 5s -o "$WORKDIR/cap.pcap" || fail "janusctl system pcap failed"
wait "$CURL_PID"

python3 - "$WORKDIR/cap.pcap" <<'EOF' || fail "the captured pcap doesn't hold what it should"
import struct, sys
data = open(sys.argv[1], "rb").read()
magic, major, minor, _, _, snap, link = struct.unpack("<IHHiIII", data[:24])
assert (magic, major, minor, link) == (0xa1b2c3d4, 2, 4, 1), "bad pcap header"
off, n = 24, 0
conns = {}  # client port -> [packets, saw SYN, saw HTTP 200]
while off < len(data):
    _, _, incl, orig = struct.unpack("<IIII", data[off:off + 16])
    pkt = data[off + 16:off + 16 + incl]
    off += 16 + incl
    n += 1
    assert struct.unpack(">H", pkt[12:14])[0] == 0x0800, f"packet {n}: not IPv4"
    ihl = (pkt[14] & 0x0f) * 4
    assert pkt[23] == 6, f"packet {n}: not TCP"
    l4 = 14 + ihl
    sport, dport = struct.unpack(">HH", pkt[l4:l4 + 4])
    assert 8080 in (sport, dport), f"packet {n}: ports {sport}->{dport} don't match 'tcp port 8080'"
    c = conns.setdefault(dport if sport == 8080 else sport, [0, False, False])
    c[0] += 1
    if dport == 8080 and pkt[l4 + 13] & 0x02:
        c[1] = True
    if sport == 8080 and b"HTTP/1.1 200" in pkt:
        c[2] = True
assert off == len(data), "trailing garbage after the last record"
for port, (count, syn, ok) in sorted(conns.items()):
    print(f"  connection from :{port}: {count} packets, SYN={syn}, HTTP 200={ok}")
# Counted per connection, and "at least": a slow runner's QEMU usermode
# networking can retransmit or add connections of its own - what matters
# is that the filter held (checked per packet above) and our 3 requests
# were captured whole.
complete = [p for p, (_, syn, ok) in conns.items() if syn and ok]
assert len(complete) >= 3, f"expected 3 complete HTTP exchanges, saw {len(complete)}"
print(f"pcap OK: {n} packets, all tcp port 8080, {len(complete)} complete HTTP exchanges")
EOF

if grep -q "avc:.*denied" "$LOG"; then
  grep "avc:.*denied" "$LOG" >&2
  fail "SELinux denials during the capture"
fi
echo "Packet capture test OK: real capture over mTLS on an enforcing node, filter applied in the kernel, zero AVC denials"
