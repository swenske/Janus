#!/usr/bin/env bash
# The Controller creating its own nodes on a hypervisor, end to end and
# for real (docs/hypervisors.md): a libvirt/KVM host in a container
# (hack/libvirt-host: libvirtd, QEMU with OVMF, sshd), the Controller
# inside it too - on the network its machines boot on - and talking to
# libvirt the way it does on a real host, over SSH as a dedicated user.
#
#   1. the hypervisor is added in the API's three steps: its public key
#      authorized, the host key read and checked against the host's own
#      fingerprint, then trusted; the host's state reads back;
#   2. a node is created from the image under test, boots with its
#      NoCloud volume and is admitted on its registration token - no
#      approval - and answers through the Controller's relay;
#   3. the serial console streams its boot, never a private key; a
#      hypervisor reset brings the node back;
#   3b. a node that can't register says why on its console: the machine
#      shows it as a warning (while a page reads the same console), and
#      the node registers on its own once it can - no reboot;
#   4. a record forged to point at a domain the Controller didn't create
#      (one untagged, one tagged by another Controller) is refused, and
#      the domain is left as it was;
#   5. destroying the machine leaves nothing but the cached base image.
#
# The polkit ACL that restricts the Controller's account on a real host
# can't run in a container (no polkitd); it's checked on the lab's host
# (docs/hypervisors.md says how).
#
# Usage: hack/controller-libvirt-test.sh <janus-kvm.qcow2> <dashboardd-bin>
set -euo pipefail

IMAGE_REL="${1:?usage: $0 <janus-kvm.qcow2> <dashboardd-bin>}"
DASHBOARDD_REL="${2:?usage: $0 <janus-kvm.qcow2> <dashboardd-bin>}"
IMAGE="$(cd "$(dirname "$IMAGE_REL")" && pwd)/$(basename "$IMAGE_REL")"
DASHBOARDD="$(cd "$(dirname "$DASHBOARDD_REL")" && pwd)/$(basename "$DASHBOARDD_REL")"
[ -c /dev/kvm ] || { echo "controller-libvirt test needs /dev/kvm" >&2; exit 1; }

HOST_PORT="${CONTROLLER_LIBVIRT_TEST_PORT:-$((18270 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
TEST_NAME=controller-libvirt
# shellcheck source=libvirt-host/lib.sh
. "$(dirname "$0")/libvirt-host/lib.sh"

# =========================================================================
# The host, and the Controller inside it.
# =========================================================================
lh_start
echo "Part 1 OK: libvirt host and Controller running"

# =========================================================================
# 1. Adding the hypervisor: authorize, check the host key, trust.
# =========================================================================
api -X POST "$API/api/hypervisors" -H 'Content-Type: application/json' \
  -d '{"name":"testhost","kind":"libvirt","libvirt":{"host":"127.0.0.1","user":"janus-ctl","pool":"janus","networks":["janus-test"]}}' >"$WORKDIR/hv.json"
HV="$(json "d['id']" <"$WORKDIR/hv.json")"
[ "$(json "d['trusted']" <"$WORKDIR/hv.json")" = False ] || fail "a new hypervisor is trusted before its host key was confirmed"
status="$(api "$API/api/hypervisors/$HV/status")"
echo "$status" | grep -q "host key hasn't been confirmed" || fail "an untrusted hypervisor was connected to: $status"

json "d['authorized_key']" <"$WORKDIR/hv.json" | in_host_i sh -c 'cat >> /home/janus-ctl/.ssh/authorized_keys && chown janus-ctl: /home/janus-ctl/.ssh/authorized_keys && chmod 600 /home/janus-ctl/.ssh/authorized_keys'
probed="$(api -X POST "$API/api/hypervisors/$HV/probe" | json "d['fingerprint']")"
actual="$(in_host ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub | awk '{print $2}')"
[ "$probed" = "$actual" ] || fail "probed host key $probed, the host's own is $actual"
code="$(api -o /dev/null -w '%{http_code}' -X POST "$API/api/hypervisors/$HV/trust" -H 'Content-Type: application/json' -d '{"fingerprint":"SHA256:not-the-host-key"}')"
[ "$code" = 409 ] || fail "trusting a fingerprint the host doesn't present returned $code, want 409"
api -X POST "$API/api/hypervisors/$HV/trust" -H 'Content-Type: application/json' -d "{\"fingerprint\":\"$probed\"}" | json "d['trusted']" | grep -q True || fail "trust didn't pin the host key"
api "$API/api/hypervisors/$HV/status" >"$WORKDIR/status.json"
python3 - "$WORKDIR/status.json" <<'EOF' || fail "host status: $(cat "$WORKDIR/status.json")"
import json, sys
d = json.load(open(sys.argv[1]))
h = d["host"]
assert not d.get("error"), d.get("error")
assert h["cpus"] > 0 and h["memory_total"] > 0, h
assert h["storage"]["name"] == "janus" and h["storage"]["active"], h["storage"]
assert h["networks"] == [{"name": "janus-test", "active": True}], h["networks"]
EOF
echo "Part 2 OK: hypervisor authorized, host key checked against the host's and trusted (a wrong fingerprint refused), host state read over SSH"

