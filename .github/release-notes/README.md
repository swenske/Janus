# Release notes

Each release has hand-written notes here, named after its version
(`v2026.09.30-3.md`), committed before the release is cut:
`image-build.yml` refuses to start a release without them. The
published body is an alpha warning, then this file, then GitHub's
"Full Changelog" link.

Structure: a short **Highlights** section first, then one section per
theme (🌐 network, 🖥️ Controller, 🔒 security, 🐛 fixes, ⚠️ upgrade
notes...), each with an emoji. Write for an operator: what changed for
them and what they have to do, not how it was built. Build them from
`git log <previous tag>..HEAD`, whose `<theme>: message` subjects give
the grouping.

Link the docs on the docs site, by a file's permalink - the file's path
under `docs/` (or in the repository, for a page outside it), an anchor
if it helps: `https://janus.sw-servers.net/docs/vrrp.md#applying`. The
site sends it to the page wherever it lives, in the docs of the newest
release - the one the notes announce once it's out - and
`hack/docscheck` keeps the file and the heading there.

## 🔒 Security

When the release fixes vulnerabilities - an upstream component, a Go
module, the Go toolchain, an npm package - its notes must have a
`## 🔒 Security` section: `image-build.yml` refuses to publish without
one. Draft it with

```sh
make extensions-amd64   # for the rebuilt Go binaries they carry
make upstream-security-notes FROM=<previous tag> RELEASE=<this version> EXTENSIONS=build/extensions
```

and edit it into the notes: say what it means for an operator (which
nodes, what to update), not just the IDs. At publish time, the workflow
attaches the same list as `security.json` (read by the Controller) with
an SBOM, adds "🔒 security update" to the release's name, and publishes a
GitHub security advisory when a fix is rated high or worse
([docs/upstreams.md](../../docs/upstreams.md)).
