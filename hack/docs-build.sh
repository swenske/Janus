#!/usr/bin/env bash
# Builds the docs site (site/docs) in Docker, from a snapshot of the
# sources, into site/backend/docsdist/<channel> - what janus-site embeds
# and serves.
#
#   hack/docs-build.sh <latest|next> [<ref>]
#       one channel - latest is served at /docs/, next at /docs/next/ -
#       from <ref>'s tree (git archive), or from the working tree (its
#       tracked and untracked, not ignored, files) without one.
#   hack/docs-build.sh site
#       both channels, as site-deploy deploys them: next from HEAD,
#       latest from the newest release that has the docs site (HEAD
#       again until one has) - never a pre-release.
set -euo pipefail
cd "$(dirname "$0")/.."

out_root=site/backend/docsdist
src_root=build/docs-src

# NEWEST_RELEASE: the newest release reachable from HEAD - never a
# pre-release, which git alone can't tell from a release: GitHub's
# release list says (hack/releases). find_newest_release strict fails
# without it (site: /docs/ must never follow a pre-release); otherwise
# the newest tag will do, said - only the docs' version banner shows it.
find_newest_release() {
  if NEWEST_RELEASE=$(go run ./hack/releases newest -reachable-from HEAD); then
    return
  fi
  if [ "${1:-}" = strict ]; then
    echo "docs: GitHub doesn't say which tag is the newest release - refusing to guess /docs/'s" >&2
    exit 1
  fi
  NEWEST_RELEASE=$(git describe --tags --abbrev=0 --match 'v20*' HEAD 2>/dev/null || true)
  echo "docs: GitHub doesn't say which tag is the newest release - taking the newest tag, ${NEWEST_RELEASE:-none}" >&2
}

# build CHANNEL [REF]
build() {
  local channel=$1 ref=${2:-} src version commit date docs_ref
  src="$src_root/${ref:-worktree}"
  rm -rf "$src"
  mkdir -p "$src"
  if [ -n "$ref" ]; then
    git archive "$ref" | tar -x -C "$src"
    commit=$(git rev-parse "$ref^{commit}")
    # A release tag names its docs; anything else is main's.
    if git describe --exact-match --tags --match 'v20*' "$commit" >/dev/null 2>&1; then
      version=$(git describe --exact-match --tags --match 'v20*' "$commit")
      docs_ref=$version
    else
      version=$(git describe --tags --always "$commit")
      docs_ref=main
    fi
  else
    git ls-files -z -co --exclude-standard | tar --null --ignore-failed-read -T - -cf - | tar -x -C "$src"
    commit=$(git rev-parse HEAD)
    version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
    docs_ref=main
  fi
  date=$(git log -1 --format=%cI "$commit")
  # When each Markdown file last changed - the build itself has no git.
  git log --format='@%cI' --name-only "$commit" -- '*.md' |
    awk '/^@/ { d = substr($0, 2); next } NF && !($0 in seen) { seen[$0] = 1; print $0 "\t" d }' \
      >"$src/site/docs/lastupdated.tsv"
  echo "docs: building $channel from ${ref:-the working tree} ($version)"
  rm -rf "${out_root:?}/$channel"
  docker build -f "$src/site/docs/Dockerfile" --target export \
    --build-arg DOCS_CHANNEL="$channel" \
    --build-arg DOCS_VERSION="$version" \
    --build-arg DOCS_COMMIT="$commit" \
    --build-arg DOCS_DATE="$date" \
    --build-arg DOCS_REF="$docs_ref" \
    --build-arg DOCS_LATEST_VERSION="$NEWEST_RELEASE" \
    -o "$out_root/$channel" "$src"
}

case "${1:-}" in
latest | next)
  find_newest_release
  build "$1" "${2:-}"
  ;;
site)
  find_newest_release strict
  build next HEAD
  tag=$NEWEST_RELEASE
  if [ -n "$tag" ] && git cat-file -e "$tag:site/docs/package.json" 2>/dev/null; then
    build latest "$tag"
  else
    echo "docs: no release has the docs site yet - latest is built from HEAD too"
    build latest HEAD
  fi
  ;;
*)
  echo "usage: $0 latest|next [<ref>] | site" >&2
  exit 2
  ;;
esac
