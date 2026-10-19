# Janus Controller UI - design notes

How the Controller's web UI is built and the principles to keep when
changing it. Usage is in [`dashboard/README.md`](../dashboard/README.md);
the API it drives is in [`api-routes.md`](api-routes.md).

## Two pages, one design system

| Page | Served on | Source | Built into |
|---|---|---|---|
| Node list (login, approvals, node cards, provisioning) | the main port | `dashboard/frontend/index.html`, `src/App.jsx` | `dashboard/backend/static` |
| One node's page | the main port, under `/nodes/<id>/` (`node_pages.go`) | `dashboard/frontend/node/index.html`, `src/node/` | `dashboard/backend/internal/nodeproxy/static` |

The node page is a second app on the same origin, opened in its own tab
(`openNode`): built with relative assets (`vite.node.config.js`'s
`base: './'`), its API calls go through `apiURL` (`src/shared/base.js`:
`/api/...` under `/nodes/<id>`), with the session cookie. Its static
files are public like the main page's; its API needs a reader for a
read and an operator for a change, and the Controller relays it for the
account (`nodeproxy.WithUser`, `os:<role>`) - the node checks that role
itself, and refuses what the role doesn't allow (403 with the node's
message). A node that doesn't trust the fleet yet is reached with its
service credential: only an admin uses its page. The page shows the
account in the sidebar (`/api/me`), a banner for a reader, and a banner
when the session ended (`SIGNED_OUT` from `api.js`). Its reads are
background requests unless the user touched the page in the last
minute (`src/shared/activity.js`). They share everything
else from `src/shared/`:

- `theme.css` - design tokens (`--bg`, `--surface`, `--text`, `--muted`,
  `--accent`, `--ok`/`--warn`/`--danger`/`--info` and their `-soft`
  backgrounds, `--chart-1..5`) and the base components (cards, buttons,
  inputs, badges, tables, notices, meters). Colors come from the logo:
  warm off-white and near-black, and the brand orange (`#C2502A`).
  **The site imports it too** (`site/`): its colours are the Janus
  palette's, and a change to it, `theme.jsx` or `ui.jsx` changes the
  site's build.
- `theme.jsx` - `ThemeToggle` (system → light → dark, remembered per
  browser) and `Logo`: the site's; the Controller has its own (below).
