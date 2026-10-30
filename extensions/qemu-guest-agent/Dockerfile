# syntax=docker/dockerfile:1.27@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
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

FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad AS fetch
ARG QEMU_VERSION
ARG QEMU_SHA256
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates xz-utils \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN curl -fsSLO "https://download.qemu.org/qemu-${QEMU_VERSION}.tar.xz" \
    && echo "${QEMU_SHA256}  qemu-${QEMU_VERSION}.tar.xz" | sha256sum -c - \
    && tar xJf "qemu-${QEMU_VERSION}.tar.xz"

FROM --platform=linux/amd64 alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a AS build
ARG QEMU_VERSION
RUN apk add --no-cache build-base linux-headers python3 meson ninja pkgconf bash perl \
    glib-dev glib-static pcre2-dev pcre2-static libffi-dev zlib-dev zlib-static gettext-dev gettext-static \
    util-linux-dev util-linux-static
COPY --from=fetch /src/qemu-${QEMU_VERSION} /src/qemu
WORKDIR /src/qemu
# A static PIE's start-up (musl) applies only the relative relocations:
# one against a __thread variable would stay wrong (pkgs/haproxy/Dockerfile).
RUN ./configure --static --without-default-features --disable-system --disable-user \
      --disable-tools --disable-docs --enable-guest-agent --disable-werror \
      --prefix=/usr/local \
    && ninja -C build qga/qemu-ga \
    && strip build/qga/qemu-ga \
    && install -D -m 0755 build/qga/qemu-ga /out/usr/local/sbin/qemu-ga \
    && install -D -m 0644 COPYING /out/usr/share/licenses/qemu-guest-agent/COPYING \
    && if readelf -l /out/usr/local/sbin/qemu-ga | grep -q INTERP; then echo "qemu-ga isn't static" >&2; exit 1; fi \
    && if readelf -rW /out/usr/local/sbin/qemu-ga | grep -Eq 'R_X86_64_TPOFF|R_AARCH64_TLS_TPREL'; then echo "qemu-ga has TLS relocations a static PIE doesn't apply" >&2; exit 1; fi

FROM scratch AS export
COPY --from=build /out/ /