# Without /dev/net/tun (a CI runner's unprivileged LXC passes only
# /dev/kvm through), a machine can't be plugged into a network, so it
# can't boot here: what's left to check is that its failed start leaves
# nothing behind, and that destroying it is clean.
failed_creation() {
  SUM="$(sha256sum "$IMAGE" | cut -d' ' -f1)"
  api -X POST "$API/api/machines" -H 'Content-Type: application/json' -d "{
    \"name\": \"node1\", \"hypervisor_id\": \"$HV\", \"vcpus\": 1, \"memory_mib\": 1024,
    \"image\": {\"url\": \"http://127.0.0.1:8000/janus-kvm.qcow2\", \"sha256\": \"$SUM\"},
    \"nics\": [{\"network\": \"janus-test\", \"name\": \"lan\", \"mode\": \"dhcp\"}]}" >"$WORKDIR/m.json"
  MID="$(json "d['id']" <"$WORKDIR/m.json")" || fail "create: $(cat "$WORKDIR/m.json")"
  for _ in $(seq 1 60); do
    phase="$(api "$API/api/machines/$MID" | json "d['phase']")"
    case "$phase" in ready | failed) break ;; esac
    sleep 1
  done
  [ "$phase" = failed ] || fail "expected the start to fail without /dev/net/tun, the machine is $phase"
  api "$API/api/machines/$MID" | json "d['error']" | grep -q "tun" || fail "unexpected failure: $(api "$API/api/machines/$MID")"
  if in_host virsh list --all --name | grep -q '^janus-node1$'; then fail "a failed creation left its virtual machine"; fi
  vols="$(in_host virsh vol-list janus | awk 'NR>2 && $1 {print $1}')"
  [ "$vols" = "janus-base-sha256-${SUM:0:16}.qcow2" ] || fail "a failed creation left volumes: $vols"
  code="$(api -o /dev/null -w '%{http_code}' -X DELETE "$API/api/machines/$MID")"
  [ "$code" = 202 ] || fail "destroying a failed machine returned $code"
  for _ in $(seq 1 30); do
    [ "$(api -o /dev/null -w '%{http_code}' "$API/api/machines/$MID")" = 404 ] && break
    sleep 1
  done
  [ "$(api -o /dev/null -w '%{http_code}' "$API/api/machines/$MID")" = 404 ] || fail "the failed machine was never destroyed: $(api "$API/api/machines/$MID")"
  echo "::warning::controller-libvirt test: no /dev/net/tun on this host, so no machine boots here - the boot, registration-token admission, console and reset are only checked where it exists"
  echo "Part 3 OK (no /dev/net/tun): the machine's failed start left nothing behind (no domain, only the base image) and it was destroyed cleanly"
}

boot_cycle() {
# =========================================================================
# 2. A node, created and admitted on its token.
# =========================================================================
SUM="$(sha256sum "$IMAGE" | cut -d' ' -f1)"
api -X POST "$API/api/machines" -H 'Content-Type: application/json' -d "{
  \"name\": \"node1\", \"hypervisor_id\": \"$HV\", \"vcpus\": 1, \"memory_mib\": 1024,
  \"image\": {\"url\": \"http://127.0.0.1:8000/janus-kvm.qcow2\", \"sha256\": \"$SUM\"},
  \"nics\": [{\"network\": \"janus-test\", \"name\": \"lan\", \"mode\": \"dhcp\"}]}" >"$WORKDIR/m.json"
MID="$(json "d['id']" <"$WORKDIR/m.json")" || fail "create: $(cat "$WORKDIR/m.json")"

# The console from the moment the machine exists: its first boot prints
# the node's admin credential, whose key must never come through.
for _ in $(seq 1 120); do
  [ -n "$(api "$API/api/machines/$MID" | json "d.get('vm_uuid','')")" ] && break
  sleep 0.5
done
timeout 90 curl -skN -b "$JAR" "$API/api/machines/$MID/console" >"$WORKDIR/console1.sse" 2>/dev/null &
CONSOLE_PID=$!

for _ in $(seq 1 120); do
  phase="$(api "$API/api/machines/$MID" | json "d['phase']")"
  case "$phase" in ready | failed) break ;; esac
  sleep 2
