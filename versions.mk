# Single source of truth for every pinned upstream version used by the
# build system (kernel/, pkgs/*). Bumping a version here is the entire
# "montée de version" workflow for Phase 0/1: edit the number (+ sha256),
# open a PR, let ci.yml build the control plane and image-build.yml (manual)
# build+boot-test the image before merging.
#
# Versions below were checked live against upstream on 2026-09-22:
#   - kernel: https://www.kernel.org/releases.json, latest "longterm" branch
#   - haproxy: https://www.haproxy.org/, latest stable branch
# bird/keepalived are optional (Phase 5, NetworkService) and not yet
# pinned - confirm actual target versions before that phase starts.

KERNEL_VERSION  := 6.18.53
HAPROXY_VERSION := 3.4.0

# Single Board Computer tranche follow-up: pkgs/musl-toolchain builds a
# real aarch64-linux-musl cross-toolchain (musl-cross-make, pinned by
# commit - upstream has no version tags at all, just a rolling repo
# versioned by the GCC/musl/binutils releases its own Makefile embeds:
# GCC 9.4.0, musl 1.2.6, binutils 2.44 at this commit) - needed once a
# QEMU-user-mode-emulated `docker build --platform=linux/arm64` (the
# first approach tried) turned out to fail outright on the self-hosted
# runner specifically (`exec format error`, confirmed the runner itself
# is an LXC container whose confinement blocks a nested `docker run
# --privileged`'s own binfmt_misc registration from actually taking
# effect - not fixable by more privilege flags from inside the LXC).
# pkgs/haproxy's own arm64 path cross-compiles zlib/OpenSSL from source
# against this toolchain too (no prebuilt static aarch64 libs needed) -
# their versions are pinned here for the same reason HAPROXY_VERSION is.
MUSL_CROSS_MAKE_REF := 227df8b99103f9c59f6570babf892978e293082f
ZLIB_VERSION        := 1.3.1
OPENSSL_VERSION     := 3.5.4

# Optional network features (Phase 5) - versions TBD.
# Optional extensions (extensions/<name>/, see docs/image-factory.md).
# node_exporter: upstream's static release binaries, sha256 from its own
# sha256sums.txt, per architecture.
NODE_EXPORTER_VERSION      := 1.12.1
NODE_EXPORTER_SHA256_amd64 := b51d8a76aa2a9156a55d501aca6276fae09e262259a5e4e831d2c2222f084e63
NODE_EXPORTER_SHA256_arm64 := ad35b605f9954b9f1ffddf5ba054bdc5a98d790b9eae5291e1eeb83f1ecbd0e7
# nftables for the firewall extension: netfilter.org release tarballs,
# sha256 pinned after checking their GPG signatures - libnftnl and
# nftables by the Netfilter Core Team key
# 8C5F7146A1757A65E2422A94D70D1A666ACF2B21 (certified by its predecessor
# 37D964ACC04981C75500FB9BD55D978A8A1420E4, which signed libmnl 1.0.5;
# also the key Debian's nftables package pins).
LIBMNL_VERSION   := 1.0.5
LIBMNL_SHA256    := 274b9b919ef3152bfb3da3a13c950dd60d6e2bcd54230ffeca298d03b40d0525
LIBNFTNL_VERSION := 1.3.2
LIBNFTNL_SHA256  := c97abc3409f8fa396b4462b2bb7f147a3a47a4ddc97cfa0b2f18890c9cfde8b0
NFTABLES_VERSION := 1.1.7
NFTABLES_SHA256  := a6fbf060d8d4fff001517a2b94f356bb4366bfbf0ba366366f9d27cc38caa58f
# jansson, for nft's JSON output: signed by its author, Petri Lehtinen
# (B5D6953E6D5059ED7ADA0F2FD3657D24D058434C).
JANSSON_VERSION  := 2.15.1
JANSSON_SHA256   := 0c7114dc0b2d22a670724a1f95922029d7077c19dbf79a584cb8084d2f267f2f

# keepalived for the VRRP extension. keepalived doesn't sign its
# releases: this tarball (keepalived.org) is byte-identical to Debian's
# orig tarball (same sha256) and matches Alpine's pinned sha512.
KEEPALIVED_VERSION := 2.3.4
KEEPALIVED_SHA256  := 6afd95ddb7d3e0d3b8b8e5b3a489144131b61a01b06d29e883d0c44acc8a36bf

