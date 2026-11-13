# syntax=docker/dockerfile:1.27@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
#
# Optional extension: Prometheus node_exporter, built from source with
# this tree's Go rather than taken from upstream's release binary, which
# carries whatever Go upstream built it with (versions.mk). go mod download
# fetches the module and its dependencies through proxy.golang.org and
# checks them against Go's checksum database; the module's zip is also
# checked against the sha256 pinned in versions.mk. Static, like
# upstream's (CGO_ENABLED=0). Its symbol table stays - govulncheck reads
# what a binary reaches from it, and a stripped binary would have every
# vulnerability of every module reported - but its DWARF debug
# information goes (-w): half the binary's weight in the image's 64 MiB
# slot (11.50 -> 5.65 MiB of squashfs), and govulncheck's verbose report
# is the same byte for byte without it. The export stage is the
# extension's file tree; hack/extpack adds its manifest and packs it.

ARG NODE_EXPORTER_VERSION
ARG NODE_EXPORTER_SHA256
ARG ARCH

FROM golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9 AS build
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
       -ldflags "-w -X github.com/prometheus/common/version.Version=${NODE_EXPORTER_VERSION} -X github.com/prometheus/common/version.Branch=$v -X github.com/prometheus/common/version.BuildUser=janus" . \
    && install -D -m 0644 LICENSE /out/usr/share/licenses/prometheus-node-exporter/LICENSE

FROM scratch AS export
COPY --from=build /out/ /
