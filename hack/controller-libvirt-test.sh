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
#      approval - and answers through the Controller's relay; it inherits
#      the hypervisor's labels under its own (its own winning on a key),
#      a token scoped to them reaches it, a change to the hypervisor's
#      follows at once and one that leaves them out keeps them;
#   3. the serial console streams its boot, never a private key; a
#      hypervisor reset brings the node back;
#   3b. a node that can't register says why on its console: the machine
#      shows it as a warning (while a page reads the same console), and
#      the node registers on its own once it can - no reboot;
#   3b2. with GUEST_AGENT_IMAGE (a janus-kvm.qcow2 built with the
#      qemu-guest-agent extension): a machine made from it gets the
#      guest agent's channel, the extension's service runs (not
#      waiting) and answers libvirt (guest-ping); its record knows the
#      extension from its node and reports no hardware drift - nothing
#      for apply-settings to do - (#373);
#   3c. auto-scaling, observing: a policy restarts the machine once with
#      room to grow (vCPUs to hot-plug, a virtio-mem device), the node's
#      real load is read and the step it would take is journaled and
#      simulated - nothing resized;
#   3d. resizing for real: memory added (the host checked for room), the
#      node's kernel parameters following it on trial and confirmed, a
#      vCPU added under load (onlined, HAProxy reloaded); the load gone,
#      the vCPU, the memory and the parameters taken back; paused, then
#      removed (restarted fixed);
#   3e. recycling a cluster's member (docs/cluster.md section 5): the
#      Controller's fleet set up, a cluster of node1 and a new machine,
#      node3; node3 recycled - a new machine like it made, registered,
#      trusting the fleet, joined drained and taking node3's place - and
#      node3's machine destroyed;
#   3f. a cluster made of new machines (docs/cluster.md "On a
#      hypervisor"): a second record reaching the same libvirtd is the
#      same host (its pinned SSH host key); with anti-affinity required
#      two machines on it are refused before anything is made, with
#      preferred they are made, the cluster created from them, and its
#      page says both run on one host; scaled to three members - a third
#      machine like the newest, joined - and back to two, the newest
#      taken out and destroyed; auto-scaling observing reads the members'
#      real load;
#   3h. with CONTROLLER_LIBVIRT_UPGRADE_FROM and _TO (two published
#      releases - needs the internet): a cluster of two of the
#      Controller's machines made from the first, updated in place to
#      the second (POST /api/clusters/{id}/upgrade) - each machine's
#      record updating, then ready with its spec's version following,
#      each node on the release's signed bundle fetched from GitHub, on
#      its other slot, the cluster's journal complete. Bundles built
#      here can't stand in: a node takes an update only signed by the
#      release key, and the Controller never asks it to skip that check;
#   4. a record forged to point at a domain the Controller didn't create
#      (one untagged, one tagged by another Controller) is refused, and
#      the domain is left as it was;
#   5. destroying the machine leaves nothing but the cached base image.
#
# The polkit ACL that restricts the Controller's account on a real host
# can't run in a container (no polkitd); it's checked on the lab's host
# (docs/hypervisors.md says how).
#
# Usage: [GUEST_AGENT_IMAGE=<janus-kvm.qcow2 with qemu-guest-agent>] \
#   [CONTROLLER_LIBVIRT_UPGRADE_FROM=<release> CONTROLLER_LIBVIRT_UPGRADE_TO=<release>] \
#   hack/controller-libvirt-test.sh <janus-kvm.qcow2> <dashboardd-bin>
set -euo pipefail

IMAGE_REL="${1:?usage: $0 <janus-kvm.qcow2> <dashboardd-bin>}"
DASHBOARDD_REL="${2:?usage: $0 <janus-kvm.qcow2> <dashboardd-bin>}"
IMAGE="$(cd "$(dirname "$IMAGE_REL")" && pwd)/$(basename "$IMAGE_REL")"
DASHBOARDD="$(cd "$(dirname "$DASHBOARDD_REL")" && pwd)/$(basename "$DASHBOARDD_REL")"
[ -c /dev/kvm ] || { echo "controller-libvirt test needs /dev/kvm" >&2; exit 1; }

HOST_PORT="${CONTROLLER_LIBVIRT_TEST_PORT:-$((18270 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
TEST_NAME=controller-libvirt
if [ -n "${GUEST_AGENT_IMAGE:-}" ]; then
  GUEST_AGENT_IMAGE="$(cd "$(dirname "$GUEST_AGENT_IMAGE")" && pwd)/$(basename "$GUEST_AGENT_IMAGE")"
  EXTRA_DOCKER_ARGS="${EXTRA_DOCKER_ARGS:-} -v $GUEST_AGENT_IMAGE:/images/janus-kvm-agent.qcow2:ro"
fi
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
  -d '{"name":"testhost","kind":"libvirt","labels":{"dc":"lgs","room":"lab"},"libvirt":{"host":"127.0.0.1","user":"janus-ctl","pool":"janus","networks":["janus-test"]}}' >"$WORKDIR/hv.json"
HV="$(json "d['id']" <"$WORKDIR/hv.json")"
[ "$(json "sorted(d['labels'].items())" <"$WORKDIR/hv.json")" = "[('dc', 'lgs'), ('room', 'lab')]" ] || fail "the hypervisor's labels: $(cat "$WORKDIR/hv.json")"
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
in_host cat /proc/meminfo >"$WORKDIR/meminfo"
python3 - "$WORKDIR/status.json" "$WORKDIR/meminfo" <<'EOF' || fail "host status: $(cat "$WORKDIR/status.json")"
import json, sys
d = json.load(open(sys.argv[1]))
h = d["host"]
assert not d.get("error"), d.get("error")
assert h["cpus"] > 0 and h["memory_total"] > 0, h
# The memory the host can give: free plus buffers and page cache - free
# alone stays near zero on a host that has run a while.
mi = {l.split(":")[0]: int(l.split()[1]) * 1024 for l in open(sys.argv[2])}
avail = mi["MemFree"] + mi["Buffers"] + mi["Cached"]
assert abs(h["memory_free"] - avail) < h["memory_total"] / 20, (h["memory_free"], avail, mi["MemFree"])
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
# Its firmware: the OVMF code alone, without a variable store - no
# compaction of a full store slowing a reboot (docs/guide/updates.md).
xml="$(in_host virsh dumpxml janus-node1)"
grep -Eq "<loader readonly='yes' type='pflash' stateless='yes'( format='raw')?>/usr/share/OVMF/OVMF_CODE_4M.fd</loader>" <<<"$xml" || fail "node1's firmware isn't the stateless OVMF code: $(grep -A3 '<os' <<<"$xml")"
if grep -q '<nvram' <<<"$xml" || in_host sh -c 'ls /var/lib/libvirt/qemu/nvram/ 2>/dev/null' | grep -q node1; then fail "node1 has a UEFI variable store"; fi
echo "Part 3 OK: node1 created - the OVMF code alone, no variable store -, admitted on its registration token (no approval), reachable through the relay as $host"

