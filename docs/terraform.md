# Terraform: Janus nodes as code

`terraform-provider-janus` lets Terraform - or OpenTofu, or Terragrunt
on top of either - tell a Janus Controller which nodes to have. The
Controller does the hypervisor's work ([hypervisors.md](hypervisors.md)):
- it creates each node's virtual machine and boots it with its network
  and a registration token;
- it admits the node;
- it changes nodes in place;
- it destroys them.

The provider only ever talks to the Controller's API, never to a
hypervisor or a node.

It's built from this repository for now (`make terraform-provider-build`,
`terraform-provider-janus/`). It isn't published on the Terraform and
OpenTofu registries yet.

## Setting it up

1. **An API token**: the Controller's **API tokens** tab, **New token**.
   It's shown once: keep it in your secret store.
   - It has your rights over the API except managing tokens.
   - Revoke it there.
   - It's sent as `Authorization: Bearer`.
2. **The Controller's certificate**: the Controller's self-signed
   identity, unless you gave it a real one. Get it from its **Provision**
   panel, or `GET /api/controller-info` (`ca_cert_pem`). The provider
   checks the Controller against it, so the token never goes to whoever
   answers.
3. **The provider binary**: `make terraform-provider-build` gives
   `bin/terraform-provider-janus`. Point Terraform at its directory with
   a development override in `~/.terraformrc` (OpenTofu: `~/.tofurc`, or
   `TF_CLI_CONFIG_FILE`):

   ```hcl
   provider_installation {
     dev_overrides {
       "swenske/janus" = "/path/to/Janus/bin"
     }
     direct {}
   }
   ```

   With an override, Terraform says so on every run, and `init` isn't
   needed for this provider.

```hcl
terraform {
  required_providers {
    janus = { source = "swenske/janus" }
  }
}

provider "janus" {
  endpoint = "https://controller.example.net"   # or JANUS_ENDPOINT
  ca_cert  = "controller-ca.crt"                # PEM or a file; or JANUS_CA_CERT
  # token: JANUS_TOKEN - keep it out of the configuration
}
```

## Resources

### `janus_hypervisor`

A libvirt host the Controller creates nodes on. Prepare the host first:
[hypervisors.md](hypervisors.md), steps 1 to 5.

```hcl
resource "janus_hypervisor" "kvm01" {
  name     = "kvm01"
  host     = "kvm01.example.net"
  user     = "janus-ctl"
  pool     = "janus"
  networks = ["lan", "dmz"]
  # Read on the host: ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
  host_key_fingerprint = "SHA256:KASpY7hkwHVkvQBhNXcmojERkTMna6SV+uxg8rRr52E"
}

output "authorized_key" { value = janus_hypervisor.kvm01.authorized_key }
```

- **Trust.** The host is trusted only if it presents the key whose
  fingerprint is given: the Controller checks it. Without one, the
  hypervisor isn't trusted, and no node can be created on it.
- **`authorized_key`** is the line its account needs in
  `~/.ssh/authorized_keys`. Put it there with your configuration
  management, or by hand.

Optional:
- `name_prefix` (default `janus-`);
- `socket`;
- `controller_address`: where its nodes reach the Controller.

A hypervisor that already exists on the Controller can be read instead:
`data "janus_hypervisor" "kvm01" { name = "kvm01" }`.

### `janus_node`

```hcl
resource "janus_node" "lb1" {
  name          = "lb1"
  hypervisor_id = janus_hypervisor.kvm01.id
  vcpus         = 2
  memory_mib    = 2048
  version       = "v2026.10.02-4"           # unset: the newest
  extensions    = ["keepalived", "bird"]    # the image factory builds it

  interfaces = [
    { network = "lan", name = "mgmt", mode = "static", addresses = ["10.0.0.21/24"] },
    { network = "dmz", name = "front", mode = "static", addresses = ["192.0.2.21/24"], gateway = "192.0.2.1" },
  ]
  dns = ["10.0.0.53"]
  ntp = ["10.0.0.1"]
}
```

