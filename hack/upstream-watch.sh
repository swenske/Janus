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
# a later run - with NOT_PROPOSED set, listed in that file with the
# reason (the status issue shows it).
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

# not_proposed NAME VERSION REASON: a newer release left for a later run.
not_proposed() {
  echo "::warning::$1 $2 not proposed: $3"
  if [ -n "${NOT_PROPOSED:-}" ]; then
    echo "- **$1** $2: $3" >>"$NOT_PROPOSED"
  fi
  git checkout --quiet --force "$base"
}

if [ -z "$DRY_RUN" ]; then
  gh label create upstream --repo "$REPO" --color 0E8A16 --description "An upstream component's new release" --force >/dev/null
  gh label create security --repo "$REPO" --color B60205 --description "Fixes or reports a vulnerability" --force >/dev/null
  gh label create kernel-branch --repo "$REPO" --color 5319E7 --description "A kernel track moves to a new branch: review its config" --force >/dev/null
fi

while read -r name latest <&3; do
  branch="upstream/$name-$latest"
  if [ -z "$DRY_RUN" ] && git ls-remote --exit-code --heads origin "$branch" >/dev/null; then
    echo "$name $latest: already proposed ($branch)"
    continue
  fi
  git checkout --quiet -B "$branch" "$base"
  if ! "$WORK/upstream" bump -md "$WORK/$name.md" -json "$WORK/$name.json" "$name" "$latest" 2>"$WORK/$name.log"; then
    not_proposed "$name" "$latest" "$(tail -1 "$WORK/$name.log")"
    continue
  fi
  cat "$WORK/$name.log"
  from="$(jq -r .from "$WORK/$name.json")"
  to="$(jq -r .to "$WORK/$name.json")"
  title="$(jq -r .title "$WORK/$name.json")"
  severity="$(jq -r .max_severity "$WORK/$name.json")"
  extra_labels=""
  case "$name" in
  linux-*)
    track="${name#linux-}"
    # The list of files it builds, and its config resolved for the new
    # source - a stable release can backport a new symbol too (6.18.56 and
    # 7.2.10's CC_OPT_INLINE_MEMSET): new symbols get their defaults, the
    # pull request is where they're reviewed. The default track's arm64
    # config too (Raspberry Pi images have it) - unless raspberry-pi is
    # suspended (variants.mk): no arm64 build then, the config is
    # refreshed when the suspension is lifted. One build each: the
    # refresh saves what the build resolved, the same kernel. A track
    # moving to a new branch also takes the SELinux classes of the newest
    # kernel.
    targets="kernel-built-files-$track kernel-config-refresh"
    if [ "$track" = "$(sed -n 's/^KERNEL_DEFAULT_TRACK *:= *//p' variants.mk)" ]; then
      if hack/target-suspended.sh raspberry-pi; then
        echo "raspberry-pi is suspended (variants.mk): the arm64 kernel config isn't refreshed"
      else
        targets="$targets rpi4-kernel-config-refresh"
      fi
    fi
    if [ "$(echo "$from" | cut -d. -f1,2)" != "$(echo "$to" | cut -d. -f1,2)" ]; then
      extra_labels=",kernel-branch"
      targets="$targets selinux-classes"
    fi
    # shellcheck disable=SC2086
    make $targets KERNEL_TRACK="$track" >"$WORK/kernel.log" 2>&1 || {
      tail -20 "$WORK/kernel.log"
      not_proposed "$name" "$latest" "make $targets failed"; continue
    }
    if ! git diff --quiet -- kernel/configs/; then
      # shellcheck disable=SC2016 # Markdown's backticks
      printf '\n### ⚙️ Kernel configuration\n\nkbuild resolves the committed configuration differently for this source: review `kernel/configs/` in the diff (new symbols take their defaults).\n' >>"$WORK/$name.md"
    fi
    ;;
  esac
  git add versions.mk kernel/built-files-*.txt kernel/configs/ selinux/classes.conf
  git commit --quiet -m "upstream: $name $to" -m "$title $from -> $to, proposed by hack/upstream-watch.sh."
  if [ -n "$DRY_RUN" ]; then
    echo "--- dry run: would propose $branch"
    cat "$WORK/$name.md"
    git checkout --quiet --force "$base"
    continue
  fi
  git push --quiet --force "https://x-access-token:${GH_TOKEN}@github.com/$REPO.git" "HEAD:refs/heads/$branch"
  labels="upstream$extra_labels"
  if [ "$(jq '.fixed | length' "$WORK/$name.json")" -gt 0 ]; then
    labels="$labels,security"
  fi
  url="$(gh pr create --repo "$REPO" --base main --head "$branch" --label "$labels" \
    --title "upstream: $title $from → $to" --body-file "$WORK/$name.md")"
  echo "$name $to: $url (max severity fixed: $severity)"
  # An older bump of the same component still open is superseded - its
  # branch is upstream/<this component>-<a version> (as its feed writes
  # it: keepalived's tags start with a v), never another component's
  # whose name merely starts with this one's.
  gh pr list --repo "$REPO" --state open --json number,headRefName \
    --jq ".[] | select(.headRefName | startswith(\"upstream/$name-\")) | select(.headRefName | ltrimstr(\"upstream/$name-\") | test(\"^v?[0-9]\")) | select(.headRefName != \"$branch\") | .number" |
    while read -r old; do
      gh pr close "$old" --repo "$REPO" --delete-branch --comment "Superseded by $url."
    done
  git checkout --quiet --force "$base"
done 3< <(jq -r '.[] | select(.bumpable) | "\(.name) \(.latest)"' "$STATUS")
