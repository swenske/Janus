# syntax=docker/dockerfile:1
#
# Optional extension: the Consul agent. HashiCorp's own static release
# binary, checked against the sha256 pinned in versions.mk (from the
# release's SHA256SUMS, whose GPG signature by HashiCorp's release key was
# checked when pinning), stripped of its debug information - about a
# quarter of its size, which counts in a 64 MiB boot slot.

ARG CONSUL_VERSION
ARG CONSUL_SHA256
ARG ARCH

FROM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a AS fetch
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
