# syntax=docker/dockerfile:1.27@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
#
# Fetches the pinned HAProxy, AWS-LC and zlib sources (see
# ../../versions.mk), each checked against its pinned sha256 before
# anything is extracted, and builds HAProxy as a fully static musl binary
# linked against AWS-LC and zlib.
#
# AWS-LC instead of OpenSSL: OpenSSL 3.x took locks and atomics into
# libcrypto's hot paths, and HAProxy's own measurements ("The state of
# SSL stacks", haproxy.com, 2025) put it far behind AWS-LC as soon as
# many threads do TLS - full handshakes ~1.5x, resumptions several times
# over on 64 cores. HAProxy's INSTALL lists AWS-LC (>= 1.22) as the
# performant, complete OpenSSL derivative. Both architectures build the
# same pinned AWS-LC from source (before, amd64 linked whatever OpenSSL
# Alpine shipped and arm64 a pinned one). Missing next to OpenSSL: the
# obsolete DHE and CCM ciphers and the ENGINE API (ssl-engine,
# ssl-mode-async).
#
# AWS-LC build options, both paths:
# - DISABLE_CPU_JITTER_ENTROPY: by default AWS-LC seeds its DRBG from a
#   CPU jitter entropy collector on top of getrandom(), and abort()s the
#   whole process when that collector fails its health tests three times
#   in a row. On an appliance that runs on whatever hypervisor/timer it is
#   given, an HAProxy that can die on its entropy source is the worse
#   trade: with it off, seeds come from getrandom() (the kernel's CSPRNG,
#   what OpenSSL used) mixed with RDRAND/RNDR when the CPU has one.
# - DISABLE_GO: Go only generates err_data.c, shipped pre-generated in the
#   release tarball; Perl still generates the assembly.
# - No tests, no bssl tool, static libraries only.
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
# ../../Makefile's rpi4-haproxy-build target). zlib has no prebuilt
# static aarch64-musl library available anywhere to lean on (unlike
# Alpine's own apk packages for the native amd64 path), so it is
# cross-compiled from source first, like AWS-LC. amd64 builds the same
# pinned zlib from source too (rather than Alpine's zlib-static), so both
# architectures ship the version versions.mk names.
#
# USE_PROMEX=1 (both paths): HAProxy's own Prometheus metrics exporter
# (`http-request use-service prometheus-exporter`) is a real, separate
# build-time addon (addons/promex/), not something any USE_OPENSSL/
# USE_ZLIB/USE_PTHREAD_PSHARED combination pulls in - found on a real
# deployed alpha node (10.1.0.123), not anticipated: `ApplyConfig`
# rejected a config trying to use it with "'prometheus-exporter' unknown
# service name", confirmed by grepping this exact HAProxy version's own
# Makefile (`USE_PROMEX : enable the Prometheus exporter`, `addons/
# promex/service-prometheus.o`) rather than assumed from HAProxy's own
# generic documentation. No new library/CFLAGS needed - the Makefile's
# own `PROMEX_CFLAGS` is set automatically once `USE_PROMEX` is truthy.

# One image, several HAProxy branches (variants.mk): the TLS and zlib
# libraries are fetched and built in stages of their own, without any
# HAPROXY_* argument, so BuildKit builds them once whatever the branch -
# only the HAProxy stages differ from one branch to another.

ARG HAPROXY_VERSION
ARG HAPROXY_SHA256
ARG AWSLC_VERSION
ARG AWSLC_SHA256
ARG ZLIB_VERSION
ARG ZLIB_SHA256

FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad AS fetch-base
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src

FROM fetch-base AS fetch-deps
ARG AWSLC_VERSION
ARG AWSLC_SHA256
ARG ZLIB_VERSION
ARG ZLIB_SHA256
RUN test -n "${AWSLC_SHA256}" \
    && curl -fsSLo aws-lc-${AWSLC_VERSION}.tar.gz \
       "https://github.com/aws/aws-lc/archive/refs/tags/v${AWSLC_VERSION}.tar.gz" \
    && echo "${AWSLC_SHA256}  aws-lc-${AWSLC_VERSION}.tar.gz" | sha256sum -c - \
    && tar xf aws-lc-${AWSLC_VERSION}.tar.gz
RUN test -n "${ZLIB_SHA256}" \
    && curl -fsSLO "https://zlib.net/fossils/zlib-${ZLIB_VERSION}.tar.gz" \
    && echo "${ZLIB_SHA256}  zlib-${ZLIB_VERSION}.tar.gz" | sha256sum -c - \
    && tar xf zlib-${ZLIB_VERSION}.tar.gz

