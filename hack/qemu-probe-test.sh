#!/usr/bin/env bash
# Proves SystemService.Probe on a real node: the full disk image under
# real OVMF with SELinux enforcing, `janusctl system probe` over mTLS
# against servers on this host (QEMU's gateway, 10.0.2.2) - an HTTP
# server, a TLS server with a certificate of a CA made here, a DNS
# responder -, ICMP echo requests and a traceroute from the node's raw
# socket, a refused connection, a reader refused; then the neighbour table
# (NetworkService.NeighborList) - QEMU's gateway with slirp's MAC, an
# interface the node lacks, a reader allowed; and no AVC denial.
#
# Usage: hack/qemu-probe-test.sh <disk.img> <janusctl-bin>
set -euo pipefail
# shellcheck source=state-read.sh
. "$(dirname "$0")/state-read.sh"

export PATH="$PATH:/usr/sbin:/sbin"

DISK="${1:?usage: $0 <disk.img> <janusctl-bin>}"
CTL_BIN="${2:?usage: $0 <disk.img> <janusctl-bin>}"
HTTP_TIMEOUT_SECS="${QEMU_PROBE_HTTP_TIMEOUT:-40}"
OFFSET="${JANUS_TEST_PORT_OFFSET:-0}"
HOST_PORT_8080="$((18410 + OFFSET))"
HOST_GRPC_PORT="$((18411 + OFFSET))"
P_HTTP="$((18412 + OFFSET))" # this host's servers, 10.0.2.2 from the node
P_TLS="$((18413 + OFFSET))"
P_DNS="$((18414 + OFFSET))"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf)" >&2; exit 1; }

WORKDIR="$(mktemp -d)"
QEMU_PID=""
SERVERS_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  [ -n "$SERVERS_PID" ] && kill "$SERVERS_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

LOG="$WORKDIR/boot.log"
fail() {
  echo "Probe test FAILED: $*" >&2
  [ -f "$LOG" ] && { echo "--- console output ---" >&2; cat "$LOG" >&2; }
  exit 1
}

# A CA and a certificate it signed for probe.test.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=probe test CA" \
  -keyout "$WORKDIR/ca.key" -out "$WORKDIR/probe-ca.pem" 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=probe.test" \
  -keyout "$WORKDIR/tls.key" -out "$WORKDIR/tls.csr" 2>/dev/null
printf 'subjectAltName=DNS:probe.test\n' > "$WORKDIR/san.ext"
openssl x509 -req -in "$WORKDIR/tls.csr" -CA "$WORKDIR/probe-ca.pem" -CAkey "$WORKDIR/ca.key" -CAcreateserial \
  -days 1 -extfile "$WORKDIR/san.ext" -out "$WORKDIR/tls.crt" 2>/dev/null

