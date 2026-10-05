# Following upstreams

Janus is built from other projects' releases: the kernel, HAProxy and its
TLS library, every extension's daemon, firmware, Go modules, npm packages,
base images. This page says how each is pinned, checked, kept up to date,
and watched for vulnerabilities - and how a release says what it fixes.

## Who follows what

| What | Pinned in | Followed by |
|---|---|---|
| Go modules (both modules), npm packages (both frontends), base images, GitHub Actions | `go.mod`, `package-lock.json`, `FROM image:tag@digest`, `uses: action@sha # vX` | Dependabot (`.github/dependabot.yml`): weekly pull requests, minor/patch grouped; Dependabot alerts and security updates are on |
| Every upstream release the build downloads | [`versions.mk`](../versions.mk): version + sha256 | `hack/upstream` (this page) |

Dependabot can't follow `versions.mk`: nothing there is a package it knows,
and a bump isn't a version number - it's a checksum, checked against
upstream's signature first. `hack/upstream` does that.

## `hack/upstream`

```sh
make upstream-check                      # status of everything, Markdown
make upstream-bump C=haproxy [V=3.4.7]   # a checked bump of versions.mk
make upstream-security-notes FROM=v2026.10.03-4 RELEASE=v2026.10.10
make upstream-sbom                       # build/sbom.cdx.json (CycloneDX)
```

`go run ./hack/upstream check -only haproxy,linux` checks a few;
`-ref <tag>` checks a release's pins instead of the working tree's.
`GITHUB_TOKEN` in the environment lifts GitHub's anonymous rate limit.

### Components

Each component (`hack/upstream/components.go`) says where its new releases
come from, which ones it follows, how a download is checked, and where its
vulnerabilities are known. A test fails when `versions.mk` pins something
no component follows.