Creating a node waits until it's admitted. Its creation:
1. the image;
2. the virtual machine;
3. the boot;
4. its registration on its token.

A node whose creation fails is tainted: the next apply destroys it and
starts again. The machine's history on the Controller (**Hypervisors**
tab) and its console say why.

What changes **in place** - each change runs on the node, and the node
stays the same:

| Change | What happens |
|---|---|
| `interfaces` addresses, gateways, modes and names; `dns`; `ntp` | Put on trial on the node and confirmed from wherever it's reachable afterwards. Unconfirmed, the node goes back by itself. What only its page sets - VLANs, MTUs, search domains - stays. |
| `vcpus`, `memory_mib`; an interface added, removed or moved to another `network` | The node shuts down cleanly (HAProxy stops), the virtual machine is reconfigured and started again. A removed interface leaves the node's configuration before it's unplugged; an added one is configured once plugged in. |
| `version`, `extensions` | The node's own A/B update. The bundle is the release's, or the image factory's build of the new schematic. The node checks its signature, and confirms itself healthy. |

What makes **a new node** (Terraform plans a replacement): `name`,
`hypervisor_id`, `image`.

**Interfaces keep their MAC by their name.** Removing the first
interface doesn't hand its MAC to the second. Renamed in place (same
position, same network), an interface keeps its MAC too. An interface
with no MAC to keep is a new one, and the Controller chooses its MAC.
Constraints:
- The interface the Controller reaches the node through can't be
  removed or moved.
- An added interface is `static` or `none`: DHCP only works on the one
  the node booted with.

**`lock_ui`** (default `true`) locks the node against changes from the
Controller's pages: its hardware, network, version and extensions, and
destroying it. Those pages still show it, restart it and open its
console. Released on the Controller for a change by hand, that change
shows in the next plan; `apply` undoes it and locks the node again.

**Drift.** The provider reads the node as it is: the Controller reads it
from the node and the hypervisor before answering. A change made
anywhere else - the node's page, `janusctl`, the hypervisor - shows in
`terraform plan`, to adopt in your configuration or undo with `apply`.

**After a failed change**, the node keeps running: the steps that
succeeded stay applied. The next plan shows what's still to do, and the
error is on the machine.

**`version`**: unset, it reads back as whatever the node runs. A node
created with the newest release then keeps it until you set one.

**Old images.** An image released before registration tokens - before
the release that introduced them - registers like any node. It waits in
the Controller's **Waiting for approval**, so its creation waits too
until it's approved; approving it links it to its machine.

`image = { url, sha256 }` uses another disk image: a mirror, or a
development build. Its version and extensions can't change in place.

**Computed:**
- `id`, the machine's ID on the Controller;
- `node_id`, `node_address`, `vm_name`, `schematic`;
- each interface's `mac`, unless you set it.

**Import:** `terraform import janus_node.lb1 <machine-id>`, the machine's
ID from `GET /api/machines`.

**Timeouts:** create 30 min, update 30 min, delete 10 min, through a
`timeouts = { create = "45m" }` attribute. An extension the image
factory has to build first can take a while.

## Terragrunt

Nothing specific: one Controller per environment through the provider
block (`endpoint`, `ca_cert`), the token in `JANUS_TOKEN`.

## Tests

`make terraform-provider-test` (`hack/terraform-provider-test.sh`, run
by `image-build.yml`) drives the provider with OpenTofu (pinned in
`versions.mk`) against a real Controller and libvirt host - the
container of [hypervisors.md](hypervisors.md)'s test. It checks:
- an API token, and that a token can't create another;
- `janus_hypervisor`, trusted on the host's own fingerprint;
- `janus_node`, created and admitted;
- memory, then a static address, changed in place;
- a new name planning a replacement;
- an imported node;
- destroy.

After each step it checks that a plan has nothing left to change.

Unit tests run in `ci.yml`: the client, the schemas, what a change
sends, and the state read back.
