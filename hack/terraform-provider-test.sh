#!/usr/bin/env bash
# The Janus Terraform provider (terraform-provider-janus), driven by
# OpenTofu against a real Controller and a real libvirt host (the
# container of hack/libvirt-host, as hack/controller-libvirt-test.sh):
#
#   1. an API token from the Controller, its CA as the provider's ca_cert;
#   2. janus_hypervisor: added and trusted on the host key fingerprint
#      read on the host, its authorized_key installed there;
#   3. janus_node: created from the image under test, admitted, then a
#      plan with nothing to change;
#   4. in place: more memory (a clean stop, resize, start), then a static
#      address (on trial, confirmed where the node is now) - each followed
#      by a plan with nothing to change;
#   5. locked: the Controller's pages can't change it; released, a change
#      made there shows in the next plan, and the apply undoes it and
#      locks it again;
#   6. an interface added, then removed - in place;
#   7. a new name plans a replacement; an imported node plans nothing;
#   8. labels on the node; a user without a role over everything,
#      operator on team=web over HAProxy only, terraforms the node's
#      haproxy.cfg with a token narrowed the same way - HAProxy refusing
#      a bad one -, and may change nothing else;
#   9. destroy: nothing left;
#  10. the docs' example (examples/terraform/libvirt), applied as written:
#      two nodes admitted and serving examples/haproxy/web.cfg.
#
# Without /dev/net/tun no machine boots (a CI runner without it): only
# the hypervisor's part runs, with a CI warning.
#
# Usage: hack/terraform-provider-test.sh <janus-kvm.qcow2> <dashboardd-bin (static)> <terraform-provider-janus-bin> <tofu-bin>
set -euo pipefail

usage="usage: $0 <janus-kvm.qcow2> <dashboardd-bin> <provider-bin> <tofu-bin>"
abs() { echo "$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"; }
IMAGE="$(abs "${1:?$usage}")"
DASHBOARDD="$(abs "${2:?$usage}")"
PROVIDER="$(abs "${3:?$usage}")"
TOFU="$(abs "${4:?$usage}")"
[ -c /dev/kvm ] || { echo "terraform-provider test needs /dev/kvm" >&2; exit 1; }

HOST_PORT="${TERRAFORM_PROVIDER_TEST_PORT:-$((18280 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
TEST_NAME=terraform-provider
# shellcheck source=libvirt-host/lib.sh
. "$(dirname "$0")/libvirt-host/lib.sh"

lh_start
echo "Part 1 OK: libvirt host and Controller running"

# =========================================================================
# 1. A token and the Controller's CA for the provider.
# =========================================================================
TOKEN="$(api -X POST "$API/api/tokens" -H 'Content-Type: application/json' -d '{"name":"terraform-test","expires_in_days":1}' | json "d['token']")"
[ -n "$TOKEN" ] || fail "no API token"
api "$API/api/controller-info" | json "d['ca_cert_pem']" >"$WORKDIR/controller-ca.crt"
# A token can't make another one.
code="$(curl -sk -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" -X POST "$API/api/tokens" -H 'Content-Type: application/json' -d '{"name":"x"}')"
[ "$code" = 403 ] || fail "a token could create a token ($code)"

export JANUS_ENDPOINT="$API" JANUS_TOKEN="$TOKEN" JANUS_CA_CERT="$WORKDIR/controller-ca.crt"
mkdir -p "$WORKDIR/provider" "$WORKDIR/tf"
cp "$PROVIDER" "$WORKDIR/provider/terraform-provider-janus"
cat >"$WORKDIR/tofurc" <<EOF
provider_installation {
  dev_overrides {
    "swenske/janus" = "$WORKDIR/provider"
  }
  direct {}
}
EOF
export TF_CLI_CONFIG_FILE="$WORKDIR/tofurc" TF_IN_AUTOMATION=1
FINGERPRINT="$(in_host ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub | awk '{print $2}')"
SUM="$(sha256sum "$IMAGE" | cut -d' ' -f1)"

cat >"$WORKDIR/tf/main.tf" <<'EOF'
terraform {
  required_providers {
    janus = { source = "swenske/janus" }
  }
}

provider "janus" {}

variable "fingerprint" { type = string }
variable "image_sha256" { type = string }
variable "nodes" { default = 0 }
variable "name" { default = "tf-node1" }
variable "memory" { default = 1024 }
variable "address" { default = "" }
variable "extra_nic" { default = false }
variable "labels" {
  type    = map(string)
  default = null
}

