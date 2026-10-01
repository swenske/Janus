# syntax=docker/dockerfile:1
#
# Optional extension: BIRD - BGP (and OSPF, BFD, static routes...) to
# announce the node's addresses to the network, e.g. an anycast address
# withdrawn when HAProxy stops answering. BIRD 2 (see versions.mk for why
# not 3), built from bird.nic.cz's release tarball, checked against the
# sha256 pinned in versions.mk (BIRD doesn't sign its releases: the pin
# matches FreeBSD ports' checksum), as a static musl binary: no birdc
# (janusd speaks BIRD's control socket itself), no libssh (RPKI over SSH),
# no MPLS in the kernel protocol.
#
# Targets: export (amd64, natively on Alpine) and export-arm64 (cross-
# compiled with pkgs/musl-toolchain, given as the "musltoolchain" build
# context).

ARG BIRD_VERSION

FROM debian:trixie-slim AS fetch
ARG BIRD_VERSION
ARG BIRD_SHA256
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN curl -fsSLO "https://bird.nic.cz/download/bird-${BIRD_VERSION}.tar.gz" \
    && echo "${BIRD_SHA256}  bird-${BIRD_VERSION}.tar.gz" | sha256sum -c - \
    && tar xzf "bird-${BIRD_VERSION}.tar.gz" && mv "bird-${BIRD_VERSION}" bird
COPY --chmod=0755 <<'SCRIPT' /src/build.sh
#!/bin/sh
set -eu
host=${HOST:+--host=$HOST}
cd /src/bird
./configure $host --prefix=/usr/local --sysconfdir=/etc --runstatedir=/run \
  --disable-client --disable-libssh --disable-mpls-kernel \
  CPPFLAGS="${KHEADERS:+-isystem $KHEADERS}" LDFLAGS=-static
make -j"$(nproc)"
$STRIP bird
install -D -m 0755 bird /out/usr/local/sbin/bird
install -D -m 0644 README /out/usr/share/licenses/bird/README
if readelf -l /out/usr/local/sbin/bird | grep -q INTERP; then echo "bird isn't static" >&2; exit 1; fi
SCRIPT

FROM --platform=linux/amd64 alpine:3.22 AS build
RUN apk add --no-cache build-base linux-headers flex bison m4
COPY --from=fetch /src /src
RUN STRIP=strip /src/build.sh

FROM --platform=linux/amd64 debian:trixie-slim AS build-arm64
# Debian's arm64 kernel headers rather than the cross-toolchain's older
# ones - the kernel's own uapi directories only, nothing of glibc's.
RUN apt-get update && apt-get install -y --no-install-recommends make flex bison m4 file binutils linux-libc-dev-arm64-cross \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /opt/kheaders && cp -r /usr/aarch64-linux-gnu/include/linux /usr/aarch64-linux-gnu/include/asm \
        /usr/aarch64-linux-gnu/include/asm-generic /opt/kheaders/
COPY --from=musltoolchain / /opt/musl-cross
ENV PATH=/opt/musl-cross/bin:$PATH
COPY --from=fetch /src /src
RUN HOST=aarch64-linux-musl STRIP=aarch64-linux-musl-strip KHEADERS=/opt/kheaders /src/build.sh \
    && file /out/usr/local/sbin/bird | grep -q "ARM aarch64"

FROM scratch AS export
COPY --from=build /out/ /

FROM scratch AS export-arm64
COPY --from=build-arm64 /out/ /
