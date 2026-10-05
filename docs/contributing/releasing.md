# Releasing

Releases are cut by a maintainer, from `main`, by `image-build.yml` on
the self-hosted runners: the same run that builds and tests every image
publishes them.

## Versions

Releases are dated: `vYYYY.MM.DD`, then `-2`, `-3`... for more the same
day - several a day is normal. A release is cumulative and only the
latest is supported ([security policy](../../SECURITY.md)); releases are
marked alpha in their name, not as pre-releases.

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
3. **Dispatch** `image-build.yml` on `main` with `release_version`.

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

## Without a release

Dispatched without `release_version`, the same run builds and tests
everything and uploads the images as workflow artifacts - the way to
prove a change before a release. It also pushes the Controller's
`latest` image.
