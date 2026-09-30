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
the main page (see `nodeproxy.go`'s package doc). They share everything
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

Rules:

- **Use the tokens, never raw colors.** Every color must work in both
  themes; dark mode is not an afterthought. Check new screens in both.
- **Reuse the shared primitives** before writing new ones. Something
  both pages could use goes in `src/shared/`.
- Icons come from `lucide-react` (tree-shaken, only what's imported ships).
- No UI or chart library beyond that: the chart is our own SVG component
  (`src/node/components/Chart.jsx`) - small, and it does what we need.

## The node page

Hash routing (`#/haproxy/config`, `#/tools/files?path=/etc`), a sidebar
grouped by intent, and a sticky top bar with the node's identity, health
badges, version (and available update), the refresh interval and the
theme toggle:

- **Monitoring** - overview, metrics, processes, network, storage
- **Logs** - service logs, events, kernel (dmesg)
- **Apps** - HAProxy (with its own tabs), then optional modules (bird,
  keepalived, nftables), marked "n/a" when the image doesn't ship them
- **Tools** - packet capture, files
- **System** - network (hostname, interfaces, VLANs, DNS, NTP), services,
  update, access (client certificates), power

Updates follow the node's image schematic (`GET /api/update-check`,
`nodeproxy/update.go`): a node with the default schematic gets the
newest GitHub release; a node with extensions gets the newest release
the image factory has built from its schematic (`dashboardd
-image-factory`, janus.sw-servers.net by default), shown as "building"
until it's ready - never a plain release, which would drop its
extensions. The Update page shows the node's schematic and extensions,
and installing a bundle from another schematic takes an explicit
checkbox (`allow_schematic_change`).

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
