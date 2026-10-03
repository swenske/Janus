# syntax=docker/dockerfile:1
#
# Fetches and verifies the pinned kernel source (see ../versions.mk),
# provides a "config" stage used interactively by `make kernel-menuconfig`
# (see ../Makefile) to run `make menuconfig`, a "build" stage that compiles
# bzImage from configs/janus_defconfig, and an "export" stage
# (`FROM scratch`) that holds just the resulting bzImage so
# `docker build --target export --output type=local,dest=<dir>` can pull it
# out without a container filesystem attached.
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
FROM debian:trixie-slim AS fetch
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
ARG DEFCONFIG=janus_defconfig
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

FROM scratch AS export
ARG KERNEL_VERSION
COPY --from=build /src/linux-${KERNEL_VERSION}/arch/x86/boot/bzImage /bzImage

FROM scratch AS export-arm64
ARG KERNEL_VERSION
COPY --from=build /src/linux-${KERNEL_VERSION}/arch/arm64/boot/Image /Image
COPY --from=build /src/linux-${KERNEL_VERSION}/arch/arm64/boot/dts/broadcom/bcm2711-rpi-4-b.dtb /bcm2711-rpi-4-b.dtb
