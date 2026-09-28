# syntax=docker/dockerfile:1
#
# Fetches the pinned HAProxy source (see ../../versions.mk) and builds
# it as a fully static musl binary.
#
# amd64 (the "export" target): Alpine's own toolchain (gcc/musl-dev via
# build-base - a real, prebuilt native toolchain, no cross-compilation
# needed at all since the build host is already amd64).
#
# arm64 (the "export-arm64" target, Single Board Computer tranche
# follow-up): a real cross-toolchain instead - see
# ../musl-toolchain/Dockerfile for the full story of why (a first
# attempt using QEMU-user-mode emulation, `docker build --platform=
# linux/arm64` against this same amd64 recipe, failed outright on the
# self-hosted runner specifically - it's an LXC container whose
# confinement blocks nested Docker's own binfmt_misc registration from
# actually working). Needs ../musl-toolchain's own export supplied as
# an external build context named "musltoolchain" (`docker build
# --build-context musltoolchain=<musl-toolchain export dir> ...` - see
# ../../Makefile's rpi4-haproxy-build target). zlib/OpenSSL have no
# prebuilt static aarch64-musl libraries available anywhere to lean on
# (unlike Alpine's own apk packages for the native amd64 path), so both
# are cross-compiled from source against the same toolchain first.
#
# TODO: signature verification for the downloaded tarballs - not wired
# up yet. See ../../docs/architecture.md.

ARG HAPROXY_VERSION
ARG ZLIB_VERSION
ARG OPENSSL_VERSION

FROM debian:trixie-slim AS fetch
ARG HAPROXY_VERSION
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN branch="$(echo "${HAPROXY_VERSION}" | cut -d. -f1,2)" \
    && curl -fsSLO "https://www.haproxy.org/download/${branch}/src/haproxy-${HAPROXY_VERSION}.tar.gz"
RUN tar xf haproxy-${HAPROXY_VERSION}.tar.gz

FROM alpine:3.22 AS build
ARG HAPROXY_VERSION
RUN apk add --no-cache build-base linux-headers zlib-dev zlib-static \
    openssl-dev openssl-libs-static
COPY --from=fetch /src/haproxy-${HAPROXY_VERSION} /src/haproxy-${HAPROXY_VERSION}
WORKDIR /src/haproxy-${HAPROXY_VERSION}
RUN make -j"$(nproc)" TARGET=linux-musl \
    USE_OPENSSL=1 USE_ZLIB=1 USE_PTHREAD_PSHARED=1 \
    LDFLAGS=-static \
    && strip haproxy \
    && ./haproxy -vv

FROM scratch AS export
ARG HAPROXY_VERSION
COPY --from=build /src/haproxy-${HAPROXY_VERSION}/haproxy /haproxy

# --- arm64 cross-compile path -----------------------------------------

FROM debian:trixie-slim AS fetch-arm64-deps
ARG ZLIB_VERSION
ARG OPENSSL_VERSION
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN curl -fsSLO "https://zlib.net/fossils/zlib-${ZLIB_VERSION}.tar.gz" \
    && tar xf zlib-${ZLIB_VERSION}.tar.gz
RUN curl -fsSLO "https://www.openssl.org/source/openssl-${OPENSSL_VERSION}.tar.gz" \
    && tar xf openssl-${OPENSSL_VERSION}.tar.gz

FROM debian:trixie-slim AS build-arm64
ARG HAPROXY_VERSION
ARG ZLIB_VERSION
ARG OPENSSL_VERSION
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential perl \
    && rm -rf /var/lib/apt/lists/*
COPY --from=musltoolchain / /opt/musl-cross
ENV PATH="/opt/musl-cross/bin:${PATH}"
ENV SYSROOT=/opt/sysroot

WORKDIR /src
COPY --from=fetch-arm64-deps /src/zlib-${ZLIB_VERSION} /src/zlib-${ZLIB_VERSION}
WORKDIR /src/zlib-${ZLIB_VERSION}
RUN CC=aarch64-linux-musl-gcc AR=aarch64-linux-musl-ar RANLIB=aarch64-linux-musl-ranlib \
    ./configure --prefix="${SYSROOT}" --static \
    && make -j"$(nproc)" && make install

WORKDIR /src
COPY --from=fetch-arm64-deps /src/openssl-${OPENSSL_VERSION} /src/openssl-${OPENSSL_VERSION}
WORKDIR /src/openssl-${OPENSSL_VERSION}
# --cross-compile-prefix alone derives CC as
# aarch64-linux-musl-gcc; also passing CC= would double the prefix
# ("aarch64-linux-musl-aarch64-linux-musl-gcc: not found") - found by a
# real build failing with exactly that, not assumed.
RUN ./Configure linux-aarch64 no-shared no-dso --prefix="${SYSROOT}" \
    --cross-compile-prefix=aarch64-linux-musl- \
    && make -j"$(nproc)" \
    && make install_sw install_ssldirs

WORKDIR /src
COPY --from=fetch /src/haproxy-${HAPROXY_VERSION} /src/haproxy-${HAPROXY_VERSION}
WORKDIR /src/haproxy-${HAPROXY_VERSION}
RUN make -j"$(nproc)" TARGET=linux-musl CC=aarch64-linux-musl-gcc \
    USE_OPENSSL=1 USE_ZLIB=1 USE_PTHREAD_PSHARED=1 \
    LDFLAGS="-static -L${SYSROOT}/lib" CFLAGS="-I${SYSROOT}/include" \
    && aarch64-linux-musl-strip haproxy

FROM scratch AS export-arm64
ARG HAPROXY_VERSION
COPY --from=build-arm64 /src/haproxy-${HAPROXY_VERSION}/haproxy /haproxy