done
api "$API/api/machines/$MID" >"$WORKDIR/m.json"
[ "$phase" = ready ] || fail "machine ended $phase: $(cat "$WORKDIR/m.json")"
NODE="$(json "d['node_id']" <"$WORKDIR/m.json")"
[ "$(api "$API/api/pending" | json "len(d or [])")" = 0 ] || fail "the node waited for approval instead of being admitted on its token"
for _ in $(seq 1 30); do
  reachable="$(api "$API/api/nodes/status" | json "d.get('$NODE',{}).get('reachable')")"
  [ "$reachable" = True ] && break
  sleep 1
done
[ "$reachable" = True ] || fail "the admitted node isn't reachable through the Controller: $(api "$API/api/nodes/status")"
host="$(api "$API/api/nodes/status" | json "d['$NODE']['hostname']")"
[ "$host" = node1 ] || fail "the node's hostname is $host, not the one its NoCloud volume set"
echo "Part 3 OK: node1 created, admitted on its registration token (no approval), reachable through the relay as $host"

# =========================================================================
# 3. Console and hypervisor reset.
# =========================================================================
sleep 3
kill "$CONSOLE_PID" 2>/dev/null || true
wait "$CONSOLE_PID" 2>/dev/null || true
python3 - "$WORKDIR/console1.sse" <<'EOF' || fail "first-boot console"
import json, re, sys
out = "".join(json.loads(l[6:]) for l in open(sys.argv[1]) if l.startswith("data: "))
assert "listening on :9505" in out, "no janusd boot in the console: %r" % out[-500:]
assert not re.search(r"BEGIN [A-Z ]*PRIVATE KEY", out), "a private key came through the console"
print("  first boot: %d bytes, %s" % (len(out), "the admin key hidden" if "[private key hidden" in out else "attached after the credential's print"))
EOF
boot_before="$(api "$API/api/nodes/status" | json "d['$NODE']['boot_time_unix']")"
timeout 60 curl -skN -b "$JAR" "$API/api/machines/$MID/console" >"$WORKDIR/console2.sse" 2>/dev/null &
CONSOLE_PID=$!
sleep 2
code="$(api -o /dev/null -w '%{http_code}' -X POST "$API/api/machines/$MID/power" -H 'Content-Type: application/json' -d '{"action":"reset"}')"
[ "$code" = 204 ] || fail "reset returned $code"
for _ in $(seq 1 60); do
  boot_after="$(api "$API/api/nodes/status" | json "d['$NODE'].get('boot_time_unix') if d['$NODE'].get('reachable') else 0")"
  [ "$boot_after" != 0 ] && [ "$boot_after" != "$boot_before" ] && break
  sleep 2
done
[ "$boot_after" != "$boot_before" ] && [ "$boot_after" != 0 ] || fail "the node didn't come back from a reset"
kill "$CONSOLE_PID" 2>/dev/null || true
wait "$CONSOLE_PID" 2>/dev/null || true
python3 - "$WORKDIR/console2.sse" <<'EOF' || fail "console after a reset"
import json, sys
out = "".join(json.loads(l[6:]) for l in open(sys.argv[1]) if l.startswith("data: "))
assert "Linux version" in out and "listening on :9505" in out, out[-500:]
assert "�" not in out, "characters broken across packets"
EOF
echo "Part 4 OK: the console streamed the reboot (no private key, no broken character), the node came back from a hypervisor reset"

# =========================================================================
# 3b. A node that can't register says why - and registers once it can.
# =========================================================================
set_controller_address() {
  api "$API/api/hypervisors/$HV" | python3 -c '
import json, sys
h = json.load(sys.stdin)
print(json.dumps({"name": h["name"], "controller_address": sys.argv[1], "libvirt": h["libvirt"]}))' "$1" >"$WORKDIR/hv.json"
  api -o /dev/null -X PATCH "$API/api/hypervisors/$HV" -H 'Content-Type: application/json' -d @"$WORKDIR/hv.json"
}
# Nothing listens on 18444 yet.
set_controller_address 192.168.123.1:18444
api -X POST "$API/api/machines" -H 'Content-Type: application/json' -d "{
  \"name\": \"node2\", \"hypervisor_id\": \"$HV\", \"vcpus\": 1, \"memory_mib\": 512,
  \"image\": {\"url\": \"http://127.0.0.1:8000/janus-kvm.qcow2\", \"sha256\": \"$SUM\"},
  \"nics\": [{\"network\": \"janus-test\", \"name\": \"lan\", \"mode\": \"dhcp\"}]}" >"$WORKDIR/m2.json"