# The servers the node probes: HTTP, TLS (the same handler), and DNS
# answering probe.test's A record.
python3 - "$P_HTTP" "$P_TLS" "$P_DNS" "$WORKDIR/tls.crt" "$WORKDIR/tls.key" <<'EOF' &
import http.server, socket, socketserver, ssl, struct, sys, threading
p_http, p_tls, p_dns, crt, key = int(sys.argv[1]), int(sys.argv[2]), int(sys.argv[3]), sys.argv[4], sys.argv[5]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = b"probe ok\n"
        self.send_response(200)
        self.send_header("X-Probe-Host", self.headers.get("Host", ""))
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a): pass
class S(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
    allow_reuse_address = True
plain = S(("0.0.0.0", p_http), H)
tls = S(("0.0.0.0", p_tls), H)
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(crt, key)
tls.socket = ctx.wrap_socket(tls.socket, server_side=True)
for s in (plain, tls):
    threading.Thread(target=s.serve_forever, daemon=True).start()
dns = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
dns.bind(("0.0.0.0", p_dns))
while True:
    q, addr = dns.recvfrom(512)
    qid, = struct.unpack(">H", q[:2])
    end = 12
    while q[end] != 0:
        end += q[end] + 1
    question = q[12:end + 5]
    qtype, = struct.unpack(">H", q[end + 1:end + 3])
    labels, i = [], 12
    while q[i]:
        labels.append(q[i + 1:i + 1 + q[i]].decode())
        i += q[i] + 1
    found = ".".join(labels).lower() == "probe.test"
    answers = b""
    if found and qtype == 1:
        answers = b"\xc0\x0c" + struct.pack(">HHIH", 1, 1, 60, 4) + bytes([10, 0, 2, 2])
    flags = 0x8180 if found else 0x8183
    dns.sendto(struct.pack(">HHHHHH", qid, flags, 1, 1 if answers else 0, 0, 0) + question + answers, addr)
EOF
SERVERS_PID=$!

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

deadline=$((SECONDS + HTTP_TIMEOUT_SECS))
until [ "$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_PORT_8080}/" || true)" = "200" ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "HAProxy never answered HTTP 200 within ${HTTP_TIMEOUT_SECS}s"
  sleep 1
done
until grep -qa "listening on" "$LOG"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "janusd never listened"
  sleep 1
done

state_pki "$DISK" "$WORKDIR" || fail "couldn't extract the node's pki/ from STATE"
ctl() { "$CTL_BIN" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" "$@"; }
probe() { # <name> <grep pattern> <probe args...>: the probe succeeds and prints the pattern
  local name="$1" want="$2"
  shift 2
  out="$(ctl system probe "$@" 2>&1)" || fail "$name: the probe failed: $out"
  grep -qE "$want" <<<"$out" || fail "$name: no '$want' in: $out"
  echo "$name OK"
}

probe "HTTP" '^http +ok .*HTTP/1\.[01] 200 OK' http "http://10.0.2.2:${P_HTTP}/"
probe "HTTP with a Host header" 'X-Probe-Host: virtual\.test' http -v -H 'Host: virtual.test' "http://10.0.2.2:${P_HTTP}/"
probe "HTTPS through connect-to, the CA given" '^http +ok .*200 OK' http -connect-to 10.0.2.2 -ca-file "$WORKDIR/probe-ca.pem" "https://probe.test:${P_TLS}/"
probe "TLS verified" 'certificate verified' tls -connect-to 10.0.2.2 -ca-file "$WORKDIR/probe-ca.pem" "probe.test:${P_TLS}"
probe "TLS untrusted, shown" 'certificate NOT trusted' tls "10.0.2.2:${P_TLS}"
probe "DNS through a given server" 'probe\.test A via 10\.0\.2\.2:[0-9]+: 10\.0\.2\.2' dns -server "10.0.2.2:${P_DNS}" -type A probe.test
probe "TCP" '^connect +ok' tcp -timeout 5s "10.0.2.2:${P_HTTP}"
probe "ping the loopback" '2 sent, 2 received' ping -count 2 -interval 200ms 127.0.0.1
probe "ping its own address" '1 sent, 1 received' ping -count 1 10.0.2.15
probe "traceroute" 'reached in 1 hop' traceroute 127.0.0.1
probe "the source address" 'from 10\.0\.2\.15:' tcp -source 10.0.2.15 "10.0.2.2:${P_HTTP}"
probe "DNS from the source address" 'probe\.test A via 10\.0\.2\.2:[0-9]+: 10\.0\.2\.2' dns -source 10.0.2.15 -server "10.0.2.2:${P_DNS}" -type A probe.test

if out="$(ctl system probe tcp -timeout 2s 10.0.2.2:1 2>&1)"; then
  fail "a refused connection passed: $out"
fi
grep -qE '^connect +FAIL' <<<"$out" || fail "a refused connection isn't reported: $out"
echo "Refused connection reported OK"
if out="$(ctl system probe ping -source 192.0.2.99 10.0.2.2 2>&1)"; then
  fail "a source the node doesn't have was accepted"
fi
grep -q InvalidArgument <<<"$out" || fail "a foreign source: $out"
echo "Foreign source refused OK"

ctl pki generate-client-config -role os:reader -name probe-reader -ttl 1h "$WORKDIR/reader" > /dev/null || fail "generate-client-config failed"
if out="$("$CTL_BIN" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/reader/ca.crt" -cert "$WORKDIR/reader/client.crt" -key "$WORKDIR/reader/client.key" system probe ping 127.0.0.1 2>&1)"; then
  fail "a reader probed"
fi
grep -q PermissionDenied <<<"$out" || fail "a reader's probe: $out"
echo "Reader refused OK"

# The neighbour table, right after a connection to QEMU's gateway: slirp
# answers ARP for 10.0.2.2 with 52:55 and the address's own bytes, and
# the connection's ACKs keep the entry reachable.
probe "TCP before the neighbour table" '^connect +ok' tcp -timeout 5s "10.0.2.2:${P_HTTP}"
out="$(ctl network neighbors 2>&1)" || fail "network neighbors failed: $out"
grep -qE '^[a-z0-9.]+ +10\.0\.2\.2 +52:55:0a:00:02:02 +(reachable|delay) ' <<<"$out" || fail "no reachable entry for the gateway: $out"
iface="$(awk '$2 == "10.0.2.2" {print $1}' <<<"$out")"
out="$(ctl network neighbors -interface "$iface" 2>&1)" || fail "network neighbors -interface $iface failed: $out"
grep -q '10\.0\.2\.2' <<<"$out" || fail "$iface's neighbours lack the gateway: $out"
if out="$(ctl network neighbors -interface eth9 2>&1)"; then
  fail "an interface the node lacks was listed: $out"
fi
grep -q NotFound <<<"$out" || fail "an unknown interface: $out"
out="$("$CTL_BIN" -endpoint "127.0.0.1:${HOST_GRPC_PORT}" -ca "$WORKDIR/reader/ca.crt" -cert "$WORKDIR/reader/client.crt" -key "$WORKDIR/reader/client.key" network neighbors 2>&1)" || fail "a reader couldn't read the neighbour table: $out"
grep -q '10\.0\.2\.2' <<<"$out" || fail "a reader's neighbour table: $out"
echo "Neighbour table OK: the gateway reachable with slirp's MAC on $iface, eth9 NotFound, a reader allowed"

if grep -q "avc:.*denied" "$LOG"; then
  grep "avc:.*denied" "$LOG" >&2
  fail "SELinux denials during the probes"
fi
echo "Probe test OK: HTTP, TLS, DNS, TCP, ping and traceroute from an enforcing node over mTLS, a reader refused, the neighbour table, zero AVC denials"
