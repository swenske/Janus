# syntax=docker/dockerfile:1
#
# Optional extension: the QEMU guest agent (qemu-ga), built from QEMU's
# release tarball - checked against the sha256 pinned in versions.mk,
# itself taken after verifying the tarball's GPG signature from QEMU's
# release manager - as a static musl binary on Alpine, the same way
# pkgs/haproxy builds HAProxy. Only qemu-ga is built: no emulator, no
# tools. amd64 only (a guest agent is for VMs, and Janus has no arm64 VM
# image).

ARG QEMU_VERSION
ARG QEMU_SHA256

FROM debian:trixie-slim AS fetch
ARG QEMU_VERSION
ARG QEMU_SHA256
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates xz-utils \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN curl -fsSLO "https://download.qemu.org/qemu-${QEMU_VERSION}.tar.xz" \
    && echo "${QEMU_SHA256}  qemu-${QEMU_VERSION}.tar.xz" | sha256sum -c - \
    && tar xJf "qemu-${QEMU_VERSION}.tar.xz"

FROM --platform=linux/amd64 alpine:3.22 AS build
ARG QEMU_VERSION
RUN apk add --no-cache build-base linux-headers python3 meson ninja pkgconf bash perl \
    glib-dev glib-static pcre2-dev pcre2-static libffi-dev zlib-dev zlib-static gettext-dev gettext-static \
    util-linux-dev util-linux-static
COPY --from=fetch /src/qemu-${QEMU_VERSION} /src/qemu
WORKDIR /src/qemu
RUN ./configure --static --without-default-features --disable-system --disable-user \
      --disable-tools --disable-docs --enable-guest-agent --disable-werror \
      --prefix=/usr/local \
    && ninja -C build qga/qemu-ga \
    && strip build/qga/qemu-ga \
    && install -D -m 0755 build/qga/qemu-ga /out/usr/local/sbin/qemu-ga \
    && install -D -m 0644 COPYING /out/usr/share/licenses/qemu-guest-agent/COPYING \
    && if readelf -l /out/usr/local/sbin/qemu-ga | grep -q INTERP; then echo "qemu-ga isn't static" >&2; exit 1; fi

FROM scratch AS export
COPY --from=build /out/ /