# =========================================================================
# 2b. node1 inherits the hypervisor's labels, under its own.
# =========================================================================
node1_view() { api "$API/api/nodes" | json "[n for n in d if n['id']=='$NODE'][0]"; }
labels_of() { api "$API/api/nodes" | json "sorted([n for n in d if n['id']=='$NODE'][0]['$1'].items())"; }
[ "$(labels_of labels)" = "[]" ] || fail "node1 has labels of its own: $(node1_view)"
[ "$(labels_of inherited_labels)" = "[('dc', 'lgs'), ('room', 'lab')]" ] || fail "node1 doesn't inherit the hypervisor's labels: $(node1_view)"
[ "$(api "$API/api/nodes" | json "[n for n in d if n['id']=='$NODE'][0]['inherited_from']['name']")" = testhost ] || fail "node1's labels come from: $(node1_view)"
api -o /dev/null -X PATCH "$API/api/nodes/$NODE" -H 'Content-Type: application/json' -d '{"labels":{"room":"r2"}}'
[ "$(labels_of labels)" = "[('room', 'r2')]" ] && [ "$(labels_of inherited_labels)" = "[('dc', 'lgs'), ('room', 'lab')]" ] || fail "node1's own label: $(node1_view)"
# A token scoped to dc=lgs (inherited) and room=r2 (its own) reaches it;
# one on room=lab - the hypervisor's, its own overrides it - doesn't.
token_for() {
  api -X POST "$API/api/tokens" -H 'Content-Type: application/json' -d "{\"name\":\"$1\",\"expires_in_days\":1,\"role\":\"reader\",\"scope\":{\"selector\":$2}}" | json "d['token']"
}
sees() { curl -sk -H "Authorization: Bearer $1" "$API/api/nodes" | json "' '.join(sorted(n['name'] for n in d))"; }
LGS_TOKEN="$(token_for lgs-r2 '{"dc":"lgs","room":"r2"}')"
LAB_TOKEN="$(token_for lab '{"room":"lab"}')"
[ "$(sees "$LGS_TOKEN")" = node1 ] || fail "a token on dc=lgs,room=r2 sees '$(sees "$LGS_TOKEN")'"
[ -z "$(sees "$LAB_TOKEN")" ] || fail "a token on room=lab - node1's own room overrides it - sees '$(sees "$LAB_TOKEN")'"
# Its machine too (the node doesn't trust a fleet here: its page is an
# admin's until then).
machines_seen() { curl -sk -H "Authorization: Bearer $1" "$API/api/machines" | json "' '.join(sorted(m['id'] for m in d))"; }
[ "$(machines_seen "$LGS_TOKEN")" = "$MID" ] || fail "the dc=lgs token's machines: '$(machines_seen "$LGS_TOKEN")'"
# The hypervisor relabelled: node1 follows at once.
set_hv_labels() {
  api "$API/api/hypervisors/$HV" | python3 -c '
import json, sys
h = json.load(sys.stdin)
print(json.dumps({"name": h["name"], "controller_address": h.get("controller_address", ""), "labels": json.loads(sys.argv[1]), "libvirt": h["libvirt"]}))' "$1" >"$WORKDIR/hv-labels.json"
  api -o /dev/null -X PATCH "$API/api/hypervisors/$HV" -H 'Content-Type: application/json' -d @"$WORKDIR/hv-labels.json"
}
set_hv_labels '{"dc":"par","room":"lab"}'
[ "$(labels_of inherited_labels)" = "[('dc', 'par'), ('room', 'lab')]" ] || fail "node1 after the hypervisor's change: $(node1_view)"
[ -z "$(sees "$LGS_TOKEN")" ] || fail "the dc=lgs token still sees '$(sees "$LGS_TOKEN")' once the hypervisor is dc=par"
[ -z "$(machines_seen "$LGS_TOKEN")" ] || fail "the dc=lgs token still sees machines once the hypervisor is dc=par: '$(machines_seen "$LGS_TOKEN")'"
set_hv_labels '{"dc":"lgs","room":"lab"}'
[ "$(sees "$LGS_TOKEN")" = node1 ] || fail "the dc=lgs token after the hypervisor's labels came back: '$(sees "$LGS_TOKEN")'"
echo "Part 3a OK: node1 inherits testhost's labels (dc=lgs, room=lab) under its own (room=r2 wins): a token on dc=lgs,room=r2 reaches it and its machine, one on room=lab doesn't; the hypervisor relabelled dc=par, the token lost both at once, and got them back"

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
# A change that leaves the labels out keeps them (a client from before
# labels).
[ "$(api "$API/api/hypervisors/$HV" | json "sorted(d['labels'].items())")" = "[('dc', 'lgs'), ('room', 'lab')]" ] || fail "a change without labels lost them: $(api "$API/api/hypervisors/$HV")"
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

