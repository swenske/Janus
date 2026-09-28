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