resource "janus_hypervisor" "host" {
  name                 = "testhost"
  host                 = "127.0.0.1"
  user                 = "janus-ctl"
  pool                 = "janus"
  networks             = ["janus-test", "janus-test2"]
  host_key_fingerprint = var.fingerprint
}

resource "janus_node" "n" {
  count         = var.nodes
  name          = var.name
  labels        = var.labels
  hypervisor_id = janus_hypervisor.host.id
  vcpus         = 1
  memory_mib    = var.memory
  image = {
    url    = "http://127.0.0.1:8000/janus-kvm.qcow2"
    sha256 = var.image_sha256
  }
  interfaces = concat([{
    network   = "janus-test"
    name      = "lan"
    mode      = var.address == "" ? "dhcp" : "static"
    addresses = var.address == "" ? null : [var.address]
    gateway   = var.address == "" ? null : "192.168.123.1"
    }], var.extra_nic ? [{
    network   = "janus-test2"
    name      = "backend"
    mode      = "static"
    addresses = ["192.168.124.50/24"]
    gateway   = null
  }] : [])
}

output "authorized_key" { value = janus_hypervisor.host.authorized_key }
output "node_address" { value = try(janus_node.n[0].node_address, "") }
output "node_id" { value = try(janus_node.n[0].id, "") }
output "janus_node_id" { value = try(janus_node.n[0].node_id, "") }
EOF
tofu() { (cd "$WORKDIR/tf" && "$TOFU" "$@" -no-color -var "fingerprint=$FINGERPRINT" -var "image_sha256=$SUM" 2>&1); }
tofu_bare() { (cd "$WORKDIR/tf" && "$TOFU" "$@" 2>&1); }
tofu_out() { (cd "$WORKDIR/tf" && "$TOFU" output -raw "$1"); }
apply() {
  tofu apply -auto-approve "$@" >"$WORKDIR/apply.log" || fail "apply $*: $(tail -30 "$WORKDIR/apply.log")"
}
# A plan with nothing to change: the state matches the configuration.
no_changes() {
  local what="$1" rc=0
  shift
  tofu plan -detailed-exitcode "$@" >"$WORKDIR/plan.log" || rc=$?
  [ "$rc" = 0 ] || fail "a plan after $what isn't empty (exit $rc): $(grep -vE '^\s*$' "$WORKDIR/plan.log" | tail -30)"
}

# =========================================================================
# 2. janus_hypervisor, trusted on the fingerprint read on the host.
# =========================================================================
apply
[ "$(api "$API/api/hypervisors" | json "d[0]['trusted']")" = True ] || fail "the hypervisor isn't trusted"
tofu_out authorized_key | in_host_i sh -c 'cat >> /home/janus-ctl/.ssh/authorized_keys && chown janus-ctl: /home/janus-ctl/.ssh/authorized_keys && chmod 600 /home/janus-ctl/.ssh/authorized_keys'
no_changes "adding the hypervisor"
echo "Part 2 OK: janus_hypervisor added and trusted on the host's own fingerprint, its key authorized, nothing left to plan"

if [ ! -c /dev/net/tun ]; then
  tofu destroy -auto-approve >"$WORKDIR/destroy.log" || fail "destroy: $(tail -20 "$WORKDIR/destroy.log")"
  [ "$(api "$API/api/hypervisors" | json "len(d)")" = 0 ] || fail "the hypervisor wasn't removed"
  echo "::warning::terraform-provider test: no /dev/net/tun on this host, so no node boots here - janus_node is only checked where it exists"
  echo "terraform-provider test OK (janus_hypervisor only)"
  exit 0
fi

# =========================================================================
# 3. janus_node, created and admitted.
# =========================================================================
apply -var nodes=1
NODE_MID="$(tofu_out node_id)"
addr="$(tofu_out node_address)"
case "$addr" in 192.168.123.*:9505) ;; *) fail "node_address is $addr" ;; esac
[ "$(api "$API/api/machines/$NODE_MID" | json "d['phase']")" = ready ] || fail "the machine isn't ready"
no_changes "creating the node" -var nodes=1
echo "Part 3 OK: janus_node created and admitted at $addr, nothing left to plan"