- The Controller's own, imported by both of its pages after
  `theme.css` (never by the site): see [Appearance](#appearance) -
  `palettes.css`, `appearance.js`, `ThemePicker.jsx`, `BrandMark.jsx`,
  `controller.css` (its look over theme.css: cards, pills, figures,
  the page's glows, gauges), `Gauge.jsx`, and `chart/` - the charts
  ([Charts](#charts)).
- `ui.jsx` - `Card`, `Stat`, `Badge` (+ `stateTone`), `Tabs`,
  `PageHeader`, `Meter`, `Loading`/`ErrorBox`/`Empty`, toasts
  (`useToast`), the confirmation dialog (`useConfirm`) and `useAction`
  (busy state + success/error toast around an async call).
- `route.js` - hash routing (`useHashRoute`, `navigate`); `sse.js` -
  `useSSE`. Both pages use them (the node page re-exports them from
  `src/node/hooks.jsx`).
- `Splash.jsx` - the splash a page shows while it first loads
  (`SplashProvider`, `useSplashStep`, `useSplash`): see
  [The splash](#the-splash).

Rules:

- **Use the tokens, never raw colors.** Every color must work in every
  palette, light and dark; dark mode is not an afterthought. Check new
  screens in several (`hack/ui-preview.sh check` walks them all). A new
  token goes in every block of `palettes.css` - `npm test` holds each
  one to the same tokens and to WCAG AA. A tint (`--ok-soft`...) is
  translucent, and the test holds it over `--surface` and `--bg`: a
  status pill lays its tint over `--surface` itself (`controller.css`),
  so a strip in `--surface-2` under it changes nothing - something new
  that puts a tint on another surface does the same.
- **Reuse the shared primitives** before writing new ones. Something
  both pages could use goes in `src/shared/`.
- Icons come from `lucide-react` (tree-shaken, only what's imported ships).
- No UI or chart library beyond that: the chart is our own SVG component
  (`src/shared/chart/`) - small, and it does what we need.
- **A card's title is set in small capitals** (`controller.css`); one
  that names something - a backend, a file, a certificate, a flow node,
  a daemon, a version - or is a sentence, as a notice's is, keeps its
  case: `className="data-title"` on its `Card`, or a title in `.mono`. Figures are in the monospace face (`--mono`: JetBrains Mono,
  shipped with the pages - `src/shared/fonts/jetbrains-mono/README.md`).

### Appearance

Eighteen palettes, each light and dark (`palettes.css`): **Janus** -
the default, theme.css's own colours -, **Abysse** (a deep night blue,
slightly translucent cards, cyan and green), and the well-known ones,
each from its makers' colours, light and dark: Ayu, Catppuccin
(Latte, Mocha), Dracula (Alucard, Dracula), Everforest, Flexoki,
GitHub, Gruvbox, Kanagawa (Lotus, Wave), Night Owl (Light Owl, Night
Owl), Nightfox (Dayfox, Nightfox), Nord, One (Light, Dark), Rosé Pine
(Dawn, Main), Solarized, Tokyo Night (Day, Night) and Tomorrow - a
colour that falls short of AA here moved in lightness just enough, its
hue kept. The top bar's palette button (`ThemePicker`, on both pages and the
sign-in screen) chooses the mode - the system's, light or dark - and the
palette, from tiles drawn in each palette's own colours - and **T**,
anywhere but in a field, takes the next palette, shift+T the previous,
its name said a moment at the window's foot (a key alone: the picker
turns it off, WCAG 2.1.4; `janus-palette-shortcut`): a block of
`palettes.css` applies to any element carrying `data-palette` and
`data-theme`, not only to the page. Remembered per browser
(`appearance.js`: `janus-theme`, the site's key for the mode, and
`janus-palette`) and followed at once by every open page of the
Controller (`storage` events) and by the system turning dark.

`<html>` always carries the palette and the mode in effect
(`data-palette`, `data-theme`), set before the page paints by
`public/theme-boot.js` - a module script runs too late, and a dark
palette would flash the light default; keep its lists equal to
`appearance.js`' (a test runs it). `BrandMark` draws the symbol in the
palette's accent and text colours (the favicon follows only the system's
mode).

Every block defines the same tokens - theme.css's, plus the Controller's
own: `--card-bg` (a card's background, translucent in Abysse;
`--surface` stays opaque for sticky table headers and inputs),
`--card-shadow`, `--glow-1`/`--glow-2` (the page's two faint glows),
`--danger-solid` (a solid danger button's fill) and the traffic flow's
`--flow-tcp`/`--flow-log`/`--flow-api`. `palettes.test.js` holds each
block to WCAG AA: text and muted text 4.5:1 on the page and its
surfaces, `--accent-ink` and the status colours 4.5:1 on the page and
on their `-soft` tints (composited), a danger button's text, the
accent 3:1, chart colours 3:1 on a card, a flow badge's text 4.5:1 -
and theme.css to the Janus blocks.

**Gauges** (`Gauge.jsx`, `GaugeStat`): how full something is, a half
circle of 28 segments lit from the left in a chart colour, amber past
75 % and red past 90 % (Meter's thresholds), a `meter` to assistive
technology - a node's CPU, memory and STATE on its Overview, each
writable filesystem on Storage, a hypervisor's CPU, memory and pool.
`Meter` stays for bars in tables and relative sizes.

## The main page

Three tabs (hash routes `#/`, `#/hypervisors`, `#/tokens`), and two more for an admin (`#/accounts`, `#/audit`).

**Nodes**: until the fleet is set up, **Secure your fleet**
(`Fleet.jsx`: create it, store the recovery kit and its passphrase,
give both back - `/api/fleet/*`, session only); pending approvals, then
the nodes as cards - each with its last five minutes (CPU, HAProxy's
requests, its traffic: `NodeSparklines`, see [Live data](#live-data))
and its **Trust** once the fleet is ready
(`fleet`, `needs an update`, an error) - then the **Fleet** card and
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
added, removed or moved - and the machine's auto-scaling
(`Autoscale.jsx`: mode, maxima, thresholds prefilled with the defaults
from `GET /api/machines/{id}/autoscale`, a threshold left at its default
not sent, whether the kernel parameters follow). An existing interface's addresses are shown read-only -
they're the node's, changed on its page - and editable only once the
interface is added or moved. A hardware change - its maxima included -
asks first (restart).

A machine with auto-scaling shows its mode, maxima and status line on
its card (`AutoscaleSummary`) and its journal on demand; **Size** says
what it has now, or what observing would have given.

A locked machine shows neither Edit nor Destroy. Its node's page shows a
banner (`/api/node`'s `locked_by`), and refuses network changes and
updates with 423.

**API tokens** (`Tokens.jsx`): create one (name, role up to the
account's, validity) - shown once, with a copy button - see each one's
last use, revoke. Each account sees its own, an admin everyone's (an
**Account** column). Sessions only, never a token.

**Accounts** and **Audit** (`Users.jsx`, `Audit.jsx`) are admin tabs:
accounts with their role (a select, applied at once - "None": its
grants only), their **grants** (`GrantsEditor`, a dialog: per grant a
role, the labels it picks nodes by, the domains it's narrowed to),
**Reset password** / **Disable** / **Delete**, a new account's or a
reset's password shown once (`GivenPassword`), the session policy; the
audit's newest 500 entries, filtered by account on submit. A node card
shows its labels (`NodeLabels`), edited in place by an admin. Under the
Provision panel (which takes an enrollment token into its commands),
**Enrollment tokens** (`Enroll.jsx`, admins): made with a name, a
number of nodes, a validity and labels, shown once with where it goes;
listed with their uses, revoked. The API
tokens page takes a scope - labels and domains - and lists it. Last,
**HTTPS certificate** (`HTTPSCert.jsx`, admins, folded like the
Provision panel): what the page is served with (self-signed, uploaded,
or from `-tls-cert`), a bundle checked before it's served, and back to
self-signed.

**Your account** (`AccountButton`, a dialog): second factors
(`MFAPanel`), **Trusted browsers** (`TrustedBrowsers`: "this browser"
marked, each forgotten), SSH keys (`SSHKeys` - from a trusted browser's
sign-in, `SecondFactorForm confirming` first), the password. Giving the
second factor offers "Trust this browser for N h" when the policy
(Accounts › Sessions) allows it.

**Roles on the page.** `AuthGate` gives the signed-in account
(`/api/auth/status`'s `user`) to `MeContext` (`me.jsx`); a component asks
`useCan()('admin' | 'operator')` and leaves out what the role would be
refused - buttons, forms, tabs - rather than showing them disabled. The
backend refuses anyway (`gate` in `auth_handlers.go`, one role for reads
and one for changes per route; `TestRoutesNeedTheirRole`). Reader:
reads. Operator: + `PowerButtons` (start/reset/force-off, console).
Admin: the rest. An account's grants (`me.grants`) count where a node is:
`nodeCan(me, labels, role, domain)` (`me.jsx`, the Controller's
`access.go` mirrored) - `PowerButtons` takes the node's labels and the
machines domain. An account without a role over everything gets neither
the Hypervisors tab, nor the Controller's update card, nor the fleet's
(`/api/fleet` isn't asked); the Controller answers its node list and
statuses with the nodes it reaches, hypervisors and pending ones empty. A session that needs something first (`needs`) gets only that:
`mfa` - `SecondFactorForm` (passkey, app code, recovery code) -, then
`password` - the forced change -, then `mfa_enroll` - `EnrollMFA`, the
role's required first factor, then its recovery codes once
(`RecoveryCodes`). The account button opens `MFAPanel` (factors, add,
remove and new recovery codes with the password), `SSHKeys` (janusctl's
keys: add - only once the account has a second factor -, remove, and the
`janusctl login` command with the Controller's fingerprint from
`/api/controller-info`) and the password form. `#/cli-login` and `#/cli-device` (`CliLogin.jsx`,
rendered in place of the tabs once signed in) approve a janusctl
certificate: the key's fingerprint, and what it carries - the account's
role, or for an account with grants "what your account may do, node by
node" (a scoped certificate) -, or a lower role up to the account's
highest (`max_role`); the first sends the browser back to janusctl's
`127.0.0.1` listener.

A failed backup, or none for twice the interval, puts **Backup
failing**/**Backup late** in the main page's header for admins (a link
to the tab, `backupAlert.js`) and a notice on the tab.

**Backups** (`Backups.jsx`, admins): the backup kit - the fleet
wizard's three steps (`Step` from `Fleet.jsx`) -, then the bucket and
schedule (the secret write-only: "kept - type to change it"), **Back up
now**, **Download a backup**, the recent runs and the signing key. A new
Controller's first page offers **Restore a backup instead**
(`Restore.jsx`): the bucket (listed) or a file, the kit and its
passphrase; it then waits for the Controller to start again and reloads
into its sign-in.
Passkeys go through `webauthn.js` (base64url options to
`navigator.credentials` and back); they're offered only where the page's
host is a name (`mfa.rp_id`), and the TOTP QR code is an SVG the
Controller draws, shown as an `<img>`. Checked with Chromium's virtual
authenticator (CDP `WebAuthn.addVirtualAuthenticator`) on
`https://localhost`, the Controller's certificate accepted with
`--ignore-certificate-errors-spki-list` - Chromium refuses WebAuthn on a
page with a certificate error.

**Sessions on the page.** A request the page makes by itself - every
periodic refresh - is `call(path, { background: true })` (or
`backgroundHeaders()` for a raw `fetch`): it carries `X-Janus-Background`
unless the user touched the page (pointer, keys, wheel) in the last
minute, and the Controller doesn't count it as use - an idle page lets
its session end. Any 401 outside `/api/auth/` sends `SIGNED_OUT`:
`AuthGate` shows the sign-in again ("Your session ended"). A dialog
opened from the top bar renders in a portal (`createPortal`): the bar's
backdrop blur would hold a fixed element inside it.

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
and `ReleaseInfo.SecurityUpdate(version, target, NodeImage)` is the
worst severity the releases after a version fix - an extension's fix
only for a node that has it, a HAProxy branch's or kernel track's only
for the nodes its audience names: those taking the default, or pinned
to it (`NodeSchematic`: what the node's image reports) - `security_update` in `/api/nodes/status`,
a node's `/api/update-check` (with `security_release`) and
`/api/controller/update`; empty for a development build. Where an update
shows, a security update shows instead: `SecurityBadge.jsx` (a shield,
`danger` for critical/high, `warn` below; `severity.js` for the tone and
the sentence), on the node card, the header's count, the node page's top
bar, its Update view (with a notice linking the release's notes), the
Controller's update card, and the node page's Update dot.

**Pre-releases** (GitHub's prerelease flag, [releasing](contributing/releasing.md#pre-releases))
are never an update. `fetchLatestRelease` takes the newest release that
isn't one; `ReleaseInfo.Prerelease` is the newest pre-release when newer
than it, offered only to what runs an older version (`PrereleaseFor`) -
`prerelease` in a node's `/api/update-check` and in
`/api/controller/update`, `running_prerelease` beside them,
`prerelease` in `/api/nodes/status` and the machine catalog,
`prereleases` in janusctl's sign-ins and inventory. A pre-release's
fixes never count in security updates, and it never moves
`update_available`, a badge or the fleet's counts. Where it shows, it is
folded away (`<details className="prerelease-offer">`) - under the
node's latest release, on the Controller's update card for an admin -
and using it takes its version typed (`useConfirm`'s `typeToConfirm`):
on a node it only fills the installation form in (a node with
extensions asks `POST /api/factory/prerelease` for the factory's build,
only then - viewing the page never starts one), and the installation's
confirmation repeats the warning; the Controller's `POST` needs
`"prerelease": true`. `PrereleaseBadge.jsx` marks what runs one.

## The node page

Hash routing (`#/haproxy/config`, `#/tools/files?path=/etc`), a sidebar
grouped by intent, and a sticky top bar with the node's identity, health
badges, version (and available update), the refresh interval and the
appearance picker. On a wide screen with a mouse the sidebar is a rail
of its icons (`rail.js`, `node.css`) that opens over the page - never
pushing it - while the pointer or the keyboard's focus is in it, or
stays open pinned ("Keep it open", remembered per browser as
`janus-sidebar`); folding moves nothing - the labels only fade, still
read by assistive technology. A touch screen keeps it open, a narrow one
has its drawer. Its groups:

- **Monitoring** - overview, metrics, traffic flow, processes, network,
  storage
- **Logs** - service logs, events, kernel (dmesg)
- **Apps** - HAProxy (with its own tabs) and the Janus exporter (built
  into janusd: its settings and a scrape example), then only what the
  node's image has: the extensions' apps (Node exporter - status and
  settings; QEMU guest agent - status, what the hypervisor may do) and
  the modules (bird, keepalived, nftables) - HAProxy's tabs include
  Configuration (the editor, then the History panel - `views/haproxy/
  ConfigHistory.jsx`: the versions, a version's diff against the running
  configuration or the one before it, "Open in the editor", "Apply this
  version…" when `may` allows ApplyConfig - a POST naming the version),
  Files (`/etc/haproxy/files`: error pages, maps, certificates; edit,
  upload, remove, each refused by the node when haproxy.cfg wouldn't
  load with it; files holding a private key are never shown), Stick
  tables, Sessions and Errors (`views/haproxy/StickTables.jsx`,
  `Sessions.jsx`, `Errors.jsx`, relayed by `nodeproxy/haproxy_runtime.go`
  as protojson - 64-bit numbers arrive as strings; a node older than them
  answers 501, `RuntimeError.jsx` says to update it;
  docs/guide/haproxy-runtime.md) - Let's Encrypt (account, the
  HTTP-01 rule, certificates and DNS providers as forms - secrets never
  shown, kept when left empty - and the configuration as JSON) and Consul
  (agent, members, the configuration's files, the configuration in the
  shared ModuleConfigEditor). An app the node doesn't have
  isn't listed - "Add or remove apps…", last, opens the Update page's
  image panel.
  Firewall: ruleset editor (check, diff, apply on trial - the Controller
  confirms over a *fresh* connection, never the shared one, which
  conntrack keeps open whatever the ruleset), live sets, live ruleset.
  VRRP (and BGP): instances/sessions, then the daemon's configuration in
  the shared ModuleConfigEditor (check by the daemon itself, diff, apply,
  remove)
- **Tools** - packet capture, files, connectivity test (`views/Probe.jsx`:
  each step as the node streams it - POST `/api/system/probe`, NDJSON,
  read with fetch: an EventSource would run the probe again once it
  ended), diagnostic bundle (`views/Support.jsx`: what the bundle holds
  for the account, from `may`; GET `/api/support`, `nodeproxy/support.go`:
  `internal/support` with the account's permissions, streamed; the
  Controller's audit records each download)
- **System** - network (hostname, interfaces, VLANs, DNS, NTP, and the
  neighbour table under the interfaces - its own call, `NeighborList`,
  shown when `may` allows it), sysctl
  (the kernel parameters HAProxy depends on: edited in the table, checked
  as typed from the node's own bounds, tested on trial with a countdown -
  Apply over a fresh connection, Cancel -, reset one or all; values
  suggested for the node - a chip under the value, **Why** opens the rule,
  its measurements and sources, **Use** fills the field, never more -
  and **What the node observed**, the signals they rest on by hour; the
  CIS benchmark's controls, locked; the read-only ones; the history), services,
  update, access (who the node lets in - its fleet's root, bundle and
  issuing CAs, its own CA -; replacing its own CA, the new admin
  credential for a key the browser makes with WebCrypto (P-256, the
  private half never sent) or printed on the node's console, refused
  while the Controller holds a credential that CA issued; client
  certificates: role, name, validity - the Controller refuses a
  certificate the node didn't issue as asked, which an older node does),
  power

**Only what the role may do.** `/api/me` carries `may`: the RPCs the
account's permissions on the node allow - roles and domains
(`internal/rbac`, the tables janusd enforces; `nodeproxy.User.May`).
Nothing is offered, and no view mounts, before it's known (a loader).
Reading a configuration is its domain's: HAProxy's configuration, maps,
certificates and files tabs, Let's Encrypt, the network's saved
configuration, the firewall's ruleset and sets, VRRP's and BGP's
configuration aren't fetched without it (`readMethod` on
`ModuleConfigEditor`) - the state stays. `useMay()` (`src/node/may.js`) answers per RPC: the nav hides
what the role can't open (logs, dmesg, capture, files, connectivity test, "Add or remove
apps…"), a group left empty goes too, a route opened by hand says the
role doesn't reach it; views hide their actions (Apply, Reload, maps,
certificates, servers' state, the modules' check/apply, power actions
one by one, updates, the CA) and keep what reads - an editor stays
editable, to validate and download. Consul's configuration (it may hold
secrets) isn't even fetched. The node still refuses anything else.

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
its HAProxy (version, branch, pinned or the default) and its kernel
track, from what the node's image reports (`VersionResponse`'s
`schematic`, `haproxy`, `kernel` - `nodeproxy.NodeSchematic`, the
extensions alone for a node from before images said), and installing a
bundle from another schematic takes an explicit checkbox
(`allow_schematic_change`). **Change the image…** opens a panel with the
factory's catalog for the newest release (`GET /api/factory/catalog`:
extensions, HAProxy branches with their end of upstream support, kernel
tracks); **Prepare the update** sends the choices to `POST
/api/factory/update` (`extensions`, and `haproxy`/`kernel` - left out,
the node's own), which registers that schematic with the factory and
asks for its update - the same resolution as the node's own check, so
going back to no extension and the defaults means GitHub - and the
panel follows a build every 20 s until it's ready. It only fills in the
installation form (URL, sha256, the schematic-change checkbox); the
confirmation lists what the node gains and loses, and installing stays
the usual A/B update - with the automatic revert forced on when the
HAProxy branch changes (another branch may refuse the configuration).
A node pinned to a HAProxy branch gets a `support` in its update check
and status (`nodeproxy.EndOfSupport`, from the factory's newest
catalog): `SupportBadge.jsx` warns from six months before the branch's
end of upstream support, and turns red once the releases no longer
offer it (no newer update will come), with a notice on the Update page.
The node card shows the HAProxy version and the kernel and its track, and what the other slot holds (`version.inactive_slot`) - the release an install writes over, which its confirmation names. A release that renamed one of the node's
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
case. What the page filled the form in with - the latest release (**Use
this update**, offered only when it's newer than the node's), the
pre-release, the prepared image - it names, while the URL is still that
one: in the form, and in the confirmation's title and button. Older
than what the node runs (`releases.js`' `compareRelease`, the Go
`updaterapi.Newer`'s rule), the form and the confirmation say so in red,
and installing takes the version typed. A URL or files typed by hand
have no version here. **Reboot through kexec** (off each time the page opens) sends
`reboot_mode: "kexec"` with any of the three, `"firmware"` otherwise:
the node's last stage message - the toast, then the label the page
follows the reboot under - says whether it jumps into the new kernel or
why it goes through the firmware instead. Power's Reboot has the same
box (`mode: "kexec"` on `/api/system/power`); the node refuses that
one, nothing rebooted, when it can't kexec.

Shared caches (latest release, factory updates, catalog) fetch with a
context detached from the request that triggers them
(`sharedFetchContext`): a browser leaving the page must never put
"context canceled" in a cache every node's page reads.

A new feature goes into the group matching what the operator is trying
to do, not into whichever page has room. A page that grows several
distinct concerns gets tabs (as HAProxy does) rather than a longer
scroll.

### Traffic flow

Monitoring › Traffic flow (`views/Flow.jsx`, the logic in `flow.js` -
pure, `npm test`) draws the node in one of three shapes: columns left
to right - interfaces, Janus (its HAProxy: the node in the middle, the
symbol in the palette's colours), frontends, backends, servers - with the
labels under the nodes (`across`) or beside them (`dense`: a name above
the line its links leave by, the numbers below it, rows half as tall,
for many backends), or rows top to bottom (`down`), the backends'
servers hanging below them on a trunk. A label's size never follows its
numbers - room is kept for them - so a refresh never moves the map.
**Auto**, the default, measures the room (`useRoom`: the layout's width,
the window's height below the card) and `arrange` picks the shape and
the side panel's place - beside the map or below it - that show the map
whole in the window the biggest, between 75 % and 150 % of its size
(any at full size or more counting as one, the earlier shape winning
when nearly as big); when none can be whole, the first as wide as its
card down to 85 %, the page scrolling down it; else the one overflowing
less, scrolling in its card, opened on Janus. **Horizontal** keeps to
the two left-to-right shapes, **Vertical** to top to bottom. The page
drops its 1500 px limit for it: a full screen is room. With the panel
below, a selected node's details are pinned to the window's bottom,
over the map, with a close button. The choice is remembered per
browser, like the **Servers** chip that hides the servers (past eight,
"+N more"). `show stat`
(`/api/haproxy/stats`, at the refresh interval) gives the proxies and
their counters - traffic is the difference of `bin`/`bout` between two
answers, none when a counter went back - but never which frontend sends
to which backend: that's read from the configuration
(`/api/haproxy/config`, every 30 s and again when HAProxy's uptime goes
back - a reload): `default_backend` (a `defaults` section's too),
`use_backend` with its condition, a listen section's own servers, and a
`use_backend %[...]` through the maps it reads - directly or through a
`set-var` - whose values name the backends and whose keys say for what
(`/api/haproxy/maps/<path>`, every 30 s). A `use_backend` nothing
resolves is said in the frontend's details. Where traffic goes, a link
is lit and particles run along it - glowing dots with a short trail,
closer together and faster the more bytes (`flow.js` `particles`: as
many as fit the link's length at that spacing, at most eight), each an
SVG `animateMotion` (no script runs a frame); under
`prefers-reduced-motion` the link is only lit. A backend fed by several frontends has its
traffic on each link at most the smaller side's, as HAProxy doesn't
count the split. A link's colour is its protocol's - HTTP the accent,
TCP `--flow-tcp`, logs `--flow-log` (tokens of `node.css`, each 3:1 on
the surface in both themes, the badges' text 4.5:1) - from HAProxy's
`mode` in `show stat`: a frontend's up to it, a backend's from its
frontend on (a TCP frontend may hand its connections to an HTTP
backend); an interface's link, all of its traffic, is neutral. A
frontend or backend wears its mode as a badge at its ring's top left,
where no link comes in nor label goes. The log targets
(`/api/log-forwarding`, at the refresh interval, records and bytes a
second from `sent` and `bytes_sent`) sit beyond the interfaces - a
column left of them, a row above them top to bottom, only when the node
has some, so a map without them is the one it was - each linked to the
interface its traffic leaves by (the node's `interface`: the kernel's
route to the target's connection), its pulse going out of the node; a
target no interface on the map says (not connected yet, a node older
than that field) is drawn unlinked, its details saying why. In the
same column, first, who is connected to the node's API
(`/api/system/clients` = `SystemService.ClientList`, at the refresh
interval): the Controller - the connection this very page comes
through - and the direct clients (janusctl, its tui, a script with a
certificate of its own), one node each per interface they come by
(`apiPeers`), with their calls and bytes a second (TLS included,
counted by janusd's listener) and a pulse going into the node, in
`--flow-api`; a badge at the ring's top left counts the users the
Controller acts for, or the direct clients. The Controller's details
list those users - whom its calls were for in the last 2 minutes (each
call names one, `janus-as-user`), with their role, last call and open
streams, the viewer marked *you*, `automation` and `auto-scaling` being
the Controller's own work; the direct clients', each connection. The
node gives names and addresses to an admin only: anyone else gets the
Controller's users counted, the direct clients nameless. A number
that starts from zero - a node's traffic, sessions, requests,
connections, records, calls - flashes (`figures` and `risen` in
`flow.js`, `useFlashes`): a ring widening off the node, the number in the accent
a moment, its link lit; CSS animations replayed by a new key, only the
colour under `prefers-reduced-motion`; a number unknown before (the
first sample, a counter that went back) never flashes. Ctrl (or ⌘) +
scroll zooms the map - a trackpad's pinch too - around the pointer
(a non-passive `wheel` listener on the card: a scroll alone still
scrolls the page), from 30 % to 400 % of its size, as do the − and +
buttons; zoomed, the map scrolls in its card, as tall as the window,
and the mouse drags it; **Fit** is back to `arrange`'s size, as is a
change of orientation. Hovering a node or link brings its paths forward;
selecting one shows its counters and rules in the side panel - a
server's Ready/Drain/Maint (`ServerStateButtons`, shared with HAProxy ›
Backends). Without `HAProxyService/GetConfig` (a grant narrowed to other
domains) the configuration and maps aren't asked: the page says so, and
links only servers to their backends. `janusctl tui` reads
configurations the same way in Go (`internal/haproxyflow`): both are
held to `internal/haproxyflow/testdata/cases.json` - change one, run
both tests.

### Live data

- **Polling** follows the global refresh interval (Off, 1 s … 30 s,
  every second by default, remembered per browser) through `usePoll`,
  and pauses while the tab is
  hidden. Use a longer fixed `every` only for slow-changing data. A
  failed poll is retried after 5 s at most, so a node coming back from
  a reboot replaces what it left on screen quickly. A repeated request
  has a timeout (`getJSON(path, { timeout })`, `call(path, { timeout
  })`: a few periods, 30 s at least for `usePoll`, 10 s for the
  metrics): one sent on a connection that died while the computer
  slept never answers, and a poll waiting for it stopped for good - the
  charts froze until the page was reloaded.
- **The node itself** (`/api/system/overview`, `/api/update-check`:
  version, CPU, boot time, latest release, security fixes) lives in
  `NodeStatusProvider` - one fetch for the top bar, the Update dot and
  every page (`useNodeStatus()`, never a page's own `usePoll` of
  them). It's reloaded as soon as the node changes: the metrics poll
  seeing a new boot time or the node answering again, `WaitForNode`
  seeing it back, or "Refresh now".
- **Releases** come from GitHub's list, which the Controller keeps ten
  minutes (`nodeproxy` `releaseCache`, shared by every page). A Refresh
  button - the node page's "Refresh now" (`recheck`), the Controller's
  header (`ControllerUpdate`'s `checkNow`) - asks with `?refresh`
  (`/api/update-check`, `/api/controller/update`, `RefreshAsked`): the
  list is read again unless it's younger than 30 s, and kept when GitHub
  fails. Only a click asks for it - never a poll: GitHub allows 60 calls
  an hour without a token.
- **Metrics history** lives in `MetricsProvider`, app-wide, so charts
  keep their data across page changes; it holds the last 900 samples in
  the browser only. Unlike `usePoll`, it goes on sampling while the tab
  is hidden - for 15 minutes, then stops until the tab shows again
  (`HIDDEN_SAMPLING_MS`): a look at another tab leaves no hole, a tab
  forgotten in the background doesn't keep its node busy. Its requests
  are background ones (no pointer or key reaches a hidden tab): they
  never keep the session alive - measured, a page sampling every second,
  shown or hidden, untouched, signed out on time. Once a 401 says the
  session ended, nothing polls again: the banner asks to sign in and
  reload. Counters are turned into rates client-side, and a
  counter going backwards (HAProxy reload) gives no point rather than a
  negative rate.
- **Charts** leave gaps where data is missing (e.g. during a reboot)
  and where samples are more than 2.5 intervals and 2 s apart (`isGap`:
  a tab hidden long, the node away), instead of drawing a line through
  them - a time with no sample is shaded (`.chart-gap`), so it reads as
  one, not as zero -, follow the data until the time
  window is full, and use binary steps for bytes - see [Charts](#charts).
  Each sample carries its interval (`iv`); `MetricsProvider` gives every
  chart of the page one clock (`clock`), so they scroll together.
- **Metrics' breakdowns** (`breakdown.js`, `Donut`): below the charts,
  what HAProxy's own counters say since it started (`/api/haproxy/
  stats`, every 5 s; HAProxyService/Stats): its frontends' responses by
  status class, their requests by HTTP version, their TLS handshakes -
  full, resumed (`ssl_sess` counts the new sessions only, the resumed
  ones in `ssl_reused_sess`), failed -, the bytes each backend moved,
  and how fast each backend answers (averages over its last 1024
  requests). No counter says which TLS version or cipher a connection
  used: only the access log does (`%sslv`, `%sslc` - `option httpslog`
  ends its lines with them). So **TLS versions** and **Ciphers** come
  from HAProxy's last log lines (`/api/haproxy/tls-log`, every 30 s, for
  whoever may read the node's logs - SystemService.Logs, the services
  domain): the Controller reads them on the node (this boot's 5000,
  from memory) and counts the version and cipher each line ends with
  (`nodeproxy/tlslog.go`) - a sample of what was logged, said under
  them, not every request since HAProxy started. With nothing to count,
  a card keeps its place - an empty ring, the reason under it: the role
  doesn't read logs, no TLS handshake yet, no access log on the node
  (`access`: lines with HAProxy's date), or one without the version and
  cipher - the last two with a link to how to log them
  (haproxy-config.md, "TLS versions and ciphers on Metrics"). A donut's figure shrinks to fit its
  ring (`fitSize`), a hovered part's name is cut (`clip`) - the legend
  has it whole.
- **The node list's curves** (`NodeSparklines.jsx`, `nodeMetrics.js`):
  `GET /api/nodes/metrics` - each node the account sees, its last
  minutes of CPU, HAProxy requests and traffic, from the Controller's
  sampler (below), the last two minutes at a reading every 2 s, asked
  only for what's newer (`?since=`), at the sampler's pace, only while
  the Nodes tab shows - each request keeps the sampling going ten more
  minutes, so the list back from the background has no hole -, as a
  background request.

### Charts

`src/shared/chart/`: `Chart` (and `Sparkline`, a chart reduced to its
curve), its arithmetic in `math.js` (unit-tested), one animation loop
for every chart (`scheduler.js`).

- **They move continuously** while the page samples: the time axis
  scrolls every frame, its right edge about one sampling interval behind
  the samples' clock (`ServerClock`: the Controller's, a sample's time
  being when it asked the node; the browser's offset smoothed) - each
  new sample is already there, just off the edge, when the line reaches
  it, so the line grows instead of jumping a step per sample. The edge
  keeps to 0.75-1.25 times the real pace, never goes back, and stops at
  the newest sample when samples stop; the value axis eases to a new
  scale. A pause (`isGap`) isn't counted in the edge's distance (it
  held the edge up to 32 s behind once samples came back), and an edge
  behind by a gap's length (`snapDistance`) jumps instead of catching
  up - at 1.25 times the pace, a node's 13 s reboot took it most of a
  minute.
- **Curves are monotone cubics** (Steffen's tangents): between two
  samples the line stays between their values - never below zero beside
  a burst, never past 100 % - the first series' area (or any with
  `fill`) fading down to the baseline.
- **Cheap frames**: a chart builds its paths once per sample, in the
  samples' own units; a frame sets one transform on them (the lines
  keep their width, `vector-effect`), the time ticks' and the hovered
  sample's - set directly, no render. Asleep off screen
  (IntersectionObserver) and in a hidden tab.
- **Still** - redrawn per sample, the edge on the newest sample - under
  "reduce motion", with sampling off, or `motion="static"`: what the
  docs' screenshots take (Playwright's clock drives animation frames
  too).
- Value axes end on round quarters (`niceMax`), time ticks on round
  times, scrolling with the data and fading at the ends; the left margin
  is the widest label's.
- Every series has its area, fading down to the baseline. The legend
  shows the newest figures, each in room of its own (a changing value
  never moves the next); hovering shows the sample under the pointer -
  its time and values - in a tooltip beside it, on the side it fits,
  riding with the sample as it scrolls.
- **The time shown**: Overview and Metrics share one choice
  (`WindowPicker`, `chartWindow.js`: 1 min - the default -, 5 min,
  15 min, all the history kept), remembered per browser as
  `janus-chart-window`. At one sample a second over a minute, the
  charts move about as fast as the eye follows. Overview opens on the
  node's traffic, the widest chart, its figures in its header.
- **Events** (Logs › Events): the node's event log, kept on its STATE -
  the last thousand, across reboots and updates; each `janusd.started` is
  drawn as a line of its own (version, slot), the events of another day
  have their date.
- **Streams** (logs, events, dmesg) are Server-Sent Events via `useSSE` /
  `LogViewer`. A node-side error arrives as a `failure` event - never
  `error`, which `EventSource` uses for its own connection problems.
- **Log forwarding** (the Controller's tab, admins; a node's Logs ›
  Forwarding): both edit targets with `src/shared/LogTargets.jsx` (the
  model in `src/shared/logTargets.js`: sources, defaults, `cleanTarget`
  - an empty secret field keeps the saved secret, the server merges it).
  Each target shows folded - name, kind, where, its state and counts
  (the status merged in, by name), *not saved* when it differs from the
  saved one - with Test, Edit and Remove; Edit or a new target opens the
  editor (Done folds it). Nothing applies until the page is saved: an
  `UnsavedBar` (sticky) says so with Save and Discard; a save or a
  discard bumps the list's `key`, so everything folds again. Test sends
  the target as shown (the Controller's `POST /api/log-forwarding/test`
  takes it as `config`, the saved secrets merged by name). The
  Controller's page saves its own targets and the fleet profiles whole
  (`PUT /api/log-forwarding`), lists each node's state with the
  profiles, tests a target from the Controller or a profile's node. A
  node's page is read-only while `managed_by` is set (a profile's).
  `fieldset.plain` (theme.css) has no frame: a fieldset there only
  disables a group of fields.
- **Previous boots** (Logs › Service logs, Logs › Kernel): a node with a
  LOGS partition lists its earlier boots (`/api/log-boots`) in a
  `BootSelect` next to the page's other controls - only once there is
  one. A previous boot is fetched whole (`/api/logs?boot=`,
  `/api/dmesg?boot=`, plain text) by `BootLog` and shown in `LogViewer`
  with `text` - no pause, follow or clear, a "previous boot" badge.

### The splash

Each page opens on Janus's logo while its first data comes in: the
logo's orange bar fills up with the steps done, its two faces light in
turn, and the step waited for is written under it.

- **Its steps** are the page's own (`SplashProvider steps=` in each
  `main.jsx`, `useSplashStep(key, label, done)` where the data is):
  the main page waits for the session, the fleet (`/api/nodes`...) and
  the nodes' status (`/api/nodes/status`, up to 4 s per node); a node's
  page for the node, `/api/me`, its overview and its first metrics.
- **A second at least, 8 s at most**, then a fade, then it leaves the
  DOM - it never stands in front of a click. A step that hangs leaves
  the page to say why. These times are CSS animations
  (`.splash-clock`), not timers: a test that holds the page's clock
  (`hack/browser/shots.mjs`) doesn't hold the splash - it waits for
  `.splash` to be detached.
- **`finish()`** ends it as soon as its second is up: the main page for
  a form (sign-in, setup, a second factor), a node's page when the node
  doesn't answer (its banner says so). **`reset()`** shows it again:
  after a sign-in, for the fleet's first load.
- Only a page's first load: never on a route change or a poll. With
  `prefers-reduced-motion`, the faces stay still and the bar jumps.
- A page whose first load gets a new slow call: give it a step.

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
- **The node list's curves** (`internal/nodemetrics`, `nodes_metrics.go`):
  one sampler however many pages look, reading a node (`nodeproxy.
  ReadCounters`: SystemStat, NetworkDeviceStats, ShowInfo) every 2 s
  while a page asks for it and ten minutes after - each request renews
  a 10 min lease -, 16 at once at most, 4 s each, detached from the
  request; it keeps 10 minutes, so a page opened again, or a tab back
  from the background, finds its curves whole. A lease that lapsed
  starts again without a rate across the time nothing was read. It reads as
  the Controller (`Automation`, like `/api/nodes/status`): those calls
  are a reader's, in the observe domain every permission on a node
  includes, so whoever sees a node may read its figures anyway; a node
  that doesn't trust the fleet yet gets curves for an admin only, like
  its page.

## Verifying a change

1. `make dashboard-frontend-build` (both pages) and commit the built
   assets with the source - `go build` embeds them and needs no Node.js.
2. `npx oxlint src` in `dashboard/frontend`: no errors.
3. Run it for real: a node from `make local-dev-image`, a native
   `bin/dashboardd` (`-data-dir` in a scratch directory), the node added
   through the API, then every touched page in a real browser - both
   themes, zero page/console errors, zero failed requests, and each new
   action actually performed and its effect checked on the node.
   Headless Chromium with Playwright works well for this - `make
   browser-image` builds a pinned one (`hack/browser`: `docker run
   --network host -v $PWD/check.mjs:/browser/check.mjs janus-browser
   node check.mjs`) - sign in through the page, for each role the change
   concerns:

   ```js
   const ctx = await browser.newContext({
     ignoreHTTPSErrors: true, // the Controller's self-signed identity
     colorScheme: 'dark',
   })
   ```

   Passkeys need a certificate Chromium accepts (it refuses WebAuthn on
   a certificate error): launch it with
   `--ignore-certificate-errors-spki-list=<the identity's SPKI SHA-256>`
   and open `https://localhost`, then add a virtual authenticator (CDP
   `WebAuthn.addVirtualAuthenticator`).

   Look at the screenshots, not only at the absence of errors.

   `make ui-preview` (`hack/ui-preview.sh`) sets all of that up at once,
   to look at a page or a `janusctl tui` screen yourself: the Controller
   and janusd built from the tree, three `local-dev` nodes under traffic
   that comes and goes - HTTP and TCP frontends - sending their logs to
   rsyslog and Elasticsearch (edge-1 through two interfaces; edge-3 runs
   `many.cfg`, two dozen backends as a big load balancer has), a second
   account (lea's) on edge-1's page now and then and a janusctl
   following its events - the Traffic flow's Controller users and
   direct clients -, the Controller on `https://127.0.0.1:18443`
   (`ssh -N -L 18443:127.0.0.1:18443 HOST` from another machine, or
   published on one of its addresses: `UI_PREVIEW_BIND=ADDRESS
   UI_PREVIEW_HOST=NAME`, the name in its certificate - its passwords
   are fixed), its account and the `janusctl login` printed.
   `hack/ui-preview.sh reload` after a change (the binaries built again,
   the Controller and nodes restarted, their state kept; `reload ui`:
   the Controller alone, after a frontend build), `check` to walk its
   pages in every palette and mode (`hack/ui-preview/check.mjs`: axe,
   no error, the appearance, motion and the sidebar - its screenshots
   in `build/ui-preview/check`), `down` to remove it.
4. Anything the `local-dev` container can't show (dmesg, A/B slots,
   reboot, SELinux) gets checked against a real node booted from
   `build/rootfs/disk.img` under QEMU/OVMF.
5. New relays get an assertion in `hack/qemu-dashboard-test.sh`, which
   runs against a real enforcing node in `image-build.yml`.
6. A page the docs show (`hack/browser/shots.yaml`) changed: `make
   docs-screenshots` takes the docs' screenshots again - a real
   Controller, three `local-dev` nodes - and rewrites those that
   changed; commit them with the change. `image-build.yml` reports a
   screenshot that drifted.
