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