# =========================================================================
# 4. In place: memory, then a static address.
# =========================================================================
apply -var nodes=1 -var memory=1536
grep -q "1 changed" "$WORKDIR/apply.log" || fail "the memory change wasn't in place: $(grep -E 'Plan:|Apply complete' "$WORKDIR/apply.log")"
mem="$(in_host virsh dominfo janus-tf-node1 | awk '/Max memory/ {print $3}')"
[ "$mem" = $((1536 * 1024)) ] || fail "the virtual machine has $mem KiB"
no_changes "the memory change" -var nodes=1 -var memory=1536
echo "Part 4 OK: memory changed in place (the domain has 1536 MiB, the node back), nothing left to plan"

apply -var nodes=1 -var memory=1536 -var address=192.168.123.50/24
grep -q "1 changed" "$WORKDIR/apply.log" || fail "the address change wasn't in place: $(grep -E 'Plan:|Apply complete' "$WORKDIR/apply.log")"
addr="$(tofu_out node_address)"
[ "$addr" = "192.168.123.50:9505" ] || fail "after the address change, node_address is $addr"
node_id="$(api "$API/api/machines/$NODE_MID" | json "d['node_id']")"
[ "$(api "$API/api/nodes/status" | json "d['$node_id']['reachable']")" = True ] || fail "the node isn't reachable at its new address"
no_changes "the address change" -var nodes=1 -var memory=1536 -var address=192.168.123.50/24
echo "Part 5 OK: a static address applied in place (on trial, confirmed at 192.168.123.50), nothing left to plan"

# =========================================================================
# 5. Locked: its pages can't change it. Released, a change made there is a
#    difference in the next plan, which the apply undoes - locking it again.
# =========================================================================
V="-var nodes=1 -var memory=1536 -var address=192.168.123.50/24"
[ "$(api "$API/api/machines/$NODE_MID" | json "d['spec'].get('locked')")" = True ] || fail "the node isn't locked"
code="$(api -o /dev/null -w '%{http_code}' -X PATCH "$API/api/machines/$NODE_MID" -H 'Content-Type: application/json' -d '{"memory_mib":2048}')"
[ "$code" = 423 ] || fail "a page changed a locked node ($code)"
code="$(api -o /dev/null -w '%{http_code}' -X DELETE "$API/api/machines/$NODE_MID")"
[ "$code" = 423 ] || fail "a page destroyed a locked node ($code)"
api -X PATCH "$API/api/machines/$NODE_MID" -H 'Content-Type: application/json' -d '{"locked":false}' | json "d['spec'].get('locked')" | grep -q None || fail "release"
code="$(api -o /dev/null -w '%{http_code}' -X PATCH "$API/api/machines/$NODE_MID" -H 'Content-Type: application/json' -d '{"memory_mib":2048}')"
[ "$code" = 202 ] || fail "a page couldn't change a released node ($code)"
for _ in $(seq 1 90); do
  [ "$(api "$API/api/machines/$NODE_MID" | json "d['phase']")" = ready ] && break
  sleep 2
done
rc=0
# shellcheck disable=SC2086
tofu plan -detailed-exitcode $V >"$WORKDIR/plan.log" || rc=$?
[ "$rc" = 2 ] || fail "a change made on the Controller didn't show in the plan (exit $rc): $(tail -20 "$WORKDIR/plan.log")"
grep -q 'memory_mib *= *2048 -> 1536' "$WORKDIR/plan.log" || fail "the plan doesn't undo the memory: $(grep -E 'memory|lock' "$WORKDIR/plan.log")"
grep -q 'lock_ui *= *false -> true' "$WORKDIR/plan.log" || fail "the plan doesn't lock it again: $(grep -E 'lock' "$WORKDIR/plan.log")"
# shellcheck disable=SC2086
apply $V
mem="$(in_host virsh dominfo janus-tf-node1 | awk '/Max memory/ {print $3}')"
[ "$mem" = $((1536 * 1024)) ] || fail "the apply didn't undo the memory: $mem KiB"
[ "$(api "$API/api/machines/$NODE_MID" | json "d['spec'].get('locked')")" = True ] || fail "the apply didn't lock it again"
# shellcheck disable=SC2086
no_changes "undoing a change made on the Controller" $V
echo "Part 6 OK: locked (a page's change and destroy refused); released, a change made on the Controller showed in the plan, the apply undid it and locked it again"