autoscale_cycle() {
# =========================================================================
# 3c. Auto-scaling, observing.
# =========================================================================
# Thresholds a quiet node crosses: more memory as long as less than 97 %
# is available - what it would do, journaled after 30 s of samples.
cat >"$WORKDIR/autoscale.json" <<'EOF'
{"autoscale": {"mode": "observe", "max_vcpus": 2, "max_memory_mib": 1280,
  "memory_up_available_percent": 97, "memory_down_available_percent": 99, "memory_up_after_seconds": 30}}
EOF
code="$(api -o "$WORKDIR/p.json" -w '%{http_code}' -X PATCH "$API/api/machines/$MID" -H 'Content-Type: application/json' -d @"$WORKDIR/autoscale.json")"
[ "$code" = 202 ] || fail "setting an auto-scaling policy returned $code: $(cat "$WORKDIR/p.json")"
for _ in $(seq 1 150); do
  [ "$(api "$API/api/machines/$MID" | json "d['phase']")" = ready ] && break
  sleep 2
done
api "$API/api/machines/$MID" >"$WORKDIR/m.json"
python3 - "$WORKDIR/m.json" <<'EOF' || fail "the machine with its policy: $(cat "$WORKDIR/m.json")"
import json, sys
d = json.load(open(sys.argv[1]))
assert d["phase"] == "ready" and not d.get("error"), (d["phase"], d.get("error"))
assert d["spec"]["autoscale"]["mode"] == "observe" and d["spec"]["vcpus"] == 1 and d["spec"]["memory_mib"] == 1024, d["spec"]
events = "\n".join(e["message"] for e in d["events"])
assert "room for 2 vCPU, 1280 MiB" in events and "auto-scaling observing" in events, events
EOF
xml="$(in_host virsh dumpxml janus-node1)"
grep -q "current='1'>2</vcpu>" <<<"$xml" || fail "the domain has no room for a second vCPU: $(grep vcpu <<<"$xml")"
grep -q "model='virtio-mem'" <<<"$xml" || fail "the domain has no virtio-mem device"
for _ in $(seq 1 60); do
  reachable="$(api "$API/api/nodes/status" | json "d.get('$NODE',{}).get('reachable')")"
  [ "$reachable" = True ] && break
  sleep 2
done
[ "$reachable" = True ] || fail "the node didn't come back from its restart"
# Its size as the hypervisor says - nominal, with room to grow.
for _ in $(seq 1 30); do
  api "$API/api/machines/$MID?refresh=true" >"$WORKDIR/m.json"
  [ "$(json "(d.get('size') or {}).get('max_vcpus')" <"$WORKDIR/m.json")" = 2 ] && break
  sleep 2
done
python3 - "$WORKDIR/m.json" <<'EOF' || fail "the machine's size: $(cat "$WORKDIR/m.json")"
import json, sys
s = json.load(open(sys.argv[1]))["size"]
assert s == {"vcpus": 1, "memory_mib": 1024, "max_vcpus": 2, "max_memory_mib": 1280, "memory_step_mib": 128}, s
EOF
# The node's real load read; the step it would take, journaled.
for _ in $(seq 1 60); do
  api "$API/api/machines/$MID/autoscale" >"$WORKDIR/as.json"
  [ "$(json "len(d['journal'])" <"$WORKDIR/as.json")" -ge 1 ] && break
  sleep 3
done
python3 - "$WORKDIR/as.json" <<'EOF' || fail "auto-scaling's journal: $(cat "$WORKDIR/as.json")"
import json, sys
d = json.load(open(sys.argv[1]))
e = d["journal"][0]
assert e["mode"] == "observe" and e["action"] == "up" and e["resource"] == "memory", e
assert e["from"] == {"vcpus": 1, "memory_mib": 1024} and e["to"] == {"vcpus": 1, "memory_mib": 1280}, e
assert "available on average" in e["reason"], e
st = d["state"]
assert st["mode"] == "observe" and st["size"] == {"vcpus": 1, "memory_mib": 1280}, st
assert d["defaults"]["cpu_up_idle_percent"] == 30, d["defaults"]
print("  journaled: would grow to %s MiB - %s" % (e["to"]["memory_mib"], e["reason"]))
EOF
xml="$(in_host virsh dumpxml janus-node1)"
grep -q "current='1'>2</vcpu>" <<<"$xml" && grep -q "<requested unit='KiB'>0</requested>" <<<"$xml" || fail "observing resized the machine: $(grep -E 'vcpu|requested' <<<"$xml")"
echo "Part 4c OK: an observing policy restarted node1 with room to grow (hot-pluggable vCPU, virtio-mem), its real load read, the step it would take journaled and simulated, nothing resized"

# =========================================================================
# 3d. Auto-scaling, resizing: memory, the kernel parameters following it,
# a vCPU under load - then back to its size once the load is gone.
# =========================================================================
policy() { # $1: memory up/down thresholds
  echo "{\"autoscale\": {\"mode\": \"on\", \"sysctl\": true, \"max_vcpus\": 2, \"max_memory_mib\": 1280, $1,
    \"memory_up_after_seconds\": 30, \"cpu_up_idle_percent\": 85, \"cpu_up_busy_percent\": 40, \"cpu_down_idle_percent\": 92, \"cpu_up_after_seconds\": 60,
    \"cooldown_seconds\": 60, \"return_after_seconds\": 60}}"
}
code="$(api -o "$WORKDIR/p.json" -w '%{http_code}' -X PATCH "$API/api/machines/$MID" -H 'Content-Type: application/json' \
  -d "$(policy '"memory_up_available_percent": 97, "memory_down_available_percent": 99')")"
[ "$code" = 200 ] || fail "turning auto-scaling on returned $code (it needs no restart): $(cat "$WORKDIR/p.json")"
# journal_has PYTHON-CONDITION-ON-e: an entry of the journal.
journal_has() {
  api "$API/api/machines/$MID/autoscale" | python3 -c "import json,sys; d=json.load(sys.stdin); sys.exit(0 if any($1 for e in d['journal']) else 1)"
}
wait_journal() { # $1: what, $2: condition
  for _ in $(seq 1 ${3:-60}); do
    journal_has "$2" && return 0
    sleep 3
  done
  fail "auto-scaling never $1: $(api "$API/api/machines/$MID/autoscale")"
}
node_events() { timeout 5 curl -skN -b "$JAR" "$API/nodes/$NODE/api/stream/events" 2>/dev/null || true; }
# node_said TYPE PATTERN...: waits until the node's event log has an event
# of TYPE matching every PATTERN - the node tells a change once it held
# for 5 s (internal/hotplug), and reloads HAProxy before telling a vCPU.
node_said() {
  local type="$1" ev
  shift
  for _ in $(seq 1 10); do
    ev="$(node_events | grep "\"type\":\"$type\"" || true)"
    local all=1
    for p in "$@"; do grep -q -- "$p" <<<"$ev" || all=0; done
    [ -n "$ev" ] && [ "$all" = 1 ] && return 0
    sleep 3
  done
  echo "$ev"
  return 1
}
tcp_mem() { api "$API/nodes/$NODE/api/system/sysctl" | python3 -c "import json,sys; print(next(p['value'] for p in json.load(sys.stdin)['parameters'] if p['name']=='net.ipv4.tcp_mem'))"; }
boot_tcp_mem="$(tcp_mem)"
wait_journal "added memory" "e['mode']=='on' and e['action']=='up' and e['resource']=='memory' and e['to']['memory_mib']==1280 and not e.get('error')"
grep -q "<requested unit='KiB'>262144</requested>" <<<"$(in_host virsh dumpxml janus-node1)" || fail "the virtio-mem device wasn't asked for 256 MiB"
node_said system.memory_changed >/dev/null || fail "the node didn't say it got memory"
wait_journal "made the kernel parameters follow" "e['mode']=='on' and e['action']=='sysctl' and 'net.ipv4.tcp_mem' in e['reason'] and not e.get('error')"
[ "$(tcp_mem)" != "$boot_tcp_mem" ] || fail "tcp_mem is still the boot's ($boot_tcp_mem)"
echo "  memory: 1024 to 1280 MiB, tcp_mem $boot_tcp_mem to $(tcp_mem)"

# Load: four processes of HTTP clients make the node's one vCPU about 60 %
# busy and HAProxy about 75 % idle (measured) - over the thresholds: a
# vCPU more. Memory's thresholds back to sensible ones first - a vCPU
# waits while less than memory_up_available_percent + 5 % is available
# (HAProxy's reload) - with nothing to take back yet (99 %).
code="$(api -o "$WORKDIR/p.json" -w '%{http_code}' -X PATCH "$API/api/machines/$MID" -H 'Content-Type: application/json' \
  -d "$(policy '"memory_up_available_percent": 20, "memory_down_available_percent": 99')")"
[ "$code" = 200 ] || fail "changing the thresholds returned $code: $(cat "$WORKDIR/p.json")"
node_ip="$(api "$API/api/machines/$MID" | json "d['node_address'].split(':')[0]")"
cat <<'EOF' | in_host_i sh -c 'cat >/work/load.py'
import multiprocessing, os, sys, threading, time, urllib.request
def clients(url, end):
    def work():
        while time.time() < end:
            try:
                urllib.request.urlopen(url, timeout=2).read()
            except Exception:
                pass
    ts = [threading.Thread(target=work, daemon=True) for _ in range(16)]
    for t in ts:
        t.start()
    for t in ts:
        t.join()
