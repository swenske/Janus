# syntax=docker/dockerfile:1.27@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
#
# Optional extension: keepalived - VRRP, a virtual IP shared by several
# nodes with failover. Built from keepalived.org's release tarball,
# checked against the sha256 pinned in versions.mk (keepalived signs
# neither its releases nor its tags: hack/upstream pinned it after
# comparing it with the git tag on GitHub - docs/upstreams.md), its
# autotools outputs regenerated (generated.txt), as a static musl binary:
# VRRP and its JSON status dump only - no LVS/IPVS, no iptables/nftables
# integration (janusd owns the ruleset), no SNMP/D-Bus/systemd, no VRRP
# authentication (dropped by VRRPv3), hence no OpenSSL.
#
# Targets: export (amd64, natively on Alpine) and export-arm64 (cross-
# compiled with pkgs/musl-toolchain, given as the "musltoolchain" build
# context).

ARG KEEPALIVED_VERSION

FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f AS fetch
ARG KEEPALIVED_VERSION
ARG KEEPALIVED_SHA256
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates autoconf automake pkgconf \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN curl -fsSLO "https://www.keepalived.org/software/keepalived-${KEEPALIVED_VERSION}.tar.gz" \
    && echo "${KEEPALIVED_SHA256}  keepalived-${KEEPALIVED_VERSION}.tar.gz" | sha256sum -c - \
    && tar xzf "keepalived-${KEEPALIVED_VERSION}.tar.gz" && mv "keepalived-${KEEPALIVED_VERSION}" keepalived
# What only the tarball has, not the git tag - autotools' outputs, the
# only such files hack/upstream let through (generated.txt) - is deleted
# and generated again here: what gets compiled is the tag's own source.
COPY generated.txt /src/generated.txt
RUN <<'SH'
set -eu
cd /src/keepalived
grep -v '^#' /src/generated.txt | grep . > /tmp/patterns
find . \( -type f -o -type l \) | sed 's|^\./||' | while read -r f; do
  while read -r p; do
    # shellcheck disable=SC2254 # $p is a pattern
    case "$f" in $p) rm -f "$f"; break ;; esac
  done < /tmp/patterns
done
autoreconf -fi
SH
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
# A static PIE's start-up (musl) applies only the relative relocations:
# one against a __thread variable would stay wrong (pkgs/haproxy/Dockerfile).
if readelf -rW /out/usr/local/sbin/keepalived | grep -Eq 'R_X86_64_TPOFF|R_AARCH64_TLS_TPREL'; then echo "keepalived has TLS relocations a static PIE doesn't apply" >&2; exit 1; fi
SCRIPT

FROM --platform=linux/amd64 alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS build
RUN apk add --no-cache build-base linux-headers
COPY --from=fetch /src /src
RUN STRIP=strip /src/build.sh

FROM --platform=linux/amd64 debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f AS build-arm64
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