FROM fetch-base AS fetch-haproxy
ARG HAPROXY_VERSION
ARG HAPROXY_SHA256
# haproxy.org first, then the same tarball from Gentoo's distfiles (a
# directory named by the first byte of the file name's BLAKE2b) and
# FreeBSD's: haproxy.org refused every connection, worldwide, the night
# of 2026-10-30, and the build waited on it. versions.mk's sha256
# decides, whoever served the file.
RUN test -n "${HAPROXY_SHA256}" \
    && branch="$(echo "${HAPROXY_VERSION}" | cut -d. -f1,2)" \
    && f="haproxy-${HAPROXY_VERSION}.tar.gz" \
    && gentoo="$(printf %s "$f" | b2sum | cut -c1-2)" \
    && for url in "https://www.haproxy.org/download/${branch}/src/$f" \
        "https://distfiles.gentoo.org/distfiles/${gentoo}/$f" \
        "http://distcache.freebsd.org/ports-distfiles/$f"; do \
        if curl -fsSL --connect-timeout 20 -o "$f" "$url" \
            && echo "${HAPROXY_SHA256}  $f" | sha256sum -c -; then \
            break; \
        fi; \
        echo "$url: unreachable, or not the pinned tarball" >&2; \
        rm -f "$f"; \
    done \
    && test -f "$f" \
    && tar xf "$f"

FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a AS deps
ARG AWSLC_VERSION
ARG ZLIB_VERSION
RUN apk add --no-cache build-base linux-headers cmake samurai perl
COPY --from=fetch-deps /src/zlib-${ZLIB_VERSION} /src/zlib-${ZLIB_VERSION}
WORKDIR /src/zlib-${ZLIB_VERSION}
RUN ./configure --prefix=/opt/zlib --static \
    && make -j"$(nproc)" && make install
COPY --from=fetch-deps /src/aws-lc-${AWSLC_VERSION} /src/aws-lc-${AWSLC_VERSION}
RUN cmake -S /src/aws-lc-${AWSLC_VERSION} -B /build/aws-lc -G Ninja \
    -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/opt/aws-lc \
    -DCMAKE_INSTALL_LIBDIR=lib -DBUILD_SHARED_LIBS=OFF \
    -DBUILD_TESTING=OFF -DBUILD_TOOL=OFF -DDISABLE_GO=ON \
    -DDISABLE_CPU_JITTER_ENTROPY=ON -DENABLE_SOURCE_MODIFICATION=OFF \
    && ninja -C /build/aws-lc install

# The -vv checks: the version built is the one pinned, linked against the
# pinned AWS-LC and zlib. HAProxy 3.0 words the TLS line "Built with
# OpenSSL version", 3.2 and newer "Built with SSL library version".
#
# A static PIE (what Alpine >= 3.23's gcc links for -static; Alpine
# builds every object -fPIE already): it relocates itself at start and
# the kernel loads it at a random address each time, where a static
# executable always sits at 0x400000 - HAProxy's and AWS-LC's code and
# data were at addresses anyone could read off the published build.
#
# EXPORT_SYMBOL=--no-export-dynamic: linux-musl's USE_BACKTRACE links
# with -Wl,--export-dynamic, and a static PIE that exports its symbols
# keeps R_X86_64_TPOFF64 relocations against HAProxy's __thread
# variables, which musl's static-PIE start-up doesn't apply (it only does
# the relative ones) - `haproxy -vv` segfaulted reading epoll_fd[tid]
# (#141). The export buys a static build nothing: dladdr() resolves no
# symbol there (check_if_static_build, src/tools.c), and backtraces name
# addresses from main and a few known functions, PIE or not. Checked: a
# static PIE, with relative relocations only and none in its code.
FROM deps AS build
ARG HAPROXY_VERSION
ARG AWSLC_VERSION
ARG ZLIB_VERSION
COPY --from=fetch-haproxy /src/haproxy-${HAPROXY_VERSION} /src/haproxy-${HAPROXY_VERSION}
WORKDIR /src/haproxy-${HAPROXY_VERSION}
RUN make -j"$(nproc)" TARGET=linux-musl \
    USE_OPENSSL_AWSLC=1 SSL_INC=/opt/aws-lc/include SSL_LIB=/opt/aws-lc/lib \
    USE_ZLIB=1 ZLIB_INC=/opt/zlib/include ZLIB_LIB=/opt/zlib/lib \
    USE_PTHREAD_PSHARED=1 USE_PROMEX=1 \
    LDFLAGS="-static" EXPORT_SYMBOL=--no-export-dynamic \
    && strip haproxy \
    && readelf -h haproxy | grep -Eq '^ +Type: +DYN ' \
    && ! readelf -lW haproxy | grep -q INTERP \
    && ! readelf -dW haproxy | grep -q TEXTREL \
    && test "$(readelf -rW haproxy | grep -o 'R_X86_64_[A-Z0-9_]*' | sort -u)" = R_X86_64_RELATIVE \
    && ./haproxy -vv \
    && ./haproxy -vv | grep -q "^HAProxy version ${HAPROXY_VERSION}-" \
    && ./haproxy -vv | grep -Eq "^Built with (SSL library|OpenSSL) version : .*AWS-LC ${AWSLC_VERSION}" \
    && ./haproxy -vv | grep -q "^Built with zlib version : ${ZLIB_VERSION}$"

