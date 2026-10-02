#!/usr/bin/env bash
# Proves the Consul agent (the consul extension, docs/consul.md) with two
# real nodes built with it: UEFI, SELinux enforcing, a shared LAN between
# them (a QEMU multicast socket network) next to each one's own user-mode
# NIC for its API.
#   - a configuration consul validate refuses is refused, with its message;
#   - A runs a server (bootstrap_expect 1), B a client joining it - both
#     with gossip encryption and TLS for the RPC between them, from files
#     given with the configuration (/run/janus/consul/files);
#   - A registers a service, "web", its HAProxy's port 8080; B's HAProxy
#     finds it through B's agent's DNS (server-template, SRV records on
#     127.0.0.1:8600) and proxies to it;
#   - B keeps its node ID across a reboot, and rejoins;
#   - an empty configuration stops the agent; no AVC denial.
#
# Usage: hack/qemu-consul-test.sh <disk.img> <janusctl-bin> <consul-bin>
#   (consul-bin: a host consul, for `consul keygen` and nothing else -
#   build/extensions/tree-consul-amd64/usr/local/sbin/consul)
#   CONSUL_DISCOVERY=1: don't fail on AVC denials - list them (for writing
#   the consul_t rules, with a UKI built UKI_SELINUX_ENFORCING=0 and
#   UKI_EXTRA_CMDLINE=sysctl.kernel.printk_ratelimit=0).
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

usage="usage: $0 <disk.img> <janusctl-bin> <consul-bin>"
SRC_DISK="${1:?$usage}"
CTL_BIN="${2:?$usage}"
CONSUL_BIN="${3:?$usage}"
TIMEOUT="${QEMU_CONSUL_TIMEOUT:-90}"
BASE="${QEMU_CONSUL_BASE_PORT:-$((18800 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
MCAST="${QEMU_CONSUL_MCAST:-230.0.0.$((RANDOM % 200 + 20)):$((20000 + RANDOM % 20000))}"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"

WORKDIR="$(mktemp -d)"
declare -A PID
cleanup() {
  for n in "${!PID[@]}"; do kill "${PID[$n]}" 2>/dev/null || true; done
  rm -rf "$WORKDIR"
}
trap cleanup EXIT
fail() {
  echo "Consul test FAILED: $*" >&2
  for n in a b; do
    [ -f "$WORKDIR/$n.log" ] && { echo "--- console $n ---" >&2; tail -150 "$WORKDIR/$n.log" >&2; }
  done
  exit 1
}

# node NAME INDEX: boot one node - http on BASE+10*i, API +1, metrics +2,
# its port 8090 (the frontend proxying to "web") +3.
node() {
  local n=$1 i=$2
  cp "$SRC_DISK" "$WORKDIR/$n.img"
  cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/$n.vars"
  qemu-system-x86_64 -accel kvm -accel tcg \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$WORKDIR/$n.vars" \
    -drive file="$WORKDIR/$n.img",format=raw,if=virtio \
    -nographic -display none -m 1G \
    -netdev "user,id=net0,hostfwd=tcp::$((BASE + 10 * i))-:8080,hostfwd=tcp::$((BASE + 10 * i + 1))-:9505,hostfwd=tcp::$((BASE + 10 * i + 2))-:10056,hostfwd=tcp::$((BASE + 10 * i + 3))-:8090" \
    -device "virtio-net-pci,netdev=net0,mac=52:54:00:aa:00:0$i" \
    -netdev "socket,id=lan,mcast=$MCAST,localaddr=127.0.0.1" \
    -device "virtio-net-pci,netdev=lan,mac=52:54:00:bb:00:0$i" \
    -serial file:"$WORKDIR/$n.log" &
  PID[$n]=$!
}
node a 1
node b 2

