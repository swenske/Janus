# Security policy

## Supported versions

Janus is alpha software: only the **latest release** gets security fixes.
A node moves to it with its own A/B update (from the Controller, or
`janusctl lifecycle upgrade`), signature checked and rolled back if it
doesn't come up healthy.

## Reporting a vulnerability

Please report it privately, through GitHub: **Security** tab → **Report a
vulnerability** ([new report](https://github.com/swenske/Janus/security/advisories/new)) -
never in a public issue. Say what is affected (a node's API, HAProxy's
configuration handling, the Controller, an image...), the release, and how
to reproduce it.

You get an answer within 3 days. The fix ships in a release:

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
- gets a GitHub security advisory (Security tab) when a fix is rated high
  or critical.

To be notified, watch the repository: **Watch** → **Custom** →
**Releases** (or follow the releases' Atom feed). GitHub's "Security
alerts" watch option only ever reaches the repository's maintainers, and
GitHub notifies nobody of a published advisory.

How upstream releases are followed, checked and assessed:
[docs/upstreams.md](docs/upstreams.md).
