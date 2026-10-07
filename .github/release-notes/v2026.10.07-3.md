## ✨ Highlights

- 🔒 **Security release**: the Controller's registration port is bounded (JANUS-2026-004) and a reader no longer gets a node's BGP and VRRP configurations, session passwords included (JANUS-2026-005) - see below.
- 🧱 **A node checks a HAProxy configuration's global section** before `haproxy -c`: the privilege drop (`chroot /var/empty`, `uid 1000`, `gid 1000`) and janusd's `stats socket` are required, a second stats socket, `daemon`, `master-worker`, `external-check`, `insecure-fork-wanted`, `set-dumpable`, the `*setenv` keywords and `program` sections are refused - with the line that's wrong. See [HAProxy configuration](https://janus.sw-servers.net/docs/haproxy-config.md#the-global-section).
- 🧑 **The Controller runs as user 65532**, no longer root: with the host's network, root in the container held the host's network. **Read the upgrade notes**: an installation from before has root's data, and a port below 1024 needs one host setting.
- 🛡️ **janusd keeps nine capabilities** instead of all of them (SELinux): the list every node feature was seen to use.

## 🔒 Security

- **JANUS-2026-004** (high): anyone reaching a Controller's registration port (`:8443`) could fill its disk and its approval queue - nothing authenticates an announcement, and nothing bounded them. An address may now announce 10 nodes at once, then one every 6 s (429, the node retries by itself); 200 announcements may wait for approval, further ones are refused (503) until some are approved or rejected; an announcement is 256 KiB at most. A node admitted on a token never waits in the queue.
- **JANUS-2026-005** (medium): `BGPGetConfig` and `VRRPGetConfig` answered `os:reader`, with the session passwords those files hold (`password`, `auth_pass`). Both take `os:operator`; the Controller's pages show a reader the state, not the configuration. If a reader credential given to someone untrusted may have read one, change the passwords in it and on the peers.
- **The Controller's answers carry security headers** (`X-Content-Type-Options`, `X-Frame-Options: DENY`, `Referrer-Policy`, HSTS, a Content-Security-Policy in report-only mode), both servers have header and idle timeouts, relayed JSON bodies are bounded (4 MiB).
- **CI**: the workflows' token reads only; the Claude workflows answer collaborators only; the apt publication pins the server's host key.
- **The exporter's listen address**: `janusctl system metrics -address IP` (or `'*'`), the page's Address field - on a node with a management address, the metrics stay off the networks HAProxy serves. The default stays every address.

## 🛡️ Hardening

- **janusd's capabilities** (SELinux `janusd_t`): `net_admin`, `net_raw`, `sys_admin`, `sys_boot`, `sys_time`, `sys_ptrace`, `kill`, `dac_read_search` and `syslog` - measured on every enforcing test (install, upgrade, rollback, revert, capture, sysctl, network, firewall, VRRP, BGP, Consul, ACME, extensions, fleet, registration, reset). A capability denial would show in `janusctl system dmesg` as `avc: denied { ... } ... tclass=capability` with `comm="janusd"`: report it - a rollback to the previous release is the usual A/B one.
- **The Controller's Compose example**: read-only image, no capability, `no-new-privileges`, the updater as root (`user: "0:0"`) with the Docker socket it needs.

## 🔧 Fixes

- The Controller's trust loop no longer leaves a node in error for ten minutes when its connection to it died with a reboot (the node updated a while ago, nothing used the connection since): the RPC runs once more on the connection gRPC opens again.
- `janusctl system sysctl history` prints a trial's details in local time, like the rest.
- `TestSSHLogin` could fail by chance on a fingerprint holding `//`.

## ⚠️ Upgrade notes

**The Controller runs as user 65532 from this release.** What to do depends on how it's installed:

- **With the updater** (the `compose.yaml` of the README): nothing for the data - the updater gives `/data` to the new user as a step of the update. Add `user: "0:0"` to the `janus-controller-updater` service in your `compose.yaml` **before** updating (the example has it); without it, the updater of the next release can't reach the Docker socket.
- **By hand** (`docker compose up -d`, `docker run`): give the data to the user once, with the Controller stopped:

  ```sh
  docker run --rm -v janus-controller-data:/data busybox chown -R 65532:65532 /data
  ```

  (With Compose the volume is `<project>_janus-controller-data`.) Started on data it can't write, the Controller stops at once and prints this command.
- **A port below 1024** (`-addr :443`, `JANUS_CONTROLLER_ADDR: ":443"`): the container shares the host's network, so the host must allow it - `sysctl -w net.ipv4.ip_unprivileged_port_start=443`, kept in `/etc/sysctl.d/`. A capability doesn't reach an unprivileged user. Or stay on `:8080` behind your own HAProxy.
- **The master key file** (`JANUS_CONTROLLER_MASTER_KEY_FILE`) and mounted certificates (`-tls-cert`/`-tls-key`) must be readable by 65532.
- **Restore from a backup** (`dashboardd restore` in `docker run`): the backup files must be readable by 65532, or add `--user 0:0`.

See [Who the Controller runs as](https://github.com/swenske/Janus/blob/main/dashboard/README.md#who-the-controller-runs-as).

**Nodes**: a HAProxy configuration without `uid 1000`, `gid 1000`, `chroot /var/empty` or janusd's `stats socket` line is refused from this release - the configurations a node boots with and `examples/haproxy/web.cfg` have them; one brought from a distribution may not. Nothing else to do: the update is the usual A/B one.
