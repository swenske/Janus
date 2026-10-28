#!/usr/bin/env bash
# A cluster the Controller updates in place (docs/guide/clusters.md
# "Updating a cluster", #375), from one published release to another -
# the real, signed bundles on GitHub - on real enforcing nodes and a
# native dashboardd from this tree.
#
# Two nodes boot release FROM's janus-kvm.qcow2 (checked against
# GitHub's digest), each with a user-mode NIC the Controller reaches
# (hostfwd) and a shared multicast LAN where they reach each other; they
# get the Controller's fleet and make cluster edge through its API, a
# stick table replicated. POST /api/clusters/{id}/upgrade to TO: what can
# be checked first is refused at once (not a release, an unknown member);
# then each member in turn is drained, fetches TO's bundle from GitHub
# itself (its signature checked by the node), reboots into its other
# slot, comes back - in service on its own: FROM keeps no drain across a
# reboot, so the engine drains it again until its tables are learnt -,
# and is undrained. It passes when both members run TO on slot B, in
# service, the stick table entry kept, the journal complete - and no AVC
# denial.
#
# Needs the internet (GitHub). Usage:
#   hack/qemu-controller-cluster-upgrade-test.sh <from-release> <to-release> <dashboardd-bin> <janusctl-bin>
set -euo pipefail
export PATH="$PATH:/usr/sbin:/sbin"

USAGE="usage: $0 <from-release> <to-release> <dashboardd-bin> <janusctl-bin>"
FROM="${1:?$USAGE}"
TO="${2:?$USAGE}"
DASHBOARDD="${3:?$USAGE}"
CTL_BIN="${4:?$USAGE}"
REPO="${GITHUB_REPOSITORY:-swenske/Janus}"
TIMEOUT="${QEMU_CCUPGRADE_TIMEOUT:-150}"
BASE="${QEMU_CCUPGRADE_BASE_PORT:-$((19340 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
MCAST="${QEMU_CCUPGRADE_MCAST:-230.0.0.$((RANDOM % 200 + 20)):$((20000 + RANDOM % 20000))}"
OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
NODES="1 2"
P_DASH=$BASE
P_REG=$((BASE + 1))
API="https://127.0.0.1:$P_DASH"

WORKDIR="$(mktemp -d)"
declare -A PID
cleanup() {
  for n in "${!PID[@]}"; do kill "${PID[$n]}" 2>/dev/null || true; done
  if [ "${QEMU_CCUPGRADE_KEEP:-}" = 1 ]; then echo "kept $WORKDIR" >&2; else rm -rf "$WORKDIR"; fi
}
trap cleanup EXIT

fail() {
  echo "Controller cluster upgrade test FAILED: $*" >&2
  for i in $NODES; do
    [ -f "$WORKDIR/$i.log" ] && { echo "--- console $i ---" >&2; tail -60 "$WORKDIR/$i.log" >&2; }
  done
  [ -f "$WORKDIR/dash.log" ] && { echo "--- dashboardd ---" >&2; tail -60 "$WORKDIR/dash.log" >&2; }
  exit 1
}

