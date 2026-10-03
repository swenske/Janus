# syntax=docker/dockerfile:1
#
# Optional extension: keepalived - VRRP, a virtual IP shared by several
# nodes with failover. Built from keepalived.org's release tarball,
# checked against the sha256 pinned in versions.mk (keepalived doesn't
# sign its releases: the pin matches both Debian's orig tarball and
# Alpine's checksum), as a static musl binary: VRRP and its JSON status
# dump only - no LVS/IPVS, no iptables/nftables integration (janusd owns
# the ruleset), no SNMP/D-Bus/systemd, no VRRP authentication (dropped by
# VRRPv3), hence no OpenSSL.
#
# Targets: export (amd64, natively on Alpine) and export-arm64 (cross-
# compiled with pkgs/musl-toolchain, given as the "musltoolchain" build
# context).

ARG KEEPALIVED_VERSION

FROM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a AS fetch
ARG KEEPALIVED_VERSION
ARG KEEPALIVED_SHA256
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN curl -fsSLO "https://www.keepalived.org/software/keepalived-${KEEPALIVED_VERSION}.tar.gz" \
    && echo "${KEEPALIVED_SHA256}  keepalived-${KEEPALIVED_VERSION}.tar.gz" | sha256sum -c - \
    && tar xzf "keepalived-${KEEPALIVED_VERSION}.tar.gz" && mv "keepalived-${KEEPALIVED_VERSION}" keepalived
COPY --chmod=0755 <<'SCRIPT' /src/build.sh
#!/bin/sh
set -eu
host=${HOST:+--host=$HOST}
cd /src/keepalived
./configure $host --prefix=/usr/local --sysconfdir=/etc \
  --disable-lvs --disable-iptables --disable-nftables --disable-systemd \
  --disable-track-process --disable-vrrp-auth --disable-dbus --enable-json \
  CPPFLAGS="${KHEADERS:+-isystem $KHEADERS}" LDFLAGS=-static
make -j"$(nproc)"
$STRIP bin/keepalived
install -D -m 0755 bin/keepalived /out/usr/local/sbin/keepalived
install -D -m 0644 COPYING /out/usr/share/licenses/keepalived/COPYING
if readelf -l /out/usr/local/sbin/keepalived | grep -q INTERP; then echo "keepalived isn't static" >&2; exit 1; fi
SCRIPT

FROM --platform=linux/amd64 alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS build
RUN apk add --no-cache build-base linux-headers
COPY --from=fetch /src /src
RUN STRIP=strip /src/build.sh

FROM --platform=linux/amd64 debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a AS build-arm64
# The cross-toolchain's kernel headers predate what keepalived includes
# (linux/close_range.h, 5.9): Debian's arm64 kernel headers instead - the
# kernel's own uapi directories only, nothing of glibc's.
RUN apt-get update && apt-get install -y --no-install-recommends make pkgconf file binutils linux-libc-dev-arm64-cross \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /opt/kheaders && cp -r /usr/aarch64-linux-gnu/include/linux /usr/aarch64-linux-gnu/include/asm \
        /usr/aarch64-linux-gnu/include/asm-generic /opt/kheaders/
COPY --from=musltoolchain / /opt/musl-cross
ENV PATH=/opt/musl-cross/bin:$PATH
COPY --from=fetch /src /src
RUN HOST=aarch64-linux-musl STRIP=aarch64-linux-musl-strip KHEADERS=/opt/kheaders /src/build.sh \
    && file /out/usr/local/sbin/keepalived | grep -q "ARM aarch64"

FROM scratch AS export
COPY --from=build /out/ /

FROM scratch AS export-arm64
COPY --from=build-arm64 /out/ /
