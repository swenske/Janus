# Conventions

The rules the code, the commits and the docs follow - most of them
checked by a test, so a change that breaks one says so.

## Commits

- **The subject is `<theme>: what changed`**, written for whoever reads
  the history: `controller: the extensions panel opens again`,
  `docs: a quick start`, `ci: a release deploys its docs`. Themes are
  the parts of the project - `controller`, `janusctl`, `network`,
  `haproxy`, `site`, `docs`, `ci`, `build`, `test`, `deps`, `release`...
  - and the release notes are grouped by them.
- **The body says why**, and what was checked; a test that proves it is
  named.
- **The docs change with the code**: a new flag, RPC or page behaviour
  updates its page in the same commit; `docs/api-routes.md`'s ✅/⬜
  flips with the RPC.

## Code

- **Comments say why**, in sentences; the code says what. Match the
  surrounding file's style, naming and comment density.
- **Pure logic stays pure** - parsers, planners, layout math - so it gets
  real unit tests; what touches the system gets a system test.
- **Lint and vet clean**: `make lint vet` (golangci-lint,
  `.golangci.yml`).
- **Every RPC declares its role** (`internal/rbac`), **every Controller
  route its gate**, **every janusctl command and flag its place** in the
  command tree: tests fail otherwise.

## Design rules

- **The build system isn't the target.** Tooling never leaks into the
  image: a node has no shell, no package manager, no build tool - it
  only moves pre-built bytes into place.
- **No generic "run a command" API**, ever. The no-shell calls - list,
  read, copy, logs, dmesg, packet capture - are read-only and narrow, and
  never serve the node's keys.
- **A dangerous change has a way back**: an update, a reset, a network
  or firewall change streams its progress and, where it makes sense,
  reverts by itself - never a fire-and-forget call that can leave a node
  unreachable.
- **Optional stays optional, at the image level**: a node without BGP has
  no BGP daemon at all, and its API says the module isn't enabled.
- **Credentials stay where they belong**: the Controller never stores a
  user's own admin credential, and a node never sends its own anywhere.

## Upstreams

Every upstream is pinned and followed: versions and checksums in
`versions.mk` (bumped with `make upstream-bump C=<component>`, which
checks signatures and published sums first), base images by
`tag@digest`, GitHub Actions by commit with the version in a comment,
Go modules and npm packages by their lock files - Dependabot and a daily
watch keep them current ([following upstreams](../upstreams.md)).

A fix to a vulnerability in Janus's own code comes with its record in
`security/fixes/` - the release that ships it says so, and publishes an
advisory ([Janus's own vulnerabilities](../upstreams.md#januss-own-vulnerabilities)).

## Docs

Written for the reader who has to act - what it does, what to do - in
short sentences; sentence-case headings; examples that run. How the
docs site is built and checked, and its rules: [writing
docs](writing-docs.md).
