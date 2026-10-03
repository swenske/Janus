#!/usr/bin/env bash
# Proposes a pull request for every upstream component that has a newer
# release it follows - hack/upstream bump, which checks every download
# before versions.mk changes. Run by .github/workflows/upstream-watch.yml
# from main's checkout, after `hack/upstream check -json STATUS`.
#
# Usage: hack/upstream-watch.sh STATUS.json [--dry-run]
#
# GH_TOKEN pushes the branch and opens the pull request: a token whose
# pull requests run ci.yml (GitHub never runs workflows for those the
# workflow's own GITHUB_TOKEN opens). One branch per component and
# version, upstream/<component>-<version>: a bump already proposed isn't
# proposed again, and an older one still open is closed in its favor.
# A bump that can't be checked yet (no independent checksum) is left for
# a later run.
set -euo pipefail

STATUS="${1:?usage: $0 STATUS.json [--dry-run]}"
DRY_RUN="${2:-}"
REPO="${GITHUB_REPOSITORY:-swenske/Janus}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

go build -o "$WORK/upstream" ./hack/upstream
git config user.name "janus-upstream[bot]"
git config user.email "janus-upstream@users.noreply.github.com"
base="$(git rev-parse HEAD)"

if [ -z "$DRY_RUN" ]; then
  gh label create upstream --repo "$REPO" --color 0E8A16 --description "An upstream component's new release" --force >/dev/null
  gh label create security --repo "$REPO" --color B60205 --description "Fixes or reports a vulnerability" --force >/dev/null
fi

while read -r name latest <&3; do
  branch="upstream/$name-$latest"
  if [ -z "$DRY_RUN" ] && git ls-remote --exit-code --heads origin "$branch" >/dev/null; then
    echo "$name $latest: already proposed ($branch)"
    continue
  fi
  git checkout --quiet -B "$branch" "$base"
  if ! "$WORK/upstream" bump -md "$WORK/$name.md" -json "$WORK/$name.json" "$name" "$latest" 2>"$WORK/$name.log"; then
    echo "::warning::$name $latest not proposed: $(tail -1 "$WORK/$name.log")"
    git checkout --quiet --force "$base"
    continue
  fi
  cat "$WORK/$name.log"
  if [ "$name" = linux ]; then
    # The kernel's list of built files may change with its source.
    make kernel-built-files >"$WORK/kernel.log" 2>&1 || {
      echo "::warning::linux $latest: make kernel-built-files failed"; tail -20 "$WORK/kernel.log"
      git checkout --quiet --force "$base"; continue
    }
  fi
  from="$(jq -r .from "$WORK/$name.json")"
  to="$(jq -r .to "$WORK/$name.json")"
  title="$(jq -r .title "$WORK/$name.json")"
  severity="$(jq -r .max_severity "$WORK/$name.json")"
  git add versions.mk kernel/built-files-*.txt
  git commit --quiet -m "upstream: $name $to" -m "$title $from -> $to, proposed by hack/upstream-watch.sh."
  if [ -n "$DRY_RUN" ]; then
    echo "--- dry run: would propose $branch"
    cat "$WORK/$name.md"
    git checkout --quiet --force "$base"
    continue
  fi
  git push --quiet --force "https://x-access-token:${GH_TOKEN}@github.com/$REPO.git" "HEAD:refs/heads/$branch"
  labels=upstream
  if [ "$(jq '.fixed | length' "$WORK/$name.json")" -gt 0 ]; then
    labels="$labels,security"
  fi
  url="$(gh pr create --repo "$REPO" --base main --head "$branch" --label "$labels" \
    --title "upstream: $title $from → $to" --body-file "$WORK/$name.md")"
  echo "$name $to: $url (max severity fixed: $severity)"
  # An older bump of the same component still open is superseded.
  gh pr list --repo "$REPO" --state open --json number,headRefName \
    --jq ".[] | select(.headRefName | startswith(\"upstream/$name-\")) | select(.headRefName != \"$branch\") | .number" |
    while read -r old; do
      gh pr close "$old" --repo "$REPO" --delete-branch --comment "Superseded by $url."
    done
  git checkout --quiet --force "$base"
done 3< <(jq -r '.[] | select(.bumpable) | "\(.name) \(.latest)"' "$STATUS")