| Component | Follows | Checked by | Vulnerabilities |
|---|---|---|---|
| `linux` | its longterm branch (6.18.x) | Greg Kroah-Hartman's or Linus Torvalds's signature on the tar, and kernel.org's signed sha256sums | kernel.org CNA, filtered by the files Janus's kernels build |
| `haproxy` | its LTS branch (3.4.x) | haproxy.org's published sha256 (`.sha256`, `releases.json`) - HAProxy signs nothing | haproxy.org's per-version bug lists |
| `zlib` | every release | Mark Adler's signature, and Alpine's sha512 | osv.dev |
| `aws-lc` | every release | Alpine's or FreeBSD's checksum of GitHub's tag archive - AWS-LC signs nothing | AWS-LC's own GitHub advisories |
| `musl-cross-make` | by hand (a commit) | - | - |
| `node-exporter` | its major - built from source with this tree's Go, not upstream's binary | Go's checksum database (`go mod download`) on the module's zip | govulncheck on its source, with this tree's Go |
| `libmnl`, `libnftnl`, `nftables` | every release | the Netfilter Core Team's signature | - (none published) |
| `jansson` | every release | Petri Lehtinen's signature | osv.dev |
| `keepalived` | its major | Alpine's sha512 - keepalived signs nothing | osv.dev |
| `qemu-guest-agent` | every release | Michael Roth's signature | osv.dev, qemu-ga issues only |
| `bird` | its major (2.x: BIRD 3 aborts on janusd's protocol disabling) | FreeBSD ports' sha256 - BIRD signs nothing | osv.dev |
| `rpi4-uefi`, `rpi5-uefi` | every release (rpi5: pre-releases too) | GitHub's asset digest | - |
| `docker-compose` | its major | upstream's `.sha256` and GitHub's asset digest | govulncheck on the binary |
| `opentofu` (tests only) | its major | OpenTofu's signed SHA256SUMS | - |
| `pebble` (tests only) | its major | Go's checksum database (`go install`) | - |
| `versitygw` (tests only) | its major | Go's checksum database (`go install`) | - |
| `consul` | its major | HashiCorp's signed SHA256SUMS | govulncheck on the binary |

"Follows" is what gets proposed on its own: a patch release of a pinned
branch, a new release of a pinned major. A newer branch or major is only
shown - moving to it is a decision, and usually more than a version number.
`check` also shows the release cycle's support (endoflife.date) and the
other maintained branches, for the kernel's and HAProxy's choice of
versions to come.

### A bump is checked before anything changes

`bump` downloads every artifact (each architecture's) and runs its
component's checks; `versions.mk` is only rewritten when they all pass.

- **Signatures** are checked with `gpgv` against a keyring holding only the
  keys that component expects (`hack/upstream/keys/<fingerprint>.asc`), and
  the primary key that signed must be one of them.
- **Independent checksums** (Alpine's aports, FreeBSD ports) cover
  upstreams that sign nothing: another party downloaded the same file
  separately. At least one must agree. When none knows the new version
  yet, the bump is refused ("no independent checksum yet") and a later run
  tries again.
- A key that changes (a new release manager) fails the check: adding a key
  is a reviewed commit - import it **by its full fingerprint**, from the
  upstream's own announcement, and export it with
  `gpg --armor --export-options export-minimal --export <FPR> > hack/upstream/keys/<FPR>.asc`.

### Vulnerabilities, and which apply

Each source lists what affects a version; a bump's security summary is
what the old version had and the new one no longer has. Some can't apply to
Janus - they're counted, with the reason, never silently dropped:

- **Kernel**: every stable release fixes CVEs (the kernel's CNA assigns one
  to most fixes). Only those whose fix changes a file Janus's kernels build
  apply: `kernel/built-files-{amd64,arm64}.txt`, from kbuild's own records
  of each build (`make kernel-built-files`; `image-build.yml` fails when
  they no longer match the build - regenerate them whenever a defconfig
  changes).
- **HAProxy**: haproxy.org rates every fix. CRITICAL ("a short-term
  reliability or security issue") and MAJOR count as security fixes for a
  load balancer, MEDIUM and MINOR as bugs. Fixes in what Janus's HAProxy is
  built without (QUIC/HTTP/3, Lua, OpenTracing, device detection, PCRE)
  don't apply.
- **zlib**: HAProxy only calls its in-memory deflate API - not the `gz*`
  file functions, nor the `contrib/` programs (minizip, untgz...); osv.dev
  also files under zlib the wrappers that bundle it (Perl's).
- **QEMU**: only qemu-ga is shipped - the emulator's issues don't apply.

A kernel CVE is often published weeks after the stable release that
fixed it: a release's 🔒 section lists what is known when it's written,
and the status issue keeps counting.

Severity comes from the source (GitHub advisories, HAProxy's rating), else
from the CVE record's CVSS (its CNA's, else CISA's ADP). A CVE in CISA's
Known Exploited Vulnerabilities catalog is critical, whatever its score.

`check` also runs govulncheck on Janus's own Go code (only what it
reaches) and `npm audit` on the Controller frontend's production packages.
A downloaded Go binary (Consul, Compose) is checked as built: what its
symbols reach, the standard library of the Go that built it included - a
stripped binary is refused, govulncheck would report every vulnerability
of every module in it. When an upstream's binary lags behind on Go,
building it from source fixes it: node_exporter 1.12.1's carried seven
vulnerabilities of Go 1.26.5's standard library, the one built here none.

## Every day: `upstream-watch.yml`

A scheduled workflow (GitHub-hosted, never on pull requests, main's code
only):

- keeps the **📦 Upstream status** issue: `check`'s report;
- keeps a **🔒 Security fixes waiting for a release** issue while `main`
  fixes vulnerabilities the latest release still has (`security-notes`
  from that release) - closed once there's none; [SECURITY.md](../SECURITY.md)
  sets how soon a release follows;
- opens a pull request per component with a newer release it follows
  (`hack/upstream-watch.sh`): branch `upstream/<component>-<version>`, the
  checks that passed and what it fixes in the body, labeled `upstream`
  (and `security` when it fixes something). A newer one supersedes and
  closes an older one. Merge after `image-build.yml`'s boot tests on the
  branch, as for any change.

Pull requests need `UPSTREAM_BOT_TOKEN`: a fine-grained token on this
repository with **Contents**, **Pull requests** and **Repository security
advisories** read/write (the last one for releases, below) - GitHub runs
no workflow for pull requests the workflow's own token opens, so `ci.yml`
wouldn't check them. Without it, only the issues are kept.
`workflow_dispatch` defaults to a dry run: everything is checked, bumps
included, nothing is pushed or posted.

## A release says what it fixes

`image-build.yml`, when it cuts a release:

1. runs `security-notes` from the previous release: `security.json`,
   what the release fixes - Janus's own code (below), upstreams, Go
   modules linked into Janus's programs, the Go toolchain, the
   Controller's npm packages - with each fix's severity and whether it
   reaches nodes or the Controller. The Go
   binaries the new extensions carry are also compared with the previous
   release's (govulncheck on both): a binary rebuilt with a newer Go fixes
   things without any version changing - node_exporter, built here. A
   stripped one (Consul's) is compared by version only;
2. refuses to go on if the release fixes something and its notes have no
   `## 🔒` section (draft it with `make upstream-security-notes FROM=<previous
   tag> RELEASE=<version> EXTENSIONS=build/extensions`, after `make
   extensions-amd64`, then make it say what it means for an operator);
3. names the release "Janus vX (Alpha) - 🔒 security update" when it fixes
   something, and attaches `security.json` and the SBOM (`sbom.cdx.json`);
4. publishes repository security advisories, ecosystem "other" (Janus
   isn't a package GitHub knows: no Dependabot alert anywhere): one per
   vulnerability of Janus's own code it fixes, whatever its severity, and
   - when what the components it updates fix is rated high or critical -
   one "Janus before vX ships known vulnerabilities", those fixes listed.
   It takes `UPSTREAM_BOT_TOKEN` with **Repository security advisories**
   read/write; without it the job prints the requests to file them by
   hand.

### Janus's own vulnerabilities

A vulnerability in Janus's own code gets a record, committed with its
fix: `security/fixes/JANUS-<year>-<nnn>.json` - its title (what an
attacker could do), severity and CVSS 3.1 vector (`go test
./hack/upstream` refuses a severity its vector doesn't score), CWE,
`target` (`node`, `controller` or `client`) and description: what was
wrong, who is affected, what the fix does, what to do besides updating.
The description is the advisory's body, written for operators.

A release fixes the records added since the previous one: they're in its
`security.json` (as `janus`, `janus-controller` or `janusctl`, marked
`first_party`), so its name, its required 🔒 section and the Controller's
🔒 badge count them like any other fix - and each gets its own advisory.
The daily status issue counts them while they wait for a release.
[SECURITY.md](../SECURITY.md) sets how soon that is.

The Controller reads `security.json` from every release: a node running
an older release - or the Controller itself - gets a 🔒 security update
badge, rated by the worst vulnerability it misses; a fix to an extension
(its update names it) only counts for nodes that have that extension
([controller-ui.md](controller-ui.md)).

GitHub's "Security alerts" watch option only reaches the repository's
maintainers (Dependabot, code and secret scanning alerts), and GitHub
notifies nobody when an advisory is published: people who want to know
watch **Releases** - the release's name says it's a security update - and
operators see it in their Controller.

## Adding an upstream

1. Pin it in `versions.mk`: the version, the sha256 of each download, and
   how it was trusted (whose signature, which independent checksum).
2. Add its component to `hack/upstream/components.go` - feed, track,
   checks, vulnerability sources - and its row above. `go test
   ./hack/upstream` fails until `versions.mk` and this page agree.
3. Its signing key, if any, in `hack/upstream/keys/`.