# =========================================================================
# 6. An interface added, then removed - in place, its MAC kept by name.
# =========================================================================
# shellcheck disable=SC2086
apply $V -var extra_nic=true
grep -q "1 changed" "$WORKDIR/apply.log" || fail "adding an interface wasn't in place: $(grep -E 'Plan:|replaced' "$WORKDIR/apply.log")"
[ "$(in_host virsh domiflist janus-tf-node1 | grep -c janus-test)" = 2 ] || fail "the virtual machine hasn't 2 interfaces: $(in_host virsh domiflist janus-tf-node1)"
api "$API/api/machines/$NODE_MID" | python3 -c "
import json,sys; d=json.load(sys.stdin)
n=[x for x in d['spec']['nics'] if x['name']=='backend']
assert n and n[0]['addresses']==['192.168.124.50/24'], d['spec']['nics']" || fail "the added interface isn't configured on the node"
# shellcheck disable=SC2086
no_changes "adding an interface" $V -var extra_nic=true
# shellcheck disable=SC2086
apply $V
grep -q "1 changed" "$WORKDIR/apply.log" || fail "removing an interface wasn't in place"
[ "$(in_host virsh domiflist janus-tf-node1 | grep -c janus-test)" = 1 ] || fail "the interface wasn't removed: $(in_host virsh domiflist janus-tf-node1)"
# shellcheck disable=SC2086
no_changes "removing an interface" $V
echo "Part 7 OK: an interface added (plugged in, configured) and removed (unconfigured, unplugged) in place"

# =========================================================================
# 7. A new name is a new node; an imported node plans nothing.
# =========================================================================
tofu plan -var nodes=1 -var memory=1536 -var address=192.168.123.50/24 -var name=tf-node2 >"$WORKDIR/plan.log" || true
grep -q "must be replaced" "$WORKDIR/plan.log" || fail "a new name doesn't plan a replacement: $(grep -E 'Plan:|replaced' "$WORKDIR/plan.log")"
tofu_bare state rm -no-color 'janus_node.n[0]' >/dev/null || fail "state rm"
tofu import -var nodes=1 -var memory=1536 -var address=192.168.123.50/24 'janus_node.n[0]' "$NODE_MID" >"$WORKDIR/import.log" || fail "import: $(tail -20 "$WORKDIR/import.log")"
no_changes "importing the node" -var nodes=1 -var memory=1536 -var address=192.168.123.50/24
echo "Part 8 OK: a new name plans a replacement; the node imported by its ID plans nothing"

# =========================================================================
# 8. Labels; a user who may only terraform HAProxy on team=web.
# =========================================================================
V="-var nodes=1 -var memory=1536 -var address=192.168.123.50/24"
# shellcheck disable=SC2086
apply $V -var 'labels={team="web"}'
JNODE="$(tofu_out janus_node_id)"
[ "$(api "$API/api/nodes" | json "[n for n in d if n['id']=='$JNODE'][0]['labels']")" = "{'team': 'web'}" ] || fail "the node's labels: $(api "$API/api/nodes")"
# shellcheck disable=SC2086
no_changes "labelling the node" $V -var 'labels={team="web"}'

# Scoped access is through the fleet: a node the Controller still reaches
# with its own service credential opens for admins only.
FLEET_PASS="$(api -X POST "$API/api/fleet/setup" | json "d['passphrase']")"
api "$API/api/fleet/recovery-kit" -o "$WORKDIR/fleet-kit.age"
python3 -c 'import json,sys; print(json.dumps({"kit": open(sys.argv[1]).read(), "passphrase": sys.argv[2]}))' "$WORKDIR/fleet-kit.age" "$FLEET_PASS" >"$WORKDIR/fleet-confirm.json"
[ "$(api -o /dev/null -w '%{http_code}' -X POST "$API/api/fleet/confirm" -H 'Content-Type: application/json' --data-binary @"$WORKDIR/fleet-confirm.json")" = 200 ] || fail "confirming the fleet"
for _ in $(seq 1 60); do
  [ "$(api "$API/api/fleet" | json "d['nodes'].get('$JNODE', {}).get('state', '')")" = trusted ] && break
  sleep 2
done
[ "$(api "$API/api/fleet" | json "d['nodes'].get('$JNODE', {}).get('state', '')")" = trusted ] || fail "the node doesn't trust the fleet: $(api "$API/api/fleet")"

WEB_JAR="$WORKDIR/web-dev.jar"
api -o /dev/null -X POST "$API/api/users" -H 'Content-Type: application/json' \
  -d '{"name":"web-dev","role":"none","password":"web-dev-given-pw","grants":[{"role":"operator","selector":{"team":"web"},"domains":["haproxy"]}]}'
