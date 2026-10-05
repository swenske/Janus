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
  -nic user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:19505-:9505,hostfwd=tcp:127.0.0.1:18080-:80 \
  -display none -serial file:console.log &
```

- The node's API (9505) is forwarded to `127.0.0.1:19505`, and HTTP (80)
  to `127.0.0.1:18080`.
- Its console - the serial port - goes to `console.log`.
- Without access to `/dev/kvm`, add `-accel tcg` after `-accel kvm`:
  slower, but it boots.

## 2. Its credentials

On its first boot, the node makes its own certificate authority and an
admin certificate, and prints them on its console - **once**: there's no
shell to read them from later. Within a minute:

```sh
until grep -q 'listening on' console.log; do sleep 2; done
awk '/pki: CA CERTIFICATE/ {f = "ca.crt"; next}
     /pki: ADMIN CERTIFICATE/ {f = "admin.pem"; next}
     f && /^-----BEGIN/ {w = 1}
     w {print > f}
     /^-----END/ {w = 0; if (f == "ca.crt") f = ""}' console.log
```

`ca.crt` is the node's CA; `admin.pem` its admin certificate and key,
which `janusctl` takes as both `-cert` and `-key`.

## 3. Talk to it

```sh
ctl() { janusctl -endpoint 127.0.0.1:19505 -ca ca.crt -cert admin.pem -key admin.pem "$@"; }
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
