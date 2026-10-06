# syntax=docker/dockerfile:1
#
# Fetches and verifies the pinned kernel source of a track (see
# ../versions.mk, ../variants.mk), provides a "config" stage used
# interactively by `make kernel-menuconfig` (see ../Makefile) to run
# `make menuconfig`, a "build" stage that compiles bzImage from the
# track's configs/janus_<track>_defconfig, and an "export" stage
# (`FROM scratch`) that holds just the resulting bzImage, the files the
# build read and the configuration kbuild resolved (the committed one must
# be it: `make kernel-config-refresh` otherwise), so `docker build
# --target export --output type=local,dest=<dir>` can pull them out
# without a container filesystem attached. "selinux-classes" exports the
# SELinux classes this kernel compiles in (scripts/selinux/mdp - `make
# selinux-classes`).
#
# Proven end-to-end locally (bzImage boots under QEMU with rootfs/init as
# PID 1, see ../hack/qemu-run.sh) before being encoded here - see
# ../docs/architecture.md's Phase 1.
#
# Single Board Computer tranche: also cross-builds the aarch64/BCM2711
# (Raspberry Pi 4/5) kernel + device tree from the same source, selected
# via build args (ARCH=arm64, CROSS_COMPILE=aarch64-linux-gnu-,
# DEFCONFIG=janus_rpi4_defconfig, MAKE_TARGETS="Image dtbs") and pulled
# out via the separate "export-arm64" stage, since the arm64 output
# shape (Image + a .dtb, no bzImage at all) genuinely differs from x86's
# - see kernel/configs/janus_rpi4_defconfig's own header for what's
# proven so far (console boot only, real hardware not yet covered) and
# ../hack/qemu-raspi4-boot-test.sh for the QEMU boot proof.

ARG KERNEL_VERSION
ARG KERNEL_SHA256

# The tarball is checked against KERNEL_SHA256, pinned in versions.mk
# after checking its kernel.org signature - nothing is extracted before.
FROM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a AS fetch
ARG KERNEL_VERSION
ARG KERNEL_SHA256
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl ca-certificates xz-utils \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN test -n "${KERNEL_SHA256}" \
    && major="$(echo "${KERNEL_VERSION}" | cut -d. -f1)" \
    && curl -fsSLO "https://cdn.kernel.org/pub/linux/kernel/v${major}.x/linux-${KERNEL_VERSION}.tar.xz" \
    && echo "${KERNEL_SHA256}  linux-${KERNEL_VERSION}.tar.xz" | sha256sum -c - \
    && tar xf linux-${KERNEL_VERSION}.tar.xz

FROM fetch AS config
ARG KERNEL_VERSION
ARG DEFCONFIG=janus_longterm_defconfig
# crossbuild-essential-arm64 pulls in the aarch64-linux-gnu- cross
# toolchain alongside the native one - installed unconditionally so this
# one image serves both ARCH=x86_64 and ARCH=arm64 builds without a
# separate config/build stage per architecture.
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential libncurses-dev flex bison bc libelf-dev \
    crossbuild-essential-arm64 \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src/linux-${KERNEL_VERSION}
COPY configs/${DEFCONFIG} .config
CMD ["make", "menuconfig"]

FROM config AS build
ARG KERNEL_VERSION
ARG ARCH=x86_64
ARG CROSS_COMPILE=
ARG MAKE_TARGETS=bzImage
WORKDIR /src/linux-${KERNEL_VERSION}
RUN make ARCH=${ARCH} CROSS_COMPILE=${CROSS_COMPILE} olddefconfig \
    && make ARCH=${ARCH} CROSS_COMPILE=${CROSS_COMPILE} -j"$(nproc)" ${MAKE_TARGETS}
# Every source file and header the build read, from kbuild's .o.cmd files
# (source_<obj> and deps_<obj>): hack/upstream keeps the kernel CVEs whose
# fixes touch one of them - kernel/built-files-<track>-<arch>.txt (make
# kernel-built-files). Host tools (tools/, scripts/) and system headers
# aren't the kernel.
RUN find . -name '.*.o.cmd' -print0 | xargs -0 cat \
    | sed -n -e 's/^source_[^ ]* := \([^ ]*\)$/\1/p' -e 's/^  \([^ $][^ ]*\.[chS]\)\( \\\)\{0,1\}$/\1/p' \
    | sed 's|^\./||' | grep -vE '^(/|\.\./|tools/|scripts/)' | sort -u > /built-files.txt \
    && test "$(wc -l < /built-files.txt)" -gt 1000

FROM scratch AS export
ARG KERNEL_VERSION
COPY --from=build /src/linux-${KERNEL_VERSION}/arch/x86/boot/bzImage /bzImage
COPY --from=build /built-files.txt /built-files.txt
COPY --from=build /src/linux-${KERNEL_VERSION}/.config /config

# The object classes, permissions and policy capabilities this kernel
# compiles in, as the policy's first half (../selinux/classes.conf, made
# from the newest kernel track's by `make selinux-classes`).
FROM config AS mdp
ARG KERNEL_VERSION
RUN make ARCH=x86_64 olddefconfig >/dev/null \
    && make ARCH=x86_64 prepare >/dev/null \
    && make ARCH=x86_64 scripts/selinux/ >/dev/null \
    && cd scripts/selinux/mdp && ./mdp policy.conf file_contexts \
    && sed '/^type base_t/,$d' policy.conf > /classes.conf

FROM scratch AS selinux-classes
COPY --from=mdp /classes.conf /classes.conf

FROM scratch AS export-arm64
ARG KERNEL_VERSION
COPY --from=build /src/linux-${KERNEL_VERSION}/arch/arm64/boot/Image /Image
COPY --from=build /src/linux-${KERNEL_VERSION}/arch/arm64/boot/dts/broadcom/bcm2711-rpi-4-b.dtb /bcm2711-rpi-4-b.dtb
COPY --from=build /built-files.txt /built-files.txt
