# shellcheck shell=bash
# Sourced by the QEMU tests that read files off a running node's STATE
# partition (partition 6) from the host - its first credentials, mostly.
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
#   state_pki DISK DIR           ca.crt, admin.crt and admin.key

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
  state_read "$1" "$2" pki/ca.crt pki/admin.crt pki/admin.key
}
