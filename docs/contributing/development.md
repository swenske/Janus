# Development environment

Janus is built by an ordinary Linux toolchain - nothing of it ships in
the image. A Debian or Ubuntu machine (WSL works) with Go and Docker
builds and tests the control plane; QEMU builds and boots the images.

## What to install

| For | Tools |
|---|---|
| The control plane - `janusd`, `janusctl`, the Controller's backend, the site | Go - the version `go.mod` names (`toolchain go1.26.8`); `make` |
| The gRPC contract | [`buf`](https://buf.build), `protoc-gen-go`, `protoc-gen-go-grpc` - only to change `api/proto` |
| The images, the kernel, HAProxy, extensions, the docs site | Docker (BuildKit), and for the images: `qemu-utils`, `squashfs-tools`, `cryptsetup-bin`, `e2fsprogs`, `gdisk`, `mtools`, `dosfstools`, `xorriso`, `systemd-ukify`, `sbsigntool` |
| Booting them | `qemu-system-x86` (and `-arm` for the Pi), `ovmf` (and `qemu-efi-aarch64`), `python3-virt-firmware` for Secure Boot - access to `/dev/kvm` |
| The Controller's and the site's frontends | Node.js 20.19 or later (Vite 8) - their builds are committed, so only to change them |

The self-hosted runners install the same list
(`.github/actions/runner-setup/action.yml`). The docs site needs no Node
on the machine: it builds in Docker ([writing docs](writing-docs.md)).

## The usual loop

```sh
make build              # bin/janusd, bin/janusctl
make test lint vet      # go test, golangci-lint, go vet
make proto              # after changing api/proto: regenerates gen/ (committed)
```

- **Generated code is committed**: `gen/` (`make proto`), the Controller's
  and the site's frontend builds (`make dashboard-frontend-build`, `make
  site-frontend-build`) - so `go build ./...` needs neither buf nor Node.
  CI fails when a commit doesn't carry what its sources generate.
- **Every RPC needs its role** in `internal/rbac`
  (`TestRequiredRolesCoversEveryRPC` fails otherwise); **every
  Controller route** its gate (`TestRoutesNeedTheirRole`); **every
  `janusctl` command or flag** its place in the command tree
  (`commands_test.go`) - which also regenerates the [janusctl
  reference](../guide/janusctl-reference.md) (`go test ./cmd/janusctl
  -run TestReferenceDoc -update`).

## janusd and HAProxy without an image

The [local dev container](../../local-dev/README.md) runs a real janusd
and a real HAProxy in an ordinary container - the same binaries as the
image, no QEMU - for iterating on the API and HAProxy configurations:

```sh
make local-dev-image
docker run -d --name janus-local-dev --network host janus-local-dev
docker logs janus-local-dev        # its CA and admin credential, printed once
```

## The Controller

```sh
make dashboard-build                          # the frontends, then bin/dashboardd
./bin/dashboardd -data-dir /tmp/janus-dev     # https://localhost:8080/
```

A node from the local dev container can be added to it. The frontend's
design rules, and how a UI change is checked in a real browser:
[Controller UI design](../controller-ui.md).

## An image

```sh
make disk-image             # build/rootfs/disk.img: the kernel, the rootfs, both slots
make qemu-selinux-test      # the rootfs booted, SELinux enforcing, no denial
make qemu-uefi-boot-test    # the UKI booted by OVMF
make kvm-image              # build/janus-kvm.qcow2
```

The first build compiles the kernel and AWS-LC: count tens of minutes;
Docker's cache makes the next ones fast. How the pieces fit: [how an
image is built](../internals/image-build.md). Every test:
[testing](testing.md).