http() { curl -s -m 3 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$((BASE + 10 * $1))/" || true; }
for i in 1 2; do
  deadline=$((SECONDS + TIMEOUT))
  until [ "$(http $i)" = "200" ]; do
    [ "$SECONDS" -lt "$deadline" ] || fail "node $i never answered"
    sleep 1
  done
done
sleep 2
# Credentials from each node's console (each has its own PKI).
python3 - "$WORKDIR" <<'PY'
import re, sys
w = sys.argv[1]
for n in "ab":
    log = open(f"{w}/{n}.log", errors="replace").read().replace("\r", "")
    pems = re.findall(r"(-----BEGIN ([A-Z ]+)-----.*?-----END \2-----)", log, re.S)
    for (pem, _), name in zip(pems, ["ca.crt", "admin.crt", "admin.key"]):
        open(f"{w}/{n}-{name}", "w").write(pem + "\n")
PY
ctl() { # ctl NODE args...
  local n=$1 i
  shift
  i=$([ "$n" = a ] && echo 1 || echo 2)
  "$CTL_BIN" -endpoint "127.0.0.1:$((BASE + 10 * i + 1))" -ca "$WORKDIR/$n-ca.crt" -cert "$WORKDIR/$n-admin.crt" -key "$WORKDIR/$n-admin.key" "$@"
}
for n in a b; do ctl $n version >/dev/null || fail "no API on node $n"; done

# The shared LAN: eth1, static.
for n in a b; do
  i=$([ "$n" = a ] && echo 1 || echo 2)
  cat > "$WORKDIR/$n-net.json" <<EOF
{"interfaces": [
  {"name": "eth0", "mac": "52:54:00:aa:00:0$i", "mode": "ADDRESSING_MODE_DHCP"},
  {"name": "eth1", "mac": "52:54:00:bb:00:0$i", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.168.79.$i/24"]}
]}
EOF
  out="$(ctl $n network apply "$WORKDIR/$n-net.json" 2>&1)" || fail "network apply on $n: $out"
done
echo "  ok: both nodes on the shared LAN"

out="$(ctl b network consul status)"
grep -q "Saved: *no" <<<"$out" || fail "status before any configuration: $out"
grep -q "Service: *waiting" <<<"$out" || fail "the agent isn't waiting for its configuration: $out"

printf 'datacenter = "janus"\nbind_addr = "192.168.79.2"\nbogus_key = 1\n' > "$WORKDIR/bad.hcl"
if out="$(ctl b network consul check "$WORKDIR/bad.hcl" 2>&1)"; then fail "a broken configuration passed the check"; fi
grep -q "invalid config key bogus_key" <<<"$out" || fail "consul validate's complaint isn't shown: $out"
echo "  ok: a broken configuration is refused with consul validate's message"

# TLS for the RPC between agents: a CA, A's server certificate
# (server.<datacenter>.consul, what verify_server_hostname checks).
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=Consul test CA" \
  -keyout "$WORKDIR/ca.key" -out "$WORKDIR/ca.pem" 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=server.janus.consul" \
  -keyout "$WORKDIR/server.key" -out "$WORKDIR/server.csr" 2>/dev/null
printf 'subjectAltName=DNS:server.janus.consul,DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth,clientAuth\n' > "$WORKDIR/server.ext"
openssl x509 -req -in "$WORKDIR/server.csr" -CA "$WORKDIR/ca.pem" -CAkey "$WORKDIR/ca.key" -CAcreateserial \
  -days 1 -extfile "$WORKDIR/server.ext" -out "$WORKDIR/server.pem" 2>/dev/null
KEY="$("$CONSUL_BIN" keygen)"

cat > "$WORKDIR/a.hcl" <<EOF
datacenter       = "janus"
node_name        = "node-a"
server           = true
bootstrap_expect = 1
bind_addr        = "192.168.79.1"
encrypt          = "$KEY"
tls {
  defaults {
    ca_file         = "/run/janus/consul/files/ca.pem"
    cert_file       = "/run/janus/consul/files/server.pem"
    key_file        = "/run/janus/consul/files/server.key"
    verify_outgoing = true
  }
  internal_rpc {
    verify_server_hostname = true
  }
}
services {
  name = "web"
  port = 8080
  check {
    tcp      = "127.0.0.1:8080"
    interval = "2s"
  }
}
EOF
cat > "$WORKDIR/b.hcl" <<EOF
datacenter = "janus"
node_name  = "node-b"
bind_addr  = "192.168.79.2"
retry_join = ["192.168.79.1"]
encrypt    = "$KEY"
tls {
  defaults {
    ca_file         = "/run/janus/consul/files/ca.pem"
    verify_outgoing = true
  }
  internal_rpc {
    verify_server_hostname = true
  }
}
EOF
out="$(ctl a network consul apply -file "ca.pem=$WORKDIR/ca.pem" -file "server.pem=$WORKDIR/server.pem" -file "server.key=$WORKDIR/server.key" "$WORKDIR/a.hcl" 2>&1)" || fail "apply on A: $out"
out="$(ctl b network consul apply -file "ca.pem=$WORKDIR/ca.pem" "$WORKDIR/b.hcl" 2>&1)" || fail "apply on B: $out"
out="$(ctl a network consul get 2>&1)"
grep -q "# file: server.key" <<<"$out" || fail "A's files aren't listed: $out"
if grep -q "PRIVATE KEY" <<<"$out"; then fail "get returned a file's content"; fi
echo "  ok: both configured, with their files"

deadline=$((SECONDS + 120))
until out="$(ctl b network consul status 2>&1)" && grep -Eq "^Leader: +192.168.79.1:8300" <<<"$out" &&
  grep -Eq "^node-a +192.168.79.1:8301 +alive +server" <<<"$out" && grep -Eq "^node-b +192.168.79.2:8301 +alive +client" <<<"$out"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "B never joined A's cluster: $out"
  sleep 3
done
grep -q "^Node: *node-b (.*client, datacenter janus" <<<"$out" || fail "B's agent: $out"
NODE_ID="$(sed -n 's/^Node: *node-b (\([0-9a-f-]*\),.*/\1/p' <<<"$out")"
echo "  ok: B joined A's cluster (gossip encrypted, RPC over TLS), node ID $NODE_ID"

# B's HAProxy finds "web" through B's agent's DNS and proxies to it.
ctl b haproxy get-config > "$WORKDIR/b-haproxy.cfg"
cat >> "$WORKDIR/b-haproxy.cfg" <<'EOF'

resolvers consul
    nameserver consul 127.0.0.1:8600
    accepted_payload_size 8192
    hold valid 5s

backend web
    server-template web 3 _web._tcp.service.consul resolvers consul resolve-prefer ipv4 init-addr none check

frontend via-consul
    bind *:8090
    default_backend web
EOF
out="$(ctl b haproxy apply-config "$WORKDIR/b-haproxy.cfg" 2>&1)" || fail "B's haproxy.cfg: $out"
deadline=$((SECONDS + 60))
until [ "$(curl -s -m 3 "http://127.0.0.1:$((BASE + 23))/")" = "Janus: haproxy is up" ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "B's HAProxy never reached web through Consul: $(ctl b haproxy backends 2>&1)"
  sleep 2
done
ctl b haproxy backends | grep -q "192.168.79.1" || fail "the backend's server isn't A: $(ctl b haproxy backends)"
echo "  ok: B's HAProxy found web (A:8080) through Consul's DNS and proxies to it"

# B reboots: the same node ID, back in the cluster.
ctl b system reboot >/dev/null 2>&1 || true
sleep 5
deadline=$((SECONDS + TIMEOUT + 60))
until out="$(ctl b network consul status 2>&1)" && grep -Eq "^node-b +192.168.79.2:8301 +alive +client" <<<"$out"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "B didn't rejoin after its reboot: $out"
  sleep 3
done
grep -q "^Node: *node-b ($NODE_ID," <<<"$out" || fail "B's node ID changed across the reboot: $out"
echo "  ok: after a reboot B is back with the same node ID"

out="$(ctl b network consul apply /dev/null 2>&1)" || fail "removing B's configuration: $out"
out="$(ctl b network consul status)"
grep -q "Saved: *no" <<<"$out" || fail "B's configuration is still saved: $out"
grep -q "Service: *stopped" <<<"$out" || fail "B's agent didn't stop: $out"
echo "  ok: an empty configuration stops the agent"

denials="$(cat "$WORKDIR"/a.log "$WORKDIR"/b.log | grep -a "avc:.*denied" || true)"
if [ -n "$denials" ]; then
  if [ "${CONSUL_DISCOVERY:-}" = 1 ]; then
    echo "--- AVC denials (discovery) ---"
    sed -E 's/.*avc:  denied  //; s/pid=[0-9]+ //; s/ino=[0-9]+ //; s/ permissive=[01]//' <<<"$denials" | sort | uniq -c
  else
    echo "$denials" >&2
    fail "AVC denials under enforcing"
  fi
fi
echo "Consul test OK: a server and a client with gossip encryption and TLS from their files, HAProxy discovering a service through Consul's DNS, the node ID kept across a reboot, zero AVC denials"
