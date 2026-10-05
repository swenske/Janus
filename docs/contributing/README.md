# Contributing to Janus

Janus is developed in the open on GitHub: issues and pull requests are
welcome. This section is for anyone changing Janus itself - the node's
daemons, the image build, the Controller, the site - and for maintainers
running its CI and releases.

## Principles

- **The build system is not the target.** Dockerfiles, Go modules and
  the CI runners are an ordinary Linux toolchain; the node they build has
  no shell, no package manager and no build tools - it only ever moves
  pre-built bytes into place.
- **A feature is done when it's proven on a real boot**: an image booted
  in QEMU under UEFI, SELinux enforcing with no denial, real HTTP and
  gRPC over mutual TLS - not mocks. Pure logic stays dependency-free so it
  gets plain unit tests too.
- **Every upstream is pinned** - versions and checksums in `versions.mk`,
  base images by digest, GitHub Actions by commit - and followed (see
  [Following upstreams](../upstreams.md)).
- **Commits say what changed, by theme**: `<theme>: message`, for example
  `controller: the extensions panel opens again`.

## Getting started

```sh
make build   # janusd and janusctl, in ./bin
make test
make lint
```

The image build and its boot tests need QEMU, OVMF and Docker - see the
`Makefile` and `hack/`. To try a change to janusd or the Controller
without building an image, use the [local dev
container](../../local-dev/README.md).

## In this section

- [Local dev container](../../local-dev/README.md) - a real janusd and
  HAProxy in an ordinary container.
- [Controller UI design](../controller-ui.md) - the Controller's design
  system and how to verify a UI change.
- [Raspberry Pi hardware tests](../raspberry-pi-testing.md) - testing
  the arm64 images on real boards.
- [CI runners](../ci-runners.md) - the self-hosted runners that build
  and boot-test the images.
- [Following upstreams](../upstreams.md) - how every upstream is
  pinned, checked and watched for vulnerabilities.

Found a security issue? Report it privately - see the [security
policy](../../SECURITY.md).