if __name__ == "__main__":
    open("/work/load.pid", "w").write(str(os.getpgrp()))
    url, end = sys.argv[1], time.time() + float(sys.argv[2])
    ps = [multiprocessing.Process(target=clients, args=(url, end)) for _ in range(4)]
    for p in ps:
        p.start()
    for p in ps:
        p.join()
EOF
docker exec -d "$NAME" setsid python3 /work/load.py "http://$node_ip:8080/" 600
wait_journal "added a vCPU under load" "e['mode']=='on' and e['action']=='up' and e['resource']=='cpu' and e['to']['vcpus']==2 and not e.get('error')" 100
grep -q "current='2'>2</vcpu>" <<<"$(in_host virsh dumpxml janus-node1)" || grep -q "<vcpu placement='static'>2</vcpu>" <<<"$(in_host virsh dumpxml janus-node1)" || fail "the domain doesn't have 2 vCPUs: $(in_host virsh dumpxml janus-node1 | grep vcpu)"
ev="$(node_said system.cpus_changed '"after":2' '"haproxy":"reloaded"')" || fail "the node didn't online the vCPU and reload HAProxy: $ev"
docker exec "$NAME" sh -c 'kill -- "-$(cat /work/load.pid)"'

# The load gone: a vCPU less, then memory, the kernel parameters back.
code="$(api -o "$WORKDIR/p.json" -w '%{http_code}' -X PATCH "$API/api/machines/$MID" -H 'Content-Type: application/json' \
  -d "$(policy '"memory_up_available_percent": 20, "memory_down_available_percent": 40')")"
[ "$code" = 200 ] || fail "changing the thresholds returned $code: $(cat "$WORKDIR/p.json")"
wait_journal "took the vCPU back" "e['action']=='down' and e['resource']=='cpu' and e['to']['vcpus']==1 and not e.get('error')" 100
wait_journal "took the memory back" "e['action']=='down' and e['resource']=='memory' and e['to']['memory_mib']==1024 and not e.get('error')" 100
wait_journal "put the kernel parameters back" "e['action']=='sysctl' and 'back at its size' in e['reason'] and not e.get('error')" 100
[ "$(tcp_mem)" = "$boot_tcp_mem" ] || fail "tcp_mem isn't back to the boot's: $(tcp_mem), was $boot_tcp_mem"
api "$API/api/machines/$MID" >"$WORKDIR/m.json"
python3 - "$WORKDIR/m.json" <<'EOF' || fail "the machine after auto-scaling: $(cat "$WORKDIR/m.json")"
import json, sys
d = json.load(open(sys.argv[1]))
assert d["spec"]["vcpus"] == 1 and d["spec"]["memory_mib"] == 1024, d["spec"]
assert d["size"]["vcpus"] == 1 and d["size"]["memory_mib"] == 1024, d["size"]
events = "\n".join(e["message"] for e in d["events"])
assert "not by its auto-scaling" not in events, events
EOF
# Paused: the record only, no restart.
code="$(api -o "$WORKDIR/p.json" -w '%{http_code}' -X PATCH "$API/api/machines/$MID" -H 'Content-Type: application/json' \
  -d '{"autoscale": {"mode": "off", "max_vcpus": 2, "max_memory_mib": 1280}}')"
[ "$code" = 200 ] || fail "pausing auto-scaling returned $code: $(cat "$WORKDIR/p.json")"
# Removed: started again without room to grow.
code="$(api -o "$WORKDIR/p.json" -w '%{http_code}' -X PATCH "$API/api/machines/$MID" -H 'Content-Type: application/json' -d '{"autoscale": null}')"
[ "$code" = 202 ] || fail "removing auto-scaling returned $code: $(cat "$WORKDIR/p.json")"
for _ in $(seq 1 150); do
  [ "$(api "$API/api/machines/$MID" | json "d['phase']")" = ready ] && break
  sleep 2
done
xml="$(in_host virsh dumpxml janus-node1)"
if grep -q "virtio-mem\|current=" <<<"$xml"; then fail "removed, the domain still has room to grow"; fi
for _ in $(seq 1 60); do
  [ "$(api "$API/api/nodes/status" | json "d.get('$NODE',{}).get('reachable')")" = True ] && break
  sleep 2
done
echo "Part 4d OK: resizing for real - memory added, the kernel parameters following it, a vCPU under load (onlined, HAProxy reloaded), all taken back once the load was gone; paused without a restart, removed with one"
}

recycle_cycle() {
# =========================================================================
# 3e. A cluster's member recycled into a new machine.
# =========================================================================
fleet="$(api -X POST "$API/api/fleet/setup")" || fail "fleet setup"
kit="$(api "$API/api/fleet/recovery-kit")" || fail "recovery kit"
python3 -c 'import json,sys; print(json.dumps({"kit": sys.argv[1], "passphrase": json.loads(sys.argv[2])["passphrase"]}))' "$kit" "$fleet" >"$WORKDIR/confirm.json"
api -X POST "$API/api/fleet/confirm" -H 'Content-Type: application/json' -d @"$WORKDIR/confirm.json" >/dev/null || fail "fleet confirm"
trusted() { [ "$(api "$API/api/fleet" | json "d.get('nodes',{}).get('$1',{}).get('state')")" = trusted ]; }
for _ in $(seq 1 120); do trusted "$NODE" && break; sleep 1; done
trusted "$NODE" || fail "node1 never trusted the fleet: $(api "$API/api/fleet")"
api -X POST "$API/api/machines" -H 'Content-Type: application/json' -d "{
  \"name\": \"node3\", \"hypervisor_id\": \"$HV\", \"vcpus\": 1, \"memory_mib\": 512,
  \"image\": {\"url\": \"http://127.0.0.1:8000/janus-kvm.qcow2\", \"sha256\": \"$SUM\"},
  \"nics\": [{\"network\": \"janus-test\", \"name\": \"lan\", \"mode\": \"dhcp\"}]}" >"$WORKDIR/m3.json"
MID3="$(json "d['id']" <"$WORKDIR/m3.json")" || fail "create node3: $(cat "$WORKDIR/m3.json")"
for _ in $(seq 1 120); do
  [ "$(api "$API/api/machines/$MID3" | json "d['phase']")" = ready ] && break
  sleep 2
done
NODE3="$(api "$API/api/machines/$MID3" | json "d.get('node_id','')")"
[ -n "$NODE3" ] || fail "node3 never registered: $(api "$API/api/machines/$MID3")"
for _ in $(seq 1 120); do trusted "$NODE3" && break; sleep 1; done
trusted "$NODE3" || fail "node3 never trusted the fleet"
api -X POST "$API/api/clusters" -H 'Content-Type: application/json' \
  -d "{\"name\": \"edge\", \"source\": \"$NODE\", \"members\": [{\"node_id\": \"$NODE\"}, {\"node_id\": \"$NODE3\"}]}" >"$WORKDIR/c.json" \
  || fail "create the cluster: $(cat "$WORKDIR/c.json")"
CID="$(json "d['id']" <"$WORKDIR/c.json")"
cluster_idle() { # $1: how long
  for _ in $(seq 1 "$1"); do
    [ "$(api "$API/api/clusters/$CID" | json "d['busy']")" = False ] && return 0
    sleep 2
  done
  fail "cluster edge still busy: $(api "$API/api/clusters/$CID")"
}
cluster_idle 120
err="$(api "$API/api/clusters/$CID" | json "d.get('error','')")"
[ -z "$err" ] || fail "the cluster's creation: $err"
api -X POST "$API/api/clusters/$CID/recycle" -H 'Content-Type: application/json' -d '{"members": ["node3"]}' >/dev/null || fail "recycle node3"
cluster_idle 450
api "$API/api/clusters/$CID" >"$WORKDIR/c.json"
err="$(json "d.get('error','')" <"$WORKDIR/c.json")"
[ -z "$err" ] || fail "recycling node3: $err"
members="$(json "','.join(sorted(m['name'] for m in d['members']))" <"$WORKDIR/c.json")"
[ "$members" = "edge-1,node1" ] || fail "the members after the recycle: $members"
api "$API/api/clusters/$CID/journal" | grep -q "edge-1 joins drained" || fail "edge-1 didn't join drained: $(api "$API/api/clusters/$CID/journal")"
api "$API/api/clusters/$CID/journal" | grep -q "a new machine, edge-1, on testhost" || fail "the recycle's journal doesn't name the hypervisor: $(api "$API/api/clusters/$CID/journal")"
for _ in $(seq 1 90); do
  [ "$(api -o /dev/null -w '%{http_code}' "$API/api/machines/$MID3")" = 404 ] && break
  sleep 1
done
[ "$(api -o /dev/null -w '%{http_code}' "$API/api/machines/$MID3")" = 404 ] || fail "node3's machine was never destroyed: $(api "$API/api/machines/$MID3")"
if in_host virsh list --all --name | grep -q '^janus-node3$'; then fail "node3's virtual machine is still defined"; fi
MIDE="$(api "$API/api/machines" | json "next(m['id'] for m in d if m['spec']['name']=='edge-1')")"
echo "Part 4e OK: node3 recycled - edge-1 made like it, registered, trusting the fleet, joined drained and took its place; node3's machine destroyed"
# Back as the parts after expect: the cluster gone, edge-1 destroyed.
api -o /dev/null -X DELETE "$API/api/clusters/$CID" || fail "delete the cluster"
for _ in $(seq 1 60); do
  [ "$(api -o /dev/null -w '%{http_code}' "$API/api/clusters/$CID")" = 404 ] && break
  sleep 1
done
api -o /dev/null -X DELETE "$API/api/machines/$MIDE"
for _ in $(seq 1 90); do
  [ "$(api -o /dev/null -w '%{http_code}' "$API/api/machines/$MIDE")" = 404 ] && break
  sleep 1
done
[ "$(api -o /dev/null -w '%{http_code}' "$API/api/machines/$MIDE")" = 404 ] || fail "edge-1 was never destroyed"
}

