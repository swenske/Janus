# Proxmox VE, end to end

Two Janus load balancers on a Proxmox VE node, created by a Janus
Controller through Proxmox's API with a token scoped to one pool, and
configured by Terraform (or OpenTofu) - then a virtual IP between them.

> [!NOTE]
> Proven: the Controller's Proxmox VE support, and the provider driving
> it, on a real Proxmox VE 9.2 node - a hypervisor trusted on its
> certificate's fingerprint, a node created and admitted, changed in
> place, destroyed. This example is validated by OpenTofu against the
> provider on every push (`make examples-check`).

## What you need

- **A Proxmox VE node** (9.x), a storage for the machines' disks
  (`local-lvm` below) and the bridge - or VLANs on one - the nodes go on.
- **A host for the Controller**: Docker with Compose. It reaches
  Proxmox's API (8006) and the nodes on port 9505; the nodes reach it on
  8443.
- **OpenTofu or Terraform**, and `janusctl` for the virtual IP
  ([janusctl](../../janusctl.md#installing-janusctl)).
- **Addresses**: two for the nodes, one for the virtual IP, on the
  bridge's network - `192.0.2.21`, `192.0.2.22` and `192.0.2.100` below.

## 1. The Controller

As in the [libvirt guide](kvm-libvirt.md#1-the-controller): the Compose
setup, the first account, **Secure your fleet**, and the Controller's
certificate as `controller-ca.crt`.

## 2. The Proxmox VE node

The Controller writes the node's preparation with your values: a
`janus` pool its machines go in - the token sees no other virtual
machine at all -, a directory storage of its own for the images and the
NoCloud volumes (`janus-images`), roles with only what it uses, a
`janus-ctl@pve` user and its API token, rights on the networks it may
use and nothing else. In the Controller: **Hypervisors › Add
hypervisor**, kind **Proxmox VE**, fill in the URL, the node, the pool,
the storages and the networks, then **Show host preparation** - and
cancel: Terraform adds the hypervisor itself. Run it as root on the
node. Its step 4 shows the token's secret, **once**: put it in your
secret store. What each step does: [preparing a Proxmox VE
node](../../hypervisors.md#preparing-a-proxmox-ve-node).

Its step 5 prints the API certificate's fingerprint, which the
Controller will trust:

```sh
openssl x509 -noout -fingerprint -sha256 -in /etc/pve/local/pveproxy-ssl.pem
```

## 3. Terraform's access

An **API token** of an admin's account (the **API tokens** tab), and
the provider binary from the release that matches your Controller
([setting it up](../../terraform.md#setting-it-up)), then:

```sh
export JANUS_ENDPOINT=https://controller.example.net:8080
export JANUS_CA_CERT=$PWD/controller-ca.crt
export JANUS_TOKEN=janus_...                 # from your secret store
export TF_VAR_pve_token_secret=...           # the Proxmox token's secret, step 2
```

## 4. The configuration

[`examples/terraform/proxmox`](../../../examples/terraform/proxmox/):

```hcl title="examples/terraform/proxmox/main.tf"
# Two Janus load balancers on a Proxmox VE node, created by a Janus
# Controller through the node's API with a token scoped to a pool, and
# serving ../../haproxy/web.cfg - the end-to-end example of
# docs/private-cloud/platforms/proxmox.md.
#
# The provider reads the Controller from the environment:
#   JANUS_ENDPOINT  https://controller.example.net:8080
#   JANUS_CA_CERT   the Controller's certificate (its Provision panel)
#   JANUS_TOKEN     an API token (an admin's: it creates machines)
# The Proxmox token's secret: TF_VAR_pve_token_secret.

terraform {
  required_providers {
    janus = { source = "swenske/janus" }
  }
}

provider "janus" {}

# The Proxmox VE node, prepared first (docs/hypervisors.md): the pool,
# the storages, the role, the user and the token.
resource "janus_proxmox_hypervisor" "pve" {
  name                    = var.hypervisor_name
  url                     = var.pve_url
  node                    = var.pve_node
  token_id                = "janus-ctl@pve!controller"
  token_secret            = var.pve_token_secret
  pool                    = "janus"
  storage                 = var.storage
  image_storage           = var.image_storage
  networks                = [var.network]
  certificate_fingerprint = var.certificate_fingerprint
}

resource "janus_node" "lb" {
  for_each      = var.nodes
  name          = each.key
  hypervisor_id = janus_proxmox_hypervisor.pve.id
  vcpus         = 2
  memory_mib    = var.memory_mib
  version       = var.janus_version
  extensions    = var.extensions

  interfaces = [{
    network   = var.network
    name      = "front"
    mode      = "static"
    addresses = [each.value]
    gateway   = var.gateway
  }]
  dns    = var.dns
  labels = { role = "edge" }
}

resource "janus_haproxy_config" "lb" {
  for_each = janus_node.lb
  node     = each.value.node_id
  config   = file("${path.module}/../../haproxy/web.cfg")
}

output "nodes" {
  description = "Each node's address on the Controller."
  value       = { for name, node in janus_node.lb : name => node.node_address }
}
```

```hcl title="examples/terraform/proxmox/variables.tf"
variable "hypervisor_name" {
  description = "The hypervisor's name on the Controller."
  type        = string
  default     = "pve01"
}

variable "pve_url" {
  description = "The Proxmox VE API, https://host:8006."
  type        = string
}

variable "pve_node" {
  description = "The Proxmox VE node the machines run on."
  type        = string
}

variable "pve_token_secret" {
  description = "The API token's secret, shown once when it was created."
  type        = string
  sensitive   = true
}

variable "certificate_fingerprint" {
  description = "The API's certificate, as `openssl x509 -noout -fingerprint -sha256 -in /etc/pve/local/pveproxy-ssl.pem` prints it on the node."
  type        = string
}

variable "storage" {
  description = "The storage for the machines' disks."
  type        = string
  default     = "local-lvm"
}

variable "image_storage" {
  description = "The storage for the images and NoCloud volumes (content: iso, import)."
  type        = string
  default     = "janus-images"
}

variable "network" {
  description = "The bridge - or VLAN on one, vmbr0.20 - the nodes are attached to."
  type        = string
  default     = "vmbr0"
}

variable "gateway" {
  description = "The nodes' default gateway."
  type        = string
  default     = "192.0.2.1"
}

variable "nodes" {
  description = "Each node's name and static address (CIDR)."
  type        = map(string)
  default = {
    lb1 = "192.0.2.21/24"
    lb2 = "192.0.2.22/24"
  }
}

variable "memory_mib" {
  description = "Each node's memory: HAProxy's tables grow with maxconn (docs/haproxy-config.md)."
  type        = number
  default     = 2048
}

variable "dns" {
  description = "DNS servers (unset: DHCP's)."
  type        = list(string)
  default     = null
}

variable "janus_version" {
  description = "The release to run (unset: the newest)."
  type        = string
  default     = null
}

variable "extensions" {
  description = "Extensions, built by the image factory - [\"keepalived\"] for a virtual IP."
  type        = set(string)
  default     = null
}
```

`networks` lists what the nodes may use: a bridge untagged (`vmbr0`), a
VLAN on one (`vmbr0.20`), a range of VLANs (`vmbr0.100-199`) or every
VLAN on it (`vmbr0.*`) - each interface then names one, `vmbr0.150`.
The `haproxy.cfg` both nodes get:
[`examples/haproxy/web.cfg`](../../../examples/haproxy/web.cfg).

## 5. Apply it

```sh
cat > terraform.tfvars <<'EOF'
pve_url                 = "https://pve01.example.net:8006"
pve_node                = "pve01"
certificate_fingerprint = "8D:B4:..."          # step 2
EOF
tofu apply
```

The Controller downloads the release's `janus.qcow2` - or the image
factory's build, with extensions - checks it, uploads it once to
`janus-images`, then makes each node's virtual machine: a copy of the
image, a NoCloud CD-ROM with its network and a one-time token, UEFI
with Secure Boot off, a serial console. `apply` waits until both nodes
are admitted and answer, then applies their HAProxy configuration.

## 6. Check, and a virtual IP

As in the [libvirt guide](kvm-libvirt.md#6-check): `curl` the nodes,
`janusctl -n lb1,lb2 haproxy show-info`, `tofu plan` with nothing to
change - then `extensions = ["keepalived"]` and a `keepalived.conf` per
node for the [virtual IP](kvm-libvirt.md#7-a-virtual-ip). VRRP needs the
nodes on the same bridge or VLAN; with the [firewall](../../firewall.md)
extension, accept `ip protocol 112`.

## Clean up

```sh
tofu destroy
```

Images stay in `janus-images` for the next nodes; the Controller never
removes them.

## Without Terraform

- **From the Controller**: **Hypervisors › Create node**, with a form.
- **By hand**, without a Controller's hypervisor - importing the image
  ([`image/kvm-proxmox`](../../../image/kvm-proxmox/README.md)):

  ```sh
  qm create 9001 --name lb1 --memory 2048 --cores 2 \
    --machine q35 --bios ovmf --efidisk0 local-lvm:0,pre-enrolled-keys=0 \
    --net0 virtio,bridge=vmbr0 --serial0 socket
  qm importdisk 9001 janus.qcow2 local-lvm
  qm set 9001 --scsihw virtio-scsi-pci --virtio0 local-lvm:vm-9001-disk-1 --boot order=virtio0
  qm set 9001 --ide2 janus-images:iso/lb1-cidata.iso,media=cdrom   # its NoCloud volume
  qm start 9001
  qm terminal 9001                     # the serial console
  ```

  `pre-enrolled-keys=0` leaves Secure Boot off. The NoCloud volume is an
  ISO 9660 `cidata` with the node's `user-data`
  ([first boot](../first-boot.md#the-nocloud-volume)), uploaded to an
  ISO storage.
