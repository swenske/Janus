# syntax=docker/dockerfile:1
#
# Builds a real aarch64-linux-musl cross-toolchain (musl-cross-make) -
# GCC + binutils + musl libc, targeting arm64 from an amd64 build host,
# no QEMU/emulation involved anywhere in this build. Exports just the
# toolchain (FROM scratch) via `docker build --target export -o <dir>`,
# same pattern as kernel/Dockerfile and selinux/Dockerfile.
#
# Single Board Computer tranche follow-up: pkgs/haproxy's own arm64
# path (see its own Dockerfile) needs this - a first attempt cross-
# built haproxy for arm64 by running `docker build --platform=linux/
# arm64` against the *existing*, unmodified pkgs/haproxy/Dockerfile,
# letting QEMU user-mode emulation (registered via `docker run
# --privileged tonistiigi/binfmt --install arm64`) run Alpine's own
# musl build-base *as* arm64. That worked locally, but failed outright
# on the self-hosted runner specifically with "exec format error" -
# confirmed (`systemd-detect-virt` on the runner itself) that it's an
# LXC container, and a nested `docker run --privileged`'s own
# binfmt_misc registration silently doesn't take effect from inside
# that confinement (`/proc/sys/fs/binfmt_misc/qemu-aarch64` never
# actually appears there, despite the registration command itself
# reporting success) - not fixable with more privilege flags from
# inside the LXC, since the limitation is the LXC/host boundary itself,
# not anything Docker controls. A genuine cross-compiler avoids the
# problem entirely: it's a normal amd64 program, no emulation needed
# at any point, ever - confirmed by a real local build+haproxy-link
# test *before* writing this file (~5 minutes for the whole toolchain,
# well within CI budget).
#
# See versions.mk for why MUSL_CROSS_MAKE_REF is a commit, not a
# version tag (upstream has none).

ARG MUSL_CROSS_MAKE_REF

FROM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a AS fetch
ARG MUSL_CROSS_MAKE_REF
RUN apt-get update && apt-get install -y --no-install-recommends \
    git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN git clone https://github.com/richfelker/musl-cross-make.git \
    && cd musl-cross-make && git checkout "${MUSL_CROSS_MAKE_REF}"

FROM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a AS build
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential wget bzip2 xz-utils ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src/musl-cross-make
COPY --from=fetch /src/musl-cross-make .
# GCC, binutils, GMP, MPC and MPFR from kernel.org's GNU mirror rather
# than upstream's default, ftpmirror.gnu.org: that redirector and
# ftp.gnu.org behind it went down together on 2026-10-06, and a runner
# with a cold cache couldn't build this toolchain at all. musl-cross-
# make checks every tarball against its own hashes/, whichever mirror
# served it.
RUN echo 'TARGET = aarch64-linux-musl' > config.mak \
    && echo 'OUTPUT = /out' >> config.mak \
    && echo 'GNU_SITE = https://mirrors.kernel.org/gnu' >> config.mak \
    && make -j"$(nproc)" \
    && make install

FROM scratch AS export
COPY --from=build /out /