placement_cycle() {
# =========================================================================
# 3f. A cluster made of new machines, placed by host.
# =========================================================================
api -X POST "$API/api/hypervisors" -H 'Content-Type: application/json' \
  -d '{"name":"testhost-again","kind":"libvirt","libvirt":{"host":"localhost","user":"janus-ctl","pool":"janus","networks":["janus-test"]}}' >"$WORKDIR/hv2.json"
HV2="$(json "d['id']" <"$WORKDIR/hv2.json")" || fail "a second record: $(cat "$WORKDIR/hv2.json")"
json "d['authorized_key']" <"$WORKDIR/hv2.json" | in_host_i sh -c 'cat >> /home/janus-ctl/.ssh/authorized_keys'
probed2="$(api -X POST "$API/api/hypervisors/$HV2/probe" | json "d['fingerprint']")"
api -X POST "$API/api/hypervisors/$HV2/trust" -H 'Content-Type: application/json' -d "{\"fingerprint\":\"$probed2\"}" | json "d['trusted']" | grep -q True || fail "trust the second record"
id1="$(api "$API/api/hypervisors/$HV/status" | json "d['host'].get('host_id','')")"
id2="$(api "$API/api/hypervisors/$HV2/status" | json "d['host'].get('host_id','')")"
case "$id1" in libvirt:?*) ;; *) fail "the host's identity: '$id1'" ;; esac
[ "$id1" = "$id2" ] || fail "two records reaching one libvirtd name two hosts: $id1, $id2"
cluster_body() { # $1: name, $2: anti-affinity
  printf '{"name":"%s","machines":{"count":2,"anti_affinity":"%s","hypervisors":["%s","%s"],"spec":{"vcpus":1,"memory_mib":512,"image":{"url":"http://127.0.0.1:8000/janus-kvm.qcow2","sha256":"%s"},"nics":[{"network":"janus-test","name":"lan","mode":"dhcp"}]}}}' "$1" "$2" "$HV" "$HV2" "$SUM"
}
before="$(api "$API/api/machines" | json "len(d)")"
code="$(api -o "$WORKDIR/p.json" -w '%{http_code}' -X POST "$API/api/clusters" -H 'Content-Type: application/json' -d "$(cluster_body pinned required)")"
[ "$code" = 400 ] || fail "two machines required on hosts of their own, one host: $code $(cat "$WORKDIR/p.json")"
grep -q "testhost and testhost-again: one host" "$WORKDIR/p.json" || fail "the refusal doesn't name the host: $(cat "$WORKDIR/p.json")"
[ "$(api "$API/api/machines" | json "len(d)")" = "$before" ] || fail "a refused cluster made machines"
api -X POST "$API/api/clusters" -H 'Content-Type: application/json' -d "$(cluster_body pref preferred)" >"$WORKDIR/c.json" || fail "create pref: $(cat "$WORKDIR/c.json")"
CID="$(json "d['id']" <"$WORKDIR/c.json")"
cluster_idle 600
api "$API/api/clusters/$CID" >"$WORKDIR/c.json"
err="$(json "d.get('error','')" <"$WORKDIR/c.json")"
[ -z "$err" ] || fail "the cluster made of new machines: $err"
members="$(json "','.join(sorted(m['name'] for m in d['members']))" <"$WORKDIR/c.json")"
[ "$members" = "pref-1,pref-2" ] || fail "its members: $members"
json "d['placement']['anti_affinity']" <"$WORKDIR/c.json" | grep -qx preferred || fail "its placement: $(cat "$WORKDIR/c.json")"
json "' '.join(m.get('host_name','') for m in d['members'])" <"$WORKDIR/c.json" | grep -Eq '^[^ ]+ [^ ]+$' || fail "its members' hosts: $(cat "$WORKDIR/c.json")"
json "'\n'.join(d['problems'])" <"$WORKDIR/c.json" | grep -q "pref-1 and pref-2 run on the same host" || fail "the page doesn't say both run on one host: $(json "d['problems']" <"$WORKDIR/c.json")"
# Each machine shows the host it runs on, once synced.
mid="$(api "$API/api/machines" | json "next(m['id'] for m in d if m['spec']['name']=='pref-1')")"
for _ in $(seq 1 45); do
  [ "$(api "$API/api/machines/$mid?refresh=true" | json "d.get('host_id','')")" = "$id1" ] && break
  sleep 2
