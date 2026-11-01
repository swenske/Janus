# Quick start

A Janus node in a virtual machine on your workstation, configured
through its API, serving HTTP - in about ten minutes, with nothing but
QEMU and `janusctl`. No Controller: this is the node alone, the way to
see what Janus is before deploying it ([private
cloud](../private-cloud/README.md)).

## What you need

- **Linux with QEMU and OVMF** (UEFI for QEMU): `sudo apt install
  qemu-system-x86 ovmf` on Debian or Ubuntu - and, for speed, your user
  in the `kvm` group.
- **`janusctl`**: from the apt repository, or the `.deb` attached to
  every release ([installing janusctl](../janusctl.md#installing-janusctl)).

## 1. Boot a node

The release's image for QEMU and libvirt, and a copy of OVMF's variables
for this machine:

```sh
mkdir janus-quickstart && cd janus-quickstart
curl -fsSLO https://github.com/swenske/Janus/releases/latest/download/janus-kvm.qcow2
cp /usr/share/OVMF/OVMF_VARS_4M.fd vars.fd

qemu-system-x86_64 -machine q35 -accel kvm -m 1024 -smp 2 \
  -drive if=pflash,format=raw,readonly=on,file=/usr/share/OVMF/OVMF_CODE_4M.fd \
  -drive if=pflash,format=raw,file=vars.fd \
  -drive file=janus-kvm.qcow2,if=virtio \
  -nic user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:19505-:9505,hostfwd=tcp:127.0.0.1:19506-:9506,hostfwd=tcp:127.0.0.1:18080-:80 \
  -display none -serial file:console.log &
```

- The node's API (9505) is forwarded to `127.0.0.1:19505`, its pairing
  window (9506) to `127.0.0.1:19506`, and HTTP (80) to
  `127.0.0.1:18080`.
- Its console - the serial port - goes to `console.log`.
- Without access to `/dev/kvm`, add `-accel tcg` after `-accel kvm`:
  slower, but it boots.

## 2. Its credentials

On its first boot, the node makes its own certificate authority and,
trusting no one yet, shows a one-time **pairing code** on its console -
no key ever goes there. `janusctl` makes a fleet of its own - its root's
key in a recovery kit, certificates for this machine - and gives it to
the node with that code ([a fleet without a
Controller](../fleet-without-controller.md)):

```sh
janusctl -context quickstart fleet init -name quickstart -yes kit.age
until grep -q 'pairing: code' console.log; do sleep 2; done
CODE="$(grep -o 'pairing: code [0-9A-Z-]*' console.log | tail -1 | cut -d' ' -f3)"
janusctl fleet adopt lb1 -endpoint 127.0.0.1:19505 -pairing 127.0.0.1:19506 -code "$CODE"
```

`fleet init` prints the kit's passphrase once: for a real fleet, keep
both, apart from this machine. janusctl checks the node's certificate
against the code before it sends anything; the node trusts the fleet
from then on, and its pairing window closes.

## 3. Talk to it

```sh
ctl() { janusctl -n lb1 "$@"; }
ctl version              # the release, the image schematic
ctl system info          # kernel, slot, memory, CPUs, disks
ctl system services      # janusd and haproxy, running and healthy
```

Every call goes over gRPC with mutual TLS, and the node checks the
certificate's role (`os:admin` here) on each one.

## 4. Give HAProxy a configuration

A node boots with a minimal HAProxy - a health answer on port 8080. Give
it a real configuration: [`examples/haproxy/web.cfg`](../../examples/haproxy/web.cfg)
serves HTTP on port 80, with its own health answer:

```sh
curl -fsSLO https://raw.githubusercontent.com/swenske/Janus/main/examples/haproxy/web.cfg
ctl haproxy apply-config web.cfg
curl http://127.0.0.1:18080/healthz          # ok
ctl haproxy show-info                        # HAProxy's version, uptime, connections
```

HAProxy checked the configuration before taking it - a configuration it
refuses changes nothing, and says why:

```text
[rejected] ... parsing [haproxy.cfg:3] : unknown keyword 'this-is-not-a-keyword' in 'global' section
```

What changes from a distribution's `haproxy.cfg`: [your
haproxy.cfg](../haproxy-config.md).

## 5. Look around - without a shell

```sh
ctl system logs -n 20 haproxy    # HAProxy's output
ctl system ps                    # every process
ctl system netstat               # sockets
ctl system ls /etc/haproxy       # read-only file access
ctl system dmesg                 # the kernel's messages
```

Everything a shell would show, through the API - and nothing that could
change the node outside it. Every command: [the janusctl
reference](janusctl-reference.md).

Stop the virtual machine when you're done: `kill %1`.

## Next

- **A fleet**: the [Controller](../../dashboard/README.md) manages many
  nodes - their approval, their updates, their pages - and can create
  them on [libvirt or Proxmox VE](../hypervisors.md).
- **Deploying for real**: [Janus in a private cloud](../private-cloud/README.md),
  with an end-to-end guide per platform.
- **How it works**: [the architecture](../architecture.md).
