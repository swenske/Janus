# syntax=docker/dockerfile:1.27@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
#
# Exports a plain, standard CA trust bundle (Debian's own
# ca-certificates package output) - the one thing needed for
# internal/nocloud's "mode B" seedfrom trust (verify against the system
# trust store, same as any ordinary HTTPS client) to actually mean
# something on this rootfs: nothing else in this project bundles a CA
# store at all (see dashboard/backend's own Dockerfile - FROM scratch,
# deliberately no trust store, since every gRPC call it makes is
# verified against a node's own stored CA cert, never the public trust
# store) - a Janus node's own rootfs never needed one either, until a
# seedfrom URL with no pinned CA of its own needed something to verify
# against.
FROM debian:trixie-slim@sha256:f6b08b09a05725f9431502d3b5714f5c4576e82c1a84fa1debe0c24a7e0c3ec4 AS build
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

FROM scratch AS export
COPY --from=build /etc/ssl/certs/ca-certificates.crt /ca-certificates.crt
