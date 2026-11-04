#!/usr/bin/env bash
# Proves the Controller's warm standby (internal/ha, docs/guide/
# controller-standby.md) with two native dashboardd - A and B - behind a
# small TCP forwarder standing for the VIP, and real enforcing nodes:
#   1. A set up (accounts, fleet), two nodes added by their pairing code;
#   2. B paired with A by a token from A's page (dashboardd standby): it
#      copies A every few seconds, its page and /api/ha/active say it's
#      the standby; a second Controller on B's data directory is refused;
#   3. an account and an enrollment token made on A reach B's copy;
#   4. A killed: dashboardd promote refuses while A answered recently,
#      -force promotes B - epoch 1 -, the VIP moves to B: A's account
#      signs in on B, both nodes' pages answer through B, a third node
#      registers through the VIP with A's enrollment token;
#   5. A started again finds B active above it: it starts as B's standby,
#      what it held set aside, none of its loops run, and copies B;
#   6. the switchover from B's page: B hands A its last copy and steps
#      down, A is the active at epoch 2 with the node B admitted;
#   7. the witness in a real Consul server (ACLs denying by default): A
#      keeps it at its epoch; the link between A and B cut, B promoted
#      with -force claims epoch 3 in it, and A - still running, cut off
#      from B - steps down through the witness alone;
#   8. no AVC denial on the nodes.
#
# Usage: hack/controller-standby-test.sh <disk.img> <dashboardd-bin>
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

usage="usage: $0 <disk.img> <dashboardd-bin>"
DISK="${1:?$usage}"
DASHBOARDD="$(realpath "${2:?$usage}")"
TIMEOUT="${CONTROLLER_STANDBY_TIMEOUT:-120}"
B="${CONTROLLER_STANDBY_BASE_PORT:-$((19460 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
A_MAIN=$((B + 1)) A_REG=$((B + 2)) B_MAIN=$((B + 3)) B_REG=$((B + 4)) VIP_MAIN=$((B + 5)) VIP_REG=$((B + 6))
# A and B reach each other through a link that can be cut: LINK_A to A's
# registration port, LINK_B to B's.
LINK_A=$((B + 7)) LINK_B=$((B + 8))
P_CONSUL=$((B + 40)) P_CSERVER=$((B + 41)) P_CSERF=$((B + 42))
CONSUL_NAME="janus-standby-consul-$B"
OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf)" >&2; exit 1; }
GATEWAY=10.0.2.2
PASSWORD=standby-test-admin-pw