curl -sk -c "$WEB_JAR" -o /dev/null -X POST "$API/api/auth/login" -H 'Content-Type: application/json' -d '{"name":"web-dev","password":"web-dev-given-pw"}'
curl -sk -b "$WEB_JAR" -c "$WEB_JAR" -o /dev/null -X POST "$API/api/auth/password" -H 'Content-Type: application/json' -d '{"current_password":"web-dev-given-pw","new_password":"web-dev-own-password"}'
WEB_TOKEN="$(curl -sk -b "$WEB_JAR" -X POST "$API/api/tokens" -H 'Content-Type: application/json' \
  -d '{"name":"terraform-haproxy","expires_in_days":1,"scope":{"selector":{"team":"web"},"domains":["haproxy"]}}' | json "d['token']")"
[ -n "$WEB_TOKEN" ] || fail "no token for web-dev"
mkdir -p "$WORKDIR/tf-web"
api "$API/nodes/$JNODE/api/haproxy/config" | json "d['config']" >"$WORKDIR/tf-web/haproxy.cfg"
printf '\n# terraformed by web-dev\n' >>"$WORKDIR/tf-web/haproxy.cfg"
printf 'global\n  this-is-not-a-keyword\n' >"$WORKDIR/tf-web/bad.cfg"
cat >"$WORKDIR/tf-web/main.tf" <<'TFEOF'
terraform {
  required_providers {
    janus = { source = "swenske/janus" }
  }
}

provider "janus" {}

variable "node" { type = string }
variable "file" { default = "haproxy.cfg" }

resource "janus_haproxy_config" "web" {
  node   = var.node
  config = file(var.file)
}
TFEOF
tofu_web() { (cd "$WORKDIR/tf-web" && JANUS_TOKEN="$WEB_TOKEN" "$TOFU" "$@" -no-color -var "node=$JNODE" 2>&1); }
tofu_web apply -auto-approve >"$WORKDIR/apply-web.log" || fail "web-dev's apply: $(grep -A12 'Error' "$WORKDIR/apply-web.log" | head -20)"
api "$API/nodes/$JNODE/api/haproxy/config" | json "d['config']" | grep -q '# terraformed by web-dev' || fail "the node doesn't have web-dev's configuration"
rc=0
tofu_web plan -detailed-exitcode >"$WORKDIR/plan-web.log" || rc=$?
[ "$rc" = 0 ] || fail "a plan after web-dev's apply isn't empty (exit $rc): $(tail -20 "$WORKDIR/plan-web.log")"
if tofu_web apply -auto-approve -var file=bad.cfg >"$WORKDIR/apply-bad.log"; then fail "a configuration HAProxy refuses was applied"; fi
grep -q 'the node refused the configuration' "$WORKDIR/apply-bad.log" || fail "the bad configuration refused, but not by HAProxy: $(grep -A12 'Error' "$WORKDIR/apply-bad.log" | head -20)"
api "$API/nodes/$JNODE/api/haproxy/config" | json "d['config']" | grep -q '# terraformed by web-dev' || fail "the refused configuration changed the node's"
# Its token changes nothing else: not the machine, not a service, not the Controller.
code="$(curl -sk -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $WEB_TOKEN" -X PATCH "$API/api/machines/$NODE_MID" -H 'Content-Type: application/json' -d '{"memory_mib":2048}')"
[ "$code" = 403 ] || fail "web-dev's token resized the machine ($code)"
code="$(curl -sk -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $WEB_TOKEN" -X POST "$API/nodes/$JNODE/api/system/services/haproxy/restart")"
[ "$code" = 403 ] || fail "web-dev's token restarted a service ($code)"
[ "$(curl -sk -H "Authorization: Bearer $WEB_TOKEN" "$API/api/hypervisors")" = "[]" ] || fail "web-dev's token sees the hypervisors"
tofu_web destroy -auto-approve >"$WORKDIR/destroy-web.log" || fail "web-dev's destroy: $(tail -10 "$WORKDIR/destroy-web.log")"
api "$API/nodes/$JNODE/api/haproxy/config" | json "d['config']" | grep -q '# terraformed by web-dev' || fail "destroying janus_haproxy_config changed the node's configuration"
echo "Part 9 OK: labels on janus_node; web-dev - operator on team=web over HAProxy only - terraformed its haproxy.cfg with a token narrowed the same way (a bad one refused by HAProxy, nothing changed), and may resize nothing, restart nothing, see no hypervisor"