MID2="$(json "d['id']" <"$WORKDIR/m2.json")" || fail "create node2: $(cat "$WORKDIR/m2.json")"
for _ in $(seq 1 120); do
  [ "$(api "$API/api/machines/$MID2" | json "d['phase']")" = waiting-registration ] && break
  sleep 0.5
done
# A page reads the console the Controller watches.
timeout 120 curl -skN -b "$JAR" "$API/api/machines/$MID2/console" >"$WORKDIR/console3.sse" 2>/dev/null &
CONSOLE_PID=$!
warning=""
for _ in $(seq 1 60); do
  warning="$(api "$API/api/machines/$MID2" | json "d.get('warning','')")"
  [ -n "$warning" ] && break
  sleep 2
done
case "$warning" in
*"connection refused"*"Nothing listens there"*) ;;
*) fail "node2's machine doesn't say why its node can't register: '$warning' $(api "$API/api/machines/$MID2")" ;;
esac
api "$API/api/machines/$MID2" | json "[e['message'] for e in d['events']]" | grep -q "the node says: registration failed" || fail "node2's history doesn't say what the node said"
# Now the port leads to the Controller: the node gets through on its next try.
cat <<'EOF' | in_host_i sh -c 'cat >/work/forward.py'
import socket, threading
def pipe(a, b):
    try:
        while True:
            d = a.recv(65536)
            if not d:
                break
            b.sendall(d)
    except OSError:
        pass
    for x in (a, b):
        try:
            x.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", 18444))
s.listen()
while True:
    c, _ = s.accept()
    u = socket.create_connection(("127.0.0.1", 18443))
    threading.Thread(target=pipe, args=(c, u), daemon=True).start()
    threading.Thread(target=pipe, args=(u, c), daemon=True).start()
EOF
docker exec -d "$NAME" python3 /work/forward.py
for _ in $(seq 1 100); do
  phase="$(api "$API/api/machines/$MID2" | json "d['phase']")"
  [ "$phase" = ready ] && break
  sleep 2
done
[ "$phase" = ready ] || fail "node2 never registered once it could: $(api "$API/api/machines/$MID2")"
[ -z "$(api "$API/api/machines/$MID2" | json "d.get('warning','')")" ] || fail "node2's warning outlived its registration"
# The machine is ready as soon as its node answered the Controller; the
# node's own "admitted by" line reaches the page a moment later (the
# console hub batches 100 ms) - give the stream that moment before
# closing it.
for _ in $(seq 1 30); do
  grep -q "admitted by" "$WORKDIR/console3.sse" 2>/dev/null && break
  sleep 0.5
done
kill "$CONSOLE_PID" 2>/dev/null || true
wait "$CONSOLE_PID" 2>/dev/null || true
python3 - "$WORKDIR/console3.sse" <<'EOF' || fail "the page's console while the Controller watched it"
import json, sys
out = "".join(json.loads(l[6:]) for l in open(sys.argv[1]) if l.startswith("data: "))
assert "selfregister: registration failed" in out and "selfregister: admitted by" in out, out[-800:]
EOF
set_controller_address ""
code="$(api -o /dev/null -w '%{http_code}' -X DELETE "$API/api/machines/$MID2")"
[ "$code" = 202 ] || fail "destroying node2 returned $code"
for _ in $(seq 1 90); do
  [ "$(api -o /dev/null -w '%{http_code}' "$API/api/machines/$MID2")" = 404 ] && break
  sleep 1
done
[ "$(api -o /dev/null -w '%{http_code}' "$API/api/machines/$MID2")" = 404 ] || fail "node2 was never destroyed"
echo "Part 4b OK: node2, unable to register, said why on the machine ($(echo "$warning" | cut -c1-90)...) while a page read the same console, then registered on its own once it could"

}

