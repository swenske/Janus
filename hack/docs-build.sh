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
#       latest from the newest release tag that has the docs site (HEAD
#       again until one has).
set -euo pipefail
cd "$(dirname "$0")/.."

out_root=site/backend/docsdist
src_root=build/docs-src

# newest_release: the newest release tag reachable from HEAD.
newest_release() {
  git describe --tags --abbrev=0 --match 'v20*' HEAD 2>/dev/null || true
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
  echo "docs: building $channel from ${ref:-the working tree} ($version)"
  rm -rf "${out_root:?}/$channel"
  docker build -f "$src/site/docs/Dockerfile" --target export \
    --build-arg DOCS_CHANNEL="$channel" \
    --build-arg DOCS_VERSION="$version" \
    --build-arg DOCS_COMMIT="$commit" \
    --build-arg DOCS_DATE="$date" \
    --build-arg DOCS_REF="$docs_ref" \
    --build-arg DOCS_LATEST_VERSION="$(newest_release)" \
    -o "$out_root/$channel" "$src"
}

case "${1:-}" in
latest | next)
  build "$1" "${2:-}"
  ;;
site)
  build next HEAD
  tag=$(newest_release)
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