# =========================================================================
# 9. Destroy.
# =========================================================================
tofu destroy -auto-approve -var nodes=1 -var memory=1536 -var address=192.168.123.50/24 -var 'labels={team="web"}' >"$WORKDIR/destroy.log" || fail "destroy: $(tail -30 "$WORKDIR/destroy.log")"
[ "$(api "$API/api/machines" | json "len(d)")" = 0 ] || fail "machines left: $(api "$API/api/machines")"
[ "$(api "$API/api/hypervisors" | json "len(d)")" = 0 ] || fail "the hypervisor wasn't removed"
if in_host virsh list --all --name | grep -q janus-tf; then fail "a virtual machine is left"; fi
echo "Part 10 OK: destroyed - the node, its virtual machine and the hypervisor"

# =========================================================================
# 10. The docs' end-to-end example, examples/terraform/libvirt, applied as
#     written - only its variables given (docs/private-cloud/platforms/
#     kvm-libvirt.md): the hypervisor, its key authorized, then two nodes
#     created and admitted, each serving examples/haproxy/web.cfg.
# =========================================================================
EX="$WORKDIR/example"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "$EX/terraform"
cp -r "$REPO/examples/terraform/libvirt" "$EX/terraform/libvirt"
cp -r "$REPO/examples/haproxy" "$EX/haproxy"
cat >"$EX/terraform/libvirt/terraform.tfvars" <<TFVARS
hypervisor_name      = "kvm-example"
hypervisor_host      = "127.0.0.1"
host_key_fingerprint = "$FINGERPRINT"
network              = "janus-test"
gateway              = "192.168.123.1"
nodes                = { ex1 = "192.168.123.61/24", ex2 = "192.168.123.62/24" }
memory_mib           = 1024
image                = { url = "http://127.0.0.1:8000/janus-kvm.qcow2", sha256 = "$SUM" }
TFVARS
tofu_ex() { (cd "$EX/terraform/libvirt" && "$TOFU" "$@" -no-color 2>&1); }
tofu_ex apply -auto-approve -target=janus_hypervisor.kvm >"$WORKDIR/apply-example-hv.log" || fail "the example's hypervisor: $(grep -A12 'Error' "$WORKDIR/apply-example-hv.log" | head -20)"
tofu_ex output -raw authorized_key | in_host_i sh -c 'cat >> /home/janus-ctl/.ssh/authorized_keys'
tofu_ex apply -auto-approve >"$WORKDIR/apply-example.log" || fail "the example's apply: $(grep -A12 'Error' "$WORKDIR/apply-example.log" | head -20)"
for n in ex1:192.168.123.61 ex2:192.168.123.62; do
  name="${n%%:*}" ip="${n#*:}"
  id="$(api "$API/api/nodes" | json "[x['id'] for x in d if x['name'] == '$name'][0]")"
  api "$API/nodes/$id/api/haproxy/config" | json "d['config']" >"$WORKDIR/example-$name.cfg"
  cmp -s "$WORKDIR/example-$name.cfg" "$REPO/examples/haproxy/web.cfg" || fail "$name doesn't run examples/haproxy/web.cfg"
  got="$(in_host python3 -c 'import sys, urllib.request; print(urllib.request.urlopen(sys.argv[1], timeout=10).read().decode().strip())' "http://$ip/healthz" 2>&1 || true)"
  [ "$got" = ok ] || fail "$name doesn't answer ok on $ip/healthz: $got"
done
rc=0
tofu_ex plan -detailed-exitcode >"$WORKDIR/plan-example.log" || rc=$?
[ "$rc" = 0 ] || fail "a plan after the example's apply isn't empty (exit $rc): $(tail -20 "$WORKDIR/plan-example.log")"
tofu_ex destroy -auto-approve >"$WORKDIR/destroy-example.log" || fail "the example's destroy: $(tail -20 "$WORKDIR/destroy-example.log")"
[ "$(api "$API/api/machines" | json "len(d)")" = 0 ] || fail "the example left machines: $(api "$API/api/machines")"
echo "Part 11 OK: the docs' example applied as written - two nodes created, admitted and serving examples/haproxy/web.cfg, nothing left to plan, destroyed"

echo "terraform-provider test OK"
