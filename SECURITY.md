# Security policy

## Supported versions

Janus is alpha software. Security fixes go to:

- **the latest release**, always;
- **the previous release, for 14 days** after the next one ships - the
  time to try the new release before moving to it. A vulnerability
  fixed in those days gets a **maintenance release** of the previous
  release too: that release with the fix, nothing else of the new one
  ([maintenance releases](docs/contributing/releasing.md#maintenance-releases)).

After those 14 days, only the latest release is. A node moves to it
with its own A/B update (from the Controller, or `janusctl lifecycle
upgrade`), signature checked and rolled back if it doesn't come up
healthy.

**Release pace**: a release is first published as a pre-release and
tested on the lab, then promoted - **at most once a week**. A security
fix doesn't wait for the week: it ships within the targets below.

**Pre-releases** - marked so on GitHub, published to be tested before
they become a release - aren't supported: they are never offered as an
update, may be withdrawn, and a vulnerability is fixed in the next
release, not in them. Once tested, a pre-release is promoted unchanged:
then it is the latest release.

## Reporting a vulnerability

Please report it privately, through GitHub: **Security** tab → **Report a
vulnerability** ([new report](https://github.com/swenske/Janus/security/advisories/new)) -
never in a public issue. Say what is affected (a node's API, HAProxy's
configuration handling, the Controller, an image...), the release, and how
to reproduce it.

You get an answer within 3 days. The fix ships in a release - and in a
maintenance release of the previous one while it is supported:

| Severity | Target |
|---|---|
| Critical | 72 hours |
| High | 7 days |
| Medium, low | the next release |

A vulnerability in a component Janus ships but doesn't develop (the
kernel, HAProxy, an extension's daemon...) is fixed by updating it as soon
as upstream has a fix; please report it upstream too.

## What a Janus image contains

Every upstream component is pinned: versions and checksums in
[`versions.mk`](versions.mk), each download checked against its sha256 -
pinned after checking upstream's signature where upstream signs its
releases. Go modules, the Controller's npm packages, base images and CI
actions are kept up to date by Dependabot.

## Being told about security releases

A release that fixes vulnerabilities:

- is named "Janus vX (Alpha) - 🔒 security update", and says what it fixes
  in a 🔒 section of its notes;
- carries `security.json` (what it fixes, machine-readable) and
  `sbom.cdx.json` (what it is made of, CycloneDX);
- shows up in the Controller: nodes running an older release get a 🔒
  security update badge, rated by the worst vulnerability they miss, and
  so does the Controller itself;
- gets a GitHub security advisory (Security tab) for each vulnerability
  of Janus's own code it fixes, whatever its severity, and one for the
  components it updates when what they fix is rated high or critical.

A maintenance release is named "Janus vX (Alpha) - maintenance of vY",
never marked the latest; the advisories of its fixes - the latest
release's - list it among their patched versions.

To be notified, watch the repository: **Watch** → **Custom** →
**Releases** (or follow the releases' Atom feed). GitHub's "Security
alerts" watch option only ever reaches the repository's maintainers, and
GitHub notifies nobody of a published advisory.

How upstream releases are followed, checked and assessed:
[docs/upstreams.md](docs/upstreams.md).
