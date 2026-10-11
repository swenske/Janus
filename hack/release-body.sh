#!/usr/bin/env bash
# The body of a GitHub release: the alpha warning (and, for a
# pre-release, what a pre-release is), the version's hand-written notes,
# then GitHub's generated list of changes since PREVIOUS - the newest
# release, never a pre-release: a promoted release lists what its readers
# didn't have yet. image-build.yml writes it when it cuts a release or a
# pre-release, release-promote.yml again when it promotes one.
#
# Usage: hack/release-body.sh VERSION PREVIOUS release|prerelease COMMIT OUT
#   COMMIT: what the tag points at (or will). GITHUB_TOKEN and
#   GITHUB_REPOSITORY as in a workflow; curl and python3 only.
set -euo pipefail
version=$1 prev=$2 kind=$3 commit=$4 out=$5
notes=".github/release-notes/$version.md"
[ -s "$notes" ] || { echo "::error::no release notes at $notes" >&2; exit 1; }
case "$kind" in release | prerelease) ;; *) echo "usage: $0 VERSION PREVIOUS release|prerelease COMMIT OUT" >&2; exit 2 ;; esac

generated=$(mktemp)
trap 'rm -f "$generated" "$generated.req"' EXIT
python3 - "$version" "$prev" "$commit" >"$generated.req" <<'PY'
import json, sys
version, prev, commit = sys.argv[1:4]
req = {"tag_name": version, "target_commitish": commit}
if prev:
    req["previous_tag_name"] = prev
json.dump(req, sys.stdout)
PY
curl -sS --fail-with-body -X POST \
  -H "Authorization: Bearer $GITHUB_TOKEN" -H "Accept: application/vnd.github+json" \
  "https://api.github.com/repos/$GITHUB_REPOSITORY/releases/generate-notes" \
  --data @"$generated.req" -o "$generated"
rm -f "$generated.req"

{
  echo '> [!WARNING]'
  echo '> **Alpha software.** Janus is under active early development: expect breaking changes, incomplete features and rough edges. Not yet recommended for production use.'
  echo
  if [ "$kind" = prerelease ]; then
    echo '> [!CAUTION]'
    echo "> **Pre-release**, published to be tested - in a lab, by beta testers - before it becomes a release: the same build is then promoted, unchanged. It may still be withdrawn, and it isn't supported. Nodes and Controllers are never offered it as an update: installing it is a choice, on the Update page or by its version ([pre-releases](https://janus.sw-servers.net/docs/guide/updates.md#pre-releases))."
    echo
  fi
  cat "$notes"
  echo
  python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["body"])' "$generated"
} >"$out"
