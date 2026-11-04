# Releasing

Releases are cut by a maintainer, from `main`, by `image-build.yml` on
the self-hosted runners: the same run that builds and tests every image
publishes them.

## Versions

Releases are dated: `vYYYY.MM.DD`, then `-2`, `-3`... for more the same
day. A release is cumulative; the latest is supported, and the previous
one for 14 days after it ([security policy](../../SECURITY.md)); releases
are marked alpha in their name. A [pre-release](#pre-releases) takes the
same kind of version: it is the release it may become. A
[maintenance release](#maintenance-releases) takes one of the day of the
release it fixes.

## Pace

A release is cut as a pre-release, tested on the lab, then promoted -
**at most once a week**: beta testers follow a pace they can keep. The
exception is a security fix, which ships within the
[security policy](../../SECURITY.md#reporting-a-vulnerability)'s targets:
a release cut or promoted at once, tested on the lab when the target
leaves the time - and, while the previous release is supported, a
maintenance release of it.

## Cutting one

1. **The notes**: `.github/release-notes/<version>.md`, written by hand
   for operators - a short **Highlights** section, then one section per
   theme, each with its emoji (🌐 network, 🖥️ Controller, 🐛 fixes,
   ⚠️ upgrade notes...), built from `git log <previous tag>..HEAD`,
   whose `<theme>:` subjects give the grouping
   ([the notes' README](../../.github/release-notes/README.md)). The
   workflow refuses to start without them.
2. **Security fixes** - an upstream component, a Go module, the Go
   toolchain, an npm package, or Janus's own code: the notes need a
   `## 🔒 Security` section, drafted with `make upstream-security-notes
   FROM=<previous tag> RELEASE=<version>` and rewritten for operators -
   which nodes, what to do.
3. **Dispatch** `image-build.yml` on `main` with `release_version` - and
   `prerelease` to test it first.

## What the run publishes

Once every test job passed, the publishing job - on the runner labelled
`janus-publish`, the only one trusted to build what's published:

- builds the signed release bundle and checks its signatures against
  the committed certificate;
- pushes the Controller's image to Docker Hub (`latest`, the commit, the
  version);
- creates the GitHub release - the alpha warning, the notes, the
  changelog link - with the images, the bundle, the build inputs of
  custom images, the `janusctl` packages, the Terraform provider, the
  Controller's image reference, `security.json` and an SBOM. It stays a
  draft until every file is uploaded, then is published, with its tag:
  nobody sees a release half there;
- publishes `janusctl` to the apt repository;
- deploys the docs - `/docs/` now follows the release;
- publishes a security advisory for each fix of Janus's own code, and
  for an upstream fix rated high or worse.

A release fixing something gets "🔒 security update" in its name, and
the Controller marks the update on the nodes it concerns.

## When publishing fails

Rerun the failed job (`gh run rerun <run> --failed`): it builds
everything again, then:

- **a draft left behind** - the run failed while uploading, after
  GitHub refused a file now and then (each upload is tried four times
  first) - is completed with this attempt's files, all of them: those
  already there with the same bytes stay, the others are replaced. A
  draft was never seen. Two builds of a commit give the same bytes
  (`hack/reproducible.sh`) - but for the signed UKIs and the images
  carrying them, whose Secure Boot signatures hold the time they were
  made - and should
  something break that, a bundle mixing two attempts would pair a root
  filesystem with UKIs pinning another one's;
- **a published release** - the run failed after it, publishing
  janusctl or the docs - is never changed: its files are compared with
  this attempt's, any difference is reported as a warning - expected
  for the signed UKIs and the images, else a build that stopped being
  reproducible - and the steps after it run again. One published
  without a file it should have stops the rerun: adding a file from
  another build to a release isn't done - publish another version;
- **the Controller's version tag** already on Docker Hub, from this
  commit, is kept - with `latest` and the commit's tag - and the release
  names it. From another commit, the run stops: a version's image never
  changes.

A release or a tag of that version made from another commit stops it
too. To start over from nothing, delete the draft - never a published
release: people downloaded its files, Controllers cached their sha256.

## Pre-releases

A pre-release is a release built to be tested before it becomes one - on
the lab's nodes, by beta testers - then promoted **unchanged**: what was
tested is what ships, and nothing is rebuilt.

1. **Cut it**: the same notes, then `image-build.yml` with
   `release_version` and `prerelease` ticked. It must be newer than the
   newest release. Its GitHub release is marked pre-release, never the
   latest, with a warning on top of its notes; janusctl goes to apt's
   `testing` suite only; `/docs/` doesn't move and no advisory is
   published. The Controller shows it on the Update pages, folded, never
   as an update ([updating nodes](../guide/updates.md#pre-releases)); the
   image factory builds it for a schematic when asked, not ahead.
2. **Test it**: install it on the lab from the Controller's Update page,
   or pin it - a Terraform `janus_node`'s `version`, a machine's
   `PATCH`. A problem found means a fix on `main` and another
   pre-release; the failed one stays as it is, superseded.
3. **Complete the notes** on `main` meanwhile, if testing taught
   something worth saying - they are re-read at the promotion, and
   frozen then.
4. **Promote it**: `release-promote.yml` with its `version` - a dry run
   by default, which checks it and shows, in the run's summary, the
   release it would become; then again without `dry_run`. It checks the
   pre-release is still newer than the newest release (a release cut
   meanwhile supersedes it) and has everything a release publishes, and
   its notes against its own `security.json`; then it clears the flag,
   makes it the latest, publishes janusctl on apt's `stable` suite (the
   same packages), deploys the docs and publishes the security
   advisories - what the run above does for a release cut directly.

"The previous release" - what `security.json` counts fixes from, what
the docs' `/docs/` follows, what the upgrade-from-release test starts
from - is never a pre-release: `hack/releases` asks GitHub, as git alone
can't tell one from a release.

## Maintenance releases

For 14 days after a release ships, the one before it is still supported
([security policy](../../SECURITY.md)): a vulnerability fixed then gets
a maintenance release of it - the previous release with the fix and
nothing else, for the nodes and Controllers that stay on it while their
operators try the new one.

1. **The newest line first**: the fix on `main`, released - cut, or in a
   pre-release promoted at once. That release publishes the advisories.
2. **A branch at the previous release's tag**, the fix cherry-picked
   onto it - its `security/fixes` record included -, and the notes:

   ```sh
   git switch -c release/v2026.11.04 v2026.11.04
   git cherry-pick <the fix's commits>
   $EDITOR .github/release-notes/v2026.11.04-2.md   # 🔒 section required
   ```

   Its version is one of that release's day, after it - the next one
   GitHub has neither as a release nor as a pre-release: it then sorts
   between the release it fixes and the newest. When the newest release
   is of the same day, no version is left: cut a release instead.
3. **Dispatch** `image-build.yml` on the branch, with `release_version`
   and `maintenance`:

   ```sh
   gh workflow run image-build.yml --ref release/v2026.11.04 \
     -f release_version=v2026.11.04-2 -f maintenance=true
   ```

   The run checks it is one (`hack/releases check-release
   -maintenance`): the newest release its branch reaches must be the
   previous release - or a maintenance release of it, for a second fix
   -, and its version of that day, newer, older than the newest. After
   the 14 days it warns, and publishes all the same. It builds and
   tests everything like a release, its `security.json` counts the
   fixes since the release it fixes, and it publishes:
   - the GitHub release, named "… - maintenance of v2026.11.04" with a
     note on top, **never the latest**;
   - the Controller's image under its version and its commit, **never
     `latest`**;
   - no janusctl on apt, no docs - both follow the newest release - and
     no advisory: its fixes have theirs, from step 1. The run's summary
     lists them: add the version to their patched versions on the
     Security tab. An advisory of its own would read "before
     v2026.11.04-2", which calls the newest release patched.
4. **The notes on `main` too**, for the record: copy the notes file.

Controllers and the image factory order releases by version, not by
GitHub's list (by the date of each tag's commit, where the maintenance
release comes first): the newest stays the latest, and a node on it is
never offered the maintenance release. A node on the previous release
installs it by its version ([updating
nodes](../guide/updates.md#maintenance-releases)); the next update it is
offered is the newest release, as before. Its 🔒 badge leaves out the
fixes the maintenance release already carries.

`latest` on Docker Hub is only ever pushed by `main` or a release that
isn't a maintenance release: a run of another branch pushes its commit's
tag alone.

## Without a release

Dispatched without `release_version`, the same run builds and tests
everything and uploads the images as workflow artifacts - the way to
prove a change before a release. On `main`, it also pushes the
Controller's `latest` image; on another branch, its commit's tag alone.
