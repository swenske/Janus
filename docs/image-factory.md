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
Proxmox, enable the agent in the VM's options (**QEMU Guest Agent**);
until the VM has the agent's channel, the service shows as `waiting`
rather than failing.

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

## Getting an image: janus.sw-servers.net

The companion site's **Image builder** (<https://janus.sw-servers.net/builder>)
walks through the choices - platform, release, extensions - and gives
the schematic's ID, its YAML and the download links:

- the **default schematic** downloads straight from the GitHub Release;
- any other is built on demand, the first time someone asks for it, from
  the build inputs that release published (below). It takes a few
  minutes; the page follows the build and offers the files when it's
  done. Built images stay available, for the latest releases.

The same service answers nodes' update questions, per schematic:

```sh
curl https://janus.sw-servers.net/api/v1/updates/<schematic-id>?arch=amd64
```

gives the newest release built for that schematic - the base URL of its
update bundle (what `janusctl lifecycle upgrade` and the Controller's
Update page take) and its sha256 - or starts building it. The
Controller's **Update** page asks it for each node with extensions
(`dashboardd -image-factory`, this site by default), and offers the
update once it's built. When a new
release comes out, the site builds the update bundle of every schematic
it has already served, so a node with extensions finds its update ready
like one without.

### How a custom image is built

Every release publishes, for each architecture, what a schematic's image
is made of - nothing is compiled again for a schematic, and every node
runs the release's own binaries:

| Asset | Content |
|---|---|
| `kernel-<arch>` | the release's kernel |
| `rootfs-base-<arch>.tar` | the base system tree: `init`, `janusd`, HAProxy, the SELinux policy, the CA bundle, ... with the SELinux types of its executables |
| `extension-<name>-<arch>.tar` | each extension's files, manifest and SELinux types |
| `schematic-catalog.json` | the extensions the release offers, and for which architectures |

The site starts `.github/workflows/schematic-build.yml` on the project's
runner, which checks the schematic against that catalog, layers the
extensions onto the base tree (`rootfs/assemble-from-base.sh`, refusing
any extension that would replace a file), writes the squashfs and its
dm-verity tree, signs the update bundle's UKIs with the release key -
the key never leaves the runner - builds the disk, ISO and SD card images
(`image/schematic/build.sh`), boots the amd64 disk under UEFI with SELinux
enforcing (`hack/qemu-schematic-smoke-test.sh`), and uploads everything
to the site with a manifest listing each file's sha256.

`image/schematic/build.sh` works locally too, from a release's assets or
from `make schematic-inputs` (the same files, built from the tree):

```sh
make schematic-inputs     # build/inputs/
image/schematic/build.sh build/inputs amd64 my-schematic.json out/
```

## Building an image with extensions from source

```sh
make extensions-amd64                         # build the extensions (Docker)
echo '{"customization":{"extensions":["node-exporter"]}}' > my-schematic.json
make disk-image SCHEMATIC=my-schematic.json   # or proxmox-image, iso-image, ...
go run ./hack/extpack id -schematic my-schematic.json   # its ID
```

`hack/qemu-extensions-test.sh` (`make qemu-extensions-test`) proves the
whole chain on a real UEFI boot under SELinux enforcing.