done
[ "$(api "$API/api/machines/$mid" | json "d.get('host_id','')")" = "$id1" ] || fail "pref-1's machine doesn't show its host: $(api "$API/api/machines/$mid" | json "(d.get('host_id'), d.get('host_name'))")"
echo "Part 4f OK: a second record on the same libvirtd is the same host ($id1) - two machines with anti-affinity required refused before anything was made; preferred made them, the cluster from them, and its page says both run on one host"

api -X POST "$API/api/clusters/$CID/scale" -H 'Content-Type: application/json' -d '{"members": 3}' >/dev/null || fail "scale pref to 3"
cluster_idle 600
api "$API/api/clusters/$CID" >"$WORKDIR/c.json"
[ -z "$(json "d.get('error','')" <"$WORKDIR/c.json")" ] || fail "scaling to 3: $(json "d['error']" <"$WORKDIR/c.json")"
members="$(json "','.join(sorted(m['name'] for m in d['members']))" <"$WORKDIR/c.json")"
[ "$members" = "pref-1,pref-2,pref-3" ] || fail "after scaling to 3: $members"
api "$API/api/clusters/$CID/journal" | grep -q "a member more: pref-3, a new machine like pref-[12], on testhost" || fail "the scale-out's journal doesn't name the hypervisor: $(api "$API/api/clusters/$CID/journal")"
api -X POST "$API/api/clusters/$CID/scale" -H 'Content-Type: application/json' -d '{"members": 2}' >/dev/null || fail "scale pref to 2"
cluster_idle 300
members="$(api "$API/api/clusters/$CID" | json "','.join(sorted(m['name'] for m in d['members']))")"
[ "$members" = "pref-1,pref-2" ] || fail "after scaling back to 2: $members"
for _ in $(seq 1 90); do
  api "$API/api/machines" | json "' '.join(m['spec']['name'] for m in d)" | grep -qw pref-3 || break
  sleep 1
done
if api "$API/api/machines" | json "' '.join(m['spec']['name'] for m in d)" | grep -qw pref-3; then fail "pref-3's machine is still there"; fi
code="$(api -o "$WORKDIR/as.json" -w '%{http_code}' -X PUT "$API/api/clusters/$CID/autoscale" -H 'Content-Type: application/json' -d '{"mode":"observe","min_members":2,"max_members":3}')"
[ "$code" = 200 ] || fail "auto-scaling observe: $code $(cat "$WORKDIR/as.json")"
# Two readings at least: the CPUs' busy share is between two.
for _ in $(seq 1 30); do
  [ "$(api "$API/api/clusters/$CID" | json "(d.get('load') or {}).get('members', 0) == 2 and d['load']['busy_percent'] >= 0")" = True ] && break
  sleep 2
done
api "$API/api/clusters/$CID" >"$WORKDIR/c.json"
[ "$(json "(d.get('load') or {}).get('members', 0)" <"$WORKDIR/c.json")" = 2 ] || fail "auto-scaling read no load: $(json "d.get('load')" <"$WORKDIR/c.json")"
json "d.get('scale_status','')" <"$WORKDIR/c.json" | grep -q "^observing" || fail "auto-scaling's status: $(json "d.get('scale_status')" <"$WORKDIR/c.json")"
# VRRP can't come in under auto-scaling (v2026.10.24-5's lab: it did).
code="$(api -o "$WORKDIR/apply.json" -w '%{http_code}' -X POST "$API/api/clusters/$CID/apply" -H 'Content-Type: application/json' -d '{"version": 1, "managed": ["haproxy", "files", "vrrp"]}')"
[ "$code" = 409 ] && grep -q "set it off first" "$WORKDIR/apply.json" || fail "VRRP brought in under auto-scaling: $code $(cat "$WORKDIR/apply.json")"
echo "Part 4f OK: scaled to three - pref-3 made like the newest, joined - and back to two, pref-3 out and destroyed; VRRP refused under auto-scaling; auto-scaling observing read both members' load ($(json "'idle %.0f %%, busy %.0f %%' % (d['load']['idle_percent'], d['load']['busy_percent'])" <"$WORKDIR/c.json"))"
api -o /dev/null -X DELETE "$API/api/clusters/$CID" || fail "delete pref"
for _ in $(seq 1 60); do
  [ "$(api -o /dev/null -w '%{http_code}' "$API/api/clusters/$CID")" = 404 ] && break
  sleep 1
done
for name in pref-1 pref-2; do
  mid="$(api "$API/api/machines" | json "next(m['id'] for m in d if m['spec']['name']=='$name')")"
  api -o /dev/null -X DELETE "$API/api/machines/$mid"
  for _ in $(seq 1 90); do
    [ "$(api -o /dev/null -w '%{http_code}' "$API/api/machines/$mid")" = 404 ] && break
    sleep 1
  done
  [ "$(api -o /dev/null -w '%{http_code}' "$API/api/machines/$mid")" = 404 ] || fail "$name was never destroyed"
done

# A cluster seeded from node1: its first machine takes node1's state
# (node to node, without its identity), the cluster made from it - node1
# itself left as it is.
api "$API/nodes/$NODE/api/haproxy/config" | json "d['config']" >"$WORKDIR/node1.cfg" || fail "node1's configuration"
printf '# node1-own-marker\n' >>"$WORKDIR/node1.cfg"
python3 -c 'import json,sys; print(json.dumps({"config": open(sys.argv[1]).read()}))' "$WORKDIR/node1.cfg" >"$WORKDIR/node1-apply.json"
api -X POST -H 'Content-Type: application/json' -d @"$WORKDIR/node1-apply.json" "$API/nodes/$NODE/api/haproxy/config" >"$WORKDIR/node1-applied.json" || fail "mark node1's configuration: $(cat "$WORKDIR/node1-applied.json")"
seed_body="$(cluster_body seed preferred | python3 -c 'import json,sys; d=json.load(sys.stdin); d["machines"]["seed_from"]=sys.argv[1]; print(json.dumps(d))' "$NODE")"
api -X POST "$API/api/clusters" -H 'Content-Type: application/json' -d "$seed_body" >"$WORKDIR/c.json" || fail "create seed: $(cat "$WORKDIR/c.json")"
CID="$(json "d['id']" <"$WORKDIR/c.json")"
cluster_idle 900
api "$API/api/clusters/$CID" >"$WORKDIR/c.json"
err="$(json "d.get('error','')" <"$WORKDIR/c.json")"
[ -z "$err" ] || fail "the cluster seeded from node1: $err"
[ "$(json "','.join(sorted(m['name'] for m in d['members']))" <"$WORKDIR/c.json")" = "seed-1,seed-2" ] || fail "its members: $(json "d['members']" <"$WORKDIR/c.json")"
api "$API/api/clusters/$CID/versions/1" | json "d['content']['haproxy.cfg']" | grep -q 'node1-own-marker' || fail "version 1 isn't node1's configuration"
api "$API/api/clusters/$CID/journal" | grep -q "seed-1 took node1's state, node to node" || fail "the journal doesn't say seed-1 took node1's state: $(api "$API/api/clusters/$CID/journal")"
s2="$(api "$API/api/clusters/$CID" | json "next(m['node_id'] for m in d['members'] if m['name']=='seed-2')")"
api "$API/nodes/$s2/api/haproxy/config" | json "d['config']" | grep -q 'node1-own-marker' || fail "seed-2 doesn't run node1's configuration"
api "$API/nodes/$NODE/api/haproxy/config" | json "d['config']" | grep -q 'node1-own-marker' || fail "node1's own configuration changed"
[ "$(api "$API/api/clusters" | json "sum(1 for c in d for m in c['members'] if m['node_id']=='$NODE')")" = 0 ] || fail "node1 became a member"
echo "Part 4g OK: a cluster seeded from node1 - seed-1 took its state node to node, the cluster made from it (version 1 node1's configuration, on seed-2 too), node1 left as it was"
# Deleted with its machines: the members leave, then their virtual
# machines go; node1 - no member - stays.
api -o /dev/null -X DELETE "$API/api/clusters/$CID?machines=destroy" || fail "delete seed with its machines"
for _ in $(seq 1 60); do
  [ "$(api -o /dev/null -w '%{http_code}' "$API/api/clusters/$CID")" = 404 ] && break
  sleep 1
