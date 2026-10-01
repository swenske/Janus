# syntax=docker/dockerfile:1
#
# Optional extension: nftables - the nft command janusd runs to apply the
# node's firewall ruleset and edit its sets (NetworkService Firewall*).
# Built from netfilter.org's release tarballs and jansson's, checked
# against the sha256 pinned in versions.mk (taken after verifying their
# GPG signatures), as a static musl binary: libmnl, libnftnl and jansson
# (JSON output, which janusd parses) linked in, mini-gmp, no interactive
# CLI, no xtables. Also /etc/services from Debian's netbase, for service
# names in rulesets (musl reads it; init copies it into the tmpfs /etc).
#
# Targets: export (amd64, natively on Alpine) and export-arm64 (cross-
# compiled with pkgs/musl-toolchain, given as the "musltoolchain" build
# context - see pkgs/haproxy for why not emulation).

ARG LIBMNL_VERSION
ARG LIBNFTNL_VERSION
ARG NFTABLES_VERSION

FROM debian:trixie-slim AS fetch
ARG LIBMNL_VERSION
ARG LIBMNL_SHA256
ARG LIBNFTNL_VERSION
ARG LIBNFTNL_SHA256
ARG NFTABLES_VERSION
ARG NFTABLES_SHA256
ARG JANSSON_VERSION
ARG JANSSON_SHA256
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates xz-utils bzip2 netbase \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN curl -fsSLO "https://www.netfilter.org/pub/libmnl/libmnl-${LIBMNL_VERSION}.tar.bz2" \
    && curl -fsSLO "https://www.netfilter.org/pub/libnftnl/libnftnl-${LIBNFTNL_VERSION}.tar.xz" \
    && curl -fsSLO "https://www.netfilter.org/pub/nftables/nftables-${NFTABLES_VERSION}.tar.xz" \
    && curl -fsSLO "https://github.com/akheron/jansson/releases/download/v${JANSSON_VERSION}/jansson-${JANSSON_VERSION}.tar.gz" \
    && printf '%s  %s\n' \
        "${JANSSON_SHA256}" "jansson-${JANSSON_VERSION}.tar.gz" \
        "${LIBMNL_SHA256}" "libmnl-${LIBMNL_VERSION}.tar.bz2" \
        "${LIBNFTNL_SHA256}" "libnftnl-${LIBNFTNL_VERSION}.tar.xz" \
        "${NFTABLES_SHA256}" "nftables-${NFTABLES_VERSION}.tar.xz" | sha256sum -c - \
    && tar xjf "libmnl-${LIBMNL_VERSION}.tar.bz2" && mv "libmnl-${LIBMNL_VERSION}" libmnl \
    && tar xJf "libnftnl-${LIBNFTNL_VERSION}.tar.xz" && mv "libnftnl-${LIBNFTNL_VERSION}" libnftnl \
    && tar xJf "nftables-${NFTABLES_VERSION}.tar.xz" && mv "nftables-${NFTABLES_VERSION}" nftables \
    && tar xzf "jansson-${JANSSON_VERSION}.tar.gz" && mv "jansson-${JANSSON_VERSION}" jansson \
    && cp /etc/services /src/services && cp /usr/share/doc/netbase/copyright /src/netbase.copyright
# The build itself, for either target: HOST is the configure --host
# triplet (empty natively), STRIP the matching strip.
COPY --chmod=0755 <<'SCRIPT' /src/build.sh
#!/bin/sh
set -eu
host=${HOST:+--host=$HOST}
deps=/opt/nftdeps
export PKG_CONFIG_PATH=$deps/lib/pkgconfig PKG_CONFIG_LIBDIR=$deps/lib/pkgconfig
cd /src/libmnl
./configure $host --prefix=$deps --enable-static --disable-shared
make -j"$(nproc)" && make install
cd /src/libnftnl
./configure $host --prefix=$deps --enable-static --disable-shared
make -j"$(nproc)" && make install
cd /src/jansson
./configure $host --prefix=$deps --enable-static --disable-shared
make -j"$(nproc)" && make install
cd /src/nftables
# jansson is found with -ljansson, not pkg-config.
./configure $host --prefix=/usr/local --sysconfdir=/etc --enable-static --disable-shared \
  --with-mini-gmp --without-cli --with-json --without-xtables --disable-man-doc \
  CPPFLAGS="-I$deps/include" LDFLAGS="-L$deps/lib"
make -j"$(nproc)" LDFLAGS="-L$deps/lib -all-static"
$STRIP src/nft
install -D -m 0755 src/nft /out/usr/local/sbin/nft
for p in libmnl libnftnl nftables; do install -D -m 0644 /src/$p/COPYING /out/usr/share/licenses/nftables/$p.COPYING; done
install -D -m 0644 /src/jansson/LICENSE /out/usr/share/licenses/nftables/jansson.LICENSE
install -D -m 0644 /src/services /out/etc/services
install -D -m 0644 /src/netbase.copyright /out/usr/share/licenses/nftables/netbase.copyright
if readelf -l /out/usr/local/sbin/nft | grep -q INTERP; then echo "nft isn't static" >&2; exit 1; fi
SCRIPT

FROM --platform=linux/amd64 alpine:3.22 AS build
RUN apk add --no-cache build-base linux-headers pkgconf bison flex
COPY --from=fetch /src /src
RUN STRIP=strip /src/build.sh

FROM --platform=linux/amd64 debian:trixie-slim AS build-arm64
RUN apt-get update && apt-get install -y --no-install-recommends make pkgconf bison flex file binutils \
    && rm -rf /var/lib/apt/lists/*
COPY --from=musltoolchain / /opt/musl-cross
ENV PATH=/opt/musl-cross/bin:$PATH
COPY --from=fetch /src /src
RUN HOST=aarch64-linux-musl STRIP=aarch64-linux-musl-strip /src/build.sh \
    && file /out/usr/local/sbin/nft | grep -q "ARM aarch64"

FROM scratch AS export
COPY --from=build /out/ /

FROM scratch AS export-arm64
COPY --from=build-arm64 /out/ /
