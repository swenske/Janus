# Image schematics and optional extensions

A Janus image is the base system - kernel, `init`, `janusd`, HAProxy - plus
the optional **extensions** you choose for it. The choice is written down
as an **image schematic**, identified by an ID, the way Talos Image
Factory does it: the same choices always give the same ID, and a node
built from a schematic keeps it through its updates.

## Extensions

| Name | Architectures | What it adds |
|---|---|---|
| `node-exporter` | amd64, arm64 | Prometheus [node_exporter](https://github.com/prometheus/node_exporter): CPU, memory, disk, filesystem and network metrics on `:9100/metrics` |
| `qemu-guest-agent` | amd64 | The [QEMU guest agent](https://www.qemu.org/docs/master/interop/qemu-ga.html), for Proxmox and other KVM hypervisors: the node's addresses and OS in the hypervisor's UI, filesystem freeze for consistent backups, clean shutdown from the hypervisor |

An extension is built into the read-only rootfs: there is no installing or
removing one on a running node. `janusd` runs its services - restarting
them if they exit - and shows them next to `janusd` and `haproxy` in
`janusctl system services`, the Controller's **System › Services** page
and the logs. Each runs in its own SELinux domain.

**qemu-guest-agent** only enables the agent commands a hypervisor needs to
observe the node and shut it down: ping and info, OS, host name, time,
network interfaces, filesystems and CPUs, filesystem freeze and thaw,
shutdown. The commands that would run programs or read and write files on
the node (`guest-exec`, `guest-file-*`, `guest-set-user-password`,
`guest-ssh-*`) are disabled - they would be a shell by another name. In
Proxmox, enable the agent in the VM's options (**QEMU Guest Agent**).

**node-exporter** listens on every address, without authentication, like
a stock node_exporter; restrict who can reach port 9100 in your network.

## The schematic

```json
{"customization": {"extensions": ["node-exporter", "qemu-guest-agent"]}}
```

The same, as shown in YAML:

```yaml
customization:
  extensions:
    - node-exporter
    - qemu-guest-agent
```

- Only the extensions a release lists (its `schematic-catalog.json`) can
  be named; order and duplicates don't matter.
- The **ID** is the sha256 of the schematic's canonical JSON form. The
  default schematic - no extension, what the official releases are built
  from - is
  `a055fbb697e2d0abb0c5911e7702b07040f49eb71befeaf9b90495a905327f47`.
- The version, platform, architecture and image format are chosen
  separately, when downloading: one schematic serves every release and
  every platform.

## Updates keep the schematic

The ID is part of the kernel command line of every image built from the
schematic (`janus.schematic=<id>`), which the release key signs. A node
therefore knows its schematic (`janusctl version` shows it, with its
extensions), and `LifecycleService.Upgrade` checks the update's command
line: **an update built from another schematic is refused**, so a node
built with an extension can't lose it to an update built without it. To
change a node's extensions on purpose, pass `-allow-schematic-change`
(`janusctl lifecycle upgrade`) - or reinstall.

## Building an image with extensions

```sh
make extensions-amd64                         # build the extensions (Docker)
echo '{"customization":{"extensions":["node-exporter"]}}' > my-schematic.json
make disk-image SCHEMATIC=my-schematic.json   # or proxmox-image, iso-image, ...
go run ./hack/extpack id -schematic my-schematic.json   # its ID
```

`hack/qemu-extensions-test.sh` (`make qemu-extensions-test`) proves the
whole chain on a real UEFI boot under SELinux enforcing.
