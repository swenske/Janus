<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="brand/logo/janus-logo-mono-fond-sombre.svg">
    <img src="brand/logo/janus-logo-mono.svg" alt="Janus" width="360" />
  </picture>
</p>

# Janus

An ultra-light, immutable, API-driven Linux distribution built from scratch
(LFS-style), inspired by [Talos Linux](https://github.com/siderolabs/talos),
centered on [HAProxy](https://www.haproxy.org/) as the primary
reverse-proxy/load-balancer. No SSH, no interactive shell, no package
manager on the running system - everything is driven through a gRPC API
secured with mTLS. **Janus Controller** (see [`dashboard/`](dashboard/)) is
the companion management dashboard for running one or more nodes.

Optional network features, chosen per image and configured through the
API and the Controller: [BGP](docs/bgp.md) via [BIRD](https://bird.nic.cz/),
[VRRP](docs/vrrp.md) via [keepalived](https://www.keepalived.org/), a
[firewall](docs/firewall.md) via nftables - keepalived and BIRD follow
HAProxy's health. Every node also serves its own
[Prometheus metrics](docs/metrics.md).

> **Janus** is the Roman god of beginnings and endings, of choices, of
> passage, and of doors - traditionally shown with two faces looking in
> opposite directions at once. This project is named for that duality
> (frontend/backend, ingress/egress, the two faces of a reverse proxy),
> not for any connection to the HAProxy project. **Janus has no
> affiliation with, and is not supported, endorsed, or reviewed by,
> HAProxy Technologies or the maintainers of HAProxy** - it is an
> independent project that happens to run HAProxy as its data plane, the
> same way it could run any other proxy. "HAProxy" above and throughout
> this repository refers strictly to the upstream software.

**Status: early alpha.** Real bootable images exist (kernel hardening,
dm-verity, A/B updates, Secure Boot, SELinux enforcing by default), built
and boot-tested on every image workflow run: an unsigned qcow2 for
Proxmox/generic KVM-libvirt, a VMDK for VMware/ESXi, and a hybrid
ISO/GPT installer/maintenance-mode medium (bootable via USB or optical
media) with a real release bundle embedded, so `LifecycleService.Install`
can provision a target disk using nothing but the medium itself. PXE/HTTP
Boot is documented (`image/pxe/README.md`) - native PXE/HTTP Boot loads
the same image directly on real hardware; an iPXE fallback path is
also documented for firmware without that stack, with a known,
firmware-specific limitation. A Raspberry Pi 4/5 (aarch64/BCM2711) port
has its first proven increment: a minimal kernel boots `rootfs/init`
as PID 1 under QEMU's `raspi4b` machine (`make qemu-raspi4-boot-test`),
though real hardware support (SD/MMC, USB, Ethernet, UEFI firmware) is
not yet built - see `docs/companion-site-builder-scope.md`. See
[`docs/architecture.md`](docs/architecture.md) for the design and roadmap,
and [`docs/api-routes.md`](docs/api-routes.md) for the gRPC API catalog.

## Installing janusctl

`janusctl`, the command-line client for a node's API, is published as a
Debian package with every [release](https://github.com/swenske/Janus/releases),
for amd64 and arm64. It is a static binary: the same package installs on
any Debian or Ubuntu release (apt 2.4 or later for the `.asc` key below -
Debian 12, Ubuntu 22.04 and newer).

```sh
# 1. The repository's signing key
sudo install -d -m 0755 /etc/apt/keyrings
sudo curl -fsSL https://apt.sw-servers.net/apt-sw-servers.net.gpg.asc \
  -o /etc/apt/keyrings/apt-sw-servers.net.asc

# 2. The repository
echo "deb [signed-by=/etc/apt/keyrings/apt-sw-servers.net.asc] https://apt.sw-servers.net/janus stable main" \
  | sudo tee /etc/apt/sources.list.d/janus.list

# 3. Install
sudo apt-get update
sudo apt-get install janusctl
janusctl version
```

The key's fingerprint is `0731 333D 9DDF FF94 08CD 6AEC A333 9293 BD0C BBDC`
(`gpg --show-keys /etc/apt/keyrings/apt-sw-servers.net.asc`). Later
releases come with `apt-get upgrade`.

Without the repository, each release also carries
`janusctl_<version>_<amd64|arm64>.deb` (`sudo apt install
./janusctl_<version>_amd64.deb`); `make build` builds it from source.

## Using janusctl

Sign in to your Janus Controller once; janusctl then reaches its nodes
directly, with a certificate of the Controller's fleet for your account:

```sh
# With an SSH key of your account - added on the Controller's page (your
# account, "SSH keys for janusctl"), which also shows this command:
janusctl login -controller janus-controller.example.com \
  -controller-fingerprint DC:CC:FF:... \  # the Controller's certificate, checked
  -user sam -ssh-key ~/.ssh/id_ed25519.pub
janusctl nodes                            # the nodes, and which trust the fleet
janusctl -n edge-1 system info            # one node
janusctl -n edge-1,edge-2 haproxy show-info
janusctl -all version                     # every node of the fleet
janusctl context list                     # the Controllers signed in to
```

The certificate is for the SSH key itself and lasts 12 hours: with the
key in ssh-agent, janusctl renews it by itself. `-ssh-key` takes the key
in ssh-agent (its `.pub`; Ed25519 - the agent can't sign a TLS handshake
with ECDSA or RSA) or a private key file (Ed25519, ECDSA or RSA; its
passphrase asked); a FIDO key (`sk-...`) can't sign janusctl's
connections. Each key can carry a lower role than your account's.

Without an SSH key: `janusctl login -controller ...` alone opens the
Controller's page in the browser - sign in there (second factor and
all), compare the key it shows with the one janusctl printed, approve;
`-device` instead shows a code to enter on the page from any machine (a
server without a browser). Either way the certificate is for a key
janusctl made and lasts 12 hours; then `janusctl login` again.

In CI, an API token of the account instead (`JANUS_TOKEN=janus_...
janusctl login -controller ...`): a certificate for an hour, with the
token's role, renewed while `JANUS_TOKEN` is set.

The first login asks for the Controller's certificate: `-controller-
fingerprint` (the account dialog's command has it; on its host:
`openssl x509 -in <data-dir>/dashboard-identity.crt -noout -fingerprint
-sha256`) or `-controller-ca FILE`. The SSH key signs a challenge for
the certificate janusctl saw: a signature relayed by another server is
refused.
Each node checks the role itself and logs who acted. The configuration
is `~/.config/janus/janusctl.json` (`JANUSCONFIG` elsewhere), each
context's key and certificate next to it.

A node's own certificate still works, and comes first: `janusctl
-endpoint NODE:9505 -ca ca.crt -cert admin.crt -key admin.key ...` - the
first-boot admin certificate, the way in when nothing else does.

### Without a Controller

janusctl can keep a fleet itself - its root's key in a recovery kit,
offline, each machine signing itself 12-hour certificates with an
issuing CA of its own:

```sh
janusctl -context lab fleet init -issuer alice-laptop janus-kit.age
janusctl fleet export provision/        # for image seed-fleet, install, NoCloud
janusctl fleet adopt edge-1 -endpoint 192.0.2.10 -ca-fingerprint 9bc3...   # its console's "ca sha256"
janusctl -all haproxy show-info
```

More machines, revoking one, recovering from the kit (a lost
Controller's too): [docs/fleet-without-controller.md](docs/fleet-without-controller.md).

## Repository layout

- `api/proto/janus/v1alpha1/` - the gRPC contract (source of truth).
- `cmd/janusd/`, `cmd/janusctl/` - control-plane daemon and CLI.
- `internal/api/` - gRPC service implementations.
- `kernel/`, `pkgs/`, `rootfs/`, `image/` - the from-scratch OS build
  system (Dockerfile-per-component, Talos-`pkgs`-style).
- `dashboard/` - **Janus Controller**, the management dashboard (web UI
  for one or more nodes). See [`dashboard/README.md`](dashboard/README.md)
  to build and run it locally.
- `hack/` - local dev tooling (QEMU test harness).

## Building the control plane

```sh
make build   # binaries in ./bin
make test
make lint
make proto   # regenerate gen/ from api/proto/**.proto (requires buf)
```

## License

[MIT](LICENSE)
