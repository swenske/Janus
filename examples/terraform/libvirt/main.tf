# Two Janus load balancers on a libvirt/KVM host, created by a Janus
# Controller and serving ../../haproxy/web.cfg - the end-to-end example of
# docs/private-cloud/platforms/kvm-libvirt.md. make terraform-provider-test
# applies it as written, against a real Controller and libvirt host.
#
# The provider reads the Controller from the environment:
#   JANUS_ENDPOINT  https://controller.example.net:8080
#   JANUS_CA_CERT   the Controller's certificate (its Provision panel)
#   JANUS_TOKEN     an API token (an admin's: it creates machines)

terraform {
  required_providers {
    janus = { source = "swenske/janus" }
  }
}

provider "janus" {}

# The host, prepared first (docs/hypervisors.md). Its account authorizes
# the Controller's key - authorized_key, below - before nodes are made:
# apply this resource alone first (-target=janus_hypervisor.kvm).
resource "janus_hypervisor" "kvm" {
  name                 = var.hypervisor_name
  host                 = var.hypervisor_host
  user                 = "janus-ctl"
  pool                 = "janus"
  networks             = [var.network]
  host_key_fingerprint = var.host_key_fingerprint
}

resource "janus_node" "lb" {
  for_each      = var.nodes
  name          = each.key
  hypervisor_id = janus_hypervisor.kvm.id
  vcpus         = 2
  memory_mib    = var.memory_mib
  version       = var.janus_version
  extensions    = var.extensions
  image         = var.image

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

# Each node's haproxy.cfg: HAProxy checks it first - a configuration it
# refuses fails the apply and changes nothing.
resource "janus_haproxy_config" "lb" {
  for_each = janus_node.lb
  node     = each.value.node_id
  config   = file("${path.module}/../../haproxy/web.cfg")
}

output "authorized_key" {
  description = "The line the host's janus-ctl account needs in ~/.ssh/authorized_keys."
  value       = janus_hypervisor.kvm.authorized_key
}

output "nodes" {
  description = "Each node's address on the Controller."
  value       = { for name, node in janus_node.lb : name => node.node_address }
}
