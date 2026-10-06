<!-- Generated from site/docs/structure.yaml: make docs-index -->

# Janus documentation

These pages are also a website - searchable, in light and dark, the
newest release's and main's: [janus.sw-servers.net/docs](https://janus.sw-servers.net/docs/).
This index is for reading them here, on GitHub.

## User guide

Install, configure and operate Janus nodes and the Controller.

- [User guide](guide/README.md)

### Getting started

- [Quick start](guide/quickstart.md)
- [Images and extensions](image-factory.md)
- [Provisioning a node](provisioning-a-node.md)

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
- [Kernel tuning](guide/kernel-tuning.md)

### Network

- [Network configuration](network-configuration.md)
- [Firewall (nftables)](firewall.md)
- [VRRP (keepalived)](vrrp.md)
- [BGP (BIRD)](bgp.md)
- [Consul](consul.md)

### Operate

- [Updating nodes](guide/updates.md)
- [Metrics](metrics.md)
- [Packet capture](packet-capture.md)
- [Troubleshooting](guide/troubleshooting.md)
- [Security policy](../SECURITY.md)

## Technical

How Janus works inside - the architecture, the boot chain, the images, the API.

- [How Janus works](internals/README.md)

### Architecture

- [Architecture](architecture.md)
- [Boot and A/B updates](internals/boot.md)
- [Trust and certificates](internals/trust.md)
- [The Controller](internals/controller.md)
- [The gRPC API](api-routes.md)
- [Roadmap](roadmap.md)

### Images

- [How an image is built](internals/image-build.md)
- [The disk image](../image/disk/README.md)
- [The ISO image](../image/iso/README.md)
- [Network boot](../image/pxe/README.md)

## Developer

Contribute to Janus, or integrate it into your private cloud.

### Contributing

- [Contributing to Janus](contributing/README.md)

#### Develop

- [Development environment](contributing/development.md)
- [Testing](contributing/testing.md)
- [Conventions](contributing/conventions.md)
- [Local dev container](../local-dev/README.md)
- [Controller UI design](controller-ui.md)
- [Writing an extension](contributing/extensions.md)
- [Writing docs](contributing/writing-docs.md)
- [Raspberry Pi hardware tests](raspberry-pi-testing.md)

#### Ship

- [Releasing](contributing/releasing.md)
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

#### Your own orchestrator

- [Without the Controller](private-cloud/own-orchestrator.md)
- [Certificates and the fleet](private-cloud/orchestrator-certificates.md)
- [First contact](private-cloud/first-contact.md)
- [Driving nodes](private-cloud/driving-nodes.md)
- [Node API reference](private-cloud/api-reference.md)

#### Platforms

- [libvirt/KVM](private-cloud/platforms/kvm-libvirt.md)
- [Proxmox VE](private-cloud/platforms/proxmox.md)
- [Bare metal](private-cloud/platforms/bare-metal.md)
- [VMware vSphere](private-cloud/platforms/vmware.md)
