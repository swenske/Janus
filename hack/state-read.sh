# shellcheck shell=bash
# Sourced by the QEMU tests that read files off a running node's STATE
# partition (partition 6) from the host - its credentials, mostly.
#
# A single read can miss them: HAProxy answers on :8080 before janusd
# has bootstrapped the PKI (by design), and what janusd has just written
# may still be only in ext4's journal, which debugfs doesn't replay. So
# STATE is copied, the copy's journal replayed (e2fsck -fy) and the
# files read again every 2 s until all are there (#127, #139).
#
#   state_read DISK DIR PATH...  each PATH on STATE (pki/ca.crt) into
#                                DIR/<its base name>; the copy is left at
#                                DIR/state.img. Waits STATE_READ_TIMEOUT
#                                seconds (30) - 0 = one try - and returns
#                                1 when a file is still missing then.
#   state_pki DISK DIR           DIR/ca.crt, and an admin certificate the
#                                node's own CA (pki/ca.key, read off STATE
#                                too) signs for the test: DIR/admin.crt
#                                (followed by the CA) and DIR/admin.key,
#                                valid STATE_PKI_VALIDITY (24h) - what the
#                                first boot's admin certificate was. Needs
#                                go (hack/fleetca, built once into DIR).
#   state_creds DISK PREFIX      the same as PREFIX-ca.crt, PREFIX-admin.crt
#                                and PREFIX-admin.key (a node of several)
#   pki_admin PKI_DIR DIR        the same from a native janusd's -pki-dir:
#                                waits PKI_ADMIN_TIMEOUT seconds (20) for its
#                                PKI (server.crt, written after the CA),
#                                read with sudo when it must

state_read() {
  local disk="$1" dir="$2" start size f out got deadline
  shift 2
  start="$(sgdisk -i 6 "$disk" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
  size="$(sgdisk -i 6 "$disk" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
  mkdir -p "$dir"
  deadline=$((SECONDS + ${STATE_READ_TIMEOUT:-30}))
  while :; do
    dd if="$disk" of="$dir/state.img" bs=512 skip="$start" count="$size" status=none
    e2fsck -fy "$dir/state.img" >/dev/null 2>&1 || true
    got=1
    for f in "$@"; do
      out="$dir/${f##*/}"
      rm -f "$out"
      debugfs -R "dump $f $out" "$dir/state.img" >/dev/null 2>&1
      [ -s "$out" ] || got=0
      # A copy taken while janusd writes can hold the file's inode before
      # its data: a PEM file counts once it ends.
      case "$f" in
        *.crt | *.key | *.pem) grep -q -- '-----END ' "$out" 2>/dev/null || got=0 ;;
      esac
    done
    [ "$got" = 1 ] && return 0
    [ "$SECONDS" -lt "$deadline" ] || return 1
    sleep 2
  done
}

state_pki() {
  local disk="$1" dir="$2"
  state_read "$disk" "$dir" pki/ca.crt pki/ca.key || return 1
  sign_admin "$dir"
}

# sign_admin DIR: DIR/admin.crt and DIR/admin.key, signed by DIR/ca.crt
# and DIR/ca.key - hack/fleetca, built once.
sign_admin() {
  local dir="$1"
  if [ -z "${STATE_FLEETCA:-}" ] || [ ! -x "$STATE_FLEETCA" ]; then
    (cd "$(dirname "${BASH_SOURCE[0]}")/.." && go build -o "$dir/fleetca" ./hack/fleetca) || return 1
    STATE_FLEETCA="$dir/fleetca"
  fi
  "$STATE_FLEETCA" sign "$dir/ca.crt" "$dir/ca.key" os:admin admin "$dir/admin" "${STATE_PKI_VALIDITY:-24h}"
}

pki_admin() {
  local pki="$1" dir="$2" deadline
  deadline=$((SECONDS + ${PKI_ADMIN_TIMEOUT:-20}))
  until pki_cat "$pki/server.crt" 2>/dev/null | grep -q -- '-----END '; do
    [ "$SECONDS" -lt "$deadline" ] || return 1
    sleep 1
  done
  mkdir -p "$dir"
  pki_cat "$pki/ca.crt" >"$dir/ca.crt" || return 1
  (umask 077 && pki_cat "$pki/ca.key" >"$dir/ca.key") || return 1
  sign_admin "$dir"
}

# pki_cat FILE: FILE's contents, read with sudo when this user can't.
pki_cat() {
  cat "$1" 2>/dev/null || sudo cat "$1"
}

state_creds() {
  local disk="$1" prefix="$2" f
  state_pki "$disk" "$prefix.pki" || return 1
  for f in ca.crt admin.crt admin.key; do
    cp "$prefix.pki/$f" "$prefix-$f" || return 1
  done
}
