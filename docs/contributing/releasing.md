# Releasing

Releases are cut by a maintainer, from `main`, by `image-build.yml` on
the self-hosted runners: the same run that builds and tests every image
publishes them.

## Versions

Releases are dated: `vYYYY.MM.DD`, then `-2`, `-3`... for more the same
day - several a day is normal. A release is cumulative and only the
latest is supported ([security policy](../../SECURITY.md)); releases are
marked alpha in their name. A [pre-release](#pre-releases) takes the same
kind of version: it is the release it may become.

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
- creates the tag and the GitHub release - the alpha warning, the notes,
  the changelog link - with the images, the bundle, the build inputs of
  custom images, the `janusctl` packages, the Terraform provider, the
  Controller's image reference, `security.json` and an SBOM;
- pushes the Controller's image to Docker Hub (`latest`, the commit, the
  version);
- publishes `janusctl` to the apt repository;
- deploys the docs - `/docs/` now follows the release;
- publishes a security advisory for each fix of Janus's own code, and
  for an upstream fix rated high or worse.

A release fixing something gets "🔒 security update" in its name, and
the Controller marks the update on the nodes it concerns.

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

## Without a release

Dispatched without `release_version`, the same run builds and tests
everything and uploads the images as workflow artifacts - the way to
prove a change before a release. It also pushes the Controller's
`latest` image.
