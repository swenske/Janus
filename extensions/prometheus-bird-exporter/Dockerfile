# syntax=docker/dockerfile:1.27@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
#
# Optional extension: bird_exporter (BIRD's protocols as Prometheus
# metrics), built from source with this tree's Go like node_exporter
# (versions.mk). go mod download fetches the module and its dependencies
# through proxy.golang.org and checks them against Go's checksum
# database; the module's zip is also checked against the sha256 pinned in
# versions.mk. Static (CGO_ENABLED=0), not stripped: govulncheck reads
# what a binary reaches from its symbols. The export stage is the
# extension's file tree; hack/extpack adds its manifest and packs it.

ARG BIRD_EXPORTER_VERSION
ARG BIRD_EXPORTER_SHA256
ARG ARCH

FROM golang:1.27.3-trixie@sha256:604106102d33b1084e37ca08adf98ac0813624ae8595bd0d7f3965ecebded09c AS build
ARG BIRD_EXPORTER_VERSION
ARG BIRD_EXPORTER_SHA256
ARG ARCH
ENV GOTOOLCHAIN=local CGO_ENABLED=0
RUN test -n "${BIRD_EXPORTER_SHA256}" \
    && module=github.com/czerwonk/bird_exporter v="v${BIRD_EXPORTER_VERSION}" \
    && go mod download "$module@$v" \
    && echo "${BIRD_EXPORTER_SHA256}  $(go env GOMODCACHE)/cache/download/$module/@v/$v.zip" | sha256sum -c - \
    && cd "$(go env GOMODCACHE)/$module@$v" \
    && GOARCH="${ARCH}" go build -trimpath -o /out/usr/local/sbin/bird_exporter \
       -ldflags "-X main.version=${BIRD_EXPORTER_VERSION}" . \
    && install -D -m 0644 LICENSE /out/usr/share/licenses/prometheus-bird-exporter/LICENSE

FROM scratch AS export
COPY --from=build /out/ /
