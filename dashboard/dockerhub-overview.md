<div align="center">
  <img src="https://raw.githubusercontent.com/swenske/Janus/main/brand/logo/janus-logo-mono-adaptive.svg" alt="Janus" height="80" />
</div>

[![Latest release](https://img.shields.io/github/v/release/swenske/Janus?sort=date&display_name=tag&label=release)](https://github.com/swenske/Janus/releases/latest)
[![Status: alpha](https://img.shields.io/badge/status-alpha-orange)](https://github.com/swenske/Janus/blob/main/docs/architecture.md#roadmap)
[![License: MIT](https://img.shields.io/github/license/swenske/Janus)](https://github.com/swenske/Janus/blob/main/LICENSE)
[![Docker pulls](https://img.shields.io/docker/pulls/swenske/janus-controller?logo=docker&logoColor=white&label=pulls)](https://hub.docker.com/r/swenske/janus-controller)
[![Image size](https://img.shields.io/docker/image-size/swenske/janus-controller/latest?logo=docker&logoColor=white&label=image%20size)](https://hub.docker.com/r/swenske/janus-controller/tags)
[![Website](https://img.shields.io/badge/website-janus.sw--servers.net-2f6feb?logo=googlechrome&logoColor=white)](https://janus.sw-servers.net/)
[![Documentation](https://img.shields.io/badge/docs-janus.sw--servers.net%2Fdocs-2f6feb?logo=readthedocs&logoColor=white)](https://janus.sw-servers.net/docs/)
[![Source on GitHub](https://img.shields.io/badge/GitHub-swenske%2FJanus-181717?logo=github&logoColor=white)](https://github.com/swenske/Janus)

# Janus Controller

The web UI for a fleet of [Janus](https://janus.sw-servers.net/) nodes.
Janus is an immutable, API-driven Linux distribution built around
HAProxy: no SSH, no shell, no package manager on a node - everything
goes through its gRPC API, over mutual TLS. The Controller is where you
run them from:

- **Nodes** - each node's live status (online, version and available
  update, HAProxy health, uptime), its labels, self-registration
  approvals, enrollment tokens, and what to provision a new node with.
- **A page per node** - live charts, logs (HAProxy, janusd, events,
  kernel), processes, network, storage; HAProxy's configuration editor
  (validate, diff, apply seamlessly), backends, maps, ACLs and
  certificates; BGP, VRRP and the firewall when the node's image has
  them; packet capture, a read-only file browser, A/B updates with
  automatic revert, reboot, shutdown, reset.
- **Accounts** - reader, operator or admin, narrowed to nodes by their
  labels if need be; a second factor (an authenticator app or a
  passkey); an audit of every change; API tokens for scripts.
- **The fleet** - the Controller's own certificate authority, which
  every node trusts: it reaches a node with a day-long certificate,
  acting for the account that opened the node's page - the node checks
  that account's role itself, and logs who acted.
- **Machines** - creates, resizes and destroys node VMs on libvirt/KVM
  or Proxmox VE hosts; the
  [Terraform provider](https://janus.sw-servers.net/docs/terraform.md)
  does the same as code.
- **janusctl** - the CLI signs in to the Controller (an SSH key, the
  browser or a token) for short-lived certificates to the nodes it may
  reach.
- **Backups** - encrypted and signed, to an S3 bucket or downloaded.
- **Its own updates** - one click from its page, rolled back by itself
  if the new version doesn't start.

![The Controller's node list: three nodes online, each with its labels, version, HAProxy health, uptime and fleet trust, then the fleet's state](https://raw.githubusercontent.com/swenske/Janus/main/docs/assets/screenshots/controller-nodes-light.webp)

The complete guide - accounts, the fleet, hypervisors, backups,
updates: [The Controller](https://janus.sw-servers.net/docs/dashboard/README.md).

## How to run

**Run the container with `--network host`.** It listens on two ports -
`:8080`, the UI (HTTPS only) with every node's page under it, and
`:8443`, where nodes register - and sees the host's own addresses for
its TLS identity that way.

```sh
docker run -d \
  --name janus-controller \
  --network host \
  -v janus-controller-data:/data \
  swenske/janus-controller
```

Or with Compose - recommended: with the second service below, the
Controller updates itself from its page when a new release is out,
after you confirm, and rolls back by itself if the new version doesn't
start. Replace `/opt/janus-controller` with the directory holding this
file, on both sides of its line:

```yaml
# /opt/janus-controller/compose.yaml
services:
  janus-controller:
    # The image comes from .env: the updater sets JANUS_CONTROLLER_IMAGE
    # there to the release it installs, pinned to its digest. Without
    # the variable, :latest.
    image: ${JANUS_CONTROLLER_IMAGE:-swenske/janus-controller:latest}
    container_name: janus-controller
    network_mode: host
    restart: unless-stopped
    # The Controller runs as user 65532 (the image's USER) and writes
    # only under /data (and the updater's socket directory): the rest of
    # its image stays read-only, it keeps no capability, and nothing it
    # starts gains any. Its ports are above 1024: a `-addr :443` needs
    # the host's `net.ipv4.ip_unprivileged_port_start=443` (the container
    # shares the host's network, so the host's setting) - a capability
    # wouldn't reach an unprivileged user.
    read_only: true
    cap_drop: [ALL]
    security_opt: [no-new-privileges:true]
    environment:
      # The master key sealing the fleet's issuing key: outside the data
      # volume, so a copy of the data (its backups) holds nothing usable.
      JANUS_CONTROLLER_MASTER_KEY_FILE: /secrets/master.key
    volumes:
      - janus-controller-data:/data
      - janus-controller-secrets:/secrets
      # Where the updater listens: the Controller asks it for an update
      # here, and tells it once started (no word from a new version
      # within 2 minutes, and the updater rolls back).
      - janus-controller-updater:/run/janus-updater

  janus-controller-updater:
    image: ${JANUS_CONTROLLER_IMAGE:-swenske/janus-controller:latest}
    container_name: janus-controller-updater
    entrypoint: ["/janus-controller-updater"]
    # Talks to the Docker daemon over its socket only: no network.
    network_mode: none
    restart: unless-stopped
    # Root, unlike the Controller: the Docker socket is root on the host
    # already (by design, it recreates the Controller's container), and
    # it gives the data to the Controller's user at an update. It at
    # least keeps no capability of its own and nothing it starts gains
    # any.
    user: "0:0"
    cap_drop: [ALL]
    security_opt: [no-new-privileges:true]
    volumes:
      # To pull the new image and recreate the Controller's container.
      - /var/run/docker.sock:/var/run/docker.sock
      # This directory, at the SAME path: the updater runs docker compose
      # on this compose.yaml and rewrites .env next to it.
      - /opt/janus-controller:/opt/janus-controller
      # The Controller's data, to back it up before an update and put it
      # back if the update is rolled back.
      - janus-controller-data:/data
      - janus-controller-updater:/run/janus-updater
      # Its own state: the last update and the backup it made.
      - janus-controller-updater-state:/var/lib/janus-updater

volumes:
  janus-controller-data:
  janus-controller-secrets:
  janus-controller-updater:
  janus-controller-updater-state:
```

Then `docker compose up -d` in that directory. The updater is the only
container with the Docker socket, and has no network: it sets
`JANUS_CONTROLLER_IMAGE` in the `.env` next to `compose.yaml` to the
release's image, runs `docker compose up -d janus-controller`, and puts
everything back if the new version doesn't come up. What each line is
for, and what to do if an update fails:
[Updating the Controller](https://janus.sw-servers.net/docs/dashboard/README.md#updating-the-controller).

The data volume is required, not optional: it holds the accounts, the
nodes, the fleet and the Controller's TLS identity.

Then open **`https://<host>:8080/`** - a self-signed certificate is
generated on first run, so your browser asks you to trust it once -
and make the first account, an admin, on the first visit.

## Who it runs as

The image runs as user **65532**, not root:

- **The data must be that user's.** A new named volume is, by itself,
  and the updater gives it the data at an update. Updating into
  v2026.10.07-3 from before (root's data), or on a bind-mounted
  directory, needs a `chown -R 65532:65532` by hand first, with the
  Controller stopped:
  [Who the Controller runs as](https://janus.sw-servers.net/docs/dashboard/README.md#who-the-controller-runs-as).
- **A port below 1024** (`JANUS_CONTROLLER_ADDR: ":443"`) needs
  `sysctl -w net.ipv4.ip_unprivileged_port_start=443` on the host - the
  container shares its network. Or stay on `:8080` behind your own
  HAProxy.
- **A mounted certificate** must be readable by 65532.

## Settings

Environment variables, all optional - handy in a Compose
`environment:` block:

- `JANUS_CONTROLLER_ADDR` - the UI's port, e.g. `:443` (see above).
- `JANUS_CONTROLLER_ADVERTISE_ADDRESS` - a hostname or address (or a
  comma-separated list) to add to the self-signed certificate, e.g. the
  DNS name you'll reach the Controller by. Read when the identity is
  first generated.
- `JANUS_CONTROLLER_MASTER_KEY_FILE` - the master key sealing the
  fleet's issuing key (default `/data/master.key`): keep it outside the
  data volume, as the Compose file above does.
- `JANUS_CONTROLLER_TLS_CERT`, `JANUS_CONTROLLER_TLS_KEY` - the page's
  own certificate (then its chain) and key, as files, read again within
  30 seconds of a change (certbot...). An admin can also upload one on
  the page itself:
  [HTTPS certificate](https://janus.sw-servers.net/docs/dashboard/README.md#https-certificate).

```sh
docker run -d \
  --name janus-controller \
  --network host \
  -v janus-controller-data:/data \
  -v /path/to/certs:/certs:ro \
  -e JANUS_CONTROLLER_TLS_CERT=/certs/fullchain.pem \
  -e JANUS_CONTROLLER_TLS_KEY=/certs/privkey.pem \
  swenske/janus-controller
```

## Tags

- `latest` - the most recent build from `main`.
- `<git-sha>` - a specific commit, for pinning.
- `<release-version>` (e.g. `v2026.10.07-3`) - matches a real, tagged
  [GitHub Release](https://github.com/swenske/Janus/releases) - only
  pushed for a run that actually cuts one, so this tag may lag behind
  `latest` between releases; the release's `controller-image.txt` names
  it with its digest. This is what the Controller's one-click update
  installs. ⚠️ Alpha software - see the release notes themselves for
  the same warning.