# QEMU source for qemu-ga: sha256 of the tarball, pinned after checking its
# GPG signature (release key CEACC9E15534EBABB82D3FA03353C9CEF108B584,
# Michael Roth).
QEMU_VERSION := 11.1.2
QEMU_SHA256  := 731b5681e4bb18be313231579b8efd0296c5b015fa36dc533874b639ba838016

# BIRD for the BGP extension: the 2.x branch, single-threaded and still
# maintained - BIRD 3.3.2 (multithreaded) aborted on an assertion
# (birdloop_inside, nest/proto.c) when reconfigured with a protocol
# disabled from its CLI, which janusd's HAProxy gate does. BIRD doesn't
# sign its releases: this tarball (bird.nic.cz) matches FreeBSD ports'
# pinned sha256 and size for the same version.
BIRD_VERSION := 2.19.2
BIRD_SHA256  := aff89abba3b92b7637bd57e0168b8d7ae887747f160ada4973378ad72f5f3660

# Real Raspberry Pi 4/5 hardware follow-up (2026-09-29): the SBBR-
# compliant (UEFI+ACPI) firmware image/rpi-uefi/assemble.sh bundles
# alongside this project's own UKI - see that file's own header for the
# full reasoning (why UEFI+ACPI rather than the project's existing
# devicetree-based QEMU boots, and the real maturity gap between the
# two boards' firmware ecosystems this pinning reflects).
#
# pftf/RPi4 is the well-established, actively-maintained standard (the
# official EDK2 Raspberry Pi 4 platform, pftf's own CI-built releases) -
# pinned to its latest release as of this date. Checksum is the
# release's own published sha256 digest (`gh api repos/pftf/RPi4/
# releases/latest`), not recomputed - GitHub's own release-asset digest
# field, confirmed to match a real local download before being trusted
# here.
PFTF_RPI4_UEFI_VERSION := v1.53
PFTF_RPI4_UEFI_SHA256  := ca9973e2a7a546b3df871cfb7382e656829114b6dfa424f40dc67cc90a217d88

# Raspberry Pi 5 (BCM2712) has no equivalent pftf release at all (no
# pftf/RPi5 repository exists) - the most current, actively-developed
# community alternative found is NumberOneGit/rpi5-uefi (a fork lineage
# of worproject/rpi5-uefi, which is itself archived/no-longer-
# maintained and explicitly warns its firmware may not work on current
# "D0"-stepping boards at all). NumberOneGit's own v0.1 release is
# explicitly the "D0"-targeted build (every Pi 5 board sold from late
# 2024 onward). Genuinely less mature than pftf/RPi4 - a single
# prerelease-tagged version, not a long-running, widely-used project -
# pinned anyway since it's the best available option, with that
# maturity gap documented here and in image/rpi-uefi/'s own header
# rather than hidden.
RPI5_UEFI_VERSION := v0.1
RPI5_UEFI_SHA256  := c4fbbec9cd0d1115c9adab884923061b960de42b4ca6d65ba5f08cb6b46c6fad

# Docker Compose's standalone binary, shipped in the Controller's image
# for janus-controller-updater (dashboard/updater), which updates the
# Controller with it - checked against the release's own .sha256 assets
# (github.com/docker/compose/releases).
DOCKER_COMPOSE_VERSION      := v5.5.1
DOCKER_COMPOSE_SHA256_AMD64 := db1889184726840f75c4f9c001048430d4f25b3be3cb084d3ddd762bc0aed576
DOCKER_COMPOSE_SHA256_ARM64 := 732e3a84c1a0f67256ce80bc2598a24546b10ca05f9faa97efceb1171ece2ef7

# Pebble, Let's Encrypt's ACME test server, and its DNS test server: the
# CA hack/qemu-acme-test.sh issues certificates from (go install'ed at
# this tag - never shipped in an image).
PEBBLE_VERSION := v2.10.1

# The Consul agent (consul extension): HashiCorp's release zip, checked
# against these (from consul_<version>_SHA256SUMS, whose signature by
# HashiCorp's release key C874 011F 0AB4 0511 0D02 1055 3436 5D94 72D7
# 468F was checked when pinning). Consul is under the Business Source
# License 1.1.
CONSUL_VERSION      := 2.0.4
CONSUL_SHA256_amd64 := 7a28033850a24fd411722593931625d8b548a27646c3ab70c1379ea7fd2af423
CONSUL_SHA256_arm64 := 8530dd2f92c1f4acddf152a96e2629a89e6f0f19229889d20dbf8092927aa742
