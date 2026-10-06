# Image schematics and optional extensions

A Janus image is the base system - kernel, `init`, `janusd`, HAProxy - plus
the optional **extensions** you choose for it, built with the **HAProxy
branch** and **kernel track** you choose. The choice is written down as an
**image schematic**, identified by an ID, the way Talos Image Factory does
it: the same choices always give the same ID, and a node built from a
schematic keeps it through its updates.

## Extensions

| Name | Architectures | What it adds |
|---|---|---|
| `prometheus-node-exporter` | amd64, arm64 | Prometheus [node_exporter](https://github.com/prometheus/node_exporter): CPU, memory, disk, filesystem and network metrics on `:9100/metrics` |
| `qemu-guest-agent` | amd64 | The [QEMU guest agent](https://www.qemu.org/docs/master/interop/qemu-ga.html), for Proxmox and other KVM hypervisors: the node's addresses and OS in the hypervisor's UI, filesystem freeze for consistent backups, clean shutdown from the hypervisor |
| `nftables` | amd64, arm64 | A firewall: the node's nftables ruleset managed through the API and the Controller, applied on trial with an automatic revert, named sets editable live - see [firewall.md](firewall.md) |
| `keepalived` | amd64, arm64 | VRRP: virtual IPs shared by several nodes, moved when one fails or its HAProxy stops answering - see [vrrp.md](vrrp.md) |
| `bird` | amd64, arm64 | BGP, OSPF, BFD with BIRD 2: announce the node's addresses - an anycast address withdrawn while HAProxy doesn't answer - see [bgp.md](bgp.md) |
| `letsencrypt` | amd64, arm64 | Let's Encrypt (or any ACME CA): the node obtains and renews its HAProxy certificates itself - HTTP-01 answered by HAProxy, DNS-01 through a DNS provider's API for wildcards - swapped in without a reload - see [letsencrypt.md](letsencrypt.md) |
| `consul` | amd64, arm64 | The [Consul](https://developer.hashicorp.com/consul) agent with your configuration: HAProxy's servers from Consul's catalog, the node's services in it - see [consul.md](consul.md). Business Source License 1.1 |

`prometheus-node-exporter` was called `node-exporter` up to v2026.10.01-2.
A node built with the old name isn't stuck on it: the image factory
offers it the newest release built with the new name - the same
extensions, a new schematic. The Controller shows that update as a
rename, to install with the schematic change accepted; with janusctl,
pass `-allow-schematic-change`.

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

**prometheus-node-exporter** listens on every address, port 9100, without
authentication, like a stock node_exporter: restrict who can reach it.
Its address, port and collectors are settings - see
[metrics.md](metrics.md#the-node-exporter).

## HAProxy branches and kernel tracks

An image carries one HAProxy and one kernel. A release offers several:

| Choice | Offered | Default |
|---|---|---|
| `haproxy` | the newest HAProxy **LTS branches** that build with AWS-LC, Janus's TLS library: today 3.4, 3.2 and 3.0 (2.8 can't use AWS-LC) | the newest LTS branch |
| `kernel` | a **kernel track**: kernel.org's newest `stable` release (7.2 today) or its newest `longterm` one (6.18) | `stable` |

A schematic that names none gets each release's default, and follows it:
when a release makes a newer LTS branch the default, such an image moves
to it with that update - as the images of the releases up to
v2026.10.06-2, which only had the longterm kernel, move to the stable
track with the release that made it the default: pin `"kernel":
"longterm"` before updating to stay on it. A schematic that names a branch (`"haproxy":
"3.2"`) keeps it from release to release - each release ships that
branch's newest version - until the branch leaves the releases'
offer: an image on it then gets no more updates and is told so (the
Controller warns months before its end of upstream support), and moving
to a newer branch is a schematic change, made on purpose. The kernel
track follows its kernel.org moniker by itself, from branch to branch.

A HAProxy branch changes what the configuration may say: a keyword a
newer branch added is refused by an older one, and every branch has its
own deprecations. Check the configuration against the branch before
moving a node to it (`haproxy -c` with that branch, or a test node).
`STATE`, which holds the applied configuration, is shared by both boot
slots: after a move to another branch and a configuration that uses what
only that branch knows, a rollback to the previous slot boots a HAProxy
that refuses it - [haproxy-config.md](haproxy-config.md) has what differs
from one branch to the next.

Each image says what it is built with, in `/usr/lib/janus/image.json` on
its read-only rootfs (protected by dm-verity like the rest): the node
reports it - `janusctl version` shows its HAProxy (version, branch,
pinned or the release's default) and kernel track, the Controller shows
them, and the node's metrics export them (`janus_component_info`).

Only amd64 images can be built with a HAProxy branch or kernel track other
than the defaults; Raspberry Pi images come with the defaults.

## The schematic

```json
{"customization": {"extensions": ["prometheus-node-exporter", "qemu-guest-agent"], "haproxy": "3.2"}}
```

The same, as shown in YAML:

```yaml
customization:
  extensions:
    - prometheus-node-exporter
    - qemu-guest-agent
  haproxy: "3.2"
```

- Only the extensions, HAProxy branches and kernel tracks a release lists
  (its `schematic-catalog.json`) can be named; order and duplicates of
  extensions don't matter. A HAProxy branch is named `major.minor`
  (`"3.2"`, never a version: the release decides which 3.2.x), a kernel
  track by its name (`"longterm"`). Left out, each is the release's
  default; named, it stays what it is even when it happens to be today's
  default - the default moves, an ID never does.
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

From the Controller, a node's **System › Update** page does it in one
place: **Change the image…** lists what the image factory offers with
the newest release - extensions (those the node's architecture can't
run greyed out), HAProxy branches and kernel tracks - and **Prepare the
update** asks the factory for that release built with the choices -
starting the build if nobody asked for it yet, and following it until
it's ready. The installation form below is then filled in (bundle URL,
sha256, schematic change accepted), and the confirmation says what the
node gains and loses. Installing is the usual update: A/B, automatic
revert if HAProxy isn't healthy (always on for another HAProxy branch),
and `Rollback` back to the previous image. Going back to no extension
and the defaults is the default image, from the GitHub Release.

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
| `rootfs-base-<arch>.tar` | the base system tree: `init`, `janusd`, the SELinux policy, the CA bundle, ... with the SELinux types of its executables |
| `haproxy-<branch>-<arch>.tar` | each HAProxy branch, laid onto the base tree (amd64: every branch the release offers; arm64: the default one) |
| `extension-<name>-<arch>.tar` | each extension's files, manifest and SELinux types |
| `schematic-catalog.json` | the extensions, HAProxy branches (with their versions and end of upstream support) and kernel tracks the release offers, and for which architectures |

The site starts `.github/workflows/schematic-build.yml` on the project's
runner, which checks the schematic against that catalog, lays the
HAProxy of its branch and its extensions onto the base tree
(`rootfs/assemble-from-base.sh`, refusing any layer that would replace a
file), writes the image's `image.json`, the squashfs and its
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
echo '{"customization":{"extensions":["prometheus-node-exporter"]}}' > my-schematic.json
make disk-image SCHEMATIC=my-schematic.json   # or proxmox-image, iso-image, ...
go run ./hack/extpack id -schematic my-schematic.json   # its ID
```

`hack/qemu-extensions-test.sh` (`make qemu-extensions-test`) proves the
whole chain on a real UEFI boot under SELinux enforcing.

A HAProxy branch or kernel track is the same: `SCHEMATIC=` with
`"haproxy": "3.2"` builds that branch (`make haproxy-build-3.2`, and AWS-LC
once for all of them) and lays it onto the rootfs. `make haproxy-build
HAPROXY_BRANCH=3.2` builds only the binary, as `build/haproxy` - for
`make examples-test` or `make local-dev-image`, say.
