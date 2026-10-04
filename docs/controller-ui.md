# Janus Controller UI - design notes

How the Controller's web UI is built and the principles to keep when
changing it. Usage is in [`dashboard/README.md`](../dashboard/README.md);
the API it drives is in [`api-routes.md`](api-routes.md).

## Two pages, one design system

| Page | Served on | Source | Built into |
|---|---|---|---|
| Node list (login, approvals, node cards, provisioning) | the main port | `dashboard/frontend/index.html`, `src/App.jsx` | `dashboard/backend/static` |
| One node's page | that node's own mTLS listener (`9500`-`9599`) | `dashboard/frontend/node/index.html`, `src/node/` | `dashboard/backend/internal/nodeproxy/static` |

They are separate origins on purpose: the browser negotiates the node's
client certificate per origin, so a node's page can't be a view inside
the main page (see `nodeproxy.go`'s package doc). Only an `os:admin`
certificate opens a node's page (`requireAdminCertificate`): the
Controller acts on the node with its admin service credential whatever
the browser presents, so a reader certificate gets a 403 explaining it. They share everything
else from `src/shared/`:

- `theme.css` - design tokens (`--bg`, `--surface`, `--text`, `--muted`,
  `--accent`, `--ok`/`--warn`/`--danger`/`--info` and their `-soft`
  backgrounds, `--chart-1..5`) and the base components (cards, buttons,
  inputs, badges, tables, notices, meters). Colors come from the logo:
  warm off-white and near-black, and the brand orange (`#C2502A`,
  `#D8643C` in dark mode).
- `theme.jsx` - `ThemeToggle` (system → light → dark, remembered per
  browser) and `Logo`.
- `ui.jsx` - `Card`, `Stat`, `Badge` (+ `stateTone`), `Tabs`,
  `PageHeader`, `Meter`, `Loading`/`ErrorBox`/`Empty`, toasts
  (`useToast`), the confirmation dialog (`useConfirm`) and `useAction`
  (busy state + success/error toast around an async call).
- `route.js` - hash routing (`useHashRoute`, `navigate`); `sse.js` -
  `useSSE`. Both pages use them (the node page re-exports them from
  `src/node/hooks.jsx`).

Rules:

- **Use the tokens, never raw colors.** Every color must work in both
  themes; dark mode is not an afterthought. Check new screens in both.
- **Reuse the shared primitives** before writing new ones. Something
  both pages could use goes in `src/shared/`.
- Icons come from `lucide-react` (tree-shaken, only what's imported ships).
- No UI or chart library beyond that: the chart is our own SVG component
  (`src/node/components/Chart.jsx`) - small, and it does what we need.

## The main page

Three tabs (hash routes `#/`, `#/hypervisors`, `#/tokens`).

**Nodes**: pending approvals first, then the nodes as cards, then
provisioning. A node the Controller created on a hypervisor shows its
virtual machine on its card (state, reset/force-off/start, console) and
**Destroy** instead of Remove; a pending registration from a machine it
created (an image too old to present its registration token) says so,
and approving it links the node to the machine.

**Hypervisors** ([hypervisors.md](hypervisors.md)): one card per host -
libvirt or Proxmox VE (the form's **Kind**, fixed once added; a Proxmox
token's secret is write-only: `has_token_secret`) - added in steps (for
libvirt the command that authorizes the Controller's key; then the SSH
host key or the API's certificate read and compared with the host's
own, then trusted - a Proxmox node given its CA needs none), then its
software, CPU, memory, pool and networks - and below, the machines: their
phase while they're created (the page polls every 3 s while one is under
way), their virtual machine's state, history, retry, destroy (type the
name) and the console. A machine waiting for its node shows the
`warning` the Controller read on the node's console (why it can't
register, with a hint). **Show host preparation** (`HostPrep`) - in the
add/edit form once its required fields are filled in, and in a modal
from a card not trusted yet, key included - shows the steps the backend
writes (`POST /api/hypervisors/preparation`, again 250 ms after each
change), each with Copy, plus Copy all and Download; the edit form warns
when a change to the account, pool, networks or prefix means preparing
the host again. **Create node** takes the hypervisor, name, size,
interfaces (network, name, static/DHCP/none), DNS/NTP, the version and the
image factory's extensions, or an image by URL and SHA-256. The console
(`Console.jsx`) is read-only, follows the SSE stream from
`/api/machines/{id}/console`, keeps the terminal's colors and attributes
(SGR - `ansi.js`, unit-tested with `npm test`; a terminal's dark
background in both themes, so the node's banner shows in its colors),
and cleans every other terminal sequence out of the
whole text - an escape sequence can be cut across two messages.

A machine's card shows:
- what manages it as code (`ManagedBadge`), and its lock (`LockNotice`,
  with **Release**, confirmed);
- when its record was last read from its node and hypervisor;
- the node's hostname when its page changed it.

**Edit** (`EditMachine`) is the hardware only: vCPUs, memory, interfaces
added, removed or moved. An existing interface's addresses are shown
read-only - they're the node's, changed on its page - and editable only
once the interface is added or moved. A hardware change asks first
(restart).

A locked machine shows neither Edit nor Destroy. Its node's page shows a
banner (`/api/node`'s `locked_by`), and refuses network changes and
updates with 423.

**API tokens** (`Tokens.jsx`): create one (name, validity) - shown once,
with a copy button - see each one's last use, revoke. Only the admin's
session reaches it, never a token.

On the Nodes tab, above the rest, `ControllerUpdate.jsx` is the
Controller's own update
(`GET`/`POST /api/controller/update`, `dashboard/backend/selfupdate.go`):
a newer release than this Controller's `main.version`
(`updaterapi.Newer` - CalVer, a git-describe build counts as after its
release), with **Update to vX** when janus-controller-updater is ready,
or what keeps it from being ready and the manual `.env` line otherwise;
during an update it polls `/api/auth/status` every 2 s and reloads into
sign-in once the new Controller answers (sessions are in memory); after
it, how the update ended with the updater's log, dismissable per
browser. The updater itself, its contract with the Controller
(`dashboard/updater/updaterapi`) and the Compose setup:
`dashboard/README.md`, "Updating the Controller".

**Security updates.** A release's `security.json` asset lists what it
fixes, per target ([upstreams.md](upstreams.md)). `nodeproxy`'s release
fetch reads it for every release (once each: a release doesn't change),
and `ReleaseInfo.SecurityUpdate(version, target, extensions)` is the
worst severity the releases after a version fix - an extension's fix
only for a node that has it - `security_update` in `/api/nodes/status`,
a node's `/api/update-check` (with `security_release`) and
`/api/controller/update`; empty for a development build. Where an update
shows, a security update shows instead: `SecurityBadge.jsx` (a shield,
`danger` for critical/high, `warn` below; `severity.js` for the tone and
the sentence), on the node card, the header's count, the node page's top
bar, its Update view (with a notice linking the release's notes), the
Controller's update card, and the node page's Update dot.

## The node page

Hash routing (`#/haproxy/config`, `#/tools/files?path=/etc`), a sidebar
grouped by intent, and a sticky top bar with the node's identity, health
badges, version (and available update), the refresh interval and the
theme toggle:

- **Monitoring** - overview, metrics, processes, network, storage
- **Logs** - service logs, events, kernel (dmesg)
- **Apps** - HAProxy (with its own tabs) and the Janus exporter (built
  into janusd: its settings and a scrape example), then only what the
  node's image has: the extensions' apps (Node exporter - status and
  settings; QEMU guest agent - status, what the hypervisor may do) and
  the modules (bird, keepalived, nftables) - HAProxy's tabs include
  Files (`/etc/haproxy/files`: error pages, maps, certificates; edit,
  upload, remove, each refused by the node when haproxy.cfg wouldn't
  load with it; files holding a private key are never shown) - Let's Encrypt (account, the
  HTTP-01 rule, certificates and DNS providers as forms - secrets never
  shown, kept when left empty - and the configuration as JSON) and Consul
  (agent, members, the configuration's files, the configuration in the
  shared ModuleConfigEditor). An app the node doesn't have
  isn't listed - "Add or remove apps…", last, opens the Update page's
  extensions panel.
  Firewall: ruleset editor (check, diff, apply on trial - the Controller
  confirms over a *fresh* connection, never the shared one, which
  conntrack keeps open whatever the ruleset), live sets, live ruleset.
  VRRP (and BGP): instances/sessions, then the daemon's configuration in
  the shared ModuleConfigEditor (check by the daemon itself, diff, apply,
  remove)
- **Tools** - packet capture, files
- **System** - network (hostname, interfaces, VLANs, DNS, NTP), services,
  update, access (client certificates), power

The sidebar shows the Controller's version under its name, as the main
page does under its title (`dashboardd`'s `main.version`, stamped by the
Makefile and the Docker image's `VERSION` build argument).

Updates follow the node's image schematic (`GET /api/update-check`,
`nodeproxy/update.go`): a node with the default schematic gets the
newest GitHub release; a node with extensions gets the newest release
the image factory has built from its schematic (`dashboardd
-image-factory`, janus.sw-servers.net by default), shown as "building"
until it's ready - never a plain release, which would drop its
extensions. The Update page shows the node's schematic and extensions,
and installing a bundle from another schematic takes an explicit
checkbox (`allow_schematic_change`). **Change extensions…** opens a
panel with the factory's catalog for the newest release (`GET
/api/factory/catalog`); **Prepare the update** sends the chosen set to
`POST /api/factory/update`, which registers that schematic with the
factory and asks for its update - the same resolution as the node's
own check, so going back to no extension means GitHub - and the panel
follows a build every 20 s until it's ready. It only fills in the
installation form (URL, sha256, the schematic-change checkbox); the
confirmation lists the extensions gained and lost, and installing stays
the usual A/B update. A release that renamed one of the node's
extensions (the catalog's `replaces`) is offered as a rename: the update
check reports `renamed` and the migrated schematic, "Use this update"
fills the form in as a schematic change, and the confirmation says
"Renamed" rather than added and removed.

The bundle reaches the node one of three ways, the choice remembered
per browser: **the node downloads it** (`upgrade-url`), **the Controller
pushes it** (`upgrade-relay`: dashboardd downloads the four files and
streams each into `UploadReleaseFile` as it arrives - sha256 checked on
the way - then calls `Upgrade` on the staged copy; progress comes back
as NDJSON and the page shows it), or **upload files** from the browser
(`upgrade-upload`). The node verifies the bundle's signature in every
case.

Shared caches (latest release, factory updates, catalog) fetch with a
context detached from the request that triggers them
(`sharedFetchContext`): a browser leaving the page must never put
"context canceled" in a cache every node's page reads.

A new feature goes into the group matching what the operator is trying
to do, not into whichever page has room. A page that grows several
distinct concerns gets tabs (as HAProxy does) rather than a longer
scroll.

### Live data

- **Polling** follows the global refresh interval (Off, 1 s … 30 s,
  remembered per browser) through `usePoll`, and pauses while the tab is
  hidden. Use a longer fixed `every` only for slow-changing data.
- **Metrics history** lives in `MetricsProvider`, app-wide, so charts
  keep their data across page changes; it holds the last 900 samples in
  the browser only. Counters are turned into rates client-side, and a
  counter going backwards (HAProxy reload) gives no point rather than a
  negative rate.
- **Charts** leave gaps where data is missing (e.g. during a reboot)
  instead of drawing a line through them, follow the data until the time
  window is full, and use binary steps for bytes.
- **Streams** (logs, events, dmesg) are Server-Sent Events via `useSSE` /
  `LogViewer`. A node-side error arrives as a `failure` event - never
  `error`, which `EventSource` uses for its own connection problems.

### Interaction rules

- **Anything that disrupts traffic or the node asks first** (`useConfirm`
  with `danger`), and says what will happen in plain words. Irreversible
  actions make the user type a word (reset: the node's hostname).
- **Show before applying**: config changes are validated on demand and
  their diff against the running config is shown in the confirmation.
- **Follow disruptive actions through** (`WaitForNode`): restart,
  reboot and update show progress until the node answers again, then
  report what came back. When the node can't come back to this
  Controller (reset regenerates its PKI), say so instead of waiting.
- **Errors are shown as the node reports them** - relays pass the gRPC
  status message and a matching HTTP status - and never as a broken
  download: a stream's first message is awaited before headers are sent.
- **Changes that can cut the node off are trials**: System › Network
  applies on trial and the Controller confirms by reaching the node
  again (recording its new address if it moved); unconfirmed, the node
  reverts by itself. The outcome stays on the page, not only in a toast.
- Every async action goes through `useAction`: disabled buttons while
  busy, a toast on success, the real error on failure.
- Explain the non-obvious once, near the control (what a soft stop does,
  why a module shows "n/a"), not in a separate help page.

## Backend (dashboard/backend/internal/nodeproxy)

- **One gRPC connection per node** (`dialNode`), shared by every handler
  and closed only when the node's listener stops. Callers must not close
  it. Its reconnect backoff is capped at 5 s so a rebooted node reappears
  quickly.
- A new API method gets a relay next to its siblings (`system.go`,
  `ops.go`, `lifecycle.go`, `pcap.go`) using `unary` / `relayData` /
  `streamLines`, so error mapping and streaming behave like the rest.
- The Controller never exposes its service credential, and the
  browser's certificate is never used to reach the node (see the package
  doc) - a new feature must not change that.

## Verifying a change

1. `make dashboard-frontend-build` (both pages) and commit the built
   assets with the source - `go build` embeds them and needs no Node.js.
2. `npx oxlint src` in `dashboard/frontend`: no errors.
3. Run it for real: a node from `make local-dev-image`, a native
   `bin/dashboardd` (`-data-dir` in a scratch directory), the node added
   through the API, then every touched page in a real browser - both
   themes, zero page/console errors, zero failed requests, and each new
   action actually performed and its effect checked on the node.
   Headless Chromium with Playwright works well for this; the per-node
   origin needs the node's client certificate:

   ```js
   const ctx = await browser.newContext({
     ignoreHTTPSErrors: true, // the Controller's self-signed identity
     colorScheme: 'dark',
     clientCertificates: [{ origin: 'https://127.0.0.1:9500', certPath: 'admin.crt', keyPath: 'admin.key' }],
   })
   ```

   Look at the screenshots, not only at the absence of errors.
4. Anything the `local-dev` container can't show (dmesg, A/B slots,
   reboot, SELinux) gets checked against a real node booted from
   `build/rootfs/disk.img` under QEMU/OVMF.
5. New relays get an assertion in `hack/qemu-dashboard-test.sh`, which
   runs against a real enforcing node in `image-build.yml`.
