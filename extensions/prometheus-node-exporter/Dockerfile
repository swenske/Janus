# syntax=docker/dockerfile:1
#
# Optional extension: Prometheus node_exporter, built from source with
# this tree's Go rather than taken from upstream's release binary, which
# carries whatever Go upstream built it with (versions.mk). go mod download
# fetches the module and its dependencies through proxy.golang.org and
# checks them against Go's checksum database; the module's zip is also
# checked against the sha256 pinned in versions.mk. Static, like
# upstream's (CGO_ENABLED=0), not stripped: govulncheck reads what a
# binary reaches from its symbols. The export stage is the extension's
# file tree; hack/extpack adds its manifest and packs it.

ARG NODE_EXPORTER_VERSION
ARG NODE_EXPORTER_SHA256
ARG ARCH

FROM golang:1.26.8-trixie@sha256:eae2aaa6add2936cbf350dd0d2628b363461542f0c4b3c0b558957e0f2997379 AS build
ARG NODE_EXPORTER_VERSION
ARG NODE_EXPORTER_SHA256
ARG ARCH
ENV GOTOOLCHAIN=local CGO_ENABLED=0
RUN test -n "${NODE_EXPORTER_SHA256}" \
    && module=github.com/prometheus/node_exporter v="v${NODE_EXPORTER_VERSION}" \
    && go mod download "$module@$v" \
    && echo "${NODE_EXPORTER_SHA256}  $(go env GOMODCACHE)/cache/download/$module/@v/$v.zip" | sha256sum -c - \
    && cd "$(go env GOMODCACHE)/$module@$v" \
    && GOARCH="${ARCH}" go build -trimpath -o /out/usr/local/sbin/node_exporter \
       -ldflags "-X github.com/prometheus/common/version.Version=${NODE_EXPORTER_VERSION} -X github.com/prometheus/common/version.Branch=$v -X github.com/prometheus/common/version.BuildUser=janus" . \
    && install -D -m 0644 LICENSE /out/usr/share/licenses/prometheus-node-exporter/LICENSE

FROM scratch AS export
COPY --from=build /out/ /
