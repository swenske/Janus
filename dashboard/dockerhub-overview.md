<div align="center">
  <img src="https://raw.githubusercontent.com/swenske/Janus/main/brand/logo/janus-logo-mono-adaptive.svg" alt="Janus" height="80" />
</div>

# Janus Controller

A web UI for managing one or more [Janus](https://github.com/swenske/Janus)
nodes: a node list with live status (online, version, update available,
HAProxy health), self-registration approvals, and a full page per node -
live charts, service and kernel logs, events, processes, network,
storage, HAProxy configuration/backends/maps/ACLs/certificates, packet
capture, a file browser, A/B updates, client certificates, and
reboot/shutdown/reset. Light and dark themes.

Your browser authenticates to *this dashboard* per node using a TLS
client certificate issued by that node's own PKI (never uploaded -
selected from what your browser already has installed), while the
dashboard itself talks to the real node using a separate service
credential it generates for itself once, when you add the node - your
own credential is never written to disk.

See the [full README](https://github.com/swenske/Janus/blob/main/dashboard/README.md)
for the complete architecture, self-registration flow, and node-side
setup.

## How to run

**The container must run with `--network host`.** It listens on three
ports, one of which (the per-node listener pool) is a dynamic range -
host networking makes every one of them directly reachable at the
host's own address, with no port mapping and no extra configuration.

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
    image: ${JANUS_CONTROLLER_IMAGE:-swenske/janus-controller:latest}
    container_name: janus-controller
    network_mode: host
    restart: unless-stopped
    volumes:
      - janus-controller-data:/data
      - janus-controller-updater:/run/janus-updater

  # Optional: one-click updates of the Controller from its page.
  janus-controller-updater:
    image: ${JANUS_CONTROLLER_IMAGE:-swenske/janus-controller:latest}
    container_name: janus-controller-updater
    entrypoint: ["/janus-controller-updater"]
    network_mode: none
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - /opt/janus-controller:/opt/janus-controller   # this directory, same path
      - janus-controller-data:/data
      - janus-controller-updater:/run/janus-updater
      - janus-controller-updater-state:/var/lib/janus-updater

volumes:
  janus-controller-data:
  janus-controller-updater:
  janus-controller-updater-state:
```

The updater is the only container with the Docker socket, and has no
network: it sets `JANUS_CONTROLLER_IMAGE` in the `.env` next to
`compose.yaml` to the release's image (pinned to its digest), runs
`docker compose up -d janus-controller`, and puts everything back if
the new version doesn't come up. What each line is for, and what to do
if an update fails:
[Updating the Controller](https://github.com/swenske/Janus/blob/main/dashboard/README.md#updating-the-controller).

`-v .../data` is required, not optional: it's where the node registry
and the dashboard's own TLS identity persist across restarts.

Then open **`https://<host>:8080/`** (HTTPS only - a self-signed
certificate is generated on first run, so your browser will ask you to
click through a trust warning once, the same as it would for any
self-hosted admin tool) and set the admin password on first visit.

Two environment variables tune this without overriding the container's
command - handy for a Compose `environment:` block:

- `JANUS_CONTROLLER_ADDR` - the main port, e.g. `:443` to use the
  standard HTTPS port instead of `:8080`.
- `JANUS_CONTROLLER_ADVERTISE_ADDRESS` - a hostname (or comma-separated
  list) to add to the TLS certificate's SAN list, e.g. a real DNS name
  you'll reach this Controller through.

To use your own certificate instead of the auto-generated one, mount it
in and pass `-tls-cert`/`-tls-key`:

```sh
docker run -d \
  --name janus-controller \
  --network host \
  -v janus-controller-data:/data \
  -v /path/to/certs:/certs:ro \
  swenske/janus-controller -tls-cert /certs/fullchain.pem -tls-key /certs/privkey.pem
```

## Tags

- `latest` - the most recent build from `main`.
- `<git-sha>` - a specific commit, for pinning.
- `<release-version>` (e.g. `v2026.10.01-3`) - matches a real, tagged
  [GitHub Release](https://github.com/swenske/Janus/releases) - only
  pushed for a run that actually cuts one, so this tag may lag behind
  `latest` between releases; the release's `controller-image.txt` names
  it with its digest. This is what the Controller's one-click update
  installs. ⚠️ Alpha software - see the release notes themselves for
  the same warning.
