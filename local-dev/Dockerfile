# syntax=docker/dockerfile:1
#
# "Local Platform" tranche (docs/companion-site-builder-scope.md,
# user-requested 2026-09-29 after a real config rejection on a deployed
# alpha node - "'prometheus-exporter' unknown service name" - traced to
# pkgs/haproxy/Dockerfile missing USE_PROMEX=1, fixed there). Runs a
# real janusd + haproxy pair as an ordinary Docker container, for fast
# local iteration on HAProxy config and the janusd/gRPC API - QEMU boot
# + squashfs/dm-verity assembly (make qemu-network-test et al.) takes
# real minutes per change and leaves nothing running to poke at
# afterward; this takes seconds and stays up.
#
# This is deliberately NOT a target-OS image (see CLAUDE.md's own
# "build system vs. target OS" distinction) - it lives at the repo
# root next to dashboard/, not under image/, for the same reason
# dashboard/'s own Dockerfile does: it's a real, ordinary running
# service/tool, not something meant to boot as the immutable appliance
# itself. Unlike every image/*/Dockerfile in this project (which all
# minimize for a real deployment), this one deliberately keeps a real
# distro base (debian:trixie-slim, not FROM scratch) specifically so
# `docker exec -it ... bash` works for debugging - debuggability is the
# actual goal here, not minimalism.
#
# No dm-verity, no A/B slots, no UEFI/UKI, no SELinux - none of that
# machinery is what's being tested here. janusd itself needs zero
# container-specific configuration: every one of its own default flag
# values (-pki-dir, -haproxy-config, -haproxy-chroot-dir, ...) already
# matches the real target's own paths, and janusd already creates every
# directory it needs itself (internal/pki.LoadOrBootstrap's own
# os.MkdirAll for -pki-dir, cmd/janusd/main.go's own os.MkdirAll for
# -haproxy-chroot-dir/-haproxy-pid's parent) - the same "first boot"
# behavior a real node has, just on a normal writable container
# filesystem instead of a read-only squashfs+tmpfs-overlay one, so none
# of rootfs/init's own mountEphemeral/mountState dance is needed either.
#
# Usage: see local-dev/README.md for docker run flags (port mapping vs.
# --network host, and how to persist PKI/config across restarts).
FROM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a AS runtime

RUN mkdir -p /etc/haproxy
COPY build/janusd /usr/local/bin/janusd
COPY build/haproxy /usr/local/sbin/haproxy
COPY rootfs/base/etc/haproxy/haproxy.cfg /etc/haproxy/haproxy.cfg

# 9505: janusd's own gRPC (mTLS) listener. 8080: the bootstrap
# haproxy.cfg's own health-check frontend - whatever real listener
# port a later ApplyConfig call adds (e.g. testing the Prometheus
# exporter on :8404) isn't known ahead of time and needs its own -p at
# `docker run` time, or --network host to sidestep the question
# entirely - see local-dev/README.md.
EXPOSE 9505 8080

ENTRYPOINT ["/usr/local/bin/janusd"]
