# syntax=docker/dockerfile:1
#
# Exports systemd's aarch64 UKI stub (linuxaa64.efi.stub) - the one
# piece `ukify` (systemd-ukify) needs to assemble an arm64 Unified
# Kernel Image that this project's own host `ukify` install can't
# provide on its own: Debian only ships one stub per *installed*
# architecture (`systemd-boot-efi` is `Multi-Arch: same`, so an arm64
# build exists, but a build host only has its own native amd64 stub
# installed by default - `ukify --stub` takes an explicit path, so
# there's no need to touch the host's own architecture/package state at
# all, matching the same reasoning `ca-certificates/Dockerfile` and
# `pkgs/musl-toolchain/Dockerfile` already established for other
# arm64-only build inputs: fetch it in an isolated container, export
# just the file, never install a foreign architecture on the build host
# itself.
FROM debian:trixie-slim AS build
RUN dpkg --add-architecture arm64 \
    && apt-get update \
    && apt-get install -y --no-install-recommends systemd-boot-efi:arm64 \
    && rm -rf /var/lib/apt/lists/*

FROM scratch AS export
COPY --from=build /usr/lib/systemd/boot/efi/linuxaa64.efi.stub /linuxaa64.efi.stub