wait_for() { # wait_for SECONDS WHAT COMMAND...: polls COMMAND every second
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

# Release FROM's disk, as GitHub publishes it.
curl -fsSL -H "Accept: application/vnd.github+json" ${GITHUB_TOKEN:+-H "Authorization: Bearer $GITHUB_TOKEN"} \
  "https://api.github.com/repos/$REPO/releases/tags/$FROM" > "$WORKDIR/release.json"
read -r URL DIGEST < <(python3 -c '
import json, sys
for a in json.load(open(sys.argv[1]))["assets"]:
    if a["name"] == "janus-kvm.qcow2":
        print(a["browser_download_url"], a["digest"].removeprefix("sha256:"))' "$WORKDIR/release.json")
[ -n "${DIGEST:-}" ] || fail "$FROM has no janus-kvm.qcow2 with a digest"
curl -fsSL -o "$WORKDIR/janus.qcow2" "$URL"
echo "$DIGEST  $WORKDIR/janus.qcow2" | sha256sum -c --quiet - || fail "janus-kvm.qcow2 isn't what GitHub says it is"
qemu-img convert -O raw "$WORKDIR/janus.qcow2" "$WORKDIR/from.img"
rm -f "$WORKDIR/janus.qcow2"

node() { # node INDEX: http on BASE+10*i, API on +1
  local i=$1
  cp --sparse=always "$WORKDIR/from.img" "$WORKDIR/$i.img"
  cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/$i.vars"
  qemu-system-x86_64 -accel kvm -accel tcg \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$WORKDIR/$i.vars" \
    -drive file="$WORKDIR/$i.img",format=raw,if=virtio \
    -nographic -display none -monitor none -m 1024M \
    -netdev "user,id=net0,hostfwd=tcp::$((BASE + 10 * i))-:8080,hostfwd=tcp::$((BASE + 10 * i + 1))-:9505" \
    -device "virtio-net-pci,netdev=net0,mac=52:54:00:aa:03:0$i" \
    -netdev "socket,id=lan,mcast=$MCAST,localaddr=127.0.0.1" \
    -device "virtio-net-pci,netdev=lan,mac=52:54:00:bb:03:0$i" \
    -serial file:"$WORKDIR/$i.log" </dev/null &
  PID[$i]=$!
}
for i in $NODES; do node "$i"; done
for i in $NODES; do
  deadline=$((SECONDS + TIMEOUT))
  until grep -aq "listening on .* (mTLS required)" "$WORKDIR/$i.log" 2>/dev/null; do
    [ "$SECONDS" -lt "$deadline" ] || fail "node $i's janusd never listened"
    sleep 1
  done
done
sleep 2
python3 - "$WORKDIR" "$NODES" <<'PY'
import re, sys
w = sys.argv[1]
for n in sys.argv[2].split():
    log = open(f"{w}/{n}.log", errors="replace").read().replace("\r", "")
    pems = re.findall(r"(-----BEGIN ([A-Z ]+)-----.*?-----END \2-----)", log, re.S)
    for (pem, _), name in zip(pems, ["ca.crt", "admin.crt", "admin.key"]):
        open(f"{w}/{n}-{name}", "w").write(pem + "\n")
PY
ctl() { # ctl INDEX args... - the node's own admin, straight to it
  local i=$1
  shift
  "$CTL_BIN" -endpoint "127.0.0.1:$((BASE + 10 * i + 1))" -ca "$WORKDIR/$i-ca.crt" -cert "$WORKDIR/$i-admin.crt" -key "$WORKDIR/$i-admin.key" "$@"
}
for i in $NODES; do
  cat > "$WORKDIR/$i-net.json" <<EOF
{"hostname": "lb-$i", "interfaces": [
  {"name": "eth0", "mac": "52:54:00:aa:03:0$i", "mode": "ADDRESSING_MODE_DHCP"},
  {"name": "eth1", "mac": "52:54:00:bb:03:0$i", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.168.88.$i/24"]}
]}
EOF
  out="$(ctl "$i" network apply "$WORKDIR/$i-net.json" 2>&1)" || fail "network apply on $i: $out"
done
for i in $NODES; do check "lb-$i runs $FROM" "^Node: $FROM$" "$(ctl "$i" version)"; done
echo "  ok: two nodes up on Janus $FROM (GitHub's janus-kvm.qcow2), on the LAN as lb-1, lb-2"

# The Controller, its fleet, the nodes.
"$DASHBOARDD" -addr "127.0.0.1:$P_DASH" -register-addr "127.0.0.1:$P_REG" -data-dir "$WORKDIR/data" -image-factory "" > "$WORKDIR/dash.log" 2>&1 &
PID[dash]=$!
for _ in $(seq 30); do curl -sk -o /dev/null "$API/api/auth/status" && break; sleep 1; done
JAR="$WORKDIR/jar"
api() { # api METHOD PATH [JSON] - the body; fails on an HTTP error
  local out code
  out="$(curl -sk -b "$JAR" -c "$JAR" -X "$1" -H 'Content-Type: application/json' ${3:+--data-binary "$3"} -w '\n%{http_code}' "$API$2")"
  code="${out##*$'\n'}"
  out="${out%$'\n'*}"
  [ "${code:0:1}" = 2 ] || { echo "$1 $2: $code $out" >&2; return 1; }
  printf '%s' "$out"
}
jq_() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
api POST /api/auth/setup '{"password":"cluster-test-admin-pw","mfa_required":"nobody"}' >/dev/null || fail "setup"
declare -A ID
for i in $NODES; do
  body="$(python3 - "$WORKDIR" "$i" "$((BASE + 10 * i + 1))" <<'PY'
import json, sys
w, i, port = sys.argv[1:]
r = lambda n: open(f"{w}/{i}-{n}").read()
print(json.dumps({"name": f"lb-{i}", "address": f"127.0.0.1:{port}", "ca_cert_pem": r("ca.crt"), "bootstrap_cert_pem": r("admin.crt"), "bootstrap_key_pem": r("admin.key")}))
PY
)"
  ID[$i]="$(api POST /api/nodes "$body" | jq_ 'd["id"]')" || fail "add lb-$i"
done
fleet="$(api POST /api/fleet/setup)" || fail "fleet setup"
kit="$(api GET /api/fleet/recovery-kit)" || fail "recovery kit"
pass="$(jq_ 'd["passphrase"]' <<<"$fleet")"
api POST /api/fleet/confirm "$(python3 -c 'import json,sys; print(json.dumps({"kit": sys.argv[1], "passphrase": sys.argv[2]}))' "$kit" "$pass")" >/dev/null || fail "fleet confirm"
trusted() {
  [ "$(api GET /api/fleet | python3 -c "import json,sys; d=json.load(sys.stdin); print(sum(1 for i in sys.argv[1:] if d.get('nodes',{}).get(i,{}).get('state')=='trusted'))" "${ID[@]}")" = 2 ]
}
wait_for 120 "the nodes to trust the fleet" trusted

cat > "$WORKDIR/v1.cfg" <<'EOF'
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
    stick-table type ip size 10k expire 1h store http_req_cnt peers janus
    http-request track-sc0 src
    http-request return status 200 content-type text/plain lf-string "%[var(proc.janus_member)]\n"
EOF
out="$(ctl 1 haproxy apply-config "$WORKDIR/v1.cfg" 2>&1)" || fail "apply-config on lb-1: $out"
create="$(python3 - "${ID[1]}" "${ID[2]}" <<'PY'
import json, sys
ids = sys.argv[1:]
print(json.dumps({"name": "edge", "source": ids[0],
  "members": [{"node_id": ids[i], "name": f"lb-{i+1}", "address": f"192.168.88.{i+1}"} for i in range(2)]}))
PY
)"
CID="$(api POST /api/clusters "$create" | jq_ 'd["id"]')" || fail "create the cluster"
idle() { [ "$(api GET "/api/clusters/$CID" | jq_ 'd["busy"]')" = False ]; }
cluster_error() { api GET "/api/clusters/$CID" | jq_ 'd.get("error","")'; }
wait_for 180 "the cluster created" idle
err="$(cluster_error)"
[ -z "$err" ] || fail "the cluster's creation: $err"
peers_up() { [ "$(ctl "$1" cluster status 2>/dev/null | grep -cE '^lb-[0-9]+ +192\.168\.88\.[0-9]+:10057 +up ')" = "$2" ]; }
for i in $NODES; do wait_for 60 "lb-$i's peers up" peers_up "$i" 1; done
curl -s --max-time 5 "http://127.0.0.1:$((BASE + 10))/" >/dev/null
has_entry() { grep -Eq '10\.0\.2\.2' <<<"$(ctl "$1" haproxy table fe 2>/dev/null)"; }
wait_for 20 "lb-2 learnt lb-1's entry" has_entry 2
api POST "/api/clusters/$CID/check" '{}' >/dev/null || fail "check"
wait_for 60 "the check" idle
echo "  ok: the Controller's cluster edge of lb-1 and lb-2 on $FROM, a stick table replicated"

# Refused before anything is touched.
code() { curl -sk -b "$JAR" -o "$WORKDIR/code.out" -w '%{http_code}' -X POST -H 'Content-Type: application/json' --data-binary "$2" "$API$1"; }
[ "$(code "/api/clusters/$CID/upgrade" '{"version": "latest"}')" = 400 ] || fail "a version that isn't a release: $(cat "$WORKDIR/code.out")"
[ "$(code "/api/clusters/$CID/upgrade" "{\"version\": \"$TO\", \"members\": [\"lb-9\"]}")" = 400 ] || fail "an unknown member: $(cat "$WORKDIR/code.out")"
check "the unknown member named" "lb-9 isn't a member" "$(cat "$WORKDIR/code.out")"
for i in $NODES; do check "lb-$i untouched by the refusals" "^Node: $FROM$" "$(ctl "$i" version)"; done
echo "  ok: refused at once - a version that isn't a release, an unknown member"

# The update.
api POST "/api/clusters/$CID/upgrade" "{\"version\": \"$TO\"}" >/dev/null || fail "POST upgrade"
sleep 2
check "the cluster says it updates" "upgrading" "$(api GET "/api/clusters/$CID" | jq_ 'd.get("phase","")')"
wait_for 1800 "the update" idle
err="$(cluster_error)"
[ -z "$err" ] || fail "the update: $err"
journal="$(api GET "/api/clusters/$CID/journal?n=200" | python3 -c 'import json,sys; [print(e["message"]) for e in reversed(json.load(sys.stdin))]')"
echo "$journal" | grep -E 'updat|drain|reboot|runs Janus|back in service|balancer' | sed 's/^/    /'
check "the order" "updating edge to Janus $TO, one at a time: lb-1, lb-2" "$journal"
for i in $NODES; do
  check "lb-$i drained" "lb-$i drained" "$journal"
  check "lb-$i updated" "lb-$i updating from Janus $FROM \(slot A\) to $TO" "$journal"
  check "lb-$i drained again on its way back" "lb-$i came back in service on its own" "$journal"
  check "lb-$i back on slot B" "lb-$i runs Janus $TO on slot B, in service" "$journal"
  v="$(ctl "$i" version)"
  check "lb-$i runs $TO" "^Node: $TO$" "$v"
  check "lb-$i on slot B" "^Active slot: B$" "$v"
  if ctl "$i" cluster get | grep -q "Drained"; then fail "lb-$i still drained after the update"; fi
  has_entry "$i" || fail "lb-$i lost the stick table entry"
  wait_for 60 "lb-$i's peers up after the update" peers_up "$i" 1
done
check "the update's end" "edge runs Janus $TO: 2 member\(s\) updated, 0 already on it" "$journal"
view="$(api GET "/api/clusters/$CID")"
check "the view says each member's release" "\"release\": ?\"$TO\"" "$view"
echo "  ok: lb-1 then lb-2 updated by the Controller to $TO from GitHub's signed bundle - each drained, back on slot B, drained again until its tables were learnt ($FROM keeps no drain across a reboot), in service; the stick table entry kept"

for i in $NODES; do
  if grep -a "avc:.*denied" "$WORKDIR/$i.log"; then fail "AVC denials on node $i"; fi
done
echo "Controller cluster upgrade test PASSED: cluster edge updated in place by the Controller from $FROM to $TO, one member at a time - refusals first, each member drained, its own signed A/B update, back on slot B, drained again until its tables were learnt, in service; the stick table kept; no AVC denial"
