# VMware vSphere, end to end

Janus nodes on VMware ESXi or vCenter: the release's VMDK imported, a
virtual machine around it with VMware's paravirtual devices, a NoCloud
CD-ROM for its first boot - then the same Controller, `janusctl` and
HAProxy configuration as on any platform.

> [!WARNING]
> Not verified on a real ESXi host yet. VMware's devices - the PVSCSI
> disk controller, the VMXNET 3 network card - and the screen console
> are proven under QEMU (`make qemu-baremetal-test`), not on ESXi
> itself. The Controller can't create nodes on vSphere yet (planned):
> the virtual machines are made by hand or with `govc`, as below.

## What you need

- **ESXi or vCenter** (7 or 8), a datastore, a port group for the nodes,
  and [`govc`](https://github.com/vmware/govmomi/tree/main/govc) (or the
  vSphere Client).
- **The Controller** (the [libvirt guide](kvm-libvirt.md#1-the-controller)'s
  step 1), its address and certificate, and an enrollment token for the
  nodes - or approve each one.
- **The release's `janus.vmdk`** - or one with extensions from the
  [image builder](https://janus.sw-servers.net/builder).

## 1. The node's NoCloud volume

Each node's first boot reads a `cidata` ISO on a CD-ROM: the
Controller, the token, the node's network
([first boot](../first-boot.md#the-nocloud-volume)).

```sh
mkdir -p seed
cat > seed/user-data <<'EOF'
{"controller_address": "controller.example.net:8443",
 "controller_ca_cert": "-----BEGIN CERTIFICATE-----\n...\n-----END CERTIFICATE-----\n",
 "registration_token": "janus-enroll_...",
 "network": {"hostname": "lb1", "interfaces": [{"name": "front", "mode": "ADDRESSING_MODE_STATIC",
   "addresses": ["192.0.2.21/24"], "gateway": "192.0.2.1"}]}}
EOF
xorriso -as mkisofs -V cidata -J -r -o lb1-cidata.iso seed/
```

## 2. The virtual machine

```sh
export GOVC_URL=vcenter.example.net GOVC_USERNAME=... GOVC_PASSWORD=... GOVC_DATASTORE=datastore1
govc import.vmdk janus.vmdk lb1                      # the node's own disk: lb1/janus.vmdk
govc datastore.upload lb1-cidata.iso lb1/lb1-cidata.iso
govc vm.create -g otherLinux64Guest -firmware=efi -c 2 -m 2048 \
  -disk.controller=pvscsi -disk=lb1/janus.vmdk \
  -net=<port group> -net.adapter=vmxnet3 -on=false lb1
govc vm.change -vm lb1 -e efi-secure-boot-enabled=FALSE
govc device.cdrom.add -vm lb1
govc device.cdrom.insert -vm lb1 lb1/lb1-cidata.iso
govc vm.power -on lb1
```

- **The disk controller** must be VMware Paravirtual (PVSCSI), SATA or
  NVMe: the kernel has no driver for the LSI Logic ones, which `govc`
  picks unless told otherwise.
- **The network card**: VMXNET 3 (or E1000/E1000e).
- **Firmware**: EFI, Secure Boot off - the VM image's boot entries
  aren't signed.
- **A serial port** to a file or a named pipe helps: the node's console
  - its first boot's credentials, its registration's messages - is on
  the screen and on the serial port.

In the vSphere Client: a new virtual machine with "Other Linux
(64-bit)", EFI firmware with Secure Boot unchecked, a VMware
Paravirtual SCSI controller with the VMDK as its disk, a VMXNET 3
adapter, and a CD/DVD drive with the `cidata` ISO.

## 3. Admitted, then configured

On its first boot the node applies its network, registers with the
Controller on its token, and is admitted. Then, as on every platform:

```sh
janusctl login -controller controller.example.net:8080 -controller-ca controller-ca.crt
janusctl -n lb1 haproxy apply-config examples/haproxy/web.cfg
```

A second node, a virtual IP with keepalived, updates: as in the
[libvirt guide](kvm-libvirt.md#7-a-virtual-ip) and
[lifecycle](../lifecycle.md). With vSphere HA or DRS, keep the two
nodes of a pair on different hosts (an anti-affinity rule), so one host
failing doesn't take both.

## Tell us

Running Janus on ESXi? An issue saying what worked - or didn't - helps
take the warning above off:
[GitHub issues](https://github.com/swenske/Janus/issues).