W="$(mktemp -d)"
declare -A PID
cleanup() {
  for p in "${PID[@]}"; do kill "$p" 2>/dev/null || true; done
  for p in "${PID[@]}"; do wait "$p" 2>/dev/null || true; done
  docker rm -f -v "$CONSUL_NAME" >/dev/null 2>&1 || true
  if [ -d "$W/consul" ]; then sudo rm -rf "$W/consul"; fi # the image's user owns it now
  rm -rf "$W"
}
trap cleanup EXIT
trap 'echo "controller-standby-test: line $LINENO: $BASH_COMMAND failed" >&2' ERR
fail() {
  echo "Standby test FAILED: $*" >&2
  for l in "$W"/*.log; do
    [ -f "$l" ] && { echo "--- $(basename "$l") ---" >&2; grep -av "^$" "$l" | grep -a "ha:\|standby\|promot\|dashboardd\|error\|FAILED\|selfregister\|avc:" | tail -25 >&2; }
  done
  exit 1
}
wait_for() { # wait_for WHAT SECONDS COMMAND...
  local what=$1 secs=$2 deadline
  shift 2
  deadline=$((SECONDS + secs))
  until "$@" >/dev/null 2>&1; do
    [ "$SECONDS" -lt "$deadline" ] || fail "$what"
    sleep 1
  done
}

# --- the VIP: a TCP forwarder to A's or B's ports, by a file - A or B;
# a change closes the connections open to the other. The link between A
# and B is another: A = connected, B = cut (port 1: refused).
echo A >"$W/vip"
echo A >"$W/link"
cat >"$W/vip.py" <<'EOF'
import asyncio, sys
target_file, ports, open_conns, known = sys.argv[1], {}, set(), {}
for spec in sys.argv[2:]:
    listen, a, b = (int(p) for p in spec.split(":"))
    ports[listen] = {"A": a, "B": b}

def target():
    # Read at each connection; empty while echo rewrites it: the last.
    t = open(target_file).read().strip()
    if t:
        known["last"] = t
    return known.get("last", t)

async def pipe(r, w):
    try:
        while data := await r.read(65536):
            w.write(data)
            await w.drain()
    except Exception:
        pass
    finally:
        w.close()

def handler(listen):
    async def handle(r, w):
        who = target()
        try:
            ur, uw = await asyncio.open_connection("127.0.0.1", ports[listen][who])
        except OSError:
            w.close()
            return
        conn = (w, uw, who)
        open_conns.add(conn)
        try:
            await asyncio.gather(pipe(r, uw), pipe(ur, w))
        finally:
            open_conns.discard(conn)
    return handle

async def watch():
    last = target()
    while True:
        await asyncio.sleep(0.3)
        now = target()
        if now != last:
            last = now
            for w, uw, who in list(open_conns):
                if who != now:
                    w.close()
                    uw.close()

async def main():
    servers = [await asyncio.start_server(handler(p), "0.0.0.0", p) for p in ports]
    await asyncio.gather(watch(), *(s.serve_forever() for s in servers))

asyncio.run(main())
EOF
python3 "$W/vip.py" "$W/vip" "$VIP_MAIN:$A_MAIN:$B_MAIN" "$VIP_REG:$A_REG:$B_REG" >"$W/vip.log" 2>&1 &
PID[vip]=$!
python3 "$W/vip.py" "$W/link" "$LINK_A:$A_REG:1" "$LINK_B:$B_REG:1" >"$W/link.log" 2>&1 &
PID[link]=$!

ctl_a() { exec "$DASHBOARDD" -addr ":$A_MAIN" -register-addr ":$A_REG" -data-dir "$W/a" -advertise-address "$GATEWAY" -image-factory "" -releases-url "http://127.0.0.1:1/none"; }
ctl_b() { exec "$DASHBOARDD" -addr ":$B_MAIN" -register-addr ":$B_REG" -data-dir "$W/b" -advertise-address "$GATEWAY" -image-factory "" -releases-url "http://127.0.0.1:1/none"; }
start() { # start a|b: its log rotated, its PID kept
  [ -f "$W/$1.log" ] && cat "$W/$1.log" >>"$W/$1.prev"
  : >"$W/$1.log"
  JANUS_CONTROLLER_STANDBY_PULL=3s "ctl_$1" >>"$W/$1.log" 2>&1 </dev/null &
  PID[$1]=$!
}
answers() { curl -sk -m 3 -o /dev/null "https://127.0.0.1:$1/api/ha/active"; }
active_code() { curl -sk -m 3 -o /dev/null -w '%{http_code}' "https://127.0.0.1:$1/api/ha/active" || true; }
login() { # login PORT JAR [NAME PASSWORD]
  curl -sk -c "$2" -o /dev/null -w '%{http_code}' -X POST "https://127.0.0.1:$1/api/auth/login" -H 'Content-Type: application/json' \
    -d "{\"name\":\"${3:-admin}\",\"password\":\"${4:-$PASSWORD}\"}"
}
signs_in() { case "$(login "$@")" in 200 | 204) return 0 ;; esac; return 1; }
api() { # api PORT JAR METHOD PATH [BODY-FILE]
  local extra=()
  [ -n "${5:-}" ] && extra=(-H 'Content-Type: application/json' --data-binary @"$5")
  curl -sk -m 30 -b "$2" -X "$3" "https://127.0.0.1:$1$4" "${extra[@]}"
}
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }

# --- 1. A, its fleet, two nodes added by their pairing code ---
mkdir -p "$W/a" "$W/b"
start a
wait_for "A never answered" 30 answers "$A_MAIN"
JAR="$W/a.jar"
code="$(curl -sk -c "$JAR" -o /dev/null -w '%{http_code}' -X POST "https://127.0.0.1:$A_MAIN/api/auth/setup" -H 'Content-Type: application/json' -d "{\"password\":\"$PASSWORD\",\"mfa_required\":\"nobody\"}")"
case "$code" in 200|204) ;; *) fail "setting A up: $code" ;; esac
api "$A_MAIN" "$JAR" POST /api/fleet/setup >"$W/fleet-setup.json"
api "$A_MAIN" "$JAR" GET /api/fleet/recovery-kit >"$W/kit.age"
python3 -c 'import json,sys; s=json.load(open(sys.argv[1])); print(json.dumps({"kit": open(sys.argv[2]).read(), "passphrase": s["passphrase"]}))' "$W/fleet-setup.json" "$W/kit.age" >"$W/confirm.json"
api "$A_MAIN" "$JAR" POST /api/fleet/confirm "$W/confirm.json" | grep -q '"state"' || fail "confirming A's fleet"
api "$A_MAIN" "$JAR" GET /api/controller-info | json 'd["ca_cert_pem"]' >"$W/identity-ca.pem"

node_api() { case "$1" in n1) echo $((B + 11)) ;; n2) echo $((B + 12)) ;; n3) echo $((B + 13)) ;; esac; }
node_pair() { case "$1" in n1) echo $((B + 21)) ;; n2) echo $((B + 22)) ;; n3) echo $((B + 23)) ;; esac; }
boot() { # boot NODE [cidata image]
  local n=$1 extra=()
  cp --sparse=always "$DISK" "$W/$n.img"
  cp "$OVMF_VARS_TEMPLATE" "$W/$n.vars"
  [ -n "${2:-}" ] && extra=(-drive file="$2",format=raw,if=virtio)
  qemu-system-x86_64 -accel kvm -accel tcg \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$W/$n.vars" \
    -drive file="$W/$n.img",format=raw,if=virtio "${extra[@]}" \
    -nographic -display none -m 512M \
    -netdev "user,id=net0,hostfwd=tcp::$(node_api "$n")-:9505,hostfwd=tcp::$(node_pair "$n")-:9506" \
    -device virtio-net-pci,netdev=net0 \
    -serial file:"$W/$n.console" </dev/null &
  PID[$n]=$!
}
code_of() { { grep -ao 'pairing: code [0-9A-Z-]*' "$W/$1.console" || true; } | tail -1 | awk '{print $3}'; }
has_code() { [ -n "$(code_of "$1")" ]; }
boot n1
boot n2
declare -A NODE_ID
for n in n1 n2; do
  wait_for "$n shows no pairing code" "$TIMEOUT" has_code "$n"
  printf '{"name":"%s","address":"127.0.0.1:%s","pairing_address":"127.0.0.1:%s","code":"%s"}' "$n" "$(node_api "$n")" "$(node_pair "$n")" "$(code_of "$n")" >"$W/pair.json"
  out="$(api "$A_MAIN" "$JAR" POST /api/nodes "$W/pair.json")"
  NODE_ID[$n]="$(json 'd["id"]' <<<"$out")" || fail "adding $n by its code: $out"
done
page() { [ "$(curl -sk -m 5 -b "$2" -o /dev/null -w '%{http_code}' "https://127.0.0.1:$1/nodes/${NODE_ID[$3]}/api/info")" = 200 ]; }
for n in n1 n2; do wait_for "$n's page doesn't answer through A" 60 page "$A_MAIN" "$JAR" "$n"; done
echo "  ok: A set up, its fleet, n1 and n2 added by their pairing code"

# --- 2. B paired as A's standby ---
api "$A_MAIN" "$JAR" POST /api/ha/pairing >"$W/token.json"
TOKEN="$(json 'd["token"]' <"$W/token.json")"
case "$TOKEN" in janus-standby_*) ;; *) fail "no pairing token: $(cat "$W/token.json")" ;; esac
"$DASHBOARDD" standby -data-dir "$W/b" -peer "127.0.0.1:$A_REG" -pair "janus-standby_$(printf '0%.0s' {1..64})_x" -name b -address "127.0.0.1:$B_REG" >"$W/pair-wrong.out" 2>&1 \
  && fail "paired with a token naming another Controller"
grep -q "isn't the peer paired" "$W/pair-wrong.out" || fail "a token naming another Controller: $(cat "$W/pair-wrong.out")"
out="$("$DASHBOARDD" standby -data-dir "$W/b" -peer "127.0.0.1:$LINK_A" -pair "$TOKEN" -name b -address "127.0.0.1:$LINK_B" 2>&1)" || fail "dashboardd standby: $out"
grep -q "as its standby, epoch 0 - a first copy of" <<<"$out" || fail "dashboardd standby: $out"
if "$DASHBOARDD" standby -data-dir "$W/c" -peer "127.0.0.1:$A_REG" -pair "$TOKEN" -name c -address "127.0.0.1:1" >"$W/pair-twice.out" 2>&1; then fail "the token paired twice"; fi
start b
wait_for "B's standby page never answered" 30 answers "$B_MAIN"
grep -q "the standby of" "$W/b.log" || fail "B didn't start as the standby: $(cat "$W/b.log")"
[ "$(active_code "$B_MAIN")" = 503 ] && [ "$(active_code "$A_MAIN")" = 200 ] || fail "/api/ha/active: A $(active_code "$A_MAIN"), B $(active_code "$B_MAIN")"
curl -sk "https://127.0.0.1:$B_MAIN/" | grep -q "Janus Controller - standby" || fail "B's page doesn't say it's the standby"
second="$("$DASHBOARDD" -addr ":$((B + 30))" -register-addr ":$((B + 31))" -data-dir "$W/b" 2>&1 || true)"
grep -q "another Controller runs on" <<<"$second" || fail "a second Controller on B's data directory: $second"
api "$A_MAIN" "$JAR" GET /api/ha | json 'd["standby"]["name"]' | grep -qx b || fail "A doesn't show its standby: $(api "$A_MAIN" "$JAR" GET /api/ha)"
echo "  ok: B paired by A's token (another Controller's refused, the token good once), B the standby - its page, /api/ha/active 503 -, a second Controller on its data directory refused"

# --- 3. what's made on A reaches B ---
printf '{"name":"rita","role":"reader","password":"rita-standby-pw-1"}' >"$W/rita.json"
api "$A_MAIN" "$JAR" POST /api/users "$W/rita.json" | grep -q rita || fail "making rita on A"
printf '{"name":"rack-9","max_uses":1,"expires_in_days":1,"labels":{"rack":"r9"}}' >"$W/enroll.json"
ENROLL="$(api "$A_MAIN" "$JAR" POST /api/enroll-tokens "$W/enroll.json" | json 'd["token"]')"
made="$(date -u +%s)"
copied() { # copied [a|b]: the standby copied its active since $made
  python3 - "$W/${1:-b}/ha/replica.json" "$made" <<'EOF'
import json, sys, datetime
p = json.load(open(sys.argv[1]))
at = datetime.datetime.strptime(p["last"]["at"][:19], "%Y-%m-%dT%H:%M:%S").replace(tzinfo=datetime.timezone.utc).timestamp()
sys.exit(0 if at > int(sys.argv[2]) + 1 else 1)
EOF
}
wait_for "B never copied A after the changes" 30 copied
"$DASHBOARDD" ha status -data-dir "$W/b" >"$W/ha-status.out" || fail "dashboardd ha status"
grep -q "role:   standby" "$W/ha-status.out" && grep -q "copy:   .* ago" "$W/ha-status.out" || fail "ha status: $(cat "$W/ha-status.out")"
api "$A_MAIN" "$JAR" GET /api/ha | json 'd["last_copy"]["at"]' >/dev/null || fail "A doesn't show B's last copy"
echo "  ok: an account and an enrollment token made on A, B copied them (dashboardd ha status says so)"

# --- 4. A lost: B promoted, the VIP moves ---
kill -9 "${PID[a]}"
wait "${PID[a]}" 2>/dev/null || true
unset 'PID[a]'
out="$("$DASHBOARDD" promote -data-dir "$W/b" 2>&1)" && fail "promoted while A answered seconds ago: $out"
grep -q "may still run" <<<"$out" || fail "promote without -force: $out"
out="$("$DASHBOARDD" promote -data-dir "$W/b" -force 2>&1)" || fail "promote -force: $out"
grep -q "promoted - the active now, epoch 1" <<<"$out" || fail "promote -force: $out"
echo B >"$W/vip"
on_b() { [ "$(active_code "$VIP_MAIN")" = 200 ]; }
wait_for "B never came back as the active" 60 on_b
VJAR="$W/vip.jar"
signs_in "$VIP_MAIN" "$VJAR" || fail "A's admin can't sign in on B"
signs_in "$VIP_MAIN" "$W/rita.jar" rita rita-standby-pw-1 || fail "rita, made on A, can't sign in on B"
api "$VIP_MAIN" "$VJAR" GET /api/ha | json '"%s %s" % (d["role"], d["epoch"])' | grep -qx "active 1" || fail "B's /api/ha: $(api "$VIP_MAIN" "$VJAR" GET /api/ha)"
for n in n1 n2; do wait_for "$n's page doesn't answer through B" 60 page "$VIP_MAIN" "$VJAR" "$n"; done
grep -q "restore: first start on the backup" "$W/b.log" || fail "B doesn't say what to check after its promotion"
python3 - "$W/n3-user-data" "$GATEWAY:$VIP_REG" "$W/identity-ca.pem" "$ENROLL" <<'EOF'
import json, sys
open(sys.argv[1], "w").write(json.dumps({"controller_address": sys.argv[2], "controller_ca_cert": open(sys.argv[3]).read(), "registration_token": sys.argv[4]}))
EOF
truncate -s 1M "$W/n3-cidata.img"
mkfs.vfat -F 12 -n cidata "$W/n3-cidata.img" >/dev/null
mcopy -i "$W/n3-cidata.img" "$W/n3-user-data" ::user-data
boot n3 "$W/n3-cidata.img"
three() { [ "$(api "$VIP_MAIN" "$VJAR" GET /api/nodes | json 'len(d)')" = 3 ]; }
wait_for "n3 never registered through the VIP with A's enrollment token" "$TIMEOUT" three
api "$VIP_MAIN" "$VJAR" GET /api/nodes | grep -q '"rack":"r9"' || fail "n3 isn't labelled by A's enrollment token"
echo "  ok: A killed - promote refused without -force, -force made B the active at epoch 1: A's accounts sign in on B, n1 and n2 answer through B, n3 registered through the VIP with A's enrollment token"

# --- 5. A back: fenced, B's standby ---
start a
wait_for "A never answered again" 60 answers "$A_MAIN"
grep -q "starting as its standby" "$W/a.log" || fail "A didn't see B above it: $(cat "$W/a.log")"
[ "$(active_code "$A_MAIN")" = 503 ] || fail "A answers as the active"
ls -d "$W"/a/ha/previous/*/ >/dev/null 2>&1 || fail "A's data isn't set aside"
if grep -q "listening on :$A_MAIN (HTTPS only)\|registration endpoint listening" "$W/a.log"; then fail "A, fenced, started the active's servers"; fi
a_copied() { "$DASHBOARDD" ha status -data-dir "$W/a" | grep -q "copy:   .* ago"; }
wait_for "A never copied B" 30 a_copied
echo "  ok: A started again found B active at epoch 1: it is B's standby - its data set aside, no active's server -, and copies B"

# --- 6. the switchover from B's page ---
code="$(curl -sk -b "$VJAR" -o /dev/null -w '%{http_code}' -X POST "https://127.0.0.1:$VIP_MAIN/api/ha/switchover")"
[ "$code" = 202 ] || fail "switchover: $code"
a_active() { [ "$(active_code "$A_MAIN")" = 200 ]; }
wait_for "A never became the active" 60 a_active
echo A >"$W/vip"
b_standby() { [ "$(active_code "$B_MAIN")" = 503 ]; }
wait_for "B never became the standby" 60 b_standby
signs_in "$VIP_MAIN" "$VJAR" || fail "signing in on A after the switchover"
api "$VIP_MAIN" "$VJAR" GET /api/ha | json '"%s %s" % (d["role"], d["epoch"])' | grep -qx "active 2" || fail "A's /api/ha: $(api "$VIP_MAIN" "$VJAR" GET /api/ha)"
[ "$(api "$VIP_MAIN" "$VJAR" GET /api/nodes | json 'len(d)')" = 3 ] || fail "A lacks the node B admitted"
grep -q "handed over by the active" "$W/a.log" || fail "A didn't take B's handed-over copy"
grep -q "stepping down for the standby" "$W/b.log" || fail "B didn't step down"
for n in n1 n2; do wait_for "$n's page doesn't answer through A" 60 page "$VIP_MAIN" "$VJAR" "$n"; done
# A was a standby until now: its control socket goes at its start - the
# updater's backup refused it (v2026.11.02-2's lab); B, the standby, has its own.
[ ! -e "$W/a/ha/control.sock" ] || fail "A, the active again, kept its standby's control socket"
[ -S "$W/b/ha/control.sock" ] || fail "B, the standby, has no control socket"
# B's notes of its copies as A's standby (epoch 0) went when it stepped
# down: none shown as its copies of A at epoch 2 (v2026.11.02-2's lab).
if "$DASHBOARDD" ha status -data-dir "$W/b" 2>&1 | grep -qE "^copy: .* epoch 0,"; then
  fail "B shows a copy of its time as A's standby: $("$DASHBOARDD" ha status -data-dir "$W/b" 2>&1)"
fi
echo "  ok: the switchover from B's page - B handed A its last copy and stepped down, A the active at epoch 2 with the node B admitted, without its standby's socket"

# --- 7. the witness in Consul: a promotion behind a partition ---
# A real Consul server, HTTPS, ACLs denying by default; the Controller's
# token as docs/guide/consul-controller.md has it made, with the key the
# witness needs.
mkdir -p "$W/consul"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 -subj "/CN=Consul standby test" \
  -addext "subjectAltName=IP:127.0.0.1" -keyout "$W/consul/server.key" -out "$W/consul/server.pem" 2>/dev/null
MGMT="$(cat /proc/sys/kernel/random/uuid)"
cat >"$W/consul/server.hcl" <<HCL
datacenter       = "witness"
node_name        = "witness"
server           = true
bootstrap_expect = 1
data_dir         = "/consul/data"
bind_addr        = "127.0.0.1"
client_addr      = "127.0.0.1"
ports {
  http     = -1
  https    = $P_CONSUL
  dns      = -1
  grpc     = -1
  grpc_tls = -1
  server   = $P_CSERVER
  serf_lan = $P_CSERF
  serf_wan = -1
}
acl {
  enabled                  = true
  default_policy           = "deny"
  enable_token_persistence = true
  tokens {
    initial_management = "$MGMT"
    agent              = "$MGMT"
  }
}
tls {
  defaults {
    cert_file = "/consul/config/server.pem"
    key_file  = "/consul/config/server.key"
  }
  https {
    verify_incoming = false
  }
}
HCL
chmod 0755 "$W/consul"
chmod 0644 "$W/consul"/*
docker build -q -t janus-consul-server "$(dirname "$0")/consul-server" >/dev/null
docker rm -f -v "$CONSUL_NAME" >/dev/null 2>&1 || true
docker run -d --name "$CONSUL_NAME" --network host -v "$W/consul:/consul/config" janus-consul-server agent -config-dir=/consul/config >/dev/null
consul_api() { # consul_api METHOD PATH [BODY]: with the management token
  curl -s -m 10 --cacert "$W/consul/server.pem" -H "X-Consul-Token: $MGMT" -X "$1" "https://127.0.0.1:$P_CONSUL$2" ${3:+-d "$3"}
}
has_leader() { consul_api GET /v1/status/leader | grep -q "127.0.0.1:$P_CSERVER"; }
wait_for "the Consul server never elected itself" 60 has_leader
python3 -c 'import json; print(json.dumps({"Name": "janus-controller", "Rules": """acl = "write"
keyring = "read"
operator = "read"
node_prefix "" { policy = "write" }
service_prefix "" { policy = "read" }
agent_prefix "" { policy = "read" }
key_prefix "janus/controllers/" { policy = "write" }
"""}))' >"$W/policy.json"
consul_api PUT /v1/acl/policy "@$W/policy.json" | grep -q '"ID"' || fail "the Controller's Consul policy"
CTOKEN="$(consul_api PUT /v1/acl/token '{"Description": "janus-controller", "Policies": [{"Name": "janus-controller"}]}' | json 'd["SecretID"]')" ||
  fail "the Controller's Consul token"
python3 - "$P_CONSUL" "$W/consul/server.pem" "$CTOKEN" >"$W/consul-settings.json" <<'PY'
import json, sys
print(json.dumps({"enabled": True, "address": "https://127.0.0.1:" + sys.argv[1], "tls": {"ca_pem": open(sys.argv[2]).read()}, "token": sys.argv[3],
                  "selector": "", "services": {"janus": True, "exporters": True, "frontends": True, "urls": True}, "node_meta_from_labels": True}))
PY
made="$(date -u +%s)"
api "$VIP_MAIN" "$VJAR" PUT /api/consul "$W/consul-settings.json" | grep -q '"has_token": *true' || fail "A's Consul settings"
witness_is() { # witness_is PORT JAR "STATE HOLDER EPOCH"
  [ "$(api "$1" "$2" GET /api/ha | json '" ".join(str((d.get("witness") or {}).get(k)) for k in ("state", "holder", "epoch"))')" = "$3" ]
}
wait_for "A never wrote the witness in Consul: $(api "$VIP_MAIN" "$VJAR" GET /api/ha)" 60 witness_is "$VIP_MAIN" "$VJAR" "ok self 2"
consul_api GET "/v1/kv/janus/controllers/?recurse" | json '[e["Key"] for e in d]' | grep -q "/witness" || fail "no witness key in Consul"
wait_for "B never copied A's Consul settings" 30 copied b
echo "  ok: Consul set up on A - it keeps the witness in Consul's KV at epoch 2; B copied the settings"

# The link cut: A and B can't reach each other; both reach Consul.
standby_starts="$(grep -c "the standby of" "$W/a.log" || true)"
echo B >"$W/link"
out="$("$DASHBOARDD" promote -data-dir "$W/b" 2>&1)" && fail "promoted without -force while A answered seconds ago: $out"
out="$("$DASHBOARDD" promote -data-dir "$W/b" -force 2>&1)" || fail "promote -force behind the partition: $out"
grep -q "epoch 3 (the witness consul https://127.0.0.1:$P_CONSUL key janus/controllers/" <<<"$out" || fail "promote -force: $out"
b_active() { [ "$(active_code "$B_MAIN")" = 200 ]; }
wait_for "B never came back as the active" 60 b_active
a_replaced() { [ "$(active_code "$A_MAIN")" = 503 ]; }
wait_for "A, cut off from B, never stepped down through the witness" 60 a_replaced
grep -q "ha: the witness says the peer .* is the active at epoch 3" "$W/a.log" || fail "A doesn't say the witness made it step down"
a_standby_again() { [ "$(grep -c "the standby of" "$W/a.log" || true)" -gt "$standby_starts" ]; }
wait_for "A never started again as B's standby" 30 a_standby_again
consul_api GET "/v1/kv/janus/controllers/?recurse" | json '[json.loads(__import__("base64").b64decode(e["Value"]))["epoch"] for e in d]' | grep -qx "\[3\]" ||
  fail "the witness in Consul: $(consul_api GET "/v1/kv/janus/controllers/?recurse")"
echo B >"$W/vip"
BJAR="$W/b.jar"
signs_in "$VIP_MAIN" "$BJAR" || fail "signing in on B after the forced promotion"
wait_for "B doesn't hold the witness: $(api "$VIP_MAIN" "$BJAR" GET /api/ha)" 60 witness_is "$VIP_MAIN" "$BJAR" "ok self 3"
# The link back: A, B's standby, copies it.
made="$(date -u +%s)"
echo A >"$W/link"
wait_for "A never copied B once the link was back" 30 copied a
echo "  ok: the link cut, promote refused without -force; -force made B the active at epoch 3 in the witness, and A - cut off from B - stepped down through it; the link back, A copies B"

# --- 8. no AVC denial ---
for n in n1 n2 n3; do
  if grep -a "avc:.*denied" "$W/$n.console"; then fail "AVC denials on $n"; fi
done
echo "Controller standby test OK: pairing, copies, a forced promotion behind a VIP, the old active fenced at its start, a switchover back, a forced promotion behind a partition fencing the old active through the witness in Consul - real enforcing nodes, zero AVC denials"
