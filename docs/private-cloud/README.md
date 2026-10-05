# Janus in a private cloud

Janus nodes are virtual machines like any other: an image to import, a
first boot that gives the node its identity and its network, and an API
to configure HAProxy from then on. This section is for platform
engineers integrating Janus into a private cloud - on libvirt/KVM,
Proxmox VE, VMware or bare metal - and automating it.

## The pieces

- **Images**: each release publishes a qcow2 for Proxmox VE, a qcow2
  for libvirt/KVM, a VMDK for VMware and an installer ISO - with the
  extensions you choose, from the [image factory](../image-factory.md).
- **First boot**: a node gets its Controller, its network and its
  enrollment token from a NoCloud volume, a pre-seeded image or the
  installer - see [provisioning a node](../provisioning-a-node.md). Its
  HAProxy configuration comes afterwards, through the API.
- **Automation**: the Controller creates nodes itself on [libvirt or
  Proxmox VE hosts](../hypervisors.md), and [Terraform /
  OpenTofu](../terraform.md) drives the Controller - nodes, their
  network, their size, their version and their HAProxy configuration as
  code.
- **Networking**: virtual IPs with [VRRP](../vrrp.md), anycast with
  [BGP](../bgp.md), a [firewall](../firewall.md) per node, and the
  [network configuration](../network-configuration.md) itself.
- **Observability**: every node serves [Prometheus
  metrics](../metrics.md).
