#!/usr/bin/env bash
# runner-action-cache.sh OUT_DIR - the action archive cache of the
# self-hosted runners (docs/ci-runners.md "Action archive cache"): every
# action this repository's workflows and composite actions pin by commit,
# as the runner's ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE expects it -
# OUT_DIR/<owner>_<repo>/<sha>.tar.gz. A runner finding an action there
# doesn't download it: GitHub's archive of a commit can fail while the
# commit is fine (2026-10-15: codeload answered 404 for setup-go v7.0.0's
# commit for hours, every image-build job failed in its first seconds).
#
# Each archive is made by git from the commit itself, not downloaded from
# codeload: what the runner extracts is exactly the pinned commit.
# Existing archives are kept; a pin no workflow uses any more is removed.
set -euo pipefail
out=${1:?usage: $0 OUT_DIR}
repo=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
mkdir -p "$out"
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT

# owner/repo[/path]@<40 hex> -> owner/repo sha
pins=$(cat "$repo"/.github/workflows/*.yml "$repo"/.github/actions/*/action.yml |
  grep -oE 'uses:[[:space:]]*[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(/[^@[:space:]]*)?@[0-9a-f]{40}' |
  sed -E 's/uses:[[:space:]]*//; s#^([^/]+/[^/@]+)[^@]*@#\1 #' | sort -u)

keep=()
while read -r name sha; do
  dir="$out/${name//\//_}" file="$out/${name//\//_}/$sha.tar.gz"
  keep+=("$file")
  if [ -s "$file" ]; then
    echo "kept    $name@$sha"
    continue
  fi
  git init -q "$work/$sha"
  git -C "$work/$sha" fetch -q --depth 1 "https://github.com/$name" "$sha"
  mkdir -p "$dir"
  git -C "$work/$sha" archive --format=tar --prefix="${name//\//-}-${sha:0:7}/" "$sha" | gzip -n > "$file.tmp"
  mv "$file.tmp" "$file"
  echo "added   $name@$sha"
done <<< "$pins"

find "$out" -name '*.tar.gz' -type f | while read -r f; do
  printf '%s\n' "${keep[@]}" | grep -qxF "$f" || { rm -f "$f"; echo "removed ${f#"$out"/}"; }
done
find "$out" -mindepth 1 -type d -empty -delete
