#!/bin/sh
# Forced SSH command for the janus-publish account (authorized_keys'
# command= restriction), same design as gotochanger-publish-dispatch.sh.
# $SSH_ORIGINAL_COMMAND is what the client asked to run; this script is
# the only thing that ever runs, so it rejects anything it doesn't
# recognize.
#
# Two accepted shapes only:
#   upload janusctl_<version>_<amd64|arm64>.deb
#                 -- the file's bytes on stdin, written to incoming/ (as
#                    <name>.part, renamed once complete, so a cut upload
#                    is never published). At most 64 MiB.
#   publish       -- the one sudo-permitted script: adds incoming/*.deb
#                    to the "janus" aptly repo and (re)publishes it.
set -eu

INCOMING="/home/janus-publish/incoming"
MAX_BYTES=67108864
CMD="${SSH_ORIGINAL_COMMAND:-}"

reject() { echo "rejected: $1" >&2; exit 1; }

case "$CMD" in
  upload\ *)
    name="${CMD#upload }"
    case "$name" in
      */*|.*) reject "bad filename" ;;
      janusctl_*_amd64.deb|janusctl_*_arm64.deb) ;;
      *) reject "filename must match janusctl_<version>_<amd64|arm64>.deb" ;;
    esac
    version="${name#janusctl_}"
    version="${version%_*.deb}"
    case "$version" in
      ""|*[!0-9A-Za-z.+~-]*) reject "bad version in filename" ;;
    esac
    umask 077
    head -c "$MAX_BYTES" > "$INCOMING/$name.part"
    mv "$INCOMING/$name.part" "$INCOMING/$name"
    ;;
  publish)
    exec sudo -n /usr/local/sbin/janus-aptly-publish.sh
    ;;
  *)
    reject "unrecognized command"
    ;;
esac
