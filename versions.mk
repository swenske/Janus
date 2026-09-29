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
BIRD_VERSION       :=
KEEPALIVED_VERSION :=

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
