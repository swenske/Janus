<!-- Generated from site/docs/structure.yaml: make docs-index -->

# Janus documentation

These pages are also a website - searchable, in light and dark, the
newest release's and main's: [janus.sw-servers.net/docs](https://janus.sw-servers.net/docs/).
This index is for reading them here, on GitHub.

## User guide

Install, configure and operate Janus nodes and the Controller.

- [User guide](guide/README.md)

### The Controller

- [The Controller](../dashboard/README.md)
- [Hypervisors](hypervisors.md)

### janusctl

- [Using janusctl](janusctl.md)
- [janusctl reference](guide/janusctl-reference.md)
- [A fleet without a Controller](fleet-without-controller.md)

### HAProxy

- [Your haproxy.cfg](haproxy-config.md)
- [Files, maps and certificates](haproxy-files.md)
- [Let's Encrypt](letsencrypt.md)

### Network

- [Network configuration](network-configuration.md)
- [Firewall (nftables)](firewall.md)
- [VRRP (keepalived)](vrrp.md)
- [BGP (BIRD)](bgp.md)
- [Consul](consul.md)

### Images and provisioning

- [Images and extensions](image-factory.md)
- [Provisioning a node](provisioning-a-node.md)

### Operate

- [Metrics](metrics.md)
- [Packet capture](packet-capture.md)
- [Security policy](../SECURITY.md)

## Technical

How Janus works inside - the architecture, the boot chain, the images, the API.

- [How Janus works](internals/README.md)

### Architecture

- [Architecture](architecture.md)
- [The gRPC API](api-routes.md)
- [Roadmap](roadmap.md)

### Images

- [The disk image](../image/disk/README.md)
- [The ISO image](../image/iso/README.md)
- [Network boot](../image/pxe/README.md)

## Developer

Contribute to Janus, or integrate it into your private cloud.

### Contributing

- [Contributing to Janus](contributing/README.md)

#### Develop

- [Local dev container](../local-dev/README.md)
- [Controller UI design](controller-ui.md)
- [Raspberry Pi hardware tests](raspberry-pi-testing.md)

#### Ship

- [CI runners](ci-runners.md)
- [Following upstreams](upstreams.md)

### Private cloud integration

- [Janus in a private cloud](private-cloud/README.md)

#### Deploying

- [Images and formats](private-cloud/images.md)
- [First boot](private-cloud/first-boot.md)
- [Automated deployment](private-cloud/automation.md)
- [Terraform / OpenTofu](terraform.md)

#### Running

- [Networking](private-cloud/networking.md)
- [Observability](private-cloud/observability.md)
- [Lifecycle](private-cloud/lifecycle.md)
- [Security](private-cloud/security.md)

#### Platforms

- [libvirt/KVM](private-cloud/platforms/kvm-libvirt.md)
- [Proxmox VE](private-cloud/platforms/proxmox.md)
- [Bare metal](private-cloud/platforms/bare-metal.md)
- [VMware vSphere](private-cloud/platforms/vmware.md)