done
for _ in $(seq 1 120); do
  api "$API/api/machines" | json "' '.join(m['spec']['name'] for m in d)" | grep -Eqw 'seed-1|seed-2' || break
  sleep 1
done
if api "$API/api/machines" | json "' '.join(m['spec']['name'] for m in d)" | grep -Eqw 'seed-1|seed-2'; then fail "the seed cluster's machines are still there"; fi
if in_host virsh list --all --name | grep -Eq 'janus-seed-[12]$'; then fail "the seed cluster's virtual machines are still defined"; fi
api "$API/api/machines" | json "' '.join(m['spec']['name'] for m in d)" | grep -qw node1 || fail "node1's machine went with the cluster"
echo "Part 4g OK: the seed cluster deleted with its machines (?machines=destroy) - seed-1 and seed-2 gone, node1 kept"
api -o /dev/null -X DELETE "$API/api/hypervisors/$HV2" || fail "remove the second record"
}

guest_agent_cycle() {
# =========================================================================
# 3b2. The guest agent: libvirt's channel, the extension's service running.
# =========================================================================
ASUM="$(sha256sum "$GUEST_AGENT_IMAGE" | cut -d' ' -f1)"
api -X POST "$API/api/machines" -H 'Content-Type: application/json' -d "{
  \"name\": \"agent1\", \"hypervisor_id\": \"$HV\", \"vcpus\": 1, \"memory_mib\": 512,
  \"image\": {\"url\": \"http://127.0.0.1:8000/janus-kvm-agent.qcow2\", \"sha256\": \"$ASUM\"},
  \"nics\": [{\"network\": \"janus-test\", \"name\": \"lan\", \"mode\": \"dhcp\"}]}" >"$WORKDIR/ma.json"
AID="$(json "d['id']" <"$WORKDIR/ma.json")" || fail "create agent1: $(cat "$WORKDIR/ma.json")"
for _ in $(seq 1 120); do
  phase="$(api "$API/api/machines/$AID" | json "d['phase']")"
  case "$phase" in ready | failed) break ;; esac
  sleep 2
done
[ "$phase" = ready ] || fail "agent1 ended $phase: $(api "$API/api/machines/$AID")"
ANODE="$(api "$API/api/machines/$AID" | json "d['node_id']")"
grep -q "<target type='virtio' name='org.qemu.guest_agent.0'" <<<"$(in_host virsh dumpxml janus-agent1)" || fail "agent1 has no guest agent channel"
svc=""
for _ in $(seq 1 60); do
  svc="$(api "$API/nodes/$ANODE/api/system/services" | json "' '.join(s['id'] + '=' + s.get('state', '') for s in d.get('services', []) if s['id'] == 'qemu-guest-agent')" 2>/dev/null || true)"
  [ "$svc" = qemu-guest-agent=running ] && break
  sleep 2
done
[ "$svc" = qemu-guest-agent=running ] || fail "agent1's guest agent service: '$svc' - $(api "$API/nodes/$ANODE/api/system/services")"
pong=""
for _ in $(seq 1 30); do
  pong="$(in_host virsh qemu-agent-command janus-agent1 '{"execute":"guest-ping"}' 2>&1 || true)"
  grep -q '"return":{}' <<<"$pong" && break
  sleep 2
done
grep -q '"return":{}' <<<"$pong" || fail "the guest agent doesn't answer libvirt: $pong"
# The record learns the extension from its node (an image URL says
# nothing), and libvirt's channel being always there, nothing differs.
for _ in $(seq 1 30); do
  api "$API/api/machines/$AID?refresh=true" >"$WORKDIR/ma.json"
  [ "$(json "' '.join(d['spec'].get('extensions') or [])" <"$WORKDIR/ma.json")" = qemu-guest-agent ] && break
  sleep 2
done
[ "$(json "' '.join(d['spec'].get('extensions') or [])" <"$WORKDIR/ma.json")" = qemu-guest-agent ] || fail "agent1's record doesn't know its extension: $(cat "$WORKDIR/ma.json")"
[ "$(json "d.get('hardware_drift')" <"$WORKDIR/ma.json")" = None ] || fail "agent1 has hardware drift on libvirt: $(cat "$WORKDIR/ma.json")"
code="$(api -o /dev/null -w '%{http_code}' -X POST "$API/api/machines/$AID/apply-settings")"
[ "$code" = 200 ] || fail "apply-settings with nothing to apply returned $code, want 200"
code="$(api -o /dev/null -w '%{http_code}' -X DELETE "$API/api/machines/$AID")"
[ "$code" = 202 ] || fail "destroy agent1 returned $code"
for _ in $(seq 1 90); do
  [ "$(api -o /dev/null -w '%{http_code}' "$API/api/machines/$AID")" = 404 ] && break
  sleep 1
done
[ "$(api -o /dev/null -w '%{http_code}' "$API/api/machines/$AID")" = 404 ] || fail "agent1 was never destroyed: $(api "$API/api/machines/$AID")"
in_host virsh -q vol-delete --pool janus "janus-base-sha256-${ASUM:0:16}.qcow2" >/dev/null
echo "Part 4b2 OK: agent1 (qemu-guest-agent) has libvirt's guest agent channel, its service runs and answers guest-ping, its record knows the extension and has no hardware drift (apply-settings: nothing to do)"
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
# The release's base image part 4h cached stays too.
vols="$(in_host virsh vol-list janus | awk -v keep="${UPGRADE_BASE_VOL:-}" 'NR>2 && $1 && $1 != keep {print $1}')"
[ "$vols" = "janus-base-sha256-${SUM:0:16}.qcow2" ] || fail "volumes left in the pool: $vols"
echo "Part 6 OK: destroyed - node, virtual machine, NVRAM and volumes gone, the base image kept"

}

