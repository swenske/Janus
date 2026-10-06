#!/usr/bin/env bash
# Writes selinux/classes.conf from the class list a kernel's
# scripts/selinux/mdp emitted (make selinux-classes): mdp's classes,
# permissions and initial SIDs, between the file's own hand-written
# header and its reviewed policy capabilities - checkpolicy accepts only
# those its libsepol knows, so which to keep stays a decision; the
# capabilities mdp emitted that the file doesn't keep are printed, for
# that review.
#
# Usage: selinux/classes.sh <mdp classes.conf> <selinux/classes.conf> <kernel version>
set -euo pipefail

MDP="${1:?usage: $0 <mdp classes.conf> <selinux/classes.conf> <kernel version>}"
OUT="${2:?}"
KERNEL="${3:?}"

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
# The header: everything before the first class.
sed '/^class /,$d' "$OUT" | sed "s/^# Generated from Linux [0-9.]*\(.*\)/# Generated from Linux $KERNEL\1/" > "$tmp"
# mdp's classes, commons, access vectors and SIDs - not its policycaps.
sed -n '/^class /,$p' "$MDP" | grep -v '^policycap ' >> "$tmp"
# The reviewed policy capabilities, with their comment: from the comment
# that introduces them to the end.
sed -n '/^# mdp itself emits every policycap/,$p' "$OUT" >> "$tmp"

kept="$(sed -n 's/^policycap \(.*\);$/\1/p' "$OUT" | sort)"
emitted="$(sed -n 's/^policycap \(.*\);$/\1/p' "$MDP" | sort)"
left="$(comm -13 <(echo "$kept") <(echo "$emitted") | tr '\n' ' ')"
cp "$tmp" "$OUT"
echo "Wrote $OUT from Linux $KERNEL's classes"
[ -z "${left// /}" ] || echo "Policy capabilities this kernel knows that $OUT leaves out (checkpolicy must know them to keep one): $left"
