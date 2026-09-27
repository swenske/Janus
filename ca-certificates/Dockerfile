# syntax=docker/dockerfile:1
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
FROM debian:trixie-slim AS build
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

FROM scratch AS export
COPY --from=build /etc/ssl/certs/ca-certificates.crt /ca-certificates.crt
