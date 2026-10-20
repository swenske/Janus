# syntax=docker/dockerfile:1.27@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
#
# Exports systemd's UKI stubs - linuxx64.efi.stub (amd64) and
# linuxaa64.efi.stub (arm64) - that `ukify` embeds in every Janus UKI
# (image/uki/assemble.sh): the first code the firmware runs, inside the
# Secure Boot-signed image. Taken from Debian's systemd-boot-efi in this
# pinned base image rather than from whatever the build host has
# installed, so every image embeds a known stub; versions.txt records
# which (dpkg's version of each package).
#
# Debian ships one stub per *installed* architecture (`systemd-boot-efi`
# is `Multi-Arch: same`): the arm64 one needs arm64 added to dpkg here,
# in an isolated container, never on the build host itself.
FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f AS build
RUN dpkg --add-architecture arm64 \
    && apt-get update \
    && apt-get install -y --no-install-recommends systemd-boot-efi:amd64 systemd-boot-efi:arm64 \
    && rm -rf /var/lib/apt/lists/* \
    && dpkg-query -W -f '${Package}:${Architecture} ${Version}\n' systemd-boot-efi:amd64 systemd-boot-efi:arm64 > /versions.txt

FROM scratch AS export
COPY --from=build /usr/lib/systemd/boot/efi/linuxx64.efi.stub /linuxx64.efi.stub
COPY --from=build /usr/lib/systemd/boot/efi/linuxaa64.efi.stub /linuxaa64.efi.stub
COPY --from=build /versions.txt /versions.txt
