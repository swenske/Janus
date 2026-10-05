# Writing an extension

An extension adds an optional feature to Janus images - a daemon, its
configuration through the API, its SELinux domain - without touching the
base system: a node built without it has none of its files. The
existing ones - `keepalived`, `bird`, `nftables`, `consul`,
`letsencrypt`, `prometheus-node-exporter`, `qemu-guest-agent` - are the
models ([images and extensions](../image-factory.md)).

## What one is made of

`extensions/<name>/`:

- **`Dockerfile`** - builds the extension's files from a pinned upstream
  release, checked against its sha256 (`versions.mk`), as static
  binaries, into an `export` stage holding exactly the tree to add to
  the root filesystem (`/usr/local/sbin/...`, licenses under
  `/usr/share/licenses/<name>/`);
- **`manifest.json`** - what it is, and what `janusd` runs:

```json
{
  "name": "qemu-guest-agent",
  "description": "QEMU guest agent for Proxmox and other KVM hypervisors: ...",
  "homepage": "https://www.qemu.org/docs/master/interop/qemu-ga.html",
  "arches": ["amd64"],
  "services": [
    {
      "id": "qemu-guest-agent",
      "description": "QEMU guest agent on the virtio-serial port org.qemu.guest_agent.0",
      "path": "/usr/local/sbin/qemu-ga",
      "args": ["--method=virtio-serial", "--path=/dev/virtio-ports/org.qemu.guest_agent.0", "..."],
      "wait_for": ["/dev/virtio-ports/org.qemu.guest_agent.0"]
    }
  ],
  "selinux_labels": {
    "usr/local/sbin/qemu-ga": "qemu_ga_exec_t"
  }
}
```

| Key | |
|---|---|
| `name`, `description`, `homepage` | What the image builder and the catalog show |
| `arches` | `amd64`, `arm64` - what it's built for |
| `replaces` | Its former names: a node built with one is offered this one |
| `services` | The processes `janusd` supervises - restarted with a backoff when they exit, their output in the node's logs, listed with `janusd` and `haproxy`. `wait_for` paths keep a service `waiting`, not failing, until they exist |
| `selinux_labels` | The SELinux type of each file it installs, applied when the root filesystem is built |

The image carries the manifest at `/usr/lib/janus/extensions/<name>.json`.

## Building and packing

Each extension has a Make target - `make extension-<name>-<arch>` -
that runs its Dockerfile and packs the tree with `hack/extpack` into
`extension-<name>-<arch>.tar`; `make extensions-amd64` builds them all,
and `make schematic-catalog` lists them for a release. A schematic
layers its extensions onto the base tree - an extension that would
replace a file of the base, or of another extension, is refused.

## Its SELinux domain

Every extension's daemon runs in a domain of its own in
`selinux/policy.conf` - its executable's type (`qemu_ga_exec_t`), its
domain (`qemu_ga_t`), the files it may create - with only the rules a
real enforcing boot showed it needs ([testing](testing.md#selinux-every-rule-from-a-real-denial)).
A daemon that parses what the network sends gets the narrowest one.

## Its configuration, through the API

A daemon's configuration is managed by `janusd`, never edited on the
node: saved on STATE, checked by the daemon itself before it's used
(`keepalived --config-test`, `consul validate`, `nft -c`...), copied to
`/run/janus/<daemon>` - where the daemon reads it, in its own SELinux
type - and reloaded (`internal/modcfg`). Its API sits with its kind -
the network ones are `NetworkService` modules (`janusctl network
firewall`, `vrrp`, `bgp`, `consul`) - with a page on the Controller; on
an image without the extension, the API says the module isn't enabled.

## Proving it

An extension is done when an image built with it boots under UEFI with
SELinux enforcing - no denial - and its feature works for real: two
nodes exchanging VRRP adverts, a BIRD session with a real peer, a
certificate from a real ACME server (Pebble). Each has its own QEMU
test - `make qemu-vrrp-test`, `qemu-bgp-test`, `qemu-acme-test`,
`qemu-consul-test`, `qemu-firewall-test`, `qemu-extensions-test`.
