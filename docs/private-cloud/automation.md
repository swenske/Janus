# Automated deployment

A Janus node has no shell, no SSH and no agent to run a playbook in:
automating Janus means calling APIs. Three tools do it, each for what
it's made for:

| Tool | What it manages | Reaches |
|---|---|---|
| **Terraform / OpenTofu** with the Janus provider | Hypervisors, nodes - their virtual machine, network, size, version and extensions -, their labels and their `haproxy.cfg` | The Controller's API, with an API token |
| **The Controller's API** | All of the above, and machines' power and consoles - for scripts and other tools | The Controller, with an API token |
| **`janusctl`** | Everything a node does: VRRP, BGP, the firewall, Consul, Let's Encrypt, certificates, updates, logs... | Each node's API directly, with a certificate of the fleet |
| **Your own orchestrator**, instead of the Controller | Everything a node does, from its contracts - in any language ([your own orchestrator](own-orchestrator.md)) | Each node's API directly, with certificates of a fleet it keeps |

The usual split: Terraform owns the nodes and their HAProxy
configuration; `janusctl` - in the same pipeline - applies what
Terraform doesn't cover yet.

## Terraform and OpenTofu

The Janus provider tells a Controller which nodes to have; the
Controller does the hypervisor's work, and the provider never talks to
a hypervisor or a node itself. Setting it up - the API token, the
Controller's certificate, the provider binary attached to every release
- and every resource: [Terraform](../terraform.md). Complete
configurations, applied as written by the tests:
[`examples/terraform/libvirt`](../../examples/terraform/libvirt/) and
[`examples/terraform/proxmox`](../../examples/terraform/proxmox/), walked
through in the [libvirt/KVM](platforms/kvm-libvirt.md) and [Proxmox
VE](platforms/proxmox.md) guides.

What it gives a pipeline:

- **Nodes changed in place**: addresses, DNS and NTP on trial with an
  automatic revert; vCPUs, memory and interfaces with a clean restart;
  versions and extensions with the node's own A/B update, which reverts
  by itself if HAProxy doesn't come back healthy.
- **Drift shows in the plan**: the provider reads each node from the
  node and its hypervisor, so a change made by hand appears in the next
  `plan`.
- **Locked pages**: a node Terraform manages refuses changes from the
  Controller's pages (`lock_ui`), so a click doesn't undo the code.
- **Narrow tokens**: a team that terraforms only its HAProxy
  configuration gets a token limited to the `haproxy` domain of the
  nodes labelled for it - it can't resize, restart or destroy anything
  ([the example](../terraform.md#janus_haproxy_config)).

## The Controller's API

Everything the Controller's pages do is an HTTP API, behind an API
token for a program: the **API tokens** tab, sent as `Authorization:
Bearer`. A token acts as its account, with its role or a lower one, and
can be narrowed to some nodes' labels and domains.

```sh
export JANUS=https://controller.example.net:8080 JANUS_TOKEN=janus_...
api() { curl -fsS --cacert controller-ca.crt -H "Authorization: Bearer $JANUS_TOKEN" "$@"; }

api "$JANUS/api/nodes"                                    # the nodes the token reaches
api "$JANUS/api/machines" -X POST -H 'Content-Type: application/json' -d '{
  "name": "lb3", "hypervisor_id": "...", "vcpus": 2, "memory_mib": 2048,
  "nics": [{"network": "lan", "name": "front", "mode": "static",
            "addresses": ["192.0.2.23/24"], "gateway": "192.0.2.1"}]}'
api "$JANUS/api/machines/<id>"                            # its phase: ... ready
api "$JANUS/nodes/<node-id>/api/haproxy/config" -X POST -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json, sys; print(json.dumps({"config": open(sys.argv[1]).read()}))' haproxy.cfg)"
```

- Hypervisors and machines - adding, trusting, creating, changing,
  powering, destroying, following a console:
  [the hypervisors' API](../hypervisors.md#the-api). A creation answers
  `202` at once and runs in the background: poll the machine's `phase`.
- A node's HAProxy configuration: `GET` and `POST
  /nodes/<node-id>/api/haproxy/config` (`{"config": "..."}`, answered
  with `{"accepted": ..., "message": ...}` - HAProxy's own message for a
  configuration it refuses).
- Labels: `PATCH /api/nodes/<id>` with `{"labels": {...}}`.
- Enrollment tokens are made by an admin on the Controller's page, not
  with an API token: a token can't mint the admission of new machines.

## `janusctl` in a pipeline

`janusctl` reaches nodes directly, with a short-lived certificate of the
fleet. In CI, it gets one with an API token:

```sh
export JANUS_TOKEN=janus_...          # a token of the pipeline's account
janusctl login -controller controller.example.net:8080 -controller-ca controller-ca.crt
janusctl nodes                        # the nodes the token reaches
janusctl -n lb1,lb2 network vrrp apply keepalived.conf
janusctl -all haproxy show-info
```

The certificate lasts an hour, renewed while `JANUS_TOKEN` is set, with
the token's role - and its scope, when the token is narrowed: each node
checks what it may do itself. `-n` takes several nodes (each one's
output prefixed), `-all` every node of the context. Every command, and
how to sign in otherwise: [janusctl](../janusctl.md), [the
reference](../guide/janusctl-reference.md).

Every change a node can't afford to get wrong has a way back built in:
`network apply` and `network firewall apply` put the change on trial and
confirm it from wherever the node is reachable afterwards, or the node
reverts by itself; `lifecycle upgrade -wait-for-health` reverts an update
whose HAProxy isn't healthy.

## Many machines at once: enrollment tokens

For a batch provisioned outside the Controller - a rack of bare-metal
machines, VMs another tool creates - an admin makes an **enrollment
token** on the Controller's Nodes page: a name, a number of uses, an
expiry, labels. Every machine given it (`registration_token` in NoCloud,
`-registration-token` on `janusctl lifecycle install` or `image
seed-controller`) is admitted at once with those labels, so the grants
and tokens that pick nodes by label pick it up too. Past its uses, its
date, or once revoked, a machine waits for approval like any other.
Only the token's SHA-256 is kept.
