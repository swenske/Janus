# Janus — Architecture

Janus is an ultra-light, immutable, API-driven Linux distribution built
from scratch (LFS-style), inspired by [Talos Linux](https://github.com/siderolabs/talos)
but centered on HAProxy as the primary reverse-proxy/load-balancer, with
optional network features (BGP via [BIRD](https://bird.nic.cz/),
VRRP via [keepalived](https://www.keepalived.org/), firewalling via
nftables).

## Design goals

- **Immutable**: the root filesystem is a read-only squashfs image; nothing
  on a running node is meant to be hand-edited.
- **API-driven, no shell**: there is no SSH daemon, no interactive shell,
  no package manager on the target OS. Every operation - configuration,
  observability, upgrades - goes through the gRPC API served by
  `janusd` (see `docs/api-routes.md`).
- **Minimal attack surface / minimal CVEs**: only what HAProxy (and the
  optional network features) actually need is compiled in. No unused
  kernel drivers, no unused userspace.
- **CIS-hardened, mTLS, SELinux, trusted boot**: see the dedicated sections
  below.

### Build system vs. target OS - an important distinction

The *build system* (this repo's `kernel/`, `pkgs/`, Dockerfiles, and the
self-hosted GitHub Actions runner it runs on) is an ordinary Linux/Docker
toolchain - full shell, full package manager, the works. That's normal and
expected: it's not the thing being hardened.

The *target OS* (what actually boots on a Janus node) is what has no
shell, no package manager, and no SSH. Nothing in the build system's own
tooling ships into the target rootfs.

## Immutability: A/B partition layout

Modeled on Talos's own disk layout:

| Partition  | Purpose                                                        |
|------------|-----------------------------------------------------------------|
| ESP        | The active slot's Unified Kernel Image at the firmware's default path, and both slots' images |
| `BOOT-A-DATA`, `BOOT-A-HASH` | Slot A: the squashfs rootfs and its dm-verity hash tree |
| `BOOT-B-DATA`, `BOOT-B-HASH` | Slot B, the same |
| `STATE`    | Node identity, mTLS certs, the applied configuration - shared by both slots |

There is no writable overlay and no META partition: what persists is on
STATE, everything else is the read-only image or memory.

Only one slot is active at a time. `LifecycleService.Upgrade` writes the
new image to the *inactive* slot, switches the ESP's default image,
reboots, and watches the new slot's health; if it doesn't report healthy
within the configured timeout, the default is switched back
automatically - no manual intervention needed. Step by step, with the
boot chain: [boot, disk layout and A/B updates](internals/boot.md).

## Trusted boot

- Each `BOOT-*` slot is a **Unified Kernel Image** (kernel + initrd +
  cmdline combined into one signed EFI binary).
- The image is signed with the project's own Secure Boot key (`sbsign`);
  UEFI Secure Boot refuses to boot anything not signed by a key enrolled
  on the node.
- The squashfs rootfs is mounted through **dm-verity**, so any tampering
  with the on-disk image (not just the boot chain) is detected at mount
  time, not silently trusted.
- **Updates are authenticated on the node, before anything is written**,
  whether or not its firmware enforces Secure Boot. `Upgrade` and
  `Install` refuse a bundle whose UKI isn't signed by a release
  certificate built into `janusd` (`internal/releasetrust`, a copy of
  `image/secureboot/production-cert.pem`). The UKI's signed command line
  pins the rootfs's dm-verity root hash, so verifying the UKI
  authenticates the whole bundle, whatever transport or relay it came
  through. `ImageSource.insecure_skip_signature_check` opts out, for
  development and test bundles only.
- **Release pipeline:** the bundle a GitHub Release publishes is the one
  signed in CI (`iso-image-with-bundle` with `SIGNING_KEY`/
  `SIGNING_CERT`), and publication fails if its UKIs don't verify against
  the production certificate. Rotating the key means adding the new
  certificate to `internal/releasetrust/certs/` and keeping the old one
  until every node trusts the new one.

### Boot time

What a node's boot costs is measured, not guessed. `rootfs/init` prints
when the kernel handed over to it and when it started janusd; the boot's
first janusd logs `boot: api listening Ns after the kernel started
(kernel, init, haproxy)` and the exporter serves the same four moments as
`janus_boot_stage_seconds{stage}` ([metrics](metrics.md)). All of them
are on the kernel's uptime clock: the firmware's own time (POST, the UEFI
boot manager) comes before it and is the one part Janus can't shorten.

Under KVM (OVMF, 2 vCPUs) the kernel hands over at about 0.8 s, HAProxy
serves at about 1 s and the API listens at about 1.4 s after the kernel
started; the firmware adds about 2 s. The kernel's own second is shared
between clearing memory at boot (`init_on_free`, a hardening choice that
scales with RAM), ACPI and device probing, and `ip=dhcp`, which blocks
until the first interface has a lease - the one wait that grows on a
real network.

The image is built for that: the kernel and the squashfs are compressed
with zstd (`CONFIG_KERNEL_ZSTD`, `mksquashfs -comp zstd`, decompressed
on every CPU rather than one), which decompresses three to four times
faster than the gzip and xz used before for a few percent more bytes -
in a boot, every binary read from the root filesystem goes through that
decompressor. Measured against the previous compression on two KVM hosts,
the API listened a third sooner (2.0 s to 1.4 s, 2.3 s to 1.5 s).

The firmware's time is the one part a node can skip: a reboot or an
update with `RebootMode KEXEC` loads the UKI's kernel and command line
(its own PE sections - the same signed command line, the same verity
root hash) and jumps into it (`internal/kexec`). It keeps the
firmware's rule on what may boot: with Secure Boot enforced, only a
UKI signed by a release certificate (`internal/secureboot` reads the
UEFI variables); the kernel itself verifies nothing (no
`CONFIG_KEXEC_SIG`: it could only check a signature on the bare
bzImage, not the command line that pins the root filesystem). It is
opt-in, because a device left in an odd state by the running kernel is
kexec's known risk; a revert after a failed health check always goes
through the firmware. One such state is known (2026-10-07): under
OVMF's Secure Boot-capable firmware with the flash in secure mode -
its UEFI variable services run in SMM - the kexec'd kernel corrupts
itself as soon as it uses the EFI runtime services (`efi=noruntime`
makes it boot), so Proxmox VE's q35 machines and libvirt's
`secure-boot` machines reboot through the firmware after a crash.
Passing `efi=noruntime` to the kexec'd kernel, with the Secure Boot
state carried on its command line instead of read from variables it
no longer has, is the candidate fix.

## mTLS / PKI

Every gRPC call is authenticated with a client certificate - there is no
unauthenticated endpoint (`internal/pki`, implemented in Phase 2). Each
node maintains its own self-signed ECDSA P-256 CA (10-year validity),
and issues itself a server certificate for the gRPC listener - reissued
whenever the node's addresses or hostname change, and renewed 30 days
before it expires. On first boot
it also issues an initial admin client certificate and prints it once
(there's no shell to retrieve it later) - the trust anchor a real
deployment would instead hand out through `LifecycleService.Install`'s
side channel (Phase 3, not built yet). `SystemService.
GenerateClientConfiguration` issues further client certificates once
you already have one, named after who they're for (the common name) and
valid for the time asked - one year at most, no renewal/rotation flow;
roles are carried in the certificate's `Subject.Organization` field (the
Kubernetes client-cert-auth idiom).

The node's secrets never leave it through the file API: `Read` refuses
and `Copy` leaves out the PKI's private keys, the Controller
registration token, the ACME account key and DNS provider credentials
(by location, checked on the path the kernel resolved for the open file,
so symlinks and `/proc/self/root` don't go around it), and any file
holding a PEM private key, wherever it is - HAProxy's certificates
included (`internal/api/secretfiles.go`). No other API returns them
either: without this, an `os:admin` certificate could read the CA's key
and mint itself certificates valid long after its own expired.

A node also trusts its **fleet** once it joined one: it pins the fleet's
root CA - whose key stays offline - and applies the bundles that root
signs, each newer than the last, listing the issuing CAs whose client
certificates it accepts (`internal/pki/fleet.go`, `AccessService`). An
issuing CA leaves the fleet with the next bundle, without touching the
nodes; a certificate the root signs itself lets in too - the way back if
every issuing CA is lost. The node's own CA always lets in, and only it
can make the node forget its fleet. The TLS configuration is built per
connection, so a new bundle counts from the next one.

The Controller holds a fleet (`dashboard/backend/internal/fleet`): set
up from its page, the root's key goes in a recovery kit the operator
keeps - an age file encrypted with a passphrase - and leaves the
Controller once the operator gave both back. It keeps the issuing CA,
its key sealed with a master key kept outside its data directory
(`internal/secrets`), and signs itself a `janus:controller` certificate
valid a day. A background loop brings every node to trust the fleet -
with the service credential it got when the node was added, then
checked with the fleet certificate - and deletes that credential: from
then on the Controller holds no credential per node, and every call it
makes names the user it's made for.

Those users are the Controller's accounts (`dashboard/backend/
internal/auth`): reader, operator or admin, each mapped to the node role
of the same name for the calls the Controller makes for it - so a bug in
the Controller's checks can't make a reader an admin on a node. Every
Controller route names the role a read of it and a change of it need
(`gate`); an API token is its account's, with that role or less; every
change and sign-in is in the Controller's audit, and what reaches a node
in the node's log too. A node's page is the Controller's too, under
`/nodes/<id>/` behind the session, with no certificate in the browser.
janusctl signs in to the Controller (`janusctl login`) and gets a short
certificate of the fleet for the account - the account's name and role
in it -, then reaches the nodes directly: each node takes it through its
bundle, applies the role and logs the account. It signs in with an SSH
key of the account - an SSHSIG of the Controller's challenge, bound to
the certificate it saw - and the certificate is for that key, which
then signs the TLS handshakes with the nodes (from ssh-agent, Ed25519
only: TLS 1.3 hands other keys a digest, an agent signs messages); in
CI, with an API token, for a key janusctl makes; without an SSH key,
through the Controller's page, which approves a key janusctl made by its
fingerprint (a code handed back to janusctl on 127.0.0.1, or typed on
the page from another machine).
An account whose permissions differ from node to node - grants on the
nodes some labels pick, a token narrowed to some, no role over all of
them - gets a scoped certificate instead (`internal/pki/scope.go`): no
role in its subject (`janus:scoped` stands there, so a node of an older
release refuses it), and an extension listing what it may do on every
node and on each node it reaches, by the SHA-256 of that node's CA's
key - the key, not the certificate: after a CA rotation the Controller
pins the new CA's cross-signed certificate, same key. The node finds its
own entry and applies its roles and domains, as for the Controller's
calls; a node it doesn't name, or one labelled after the sign-in, is
refused until the next one. The extension isn't critical: Go fails a
TLS handshake on a critical extension it doesn't know, before the node
reads it - on this release too. A role-limited issuing CA signs no
scope giving more than its roles.
A second factor - TOTP, its secret sealed with
the master key, or a WebAuthn passkey - finishes the sign-in of an
account that has one, required for admins by default.

The Controller backs itself up (`dashboard/backend/internal/backup`,
`backups.go`): its data directory, its master key and its nodes'
configurations (through their API - never their private keys), a tar.gz
encrypted with age to a backup kit's identity - the Controller keeps
only its public half - and admins' keys, behind a manifest it signs
with Ed25519; to an S3 bucket (`internal/s3`: SigV4, put/get/list/
delete), on a schedule. A new Controller restores one from its first
page or `dashboardd restore`, the signature checked with the key the kit
holds: it's the same Controller again, its fleet with it.

A node registering itself with a Controller whose fleet is ready sends
no key at all (`internal/selfregister`, protocol 2): its CA's
certificate, its token if the Controller created it, and a secret it
polls with. Admitted - at once for a token, or once approved - it takes
the fleet's trust from the Controller's answer; the Controller records
it trusting the fleet from the start, with no credential for it. A
Controller without a fleet, or older, answers that it can't, and the
node announces itself with a service credential as before. A node
provisioned with the fleet's root checks the Controller through it: it
asks the registration endpoint for `controller.fleet.janus` (SNI) and
gets a certificate the issuing CA signed, renewed by the Controller -
nothing pinned that expires - and takes only that fleet's trust; the
Controller's own certificate remains for nodes provisioned with it.

A fleet needs no Controller: janusctl keeps one itself (`cmd/janusctl/
fleet.go`, [fleet-without-controller.md](fleet-without-controller.md)) -
the root in the same recovery kit (`internal/fleetkit`, shared with the
Controller), an issuing CA per machine (its key a file of janusctl's
configuration, signing that machine's 12-hour certificates on the spot),
bundles signed with the kit when a machine joins or leaves - each issuing
CA limited, in the bundle, to the roles its machine's certificates may
carry (`Bundle.Limits`, enforced by the node's `AcceptChains`: an
operator's CI signs no admin, nor a Controller) - and synced to the nodes
by any machine (`TrustGet` returns the signed bundle; the
newest wins, versions in milliseconds). A node can trust the fleet from
its first boot: provisioning writes the root and the bundle to STATE
`pki/fleet/` (`Install`, `diskseed.SeedFleet`, NoCloud via
`pki.ProvisionFleet` in init), where `Fleet.Set` keeps them. janusctl
pins each node's own CA (`TrustGet.local_ca_cert`), checked against the
fingerprint the node's console prints and against the server
certificate the node presented - never trusted on first use. With the
kit, a machine reaches a node with a certificate the root signs, and
re-signs its bundle above the node's: the way back when every machine,
or a Controller, is lost.

The node's own CA can be replaced too (`AccessService.LocalCARotate`):
every certificate it issued stops working - the first-boot admin one, a
Controller's service credential - while the fleet's keep letting in. The
new admin certificate is issued for a public key the caller sends (its
key never seen by the node), or printed on the console like at first
boot. The old CA cross-signs the new one, and the node serves that
certificate after its server certificate: a client that pinned the old
CA still verifies the node - the Controller then pins the new one
(`nodeproxy.followCA`). The new files are
written to `pki/rotation/` and marked ready before they're moved in
place; a boot finishes a ready rotation and forgets an unfinished one
(`internal/pki/rotate.go`).

Roles are enforced, not just carried: `internal/api/authz.go`'s
`UnaryAuthInterceptor`/`StreamAuthInterceptor` check every single RPC
(both services are wired via `grpc.UnaryInterceptor`/
`grpc.StreamInterceptor` in `cmd/janusd`) against a static
method -> required-roles table (`internal/rbac`, which the Controller
reads too: a node's page offers only what the account's role may call). Three roles exist: `os:admin`
(everything), `os:operator` (runs what's set up - HAProxy, services,
reboots - not how the node is set up; see [api-routes.md](api-routes.md))
and `os:reader` (observability/status RPCs only - explicitly *not*
`List`/`Read`/`Copy`/`Dmesg`/`Logs`/`DiskUsage`/`PacketCapture`, which
don't mutate anything but can expose sensitive file contents or traffic,
nor `GenerateClientConfiguration` itself, since issuing credentials is
its own privileged operation - an `os:reader` cannot mint itself an
`os:admin` cert). The table is fail-closed: an RPC with no entry
defaults to admin-only, and a test (`internal/api/authz_test.go`)
registers every service against a real `*grpc.Server` and cross-checks
its actual method list against the table in both directions, so a new
RPC that forgets an entry is caught immediately rather than silently
defaulting open. Verified for real too: an `os:reader` certificate can
call `HAProxyService.ShowInfo` but gets `PermissionDenied` calling
`ApplyConfig` or trying to self-escalate via
`GenerateClientConfiguration`.

## SELinux and CIS hardening

- A minimal, project-specific SELinux policy module confines `janusd`,
  HAProxy, and the optional network daemons to exactly the syscalls/files
  they need (Phase 4) - not a stock distro policy.
- Kernel hardening: `lockdown=confidentiality`, no loadable kernel modules
  at runtime in production builds (or a tightly restricted allow-list if
  a specific driver genuinely needs to load late), and the kernel
  parameters of the CIS benchmark's controls (CIS Debian Linux 13
  Benchmark v1.0.0, Level 2 - Server, sections 1.5 and 3.3) written by
  init at every boot - also on every network interface, where the kernel
  reads its own value, before IPv6 goes on (a node boots with it off) -
  and audited. Only a whitelist of the parameters
  HAProxy depends on can change at runtime (`internal/sysctl`, within
  bounds, on trial - see [kernel tuning](guide/kernel-tuning.md)): never
  one of the benchmark's, which SELinux keeps out of janusd's reach too
  (only the whitelist's files are `sysctl_tunable_t`) - only init writes
  them, and no other daemon writes a kernel parameter at all.
- No setuid binaries beyond what's strictly required; `janusd` runs as
  the sole privileged process, dropping capabilities it doesn't need.

## The "no shell" API surface

Talos replaces shell access with a fixed set of read-only, scoped gRPC
methods (`List`, `Read`, `Copy`, `Logs`, `Dmesg`, `PacketCapture`) instead
of arbitrary command execution. Janus follows the same approach - see
`docs/api-routes.md` for the full catalog, derived directly from Talos's
own `machine.proto`/`lifecycle.proto` (verified against
`siderolabs/talos` on GitHub, not reconstructed from memory).

## Network configuration and time

A node's hostname, interfaces (physical and 802.1Q VLANs, static or the
kernel's boot DHCP lease), resolvers and NTP servers are one declarative
document (`internal/netconfig`), applied by `janusd` itself over rtnetlink
(`internal/netmgr`) - no `ip`, no network daemon. A change is applied **on
trial** and reverted automatically unless confirmed over an address it
keeps, so a mistake can't strand a node that has no console; only a
confirmed configuration reaches STATE. The node's TLS server certificate
is reissued when its addresses or hostname change. `janusd` is also the
NTP client (`internal/timesync`, SNTP + `adjtimex`), and a first boot
whose clock is obviously wrong (no RTC) waits for NTP before generating
its certificates. See [network-configuration.md](network-configuration.md).

Known limitation, planned improvement: DHCP is the kernel's own
(`ip=dhcp`), done once at boot on a single interface and never renewed.
A userspace DHCP client with lease renewal, and DHCP on any interface,
would lift both limits.

## Optional extensions and image schematics

Optional software - today node_exporter and the QEMU guest agent - is
chosen per image, not installed on a node: an **image schematic** names
the extensions, and the build layers them onto the read-only rootfs.
The schematic's ID is written into the signed kernel command line, so a
node knows its schematic and `Upgrade` refuses an update built from
another one - a node keeps its extensions. `janusd` supervises the
extensions' services, each in its own SELinux domain. See
[image-factory.md](image-factory.md).

## Bare metal

The images aren't tied to QEMU's virtio devices. The UKIs name their
root partitions by GPT label - `dm-mod.create="... verity 1
PARTLABEL=BOOT-A-DATA PARTLABEL=BOOT-A-HASH ..."` plus `dm-mod.waitfor=`
for the same two, so a USB or NVMe disk that appears late is waited for;
the kernel resolves labels itself (`early_lookup_bdev`). The same disk
therefore boots as `/dev/vda`, `/dev/sda` or `/dev/nvme0n1`.
`internal/bootslot` resolves the label back to the partition the verity
root is mapped from (the one with a device-mapper holder, in sysfs) and
derives STATE, the ESP and the other slot from it as before;
`WholeDisk` gives `/dev/nvme0n1` where the partition prefix is
`/dev/nvme0n1p`. The installer ISO uses labels of its own
(`JANUS-ISO-DATA`/`-HASH`) so that, while it installs a disk, it never
mounts the new disk's `BOOT-A-*` instead of its own. One consequence: one
Janus installation per machine - two would carry the same labels.

The x86 kernel carries what common servers and PCs need: SMP (512 CPUs),
x2APIC with interrupt remapping, NUMA, MSI-X; AHCI, NVMe, USB storage,
virtio-scsi, pvscsi, MegaRAID, mpt3sas, SmartPQI; Intel (e1000 to i40e),
Realtek r8169, Broadcom tg3/bnxt, Mellanox mlx5 and vmxnet3 NICs - none
needs a firmware file, none is shipped. The console is the screen (EFI
framebuffer) and the serial port: `rootfs/init` copies userspace output
- janusd's motd and first-boot credentials included - to `/dev/tty0`,
best effort, never holding the serial console back. Proven by
`hack/qemu-baremetal-test.sh`.

## Optional network features

`bird` (BGP, [bgp.md](bgp.md)), `keepalived` (VRRP, [vrrp.md](vrrp.md))
and `nftables` (the firewall, [firewall.md](firewall.md)) are extensions,
chosen per image in its schematic ([image-factory.md](image-factory.md)).
When a feature isn't chosen, its binary is simply absent from the built
rootfs image - not installed-but-disabled, genuinely not present, which is
what keeps a minimal node's attack surface and image size down.
`NetworkService`'s corresponding RPCs report `MODULE_STATE_NOT_ENABLED`
rather than erroring in that case. When present, each daemon's own
configuration file is managed through the API - checked by the daemon,
saved on STATE, applied - and keepalived and BIRD follow HAProxy's health,
so a node whose HAProxy stops answering gives up its virtual IPs and
withdraws its anycast routes.

Two more extensions sit next to HAProxy rather than the network:
`letsencrypt` ([letsencrypt.md](letsencrypt.md)) - janusd obtains and
renews HAProxy's certificates through an ACME client of this project's
own (`cmd/janus-acme`, on lego), run for each exchange with the CA with
nothing on disk of its own, HTTP-01 answered statelessly by HAProxy - and
`consul` ([consul.md](consul.md)), the Consul agent with the operator's
configuration, for HAProxy's service discovery.

## The Controller's own nodes

The Controller (`dashboard/`) can create its nodes itself on the
hypervisors it's given - libvirt/KVM first, over SSH in pure Go
([hypervisors.md](hypervisors.md)). Three choices shape it:

- **Ownership is checked, not assumed.** Every virtual machine carries
  the Controller's ID and its own in its metadata, and every operation
  checks them on the hypervisor before acting; on the host, a polkit
  policy makes libvirt enforce the same boundary.
- **Creating the machine is the approval.** Its NoCloud volume carries a
  one-time registration token (only its hash is kept); the node presents
  it and is admitted at once. Every other registration still waits for a
  human.
- **Long operations belong to the machine, not to a request.** Creation
  runs in the background with its phase and history saved with the
  machine, so the API is resource-shaped - what a Terraform provider
  (users declaring nodes, the Controller doing the hypervisor work) will
  build on.

## Companion website

janus.sw-servers.net (`site/`, in this repository) is served by a small Go
backend (`site/backend`, janus-site) on the project's own Proxmox, in a
container distinct from the CI runners: the landing page, the image
factory - schematics, custom builds dispatched to the self-hosted
runner, downloads, the update lookup nodes and Controllers use (see
[image-factory.md](image-factory.md)) - and these docs, built from this
repository (`site/docs`): the newest release's at `/docs/`, main's at
`/docs/next/`.

## Roadmap

Every phase, done and planned, has its own page: [roadmap.md](roadmap.md).
