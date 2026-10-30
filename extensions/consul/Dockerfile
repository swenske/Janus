# syntax=docker/dockerfile:1.27@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
#
# Optional extension: the Consul agent. HashiCorp's own static release
# binary, checked against the sha256 pinned in versions.mk (from the
# release's SHA256SUMS, whose GPG signature by HashiCorp's release key was
# checked when pinning), stripped of its debug information - about a
# quarter of its size, which counts in a 64 MiB boot slot.

ARG CONSUL_VERSION
ARG CONSUL_SHA256
ARG ARCH

FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad AS fetch
ARG CONSUL_VERSION
ARG CONSUL_SHA256
ARG ARCH
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates unzip binutils \
        binutils-aarch64-linux-gnu \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN curl -fsSLo consul.zip "https://releases.hashicorp.com/consul/${CONSUL_VERSION}/consul_${CONSUL_VERSION}_linux_${ARCH}.zip" \
    && echo "${CONSUL_SHA256}  consul.zip" | sha256sum -c - \
    && unzip -q consul.zip \
    && case "${ARCH}" in arm64) strip=aarch64-linux-gnu-strip ;; *) strip=strip ;; esac \
    && "$strip" consul \
    && install -D -m 0755 consul /out/usr/local/sbin/consul \
    && install -D -m 0644 LICENSE.txt /out/usr/share/licenses/consul/LICENSE.txt

FROM scratch AS export
COPY --from=fetch /out/ /
