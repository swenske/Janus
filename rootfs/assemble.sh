#!/usr/bin/env bash
# Assembles the immutable rootfs: builds a squashfs image from rootfs/base
# + the given binaries/config, then computes its dm-verity hash tree.
# Requires mksquashfs (squashfs-tools) and veritysetup (cryptsetup-bin) on
# PATH - neither needs root privileges for this (only mounting/verifying
# a *live* dm-verity device does).
#
# Usage: rootfs/assemble.sh <out-dir> <init-bin> <janusd-bin> <haproxy-bin> <haproxy-cfg> <selinux-policy> <ca-bundle>
#
# Writes to <out-dir>:
#   rootfs.squashfs   - the read-only root filesystem image
#   rootfs.verity     - its dm-verity hash tree
#   rootfs.roothash   - the root hash (hex), the one thing that must be
#                        trusted out-of-band (Phase 3+: embedded in the
#                        Secure-Boot-signed kernel cmdline/UKI, not shipped
#                        as a separate file on a real node)
set -euo pipefail

# Debian installs veritysetup (cryptsetup-bin) to /usr/sbin, which isn't
# guaranteed to be on PATH for a non-interactive/non-root shell even once
# the package is installed - confirmed on janus-runner01, where this
# script's own `mksquashfs` call (installed to /usr/bin, always on PATH)
# succeeded but `veritysetup` failed with "command not found" despite
# `apt-get install cryptsetup-bin` having just run cleanly in the same job.
export PATH="$PATH:/usr/sbin:/sbin"

USAGE="usage: $0 <out-dir> <init-bin> <janusd-bin> <haproxy-bin> <haproxy-cfg> <selinux-policy> <ca-bundle>"
OUT_DIR="${1:?$USAGE}"
INIT_BIN="${2:?$USAGE}"
DAEMON_BIN="${3:?$USAGE}"
HAPROXY_BIN="${4:?$USAGE}"
HAPROXY_CFG="${5:?$USAGE}"
SELINUX_POLICY="${6:?$USAGE}"
CA_BUNDLE="${7:?$USAGE}"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

mkdir -p "$WORKDIR"/{proc,sys,dev,run,var,tmp,sbin,usr/local/sbin,etc/haproxy,etc/selinux,etc/ssl/certs}
install -m 0755 "$INIT_BIN" "$WORKDIR/sbin/init"
install -m 0755 "$DAEMON_BIN" "$WORKDIR/sbin/janusd"
install -m 0755 "$HAPROXY_BIN" "$WORKDIR/usr/local/sbin/haproxy"
install -m 0644 "$HAPROXY_CFG" "$WORKDIR/etc/haproxy/haproxy.cfg"
install -m 0644 "$SELINUX_POLICY" "$WORKDIR/etc/selinux/janus.policy"
# internal/nocloud's seedfrom "mode B" - a plain HTTPS client verifying
# against the system trust store - needs this to exist somewhere Go's
# own x509.SystemCertPool() looks by default; /etc/ssl/certs/
# ca-certificates.crt is Debian/Ubuntu's own convention and one of the
# fixed paths Go's stdlib checks.
install -m 0644 "$CA_BUNDLE" "$WORKDIR/etc/ssl/certs/ca-certificates.crt"

# Optional, by environment (the 7 positional arguments above are shared by
# many callers that don't need these):
#   JANUS_SHUTDOWN_BIN  /sbin/shutdown (rootfs/shutdown) - what the QEMU
#                       guest agent runs for a clean shutdown
#   JANUS_EXTENSIONS    space-separated extension tars (hack/extpack),
#                       layered onto the rootfs
#   JANUS_VERSION       for /usr/lib/os-release's VERSION_ID
#   JANUS_EXPORT_BASE   also write the base tree (before extensions) to
#                       this tar: a release publishes it, and an image
#                       for another schematic is built from it without
#                       rebuilding anything (rootfs/assemble-from-base.sh)

# The SELinux types of the base system's executables (see
# layer-and-squash.sh for why they're set this way): kept in the tree as
# .janus-labels, so an exported base carries them too.
{
  echo "sbin/init init_exec_t"
  echo "sbin/janusd janusd_exec_t"
  echo "usr/local/sbin/haproxy haproxy_exec_t"
} > "$WORKDIR/.janus-labels"
if [ -n "${JANUS_SHUTDOWN_BIN:-}" ]; then
  install -m 0755 "$JANUS_SHUTDOWN_BIN" "$WORKDIR/sbin/shutdown"
  echo "sbin/shutdown shutdown_exec_t" >> "$WORKDIR/.janus-labels"
fi

# /usr/lib/os-release, not /etc/os-release: /etc is a tmpfs on a running
# node, and readers (qemu-ga, node_exporter) fall back to /usr/lib.
mkdir -p "$WORKDIR/usr/lib"
{
  echo 'NAME="Janus"'
  echo 'ID=janus'
  echo "PRETTY_NAME=\"Janus${JANUS_VERSION:+ $JANUS_VERSION}\""
  [ -n "${JANUS_VERSION:-}" ] && echo "VERSION_ID=${JANUS_VERSION#v}"
  echo 'HOME_URL="https://janus.sw-servers.net"'
} > "$WORKDIR/usr/lib/os-release"

if [ -n "${JANUS_EXPORT_BASE:-}" ]; then
  tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@0 -cf "$JANUS_EXPORT_BASE" -C "$WORKDIR" .
  echo "Exported the base tree to $JANUS_EXPORT_BASE"
fi

"$(dirname "$0")/layer-and-squash.sh" "$WORKDIR" "$OUT_DIR"
