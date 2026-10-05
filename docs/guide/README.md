# User guide

Everything to run Janus: getting an image, booting and provisioning
nodes, the Controller that manages them, HAProxy, the network and the
optional extensions, and keeping a fleet running. A Janus node has no
shell and no SSH: everything here goes through the node's mTLS API -
from the Controller's web UI, from `janusctl`, or from Terraform.

## Getting started

- [Quick start](quickstart.md) - a node in a virtual machine on your
  workstation, configured through its API, serving HTTP: ten minutes.
- [Images and extensions](../image-factory.md) - the images each
  release publishes, and how to build one with the optional extensions
  you need (BGP, VRRP, firewall, Consul, Let's Encrypt...).
- [Provisioning a node](../provisioning-a-node.md) - four ways to turn
  a blank machine into a node that registers itself with a Controller.
  Deploying many, on a hypervisor or bare metal: [Janus in a private
  cloud](../private-cloud/README.md).

## The Controller

- [The Controller](../../dashboard/README.md) - the web UI for a fleet:
  installing it, accounts and roles, adding nodes, backups, updates.
- [Hypervisors](../hypervisors.md) - nodes the Controller creates
  itself, on libvirt/KVM or Proxmox VE.

## janusctl

- [Using janusctl](../janusctl.md) - installing it, signing in to a
  Controller, reaching nodes; shell completion.
- [janusctl reference](janusctl-reference.md) - every command, its
  arguments and its flags.
- [A fleet without a Controller](../fleet-without-controller.md) - the
  same trust model, driven from `janusctl` alone.

## HAProxy

- [Your haproxy.cfg](../haproxy-config.md) - what to change in an
  existing configuration to run it on Janus.
- [Files, maps and certificates](../haproxy-files.md) - error pages,
  maps, ACL files and certificates next to the configuration.
- [Let's Encrypt](../letsencrypt.md) - certificates obtained and renewed
  by the node itself.

## Network

- [Network configuration](../network-configuration.md) - interfaces,
  VLANs, routes and DNS, applied on trial and reverted unless confirmed.
- [Firewall](../firewall.md), [VRRP](../vrrp.md), [BGP](../bgp.md) and
  [Consul](../consul.md) - the optional network extensions.

## Operate

- [Updating nodes](updates.md) - A/B updates with an automatic revert,
  from the Controller or `janusctl`; going back.
- [Metrics](../metrics.md) - the Prometheus exporter every node serves.
- [Packet capture](../packet-capture.md) - tcpdump-style captures
  streamed over the API.
- [Troubleshooting](troubleshooting.md) - where each problem shows,
  without a shell: registration, access, HAProxy, the network, VRRP and
  BGP, updates.
- [Security policy](../../SECURITY.md) - supported versions and how to
  report a vulnerability.
