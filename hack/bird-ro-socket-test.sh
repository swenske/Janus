#!/usr/bin/env bash
# Proves the restricted control socket janusd gives BIRD for the
# prometheus-bird-exporter extension (internal/bgp WithReadOnlySocket,
# docs/bgp.md "Metrics") with the bird extension's own static binary,
# run here on the host: show commands answer over it, every command that
# changes routing (disable, restart, reload, configure, down...) gets
# BIRD's "Access denied" - and the same command over BIRD's own socket
# does take effect (internal/bgp TestReadOnlySocketWithBIRD).
#
# Usage: hack/bird-ro-socket-test.sh <extension-bird-ARCH.tar>
set -euo pipefail

TAR="${1:?usage: $0 <extension-bird-ARCH.tar>}"
WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT
tar -xf "$TAR" -C "$WORKDIR" usr/local/sbin/bird
JANUS_BIRD_BIN="$WORKDIR/usr/local/sbin/bird" go test -count=1 -v -run TestReadOnlySocketWithBIRD ./internal/bgp
