# Security

What protects a Janus node by construction, the secrets a deployment
holds and who keeps them, the certificates, and what to expose. The
design behind it: [architecture](../architecture.md); reporting a
vulnerability: [security policy](../../SECURITY.md).

## A node, by construction

- **Nothing to log into.** No shell, no SSH, no package manager, no
  interpreter. Every action goes through the node's API - gRPC with
  mutual TLS on port 9505, the caller's role (`os:admin`, `os:operator`,
  `os:reader`) checked on every call, every change logged with who made
  it. The API has no "run this command" call, and its file calls are
  read-only and never serve the node's keys.
- **An immutable system.** The root filesystem is read-only, and
  dm-verity checks every block it reads against a hash the kernel
  image's command line carries: a modified byte can't be read, let
  alone run.
- **Signed updates.** A node installs only releases signed with the
  Janus release key - which signs that command line, hash included -
  into its inactive slot, and goes back by itself if the new one isn't
  healthy ([lifecycle](lifecycle.md)). Secure Boot can enforce the
  chain from the firmware on: enroll the release certificate,
  [`image/secureboot/production-cert.pem`](../../image/secureboot/production-cert.pem),
  in the firmware's `db`. The VM images' own boot entries aren't signed
  yet: a virtual machine boots with Secure Boot off.
- **SELinux, enforcing.** Every daemon runs in its own domain - HAProxy,
  which parses what the internet sends, in a narrow one - and every rule
  of the policy comes from an observed need, none guessed. A denial is
  counted (`janus_selinux_denials_total`): there should be none.
- **A hardened kernel**: the KSPP's recommendations, the hardening
  sysctls set at boot, only the drivers and protocols Janus uses.
- **Optional features stay out**: a node without BGP has no BGP daemon
  at all - extensions are chosen per image ([images](images.md)).

## The secrets of a deployment

| Secret | Where it lives | Guard it |
|---|---|---|
| A node's admin certificate and key | Printed once on its console at first boot; on its STATE partition | The way in when nothing else works: keep it offline, or let it go - the fleet's certificates replace it day to day |
| A node's certificate authority, server and Let's Encrypt keys, uploaded certificates | Its STATE partition - never served by its API | STATE isn't encrypted: whoever holds a node's disk holds them. Protect the disks and their backups like the node |
| The fleet's root key | The recovery kit, encrypted with a passphrase - offline | A password manager. Needed only to renew the fleet's keys or recover a Controller |
| The fleet's issuing key | The Controller's data, sealed with its master key | Keep the master key outside the data volume (`JANUS_CONTROLLER_MASTER_KEY_FILE`): a copy of the data alone opens nothing |
| The backups | An S3 bucket, encrypted to the backup kit's key | The kit and its passphrase in a password manager; a bucket with versioning or Object Lock, and a write-only key for the Controller |
| Accounts | The Controller: bcrypt passwords, TOTP secrets sealed with the master key, passkeys | MFA - required for admins by default |
| API tokens, enrollment tokens | Shown once; the Controller keeps their SHA-256 | A secret store; the lowest role and the narrowest scope that work; an expiry; revoke what's done |
| Hypervisor credentials | The Controller's data: its SSH key for a libvirt host, the Proxmox VE token's secret | Dedicated accounts whose rights cover only the Controller's machines - polkit on libvirt, one pool on Proxmox ([hypervisors](../hypervisors.md)) |

## Certificates

- **Each node's own**: a certificate authority made on its first boot;
  the API's server certificate, issued for its addresses and hostname,
  is reissued when they change and renewed 30 days before it expires.
  `janusctl access rotate-ca` replaces the CA itself, the old one
  cross-signing the new.
- **The fleet's**: a root kept offline, an issuing CA on the Controller
  (or one per machine that runs `janusctl fleet`, each possibly limited
  to a role), and short-lived certificates - a day for the Controller's
  calls, 12 hours for `janusctl`, an hour with an API token. A node
  trusts the fleet's root, and takes only bundles that root signed.
- **HAProxy's**: uploaded and swapped without a reload, or obtained and
  renewed by the node itself with the [Let's Encrypt](../letsencrypt.md)
  extension. `janus_certificate_expiry_timestamp_seconds` tracks every
  one HAProxy has loaded.
- **The Controller's page**: its own self-signed identity, or a
  certificate of yours - public or your organization's CA - read again
  within 30 seconds of a renewal. Nodes register against its own
  identity either way.

## What to expose

- **To clients**: HAProxy's frontends, and the virtual IP or anycast
  address - nothing else.
- **To the management network only**: the node's API (9505), its
  exporters (10056, 9100), and the Controller. The
  [firewall](../firewall.md) extension enforces it on the node itself.
- **The Controller** never holds a node's admin credential, and a node
  never sends its own anywhere: admitting a node gives it the fleet's
  trust, not the node's keys.

Every port: [networking](networking.md#ports).

## Staying patched

Every upstream - the kernel, HAProxy and its TLS library, every
extension's daemon, Go modules, base images - is pinned and followed
daily; a release that fixes a vulnerability says so in its name and
notes (`🔒`), with an advisory for a fix rated high or worse, and the
Controller marks the update on each affected node. Only the latest
release is supported: [security policy](../../SECURITY.md),
[following upstreams](../upstreams.md).
