<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="brand/logo/janus-logo-mono-fond-sombre.svg">
    <img src="brand/logo/janus-logo-mono.svg" alt="Janus" width="360" />
  </picture>
</p>

<p align="center">
  <a href="https://github.com/swenske/Janus/actions/workflows/ci.yml?query=branch%3Amain"><img alt="CI" src="https://img.shields.io/github/actions/workflow/status/swenske/Janus/ci.yml?branch=main&label=CI&logo=githubactions&logoColor=white"></a>
  <a href="https://github.com/swenske/Janus/actions/workflows/image-build.yml?query=branch%3Amain"><img alt="Image build and QEMU boot tests" src="https://img.shields.io/github/actions/workflow/status/swenske/Janus/image-build.yml?branch=main&label=image%20build%20%2B%20boot%20tests&logo=qemu&logoColor=white"></a>
  <a href="https://github.com/swenske/Janus/actions/workflows/site-deploy.yml?query=branch%3Amain"><img alt="Docs site" src="https://img.shields.io/github/actions/workflow/status/swenske/Janus/site-deploy.yml?branch=main&label=docs%20site&logo=astro&logoColor=white"></a>
  <br>
  <a href="https://github.com/swenske/Janus/releases/latest"><img alt="Latest release" src="https://img.shields.io/github/v/release/swenske/Janus?sort=date&display_name=tag&label=release"></a>
  <a href="docs/architecture.md#roadmap"><img alt="Status: alpha" src="https://img.shields.io/badge/status-alpha-orange"></a>
  <a href="LICENSE"><img alt="License: MIT" src="https://img.shields.io/github/license/swenske/Janus"></a>
  <a href="go.mod"><img alt="Go version" src="https://img.shields.io/github/go-mod/go-version/swenske/Janus?logo=go&logoColor=white"></a>
  <br>
  <a href="https://janus.sw-servers.net/"><img alt="Website" src="https://img.shields.io/badge/website-janus.sw--servers.net-2f6feb?logo=googlechrome&logoColor=white"></a>
  <a href="https://janus.sw-servers.net/docs/"><img alt="Documentation" src="https://img.shields.io/badge/docs-janus.sw--servers.net%2Fdocs-2f6feb?logo=readthedocs&logoColor=white"></a>
  <a href="https://github.com/swenske/Janus"><img alt="Upstream repository on GitHub" src="https://img.shields.io/badge/GitHub-swenske%2FJanus-181717?logo=github&logoColor=white"></a>
  <a href="#installing-janusctl"><img alt="janusctl apt repository" src="https://img.shields.io/badge/janusctl-apt.sw--servers.net-a81d33?logo=debian&logoColor=white"></a>
  <a href="https://hub.docker.com/r/swenske/janus-controller"><img alt="Janus Controller on Docker Hub" src="https://img.shields.io/docker/pulls/swenske/janus-controller?logo=docker&logoColor=white&label=controller%20pulls"></a>
</p>

# Janus

An ultra-light, immutable, API-driven Linux distribution built from scratch
(LFS-style), inspired by [Talos Linux](https://github.com/siderolabs/talos),
centered on [HAProxy](https://www.haproxy.org/) as the primary
reverse-proxy/load-balancer. No SSH, no interactive shell, no package
manager on the running system - everything is driven through a gRPC API
secured with mTLS. **Janus Controller** (see [`dashboard/`](dashboard/)) is
the companion management dashboard for running one or more nodes.

**Documentation: [janus.sw-servers.net/docs](https://janus.sw-servers.net/docs/)**
- a [quick start](https://janus.sw-servers.net/docs/guide/quickstart/), the
user guide, how it works inside, and how to contribute or integrate it
into a private cloud. The same pages are the Markdown files of this
repository ([`docs/README.md`](docs/README.md) is their index).

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
Proxmox/generic KVM-libvirt, and the signed update bundle.

Three targets are **suspended since 2026-10-30, waiting for testers**:
none was ever verified on real hardware, so nothing builds, tests or
publishes them for now - their code and docs stay, and one line in
`variants.mk` brings each back
([suspended targets](docs/internals/image-build.md#suspended-targets)).
Have the hardware? Offer to test from the
[image builder](https://janus.sw-servers.net/builder).

- **VMware/ESXi**: a VMDK, proven under QEMU with VMware's devices.
- **Bare metal**: a hybrid ISO/GPT installer/maintenance-mode medium
  (bootable via USB) with a real release bundle embedded, so
  `LifecycleService.Install` can provision a target disk using nothing
  but the medium itself; PXE/HTTP Boot is documented
  (`image/pxe/README.md`) - native PXE/HTTP Boot loads the same image
  directly on real hardware, and an iPXE fallback path for firmware
  without that stack, with a known, firmware-specific limitation.
- **Raspberry Pi 4/5** (aarch64/BCM2711): SD card images with UEFI
  firmware, booted under QEMU (`raspi4b` and `virt` machines), never on
  a real board ([hardware tests](docs/raspberry-pi-testing.md)).

See [`docs/architecture.md`](docs/architecture.md) for the design and
roadmap, and [`docs/api-routes.md`](docs/api-routes.md) for the gRPC API
catalog.

## Installing janusctl

`janusctl`, the command-line client for a node's API, is published as a
Debian package with every [release](https://github.com/swenske/Janus/releases),
for amd64 and arm64, and in an apt repository - how to install it:
[docs/janusctl.md](docs/janusctl.md#installing-janusctl).

### Shell completion

Completion for bash, zsh and fish comes with the package:
[docs/janusctl.md](docs/janusctl.md#shell-completion).

## Using janusctl

Sign in to your Janus Controller once (`janusctl login`); janusctl then
reaches its nodes directly - or keeps a fleet itself, without a
Controller: [docs/janusctl.md](docs/janusctl.md#using-janusctl).

### With Claude Code

This repository is a Claude Code plugin marketplace: its janusctl skill
has Claude drive your nodes with janusctl - reading freely, changing
what you ask the safe way, nothing critical without your word
([docs/guide/claude-code.md](docs/guide/claude-code.md)):

```text
/plugin install janusctl --marketplace swenske/Janus
```

## Repository layout

- `api/proto/janus/v1alpha1/` - the gRPC contract (source of truth).
- `cmd/janusd/`, `cmd/janusctl/` - control-plane daemon and CLI.
- `internal/api/` - gRPC service implementations.
- `kernel/`, `pkgs/`, `rootfs/`, `image/` - the from-scratch OS build
  system (Dockerfile-per-component, Talos-`pkgs`-style).
- `dashboard/` - **Janus Controller**, the management dashboard (web UI
  for one or more nodes). See [`dashboard/README.md`](dashboard/README.md)
  to build and run it locally.
- `claude-code/` - the Claude Code plugin (the janusctl skill), offered by
  `.claude-plugin/marketplace.json`.
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
