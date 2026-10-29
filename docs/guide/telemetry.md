# Usage report

The Controller can send, once a day, a short report of what its fleet is
made of: how many nodes and clusters, which versions, extensions,
HAProxy branches and kernel tracks, how big the nodes are and which
features are in use. It is **off** until an admin turns it on - on the
setup screen, or in **Settings › Usage report** - and changes nothing
about how the Controller works.

## Why

Janus is built by a small team. The report says where to spend it:

- **what to work on next** - which versions nodes stay on, which
  features and platforms are used;
- **what could be retired** - an extension, a HAProxy branch or a kernel
  track nobody runs is a cost with no one to pay for it;
- **which images to pre-build first** - when a release comes out, the
  image factory builds the update of every schematic in use; the most
  run go first, and the factory's mirror (see
  [Images and extensions](../image-factory.md)) holds what was built.

## What it holds

Counts, and nothing that names anything: never a node's or a cluster's
name, an address, a label, a hostname, a certificate, a configuration or
an account. The receiver keeps no IP address either - its front end logs
nothing for it.

```json title="examples/telemetry/report.json"
{
  "schema": 1,
  "install_id": "4f9c1e7a2b6d48e0a3c5f17d9e2b6a81",
  "controller": {
    "version": "v2026.10.29-3",
    "arch": "amd64",
    "prerelease": false
  },
  "nodes": {
    "total": 7,
    "unreachable": 1,
    "versions": {
      "v2026.10.29": 2,
      "v2026.10.29-3": 4
    },
    "arches": {
      "amd64": 6
    },
    "haproxy": {
      "3.2": 1,
      "3.4": 5
    },
    "kernel": {
      "stable": 6
    },
    "console": {
      "serial": 6
    },
    "extensions": {
      "bird": 3,
      "letsencrypt": 5,
      "prometheus-node-exporter": 6,
      "qemu-guest-agent": 6
    },
    "schematics": {
      "5a8d6944047f00079ae8cd55b239a47e1561a108f665fcad553bfe6acdad1d03": 3,
      "c2b5e8a41f0d7c39e6a1b4f8d2e7c05a9b3f6d1e8a4c7b2f5e0d9a3c6b1f4e72": 3
    },
    "vcpus": {
      "1": 2,
      "2": 4
    },
    "memory": {
      "1-2GiB": 4,
      "<1GiB": 2
    }
  },
  "clusters": {
    "count": 2,
    "sizes": [
      2,
      3
    ]
  },
  "platforms": {
    "machines": {
      "libvirt": 2,
      "proxmox": 4
    },
    "added": 1
  },
  "features": {
    "sso": true,
    "mfa_required": true,
    "autoscaling": false,
    "cluster_bgp": true,
    "cluster_vrrp": false,
    "log_forwarding": true,
    "lets_encrypt": true,
    "s3_backups": true,
    "fleet": true,
    "consul": false
  }
}
```

| Field | What |
|---|---|
| `schema` | the report's format, 1 |
| `install_id` | 128 random bits naming this Controller's reports - nothing else: not the Controller's own ID, which tags its machines. **New ID** in Settings makes another, and asks the receiver to delete what it holds under the old one |
| `controller` | the Controller's version, architecture, and whether it is a pre-release |
| `nodes.total`, `nodes.unreachable` | how many nodes the Controller manages, and how many didn't answer within 4 seconds (counted nowhere else) |
| `nodes.versions`, `arches`, `haproxy`, `kernel`, `console`, `extensions` | how many nodes run each Janus version, architecture, HAProxy branch and kernel track (as their image was built), console, extension |
| `nodes.schematics` | how many nodes run each [schematic](../image-factory.md#the-schematic): its ID is the SHA-256 of the choices above - the image factory pre-builds the most run first |
| `nodes.vcpus`, `nodes.memory` | the nodes' size, by bucket: 1, 2 (2-3), 4 (4-7) or 8+ vCPUs; <1, 1-2, 2-4 or 4+ GiB |
| `clusters` | how many clusters, and how many members each has |
| `platforms.machines` | the machines the Controller created, by kind of hypervisor; `platforms.added`, the nodes added to it otherwise |
| `features` | on or off, nothing of how: single sign-on, a second factor required, auto-scaling, a cluster managing BGP or VRRP, log forwarding, Let's Encrypt through the Controller, backups to S3, the fleet PKI, Consul |

**Exactly what is sent** in the Settings card shows the report as it
would leave now.

## When and where

- Within an hour of turning it on (a moment the ID picks: Controllers
  don't all report at once), then every 24 hours; a report that fails
  is tried again an hour later, then the next day. **Send now** sends one
  at once.
- `POST https://janus-telemetry.sw-servers.net/v1/report`, over HTTPS,
  through the proxy the Controller's environment names (`HTTPS_PROXY`),
  never following a redirect.
- Each send is in the Controller's log (`telemetry: report sent`), each
  change of the setting in its audit.

## Turning it off for good

`JANUS_CONTROLLER_TELEMETRY=off` (`dashboardd -telemetry off`) keeps it
off whatever the setting says, and the page can't turn it on - for a
Controller set up from code. `JANUS_CONTROLLER_TELEMETRY_URL`
(`-telemetry-url`) sends it elsewhere.
