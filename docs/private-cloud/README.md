# Janus in a private cloud

Janus nodes are virtual machines like any other - an image to import, a
first boot that gives the node its identity and its network - with
one difference that shapes everything here: there is no shell to log
into. A node is configured through its API, by the Controller,
`janusctl` or Terraform, and an integration automates those, never a
login. This section is for platform engineers bringing Janus into a
private cloud: on libvirt/KVM or Proxmox VE, the platforms
[Janus supports](#what-janus-supports) - VMware and bare metal are
experimental, waiting for testers.

## What Janus supports

**amd64 virtual machines on KVM/libvirt and Proxmox VE**, with or
without the Controller. That is what releases are built and tested
for - the images and their updates, the Controller creating nodes on
libvirt (in the lab, before each release) and on Proxmox VE (a real
Proxmox VE 9.2 host), the Terraform provider driving it - and what the
beta will cover.

Everything else is **experimental**:

- **Bare metal, VMware ESXi and the Raspberry Pi** were proven under
  QEMU only, never on real hardware: they are
  [suspended](../internals/image-build.md#suspended-targets) since
  2026-10-30 - no release builds or publishes their images - until
  someone verifies them. The
  [image builder](https://janus.sw-servers.net/builder) links the forms
  to offer it.
- **arm64 nodes** get no updates: no A/B update was ever proven on
  arm64, and the image factory serves updates for amd64 only.
- **Other hypervisors** may boot the qcow2 - any UEFI machine with a
  virtio or SATA disk and a virtio or Intel e1000/e1000e network card,
  the devices the images' kernel drives - but nothing tests them.

## The pieces

| | |
|---|---|
| [Images and formats](images.md) | What each release publishes - qcow2, VMDK, ISO - which one each platform takes, the extensions, checking what you download |
| [First boot](first-boot.md) | How a node gets its Controller, its network and its admission on its first boot - NoCloud, a seeded image, the installer - and its HAProxy configuration after |
| [Automated deployment](automation.md) | Terraform and OpenTofu, the Controller's API, `janusctl` in a pipeline, enrollment tokens for batches |
| [Networking](networking.md) | Virtual IPs (VRRP), anycast (BGP), segmentation, the ports a node and the Controller use, DNS - and a request's path through a node |
| [Observability](observability.md) | Prometheus - Janus's exporter, node_exporter, HAProxy's own - alerts, logs, packet captures |
| [Lifecycle](lifecycle.md) | Updates with an automatic revert, rollbacks, scaling, the Controller's own updates and backups |
| [Security](security.md) | What protects a node, the secrets of a deployment and who holds them, certificates, what to expose |
| [Your own orchestrator](own-orchestrator.md) | Driving nodes without the Controller, from a private cloud's own orchestration in any language: the certificates, first contact, the API and its reference |

And one end-to-end guide per platform, each with a complete example:

| Platform | Image | Nodes created by | Proven by |
|---|---|---|---|
| [libvirt/KVM](platforms/kvm-libvirt.md) | `janus-kvm.qcow2` | the Controller, over SSH - Terraform drives it | `make terraform-provider-test` applies the example as written, on a real libvirt host |
| [Proxmox VE](platforms/proxmox.md) | `janus.qcow2` | the Controller, through Proxmox's API - Terraform drives it | a real Proxmox VE 9.2 node |
| [Bare metal](platforms/bare-metal.md) - **experimental**, waiting for testers | `janus.iso` | the installer, driven by `janusctl` | `make qemu-iso-install-test`, `make qemu-baremetal-test` (UEFI; NVMe, SATA, USB and virtio-scsi disks; Intel NICs) - not yet a physical machine; needs a kernel of its own (the images' drives a virtual machine's devices) |
| [VMware vSphere](platforms/vmware.md) - **experimental**, waiting for testers | `janus.vmdk` | hand or `govc` | QEMU with VMware's devices (pvscsi, vmxnet3) - not yet a real ESXi host; needs a kernel of its own, as bare metal |

## A typical deployment

```mermaid
flowchart TB
    accTitle: A typical Janus deployment in a private cloud
    accDescr: Clients resolve the service to a virtual IP, held by one of two Janus nodes with VRRP, or to an anycast address the nodes announce with BGP. Each node's HAProxy forwards to the application servers. On a management network, Terraform drives the Controller, the Controller reaches each node's API over gRPC with mutual TLS and the hypervisor over SSH or its API, and Prometheus scrapes the nodes' exporters.
    clients([Clients]) --> vip{{"app.example.net: a virtual IP (VRRP) or anycast (BGP)"}}
    subgraph hv [Hypervisor - or bare metal]
        lb1["Janus node lb1: HAProxy"]
        lb2["Janus node lb2: HAProxy"]
    end
    vip --> lb1
    vip -.->|when lb1 fails| lb2
    lb1 --> app[(Application servers)]
    lb2 --> app
    subgraph mgmt [Management network]
        tf[Terraform / OpenTofu]
        ctrl[Janus Controller]
        prom[Prometheus]
    end
    tf -->|"API token, HTTPS"| ctrl
    ctrl -->|"gRPC + mTLS, port 9505"| lb1
    ctrl -->|"gRPC + mTLS, port 9505"| lb2
    ctrl -.->|"libvirt over SSH, Proxmox VE API"| hv
    prom -->|"port 10056"| lb1
    prom -->|"port 10056"| lb2
```

- **Two nodes or more**, so one can be updated, rebooted or lost
  without an outage: a virtual IP moves between them with VRRP
  (keepalived), or each announces an anycast address with BGP (BIRD).
  Both follow HAProxy's health, and a node about to stop gives its
  traffic up first ([networking](networking.md)).
- **The Controller** holds the fleet: its accounts, the fleet's
  certificate authority, the nodes' updates, and - given a hypervisor -
  the nodes' virtual machines. It isn't in the data path: nodes keep
  serving while it's down.
- **A management network** for the nodes' API (9505), metrics (10056)
  and the Controller - away from the clients' network
  ([segmentation](networking.md#segmentation)).
- **Everything as code**: Terraform creates the nodes and their HAProxy
  configuration through the Controller; the node configurations
  Terraform doesn't cover yet - VRRP, BGP, the firewall - go through
  `janusctl` or the Controller's API, both scriptable
  ([automation](automation.md)).
