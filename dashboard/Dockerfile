# syntax=docker/dockerfile:1
#
# Builds dashboardd (dashboard/backend) into a minimal runtime image -
# same multi-stage/FROM-scratch-export pattern as ../kernel/Dockerfile
# and ../selinux/Dockerfile, except this image actually runs (a service,
# not a build artifact to extract). The frontend SPA (dashboard/frontend)
# is *not* built here - its output is already committed to dashboard/
# backend/static (see that tree's own README for why, the same reasoning
# gen/janus/v1alpha1 is committed), so a plain `go build` here embeds
# exactly what a real deploy expects without needing a Node.js toolchain
# in this image's own build stage.
#
# Node gRPC calls never use the system trust store - each is verified
# against that node's own CA cert from the registry (see dashboard/
# backend/internal/nodeproxy's package doc comment). The CA bundle below
# is for the one public-internet call dashboardd makes: GitHub's release
# API (nodeproxy/release.go, "Latest release" detection), which failed
# with "x509: certificate signed by unknown authority" on the real
# deployed container while this image shipped no bundle at all.
# CGO_ENABLED=0 keeps the binary static so a bare FROM scratch final
# stage can actually run it.
#
# The same image runs janus-controller-updater (dashboard/updater), the
# optional companion that updates the Controller from its page: its own
# entrypoint in the compose file (dashboard/README.md, "Updating the
# Controller"), with Docker Compose's static binary next to it - pinned in
# versions.mk, checked against its sha256 here.
FROM golang:1.26.8-trixie@sha256:eae2aaa6add2936cbf350dd0d2628b363461542f0c4b3c0b558957e0f2997379 AS compose
ARG TARGETARCH
ARG DOCKER_COMPOSE_VERSION
ARG DOCKER_COMPOSE_SHA256_AMD64
ARG DOCKER_COMPOSE_SHA256_ARM64
RUN set -eu; \
    test -n "$DOCKER_COMPOSE_VERSION" || { echo "DOCKER_COMPOSE_VERSION is unset - build with make dashboard-image (versions.mk)" >&2; exit 1; }; \
    case "${TARGETARCH:-amd64}" in \
      amd64) arch=x86_64; sum="$DOCKER_COMPOSE_SHA256_AMD64" ;; \
      arm64) arch=aarch64; sum="$DOCKER_COMPOSE_SHA256_ARM64" ;; \
      *) echo "no Docker Compose build for $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /docker-compose "https://github.com/docker/compose/releases/download/${DOCKER_COMPOSE_VERSION}/docker-compose-linux-${arch}"; \
    echo "${sum}  /docker-compose" | sha256sum -c -; \
    chmod 0755 /docker-compose

FROM golang:1.26.8-trixie@sha256:eae2aaa6add2936cbf350dd0d2628b363461542f0c4b3c0b558957e0f2997379 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY gen/ gen/
COPY internal/ internal/
COPY dashboard/backend/ dashboard/backend/
COPY dashboard/updater/ dashboard/updater/
# The Controller's version (make dashboard-image passes git describe's).
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/dashboardd ./dashboard/backend && \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/janus-controller-updater ./dashboard/updater

FROM scratch AS export
COPY --from=build /out/dashboardd /dashboardd
COPY --from=build /out/janus-controller-updater /janus-controller-updater
COPY --from=compose /docker-compose /usr/local/bin/docker-compose
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
# /data is the persistent volume (node registry + this dashboard's own
# TLS identity - see dashboard/backend/main.go's own -data-dir default)
# - declared here so a bare `docker run` without an explicit -v still
# gets an anonymous volume rather than writing into the container's own
# throwaway layer.
VOLUME /data
# :8080 - the main port, HTTPS-only (node list/add/remove, behind admin
# auth - see internal/auth). :8443 - node self-registration (see
# internal/pending and register.go), a fixed port, not a pool, since
# it's one endpoint shared by every registering node, not per-node like
# what follows. 9500-9599 - the per-node mTLS-gated listener pool (see
# dashboard/backend/main.go's portRangeStart/End) - Docker's static -p
# mapping doesn't cover a dynamic pool well, so the container must run
# with --network host - see dashboard/README.md for the full run
# command (and why: it also gives dashboardd's own TLS identity a real,
# directly-reachable SAN with zero extra configuration, see
# -advertise-address's own doc comment).
EXPOSE 8080 8443 9500-9599
ENTRYPOINT ["/dashboardd"]