FROM scratch AS export
ARG HAPROXY_VERSION
COPY --from=build /src/haproxy-${HAPROXY_VERSION}/haproxy /haproxy

# --- arm64 cross-compile path -----------------------------------------

FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad AS deps-arm64
ARG AWSLC_VERSION
ARG ZLIB_VERSION
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential perl cmake ninja-build linux-libc-dev-arm64-cross \
    && rm -rf /var/lib/apt/lists/*
COPY --from=musltoolchain / /opt/musl-cross
# musl-cross-make installs Linux 4.19's UAPI headers, older than
# HAProxy's AWS-LC kTLS code (TLS_1_3_VERSION, <linux/tls.h>, Linux 5.1):
# Debian's arm64 ones (libc-independent, what Alpine's linux-headers is
# to the amd64 path) replace them for everything built below.
RUN cp -a /usr/aarch64-linux-gnu/include/. /opt/musl-cross/aarch64-linux-musl/include/
ENV PATH="/opt/musl-cross/bin:${PATH}"
ENV SYSROOT=/opt/sysroot

WORKDIR /src
COPY --from=fetch-deps /src/zlib-${ZLIB_VERSION} /src/zlib-${ZLIB_VERSION}
WORKDIR /src/zlib-${ZLIB_VERSION}
RUN CC=aarch64-linux-musl-gcc AR=aarch64-linux-musl-ar RANLIB=aarch64-linux-musl-ranlib \
    ./configure --prefix="${SYSROOT}" --static \
    && make -j"$(nproc)" && make install

COPY --from=fetch-deps /src/aws-lc-${AWSLC_VERSION} /src/aws-lc-${AWSLC_VERSION}
RUN cmake -S /src/aws-lc-${AWSLC_VERSION} -B /build/aws-lc -G Ninja \
    -DCMAKE_SYSTEM_NAME=Linux -DCMAKE_SYSTEM_PROCESSOR=aarch64 \
    -DCMAKE_C_COMPILER=aarch64-linux-musl-gcc \
    -DCMAKE_CXX_COMPILER=aarch64-linux-musl-g++ \
    -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX="${SYSROOT}" \
    -DCMAKE_INSTALL_LIBDIR=lib -DBUILD_SHARED_LIBS=OFF \
    -DBUILD_TESTING=OFF -DBUILD_TOOL=OFF -DDISABLE_GO=ON \
    -DDISABLE_CPU_JITTER_ENTROPY=ON -DENABLE_SOURCE_MODIFICATION=OFF \
    && ninja -C /build/aws-lc install

FROM deps-arm64 AS build-arm64
ARG HAPROXY_VERSION
COPY --from=fetch-haproxy /src/haproxy-${HAPROXY_VERSION} /src/haproxy-${HAPROXY_VERSION}
WORKDIR /src/haproxy-${HAPROXY_VERSION}
RUN make -j"$(nproc)" TARGET=linux-musl CC=aarch64-linux-musl-gcc \
    USE_OPENSSL_AWSLC=1 SSL_INC="${SYSROOT}/include" SSL_LIB="${SYSROOT}/lib" \
    USE_ZLIB=1 USE_PTHREAD_PSHARED=1 USE_PROMEX=1 \
    LDFLAGS="-static -L${SYSROOT}/lib" CFLAGS="-I${SYSROOT}/include" \
    && aarch64-linux-musl-strip haproxy

FROM scratch AS export-arm64
ARG HAPROXY_VERSION
COPY --from=build-arm64 /src/haproxy-${HAPROXY_VERSION}/haproxy /haproxy
