# syntax=docker/dockerfile:1
#
# Optional extension: Prometheus node_exporter. Upstream's own static
# release binary, checked against the sha256 pinned in versions.mk (taken
# from upstream's sha256sums.txt). The export stage is the extension's
# file tree; hack/extpack adds its manifest and packs it.

ARG NODE_EXPORTER_VERSION
ARG NODE_EXPORTER_SHA256
ARG ARCH

FROM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a AS fetch
ARG NODE_EXPORTER_VERSION
ARG NODE_EXPORTER_SHA256
ARG ARCH
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN name="node_exporter-${NODE_EXPORTER_VERSION}.linux-${ARCH}" \
    && curl -fsSLo ne.tar.gz "https://github.com/prometheus/node_exporter/releases/download/v${NODE_EXPORTER_VERSION}/${name}.tar.gz" \
    && echo "${NODE_EXPORTER_SHA256}  ne.tar.gz" | sha256sum -c - \
    && tar xzf ne.tar.gz \
    && install -D -m 0755 "${name}/node_exporter" /out/usr/local/sbin/node_exporter \
    && install -D -m 0644 "${name}/LICENSE" /out/usr/share/licenses/prometheus-node-exporter/LICENSE

FROM scratch AS export
COPY --from=fetch /out/ /