destroy_cycle() {
# =========================================================================
# 5. Destroy: nothing left but the base image.
# =========================================================================
code="$(api -o /dev/null -w '%{http_code}' -X DELETE "$API/api/machines/$MID")"
[ "$code" = 202 ] || fail "destroy returned $code"
for _ in $(seq 1 90); do
  [ "$(api -o /dev/null -w '%{http_code}' "$API/api/machines/$MID")" = 404 ] && break
  sleep 1
done
[ "$(api -o /dev/null -w '%{http_code}' "$API/api/machines/$MID")" = 404 ] || fail "the machine was never destroyed: $(api "$API/api/machines/$MID")"
[ "$(api "$API/api/nodes" | json "len(d or [])")" = 0 ] || fail "its node is still registered"
if in_host virsh list --all --name | grep -q '^janus-node1$'; then fail "the virtual machine is still defined"; fi
if in_host sh -c 'ls /var/lib/libvirt/qemu/nvram/' | grep -q node1; then fail "its NVRAM is still there"; fi
vols="$(in_host virsh vol-list janus | awk 'NR>2 && $1 {print $1}')"
[ "$vols" = "janus-base-sha256-${SUM:0:16}.qcow2" ] || fail "volumes left in the pool: $vols"
echo "Part 6 OK: destroyed - node, virtual machine, NVRAM and volumes gone, the base image kept"

}

if [ -c /dev/net/tun ]; then
  boot_cycle
else
  failed_creation
fi

# =========================================================================
# 4. Domains the Controller didn't create are refused.
# =========================================================================
in_host sh -c 'cat > /tmp/f1.xml <<XML
<domain type="kvm"><name>foreign-untagged</name><memory unit="MiB">64</memory><vcpu>1</vcpu><os><type arch="x86_64" machine="q35">hvm</type></os></domain>
XML
cat > /tmp/f2.xml <<XML
<domain type="kvm"><name>janus-foreign</name><metadata><janus:machine xmlns:janus="https://janus.sw-servers.net/xmlns/libvirt/machine/1" controller="another-controller" id="aaaaaaaaaaaaaaaa"/></metadata><memory unit="MiB">64</memory><vcpu>1</vcpu><os><type arch="x86_64" machine="q35">hvm</type></os></domain>
XML
virsh -q define /tmp/f1.xml && virsh -q define /tmp/f2.xml'
U1="$(in_host virsh domuuid foreign-untagged | tr -d '[:space:]')"
U2="$(in_host virsh domuuid janus-foreign | tr -d '[:space:]')"
in_host pkill -x dashboardd
sleep 1
for pair in "f00df00df00df001:foreign-untagged:$U1" "aaaaaaaaaaaaaaaa:janus-foreign:$U2"; do
  IFS=: read -r fid fname fuuid <<<"$pair"
  in_host mkdir -p "/work/data/machines/$fid"
  printf '{"id":"%s","spec":{"name":"%s","hypervisor_id":"%s","vcpus":1,"memory_mib":64,"nics":[]},"phase":"ready","ref":{"machine_id":"%s","uuid":"%s","name":"%s","volumes":[]},"events":[],"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}' \
    "$fid" "$fname" "$HV" "$fid" "$fuuid" "$fname" | in_host_i sh -c "cat > /work/data/machines/$fid/meta.json"
done
lh_run_controller
curl -sk -c "$JAR" -o /dev/null -X POST "$API/api/auth/login" -H 'Content-Type: application/json' -d "{\"password\":\"$LH_PASSWORD\"}"
for fid in f00df00df00df001 aaaaaaaaaaaaaaaa; do
  code="$(api -o /dev/null -w '%{http_code}' -X POST "$API/api/machines/$fid/power" -H 'Content-Type: application/json' -d '{"action":"start"}')"
  [ "$code" = 403 ] || fail "starting a foreign domain through a forged record returned $code, want 403"
  api -o /dev/null -X DELETE "$API/api/machines/$fid"
  for _ in $(seq 1 20); do
    phase="$(api "$API/api/machines/$fid" | json "d['phase']")"
    [ "$phase" = failed ] && break
    sleep 0.5
  done
  api "$API/api/machines/$fid" | json "d['error']" | grep -q "wasn't created by this Controller" || fail "destroying a foreign domain wasn't refused: $(api "$API/api/machines/$fid")"
  api -o /dev/null -X DELETE "$API/api/machines/$fid?forget=true"
done
for d in foreign-untagged janus-foreign; do
  [ "$(in_host virsh domstate "$d" | tr -d '[:space:]')" = "shutoff" ] || fail "$d was touched"
done
echo "Part 5 OK: an untagged domain and another Controller's were refused (start 403, destroy failed) and left as they were"

if [ -c /dev/net/tun ]; then
  destroy_cycle
fi

echo "controller-libvirt test OK"