upgrade_cycle() {
# =========================================================================
# 3h. A cluster of the Controller's machines updated in place.
# =========================================================================
local from="$CONTROLLER_LIBVIRT_UPGRADE_FROM" to="$CONTROLLER_LIBVIRT_UPGRADE_TO" i m v rel want journal
declare -A UMID UNODE
for i in 1 2; do
  api -X POST "$API/api/machines" -H 'Content-Type: application/json' -d "{
    \"name\": \"upg-$i\", \"hypervisor_id\": \"$HV\", \"vcpus\": 1, \"memory_mib\": 512, \"version\": \"$from\",
    \"nics\": [{\"network\": \"janus-test\", \"name\": \"lan\", \"mode\": \"dhcp\"}]}" >"$WORKDIR/u$i.json"
  UMID[$i]="$(json "d['id']" <"$WORKDIR/u$i.json")" || fail "create upg-$i from $from: $(cat "$WORKDIR/u$i.json")"
done
in_fleet() { [ "$(api "$API/api/fleet" | json "d.get('nodes',{}).get('$1',{}).get('state')")" = trusted ]; }
for i in 1 2; do
  for _ in $(seq 1 240); do
    case "$(api "$API/api/machines/${UMID[$i]}" | json "d['phase']")" in ready | failed) break ;; esac
    sleep 2
  done
  UNODE[$i]="$(api "$API/api/machines/${UMID[$i]}" | json "d.get('node_id','')")"
  [ -n "${UNODE[$i]}" ] || fail "upg-$i never registered: $(api "$API/api/machines/${UMID[$i]}")"
  for _ in $(seq 1 120); do in_fleet "${UNODE[$i]}" && break; sleep 1; done
  in_fleet "${UNODE[$i]}" || fail "upg-$i never trusted the fleet: $(api "$API/api/fleet")"
  v="$(api "$API/api/machines/${UMID[$i]}" | json "d['spec'].get('version','') + ' ' + d.get('version','')")"
  [ "$v" = "$from $from" ] || fail "upg-$i's machine: version '$v', want $from"
done
api -X POST "$API/api/clusters" -H 'Content-Type: application/json' \
  -d "{\"name\": \"upg\", \"source\": \"${UNODE[1]}\", \"members\": [{\"node_id\": \"${UNODE[1]}\"}, {\"node_id\": \"${UNODE[2]}\"}]}" >"$WORKDIR/c.json" \
  || fail "create cluster upg: $(cat "$WORKDIR/c.json")"
CID="$(json "d['id']" <"$WORKDIR/c.json")"
cluster_idle 120
err="$(api "$API/api/clusters/$CID" | json "d.get('error','')")"
[ -z "$err" ] || fail "cluster upg's creation: $err"
code="$(api -o "$WORKDIR/up.json" -w '%{http_code}' -X POST "$API/api/clusters/$CID/upgrade" -H 'Content-Type: application/json' -d '{"version": "not-a-release"}')"
[ "$code" = 400 ] || fail "an update to no release: $code, want 400: $(cat "$WORKDIR/up.json")"
code="$(api -o "$WORKDIR/up.json" -w '%{http_code}' -X POST "$API/api/clusters/$CID/upgrade" -H 'Content-Type: application/json' -d "{\"version\": \"$to\"}")"
[ "$code" = 202 ] || fail "POST .../upgrade: $code $(cat "$WORKDIR/up.json")"
# Each machine's record says it updates while its node does.
local seen="" phase
for _ in $(seq 1 900); do
  for i in 1 2; do
    phase="$(api "$API/api/machines/${UMID[$i]}" | json "d['phase']")"
    [ "$phase" = updating ] && case " $seen " in *" upg-$i "*) ;; *) seen="$seen upg-$i" ;; esac
  done
  [ "$(api "$API/api/clusters/$CID" | json "d['busy']")" = False ] && break
  sleep 2
done
api "$API/api/clusters/$CID" >"$WORKDIR/c.json"
[ "$(json "d['busy']" <"$WORKDIR/c.json")" = False ] || fail "the update of upg never ended: $(cat "$WORKDIR/c.json")"
err="$(json "d.get('error','')" <"$WORKDIR/c.json")"
[ -z "$err" ] || fail "updating upg: $err - $(api "$API/api/clusters/$CID/journal")"
[ "$seen" = " upg-1 upg-2" ] || fail "the machines seen updating:$seen, want upg-1 then upg-2"
journal="$(api "$API/api/clusters/$CID/journal")"
for i in 1 2; do
  m="$(api "$API/api/machines/${UMID[$i]}")"
  [ "$(json "d['phase'] + ' ' + d['spec'].get('version','') + ' ' + d.get('version','') + ' ' + d.get('error','')" <<<"$m")" = "ready $to $to " ] ||
    fail "upg-$i's machine after the update: $(json "(d['phase'], d['spec'].get('version'), d.get('version'), d.get('error'))" <<<"$m")"
  rel="$(json "next((x['state'].get('release',''), x['state'].get('slot',''), x['state'].get('drained')) for x in d['members'] if x['name']=='upg-$i')" <"$WORKDIR/c.json")"
  [ "$rel" = "('$to', 'B', False)" ] || fail "upg-$i in the cluster's view: $rel"
  for want in "upg-$i drained" "upg-$i updating from Janus $from \(slot A\) to $to" "upg-$i runs Janus $to on slot B, in service" "upg-$i back in service"; do
    grep -Eq "$want" <<<"$journal" || fail "the journal lacks '$want': $journal"
  done
done
grep -q "updating upg to Janus $to, one at a time: upg-1, upg-2" <<<"$journal" || fail "the journal's plan: $journal"
grep -q "upg runs Janus $to: 2 member(s) updated, 0 already on it" <<<"$journal" || fail "the journal's end: $journal"
echo "Part 4h OK: cluster upg of two machines made from $from updated in place to $to - each machine updating, then ready with its spec on $to, each node on slot B, its tables learnt, back in service; the journal complete"
api -o /dev/null -X DELETE "$API/api/clusters/$CID?machines=destroy" || fail "delete upg with its machines"
for i in 1 2; do
  for _ in $(seq 1 90); do
    [ "$(api -o /dev/null -w '%{http_code}' "$API/api/machines/${UMID[$i]}")" = 404 ] && break
    sleep 1
  done
  [ "$(api -o /dev/null -w '%{http_code}' "$API/api/machines/${UMID[$i]}")" = 404 ] || fail "upg-$i was never destroyed"
done
# Left: the release's base image, cached for the next machine made from it.
UPGRADE_BASE_VOL="$(in_host virsh vol-list janus | awk -v v="$from" 'NR>2 && $1 ~ ("^janus-base-.*-" v ".qcow2$") {print $1}')"
[ -n "$UPGRADE_BASE_VOL" ] || fail "no base image of $from cached: $(in_host virsh vol-list janus)"
}

if [ -c /dev/net/tun ]; then
  boot_cycle
  if [ -n "${GUEST_AGENT_IMAGE:-}" ]; then
    guest_agent_cycle
  fi
  autoscale_cycle
  recycle_cycle
  placement_cycle
  if [ -n "${CONTROLLER_LIBVIRT_UPGRADE_FROM:-}" ] && [ -n "${CONTROLLER_LIBVIRT_UPGRADE_TO:-}" ]; then
    upgrade_cycle
  else
    echo "Part 4h skipped: CONTROLLER_LIBVIRT_UPGRADE_FROM and _TO (two published releases, the internet) not given"
  fi
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
