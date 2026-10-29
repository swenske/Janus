// hack/ui-preview's pages walked in a browser (hack/ui-preview.sh
// check): every palette in both modes - the sign-in screen, the node
// list (cards and rows), the hypervisors tab, the clusters and cluster
// edge, the Consul tab, and pages of edge-1 - with no page or
// console error, no failed request, no CSP report, and nothing serious
// for axe (WCAG 2 A/AA); a screenshot of each in $OUT. Then what the
// appearance and the sidebar do: a palette chosen in one tab followed by
// another, the T shortcut, no animation frame under "reduce motion", the
// charts moving otherwise, the sidebar opening over the page and pinned;
// cluster edge's configuration, the change an apply would make, its
// editor and journal, and the new-cluster form. Run by
// hack/ui-preview.sh in the browser image, on the preview's network.
import AxeBuilder from '@axe-core/playwright'
import { chromium } from 'playwright'

const { CONTROLLER, PASSWORD, OUT } = process.env
// Every palette (appearance.js's); PALETTES=a,b for a few.
const PALETTES = (process.env.PALETTES || 'janus,abysse,ayu,catppuccin,dracula,everforest,flexoki,github,gruvbox,kanagawa,night-owl,nightfox,nord,one,rose-pine,solarized,tokyo-night,tomorrow').split(',')
const MODES = ['light', 'dark']
const NODE_PAGES = ['/', '/metrics', '/storage', '/flow', '/network', '/logs', '/events', '/kernel', '/haproxy', '/haproxy/config', '/tools/files', '/system/network', '/system/sysctl', '/system/update']
const problems = []
const browser = await chromium.launch()

async function context(opts = {}, palette = 'janus', mode = 'light') {
  const ctx = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 1440, height: 900 }, colorScheme: mode, reducedMotion: 'reduce', ...opts })
  await ctx.addInitScript(([p, m]) => {
    localStorage.setItem('janus-palette', p)
    localStorage.setItem('janus-theme', m)
  }, [palette, mode])
  return ctx
}

async function signIn(ctx) {
  const r = await ctx.request.post(`${CONTROLLER}/api/auth/login`, { data: { name: 'admin', password: PASSWORD } })
  if (!r.ok()) throw new Error(`sign-in: ${r.status()}`)
}

// Answers a page expects may fail: the sign-in state, and the account's
// preferences (a node page's src/node/nodeCharts.js) - a Controller
// without them answers 404, the page goes on with its defaults.
const mayFail = (url) => url.includes('/api/auth/status') || url.includes('/api/me/preferences')

function watch(page, where) {
  page.on('pageerror', (e) => problems.push(`${where()}: ${e.message}`))
  page.on('console', (m) => {
    if (m.type() === 'error' && m.text().startsWith('Failed to load resource') && mayFail(m.location().url || '')) return
    if (m.type() === 'error' || m.text().includes('[Report Only]')) problems.push(`${where()}: console ${m.type()}: ${m.text()}`)
  })
  page.on('response', (r) => {
    if (r.status() >= 400 && !mayFail(r.url())) problems.push(`${where()}: ${r.status()} ${r.url()}`)
  })
}

async function settle(page) {
  await page.waitForSelector('.splash', { state: 'detached', timeout: 20000 }).catch(() => {})
  await page.evaluate(() => document.fonts.ready)
  await page.waitForTimeout(1200)
}

async function axe(page, where) {
  const res = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa']).analyze()
  for (const v of res.violations) {
    if (v.impact === 'serious' || v.impact === 'critical') problems.push(`${where}: axe ${v.impact} ${v.id} - ${v.nodes.map((n) => n.target.join(' ')).slice(0, 4).join(', ')}`)
  }
}

let edge1
let edge
for (const palette of PALETTES) {
  for (const mode of MODES) {
    const ctx = await context({}, palette, mode)
    const page = await ctx.newPage()
    let at = ''
    watch(page, () => `${palette}/${mode} ${at}`)
    at = 'sign-in'
    await page.goto(`${CONTROLLER}/`)
    await settle(page)
    await axe(page, `${palette}/${mode} sign-in`)
    await page.screenshot({ path: `${OUT}/${palette}-${mode}-signin.png` })
    await signIn(ctx)
    if (!edge1) edge1 = (await (await ctx.request.get(`${CONTROLLER}/api/nodes`)).json()).find((n) => n.name === 'edge-1').id
    if (!edge) edge = (await (await ctx.request.get(`${CONTROLLER}/api/clusters`)).json()).find((c) => c.name === 'edge').id
    for (const [name, url] of [
      ['nodes', '/'],
      ['hypervisors', '/#/hypervisors'],
      ['clusters', '/#/clusters'],
      ['cluster', `/#/clusters/${edge}`],
      ['consul', '/#/consul'],
      ...NODE_PAGES.map((p) => [`node${p.replace(/\//g, '-') || '-'}`, `/nodes/${edge1}/#${p}`]),
    ]) {
      at = name
      await page.goto(`${CONTROLLER}${url}`)
      await settle(page)
      await axe(page, `${palette}/${mode} ${name}`)
      await page.screenshot({ path: `${OUT}/${palette}-${mode}-${name}.png` })
    }
    // The node list as rows.
    at = 'nodes-list'
    await page.evaluate(() => localStorage.setItem('janus-nodes-view', 'list'))
    await page.goto(`${CONTROLLER}/`)
    await settle(page)
    await axe(page, `${palette}/${mode} nodes-list`)
    await page.screenshot({ path: `${OUT}/${palette}-${mode}-nodes-list.png` })
    await page.evaluate(() => localStorage.removeItem('janus-nodes-view'))
    // The picker, open.
    at = 'picker'
    await page.goto(`${CONTROLLER}/`)
    await settle(page)
    await page.click('.theme-picker > button')
    await page.waitForTimeout(300)
    await axe(page, `${palette}/${mode} picker`)
    await page.screenshot({ path: `${OUT}/${palette}-${mode}-picker.png` })
    await ctx.close()
  }
}

// A palette chosen in one tab: the node page open in another follows.
{
  const ctx = await context()
  await signIn(ctx)
  const main = await ctx.newPage()
  const node = await ctx.newPage()
  await main.goto(`${CONTROLLER}/`)
  await node.goto(`${CONTROLLER}/nodes/${edge1}/#/`)
  await settle(main)
  await settle(node)
  await main.click('.theme-picker > button')
  await main.click('.tp-palette:has-text("Nord")')
  await main.click('.tp-mode:has-text("Dark")')
  await node.waitForTimeout(300)
  const got = await node.evaluate(() => [document.documentElement.dataset.palette, document.documentElement.dataset.theme])
  if (got.join() !== 'nord,dark') problems.push(`another tab: ${got}`)
  await ctx.close()
}

// The palette shortcut: T the next, shift+T back, nothing while typing,
// nothing once turned off.
{
  const ctx = await context()
  await signIn(ctx)
  const page = await ctx.newPage()
  await page.goto(`${CONTROLLER}/nodes/${edge1}/#/`)
  await settle(page)
  const palette = () => page.evaluate(() => document.documentElement.dataset.palette)
  await page.mouse.move(900, 600)
  await page.keyboard.press('t')
  const next = await palette()
  const said = await page.locator('.tp-said').textContent().catch(() => '')
  await page.keyboard.press('Shift+T')
  const back = await palette()
  await page.goto(`${CONTROLLER}/nodes/${edge1}/#/tools/probe`)
  await settle(page)
  const field = page.locator('input:not([type=checkbox]):not([type=radio])').first()
  await field.click()
  await page.keyboard.type('tt')
  const typing = await palette()
  await page.mouse.click(900, 700)
  await page.click('.theme-picker > button')
  await page.click('.tp-shortcut input')
  await page.keyboard.press('Escape')
  await page.mouse.move(900, 600)
  await page.keyboard.press('t')
  const off = await palette()
  const got = { next, said, back, typing, off }
  if (next !== 'abysse' || !/Abysse/.test(said) || back !== 'janus' || typing !== 'janus' || off !== 'janus') problems.push(`shortcut: ${JSON.stringify(got)}`)
  await ctx.close()
}

// haproxy.cfg's history: the setup's apply made a version over the one
// found when it began; the oldest opens with its diff against the
// running configuration, and an admin is offered to go back to it.
{
  const ctx = await context()
  const page = await ctx.newPage()
  watch(page, () => 'config-history')
  await signIn(ctx)
  await page.goto(`${CONTROLLER}/nodes/${edge1}/#/haproxy/config`)
  await settle(page)
  const buttons = page.locator('.card', { has: page.getByText('History', { exact: true }) }).locator('button[aria-pressed]')
  const versions = await buttons.count()
  // Off the rail: the sidebar opens over the page under the pointer.
  await page.mouse.move(900, 600)
  await buttons.last().click()
  const diff = await page.waitForSelector('.diff', { timeout: 10000 }).then(() => true).catch(() => false)
  // edge-1 is a member of cluster edge, which manages HAProxy there: no
  // going back offered - the node would refuse it -, the lock said.
  const apply = await page.getByRole('button', { name: /Apply this version/ }).count()
  const locked = await page.locator('.cluster-lock', { hasText: 'is managed by cluster edge' }).count()
  if (versions < 2 || !diff || apply !== 0 || locked < 1)
    problems.push(`config history: ${versions} versions, a diff ${diff}, Apply offered ${apply} times, the cluster's lock said ${locked} times`)
  await page.screenshot({ path: `${OUT}/config-history.png`, fullPage: true })
  await ctx.close()
}

// A member of cluster edge says so: its top bar names the cluster and the
// member - a link to the cluster's page -, the Nodes page frames the
// cluster's members under its heading, in cards and as a list.
{
  const ctx = await context()
  const page = await ctx.newPage()
  watch(page, () => 'cluster membership')
  await signIn(ctx)
  await page.goto(`${CONTROLLER}/nodes/${edge1}/#/`)
  await settle(page)
  const badge = page.locator('.topbar .cluster-badge a', { hasText: 'edge · edge-1' })
  const href = (await badge.count()) ? await badge.getAttribute('href') : ''
  if (href !== `/#/clusters/${edge}`) problems.push(`edge-1's top bar: the cluster badge links to '${href}'`)
  await page.goto(`${CONTROLLER}/`)
  await settle(page)
  const group = page.locator('.cluster-group', { has: page.locator('.cluster-heading-name', { hasText: /^edge$/ }) })
  const cards = await group.locator('.node-card-name').allTextContents()
  if (cards.sort().join(',') !== 'edge-1,edge-2') problems.push(`the Nodes page: cluster edge frames ${JSON.stringify(cards)}`)
  await page.evaluate(() => localStorage.setItem('janus-nodes-view', 'list'))
  await page.goto(`${CONTROLLER}/`)
  await settle(page)
  const rows = await page.locator('tbody.cluster-rows', { has: page.locator('.cluster-heading-name', { hasText: /^edge$/ }) }).locator('.node-card-name').allTextContents()
  if (rows.sort().join(',') !== 'edge-1,edge-2') problems.push(`the Nodes list: cluster edge's rows ${JSON.stringify(rows)}`)
  await page.evaluate(() => localStorage.removeItem('janus-nodes-view'))
  await ctx.close()
}

// Cluster edge (setup.mjs): its two members; its configuration - version
// 2, not applied, opens with the change an apply would make -, the
// editor, the journal; and the new-cluster form, offering edge-3 alone.
{
  const ctx = await context()
  const page = await ctx.newPage()
  let at = 'cluster'
  watch(page, () => at)
  await signIn(ctx)
  const look = async (name) => {
    at = name
    await page.waitForTimeout(400)
    await axe(page, name)
    await page.screenshot({ path: `${OUT}/${name}.png`, fullPage: true })
  }
  await page.goto(`${CONTROLLER}/#/clusters/${edge}`)
  await settle(page)
  const members = await page.locator('tbody tr').count()
  if (members !== 2) problems.push(`cluster: ${members} members shown`)
  // The preview's nodes are containers, no machines: recycled into a
  // node picked, never a new machine.
  await page.getByRole('button', { name: 'Recycle' }).first().click()
  await page.waitForSelector('.card-title:has-text("Recycle")')
  if (!(await page.getByRole('radio', { name: /A new machine like it/ }).isDisabled())) problems.push('cluster recycle: a new machine offered for a node that is no machine')
  await look('cluster-recycle')
  await page.getByRole('button', { name: 'Cancel' }).click()
  await page.getByRole('tab', { name: 'Configuration' }).click()
  await page.waitForSelector('.version-list button')
  const versions = await page.locator('.version-list button').count()
  await page.locator('.version-list button', { hasText: 'v2' }).click()
  await page.waitForSelector('.config-view')
  await look('cluster-configuration')
  await page.getByRole('button', { name: 'Apply v2…' }).click()
  const change = await page.waitForSelector('.modal .diff', { timeout: 10000 }).then((d) => d.textContent()).catch(() => '')
  if (versions !== 2 || !change.includes('timeout client 45s')) problems.push(`cluster configuration: ${versions} versions, the change to apply: ${JSON.stringify(change.slice(0, 200))}`)
  await look('cluster-apply')
  await page.getByRole('button', { name: 'Cancel' }).click()
  await page.getByRole('button', { name: 'Edit' }).click()
  await page.waitForSelector('.editor textarea')
  await look('cluster-edit')
  await page.getByRole('button', { name: 'Cancel' }).click()
  await page.getByRole('tab', { name: 'Journal' }).click()
  await page.waitForSelector('tbody tr')
  const entries = await page.locator('tbody tr').count()
  if (entries < 2) problems.push(`cluster journal: ${entries} entries`)
  await look('cluster-journal')
  await page.goto(`${CONTROLLER}/#/clusters`)
  await settle(page)
  await page.getByRole('button', { name: 'New cluster' }).click()
  await page.waitForSelector('form tbody tr')
  const offered = await page.locator('form tbody tr').allTextContents()
  if (offered.length !== 1 || !offered[0].includes('edge-3')) problems.push(`new cluster: offered ${JSON.stringify(offered)}`)
  // No hypervisor in the preview: a cluster of new machines isn't offered.
  if (!(await page.getByRole('radio', { name: /New machines/ }).isDisabled())) problems.push('new cluster: new machines offered without a hypervisor')
  await look('cluster-new')
  await ctx.close()
}

// Motion: none under "reduce motion"; the charts' transform moving
// otherwise.
for (const reducedMotion of ['reduce', 'no-preference']) {
  const ctx = await context({ reducedMotion })
  await ctx.addInitScript(() => {
    localStorage.setItem('janus-refresh', '1000')
    window.__frames = 0
    const raf = window.requestAnimationFrame.bind(window)
    window.requestAnimationFrame = (fn) => {
      window.__frames++
      return raf(fn)
    }
  })
  await signIn(ctx)
  const page = await ctx.newPage()
  await page.goto(`${CONTROLLER}/nodes/${edge1}/#/metrics`)
  await settle(page)
  await page.waitForTimeout(4000)
  const transform = () => page.evaluate(() => document.querySelector('.chart svg g[clip-path] > g')?.getAttribute('transform'))
  const before = [await page.evaluate(() => window.__frames), await transform()]
  await page.waitForTimeout(1000)
  const after = [await page.evaluate(() => window.__frames), await transform()]
  if (reducedMotion === 'reduce' && after[0] - before[0] > 0) problems.push(`reduce motion: ${after[0] - before[0]} frames in 1 s`)
  if (reducedMotion !== 'reduce' && (after[0] - before[0] < 20 || before[1] === after[1])) problems.push(`motion: ${after[0] - before[0]} frames, transform ${before[1]} -> ${after[1]}`)
  await ctx.close()
}

// The sidebar: a rail opening over the page, folding when left, opened
// by the keyboard, pinned; a phone's drawer.
{
  const ctx = await context()
  await signIn(ctx)
  const page = await ctx.newPage()
  await page.goto(`${CONTROLLER}/nodes/${edge1}/#/`)
  await settle(page)
  const width = () => page.evaluate(() => document.querySelector('.sidebar').getBoundingClientRect().width)
  const mainLeft = () => page.evaluate(() => document.querySelector('.main').getBoundingClientRect().left)
  await page.mouse.move(900, 500)
  await page.waitForTimeout(500)
  const folded = await width()
  await page.mouse.move(25, 300)
  await page.waitForTimeout(700)
  const hovered = [await width(), await mainLeft()]
  await page.click('a.nav-item[href="#/metrics"]')
  await page.mouse.move(900, 500)
  await page.waitForTimeout(700)
  const left = await width()
  await page.keyboard.press('Tab')
  await page.keyboard.press('Tab')
  await page.waitForTimeout(500)
  const keyboard = await width()
  await page.mouse.move(25, 300)
  await page.waitForTimeout(600)
  await page.click('.rail-pin')
  await page.mouse.move(900, 500)
  await page.waitForTimeout(600)
  const pinned = [await width(), await mainLeft()]
  const got = { folded, hovered, left, keyboard, pinned }
  if (folded !== 56 || hovered[0] !== 236 || hovered[1] !== 56 || left !== 56 || keyboard !== 236 || pinned[0] !== 236 || pinned[1] !== 236) problems.push(`sidebar: ${JSON.stringify(got)}`)
  await ctx.close()
  const phone = await context({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true })
  await signIn(phone)
  const p = await phone.newPage()
  await p.goto(`${CONTROLLER}/nodes/${edge1}/#/`)
  await settle(p)
  await p.click('.nav-toggle')
  await p.waitForTimeout(400)
  const drawer = await p.evaluate(() => document.querySelector('.sidebar').getBoundingClientRect().toJSON())
  if (drawer.left !== 0 || drawer.width < 200) problems.push(`phone drawer: ${JSON.stringify(drawer)}`)
  await p.screenshot({ path: `${OUT}/phone-drawer.png` })
  await phone.close()
}

// Settings, the hypervisors' and machines' lists, a long trust error and
// the enrollment tokens' links. What the preview has none of - a
// hypervisor, a machine, a node out of the fleet - comes from stubbed
// answers (page.route); the account's preferences are put back after.
{
  const ctx = await context()
  await signIn(ctx)
  const page = await ctx.newPage()
  watch(page, () => 'settings and lists')
  const prefs = (body) => ctx.request.put(`${CONTROLLER}/api/me/preferences`, { data: body })
  const counted = (path) => {
    let n = 0
    const on = (r) => {
      if (new URL(r.url()).pathname === path) n++
    }
    page.on('request', on)
    return () => {
      page.off('request', on)
      return n
    }
  }
  // The Settings page.
  await page.goto(`${CONTROLLER}/#/settings`)
  await settle(page)
  await axe(page, 'settings')
  await page.screenshot({ path: `${OUT}/settings.png`, fullPage: true })
  if (!(await page.getByText('HTTPS certificate', { exact: false }).count())) problems.push('settings: no HTTPS certificate for an admin')
  // Node figures off: the Nodes page asks for none, draws none; the
  // charts' box can't be ticked.
  await page.getByLabel(/Node figures/).uncheck()
  await page.waitForTimeout(500)
  if (!(await page.getByLabel(/Node charts/).isDisabled())) problems.push('settings: node charts still offered without the figures')
  await page.goto(`${CONTROLLER}/`)
  await settle(page)
  let stop = counted('/api/nodes/metrics')
  await page.waitForTimeout(6000)
  let n = stop()
  const sparks = await page.locator('.sparks').count()
  if (n !== 0 || sparks !== 0) problems.push(`node figures off: ${n} metrics requests in 6 s, ${sparks} figure blocks`)
  // Figures on, charts off: the figures, no curve.
  await prefs({ node_charts: false })
  await page.reload()
  await settle(page)
  await page.waitForTimeout(3000)
  const figures = await page.locator('.sparks .spark-value').count()
  const curves = await page.locator('.sparks .spark-cell svg, .sparks .spark-cell canvas').count()
  if (!figures || curves) problems.push(`node charts off: ${figures} figures, ${curves} curves`)
  await page.screenshot({ path: `${OUT}/nodes-figures-only.png` })
  // The nodes asked every 5 s, then every 60 s.
  await prefs({ nodes_refresh_seconds: 5 })
  await page.reload()
  await settle(page)
  stop = counted('/api/nodes/status')
  await page.waitForTimeout(11500)
  const fast = stop()
  await prefs({ nodes_refresh_seconds: 60 })
  await page.reload()
  await settle(page)
  stop = counted('/api/nodes/status')
  await page.waitForTimeout(11500)
  const slow = stop()
  if (fast < 2 || slow > 0) problems.push(`nodes refreshed: ${fast} status requests in 11.5 s at 5 s, ${slow} at 60 s`)
  // Traffic in bits, times in UTC: the Nodes page's traffic, a node
  // page's events.
  await prefs({ rate_unit: 'bits', time_zone: 'utc' })
  await page.reload()
  await settle(page)
  await page.waitForTimeout(3000)
  const traffic = await page.locator('.sparks .spark-value').nth(2).textContent()
  await page.goto(`${CONTROLLER}/nodes/${edge1}/#/events`)
  await settle(page)
  const eventTimes = (await page.locator('main').textContent()).match(/\d{1,2}:\d{2}(:\d{2})?\s?(AM|PM)? UTC/g) || []
  if (!/bit\/s$/.test(traffic || '') || !eventTimes.length) problems.push(`bits and UTC: the traffic reads '${traffic}', ${eventTimes.length} times in UTC on edge-1's events`)
  // An admin's floor: 30 s at least - the choices start there, an
  // account at 5 s follows it.
  await prefs({ nodes_refresh_seconds: 5 })
  await ctx.request.put(`${CONTROLLER}/api/settings`, { data: { nodes_refresh_floor_seconds: 30 } })
  const floored = await (await ctx.request.get(`${CONTROLLER}/api/me/preferences`)).json()
  await page.goto(`${CONTROLLER}/#/settings`)
  await settle(page)
  const first = await page.getByLabel('Nodes refreshed every').locator('option').first().textContent()
  if (floored.nodes_refresh_seconds !== 30 || !first.startsWith('30 s')) problems.push(`a 30 s floor: the account at ${floored.nodes_refresh_seconds} s, the first choice '${first}'`)
  await page.screenshot({ path: `${OUT}/settings-floor.png`, fullPage: true })
  await ctx.request.put(`${CONTROLLER}/api/settings`, { data: { nodes_refresh_floor_seconds: 0 } })
  await prefs({})

  // A node whose fleet trust failed with a long error, on a phone: the
  // card holds it.
  const edges = (await (await ctx.request.get(`${CONTROLLER}/api/nodes`)).json()).map((x) => x.id)
  const long = 'latest balancer error: connection error: desc = "transport: Error while dialing: dial tcp 172.16.1.78:9505: connect: connection refused" ' + 'x'.repeat(120)
  await page.route('**/api/fleet', async (route) => {
    const r = await route.fetch()
    const f = await r.json()
    f.nodes = { ...(f.nodes || {}), [edges[0]]: { state: 'error', error: long } }
    await route.fulfill({ response: r, json: f })
  })
  await page.setViewportSize({ width: 360, height: 800 })
  await page.goto(`${CONTROLLER}/`)
  await settle(page)
  const spill = await page.evaluate(() =>
    [...document.querySelectorAll('.node-card')].flatMap((card) => {
      const edge = card.getBoundingClientRect().right + 1
      return [...card.querySelectorAll('*')].filter((e) => e.getBoundingClientRect().right > edge).map((e) => `${e.tagName.toLowerCase()}.${e.className}`)
    }),
  )
  const title = await page.locator('.trust-error').first().getAttribute('title')
  if (spill.length || title !== long) problems.push(`a long trust error at 360 px: ${spill.slice(0, 4).join(', ')} out of its card, its title ${title === long ? 'whole' : 'cut'}`)
  await page.screenshot({ path: `${OUT}/nodes-trust-error-phone.png`, fullPage: true })
  await page.unroute('**/api/fleet')
  await page.setViewportSize({ width: 1440, height: 900 })

  // Add a node → an enrollment token: the card comes into view.
  await page.goto(`${CONTROLLER}/`)
  await settle(page)
  await page.getByRole('button', { name: /Add node/ }).click()
  await page.getByRole('link', { name: /with an enrollment token/ }).click()
  await page.waitForTimeout(800)
  const inView = await page.locator('.scroll-anchor').evaluate((e) => {
    const r = e.getBoundingClientRect()
    return r.top >= 0 && r.top < window.innerHeight
  })
  if (!page.url().endsWith('#/enrollment-tokens') || !inView) problems.push(`Add node → enrollment token: ${page.url()}, the card in view ${inView}`)

  // The hypervisors and machines, as cards and as lists.
  const GiB = 2 ** 30
  const hv = { id: 'hv1', name: 'kvm-par-1', kind: 'libvirt', trusted: true, machines: 1, libvirt: { user: 'janus', host: 'kvm-par-1.example.net', networks: ['default'] } }
  const pve = { id: 'hv2', name: 'pve-par-2', kind: 'proxmox', trusted: true, machines: 0, proxmox: { node: 'pve2', token_id: 'janus@pve!controller', networks: ['vmbr0'] } }
  const status = {
    cpu_percent: 12,
    host: {
      hostname: 'kvm-par-1', hypervisor_version: 'QEMU 10.0.2', library_version: 'libvirt 11.3.0', cpus: 16, cpu_mhz: 2900,
      memory_total: 64 * GiB, memory_free: 20 * GiB,
      storage: { name: 'janus', capacity: 900 * GiB, allocated: 300 * GiB, available: 600 * GiB, active: true },
      networks: [{ name: 'default', active: true }], running_machines: 1,
    },
    machines: { m1: { power: 'running' } },
  }
  const machine = {
    id: 'm1', phase: 'ready', vm_name: 'janus-edge-par-9', hypervisor_name: 'kvm-par-1', node_address: '10.0.10.29:9505',
    error: 'resize: the host keeps 512 MiB free - asked for 4096 MiB with 3200 MiB free',
    spec: { name: 'edge-par-9', hypervisor_id: 'hv1', vcpus: 2, memory_mib: 2048, nics: [{ mac: '52:54:00:00:00:09', name: 'eth0', network: 'default', mode: 'static', addresses: ['10.0.10.29/24'] }] },
    events: [{ time: new Date().toISOString(), message: 'ready: registered as edge-par-9' }],
  }
  await page.route('**/api/hypervisors', (r) => (r.request().method() === 'GET' ? r.fulfill({ json: [hv, pve] }) : r.continue()))
  await page.route('**/api/hypervisors/hv1/status', (r) => r.fulfill({ json: status }))
  await page.route('**/api/hypervisors/hv2/status', (r) => r.fulfill({ json: { error: 'Proxmox: 401 authentication failure' } }))
  await page.route('**/api/machines', (r) => (r.request().method() === 'GET' ? r.fulfill({ json: [machine] }) : r.continue()))
  await page.goto(`${CONTROLLER}/#/hypervisors`)
  await settle(page)
  await page.screenshot({ path: `${OUT}/hypervisors-cards.png`, fullPage: true })
  for (const label of ['Show the hypervisors as', 'Show the machines as']) await page.getByRole('group', { name: label }).getByRole('button', { name: 'List' }).click()
  await page.reload()
  await settle(page)
  await axe(page, 'hypervisors as lists')
  const rows = await page.locator('table.node-table .node-card-name').allTextContents()
  const errors = await page.locator('table.node-table .table-detail .error-box').count()
  if (rows.join(',') !== 'kvm-par-1,pve-par-2,edge-par-9' || errors !== 2) problems.push(`hypervisors as lists, remembered: rows ${JSON.stringify(rows)}, ${errors} error rows`)
  await page.screenshot({ path: `${OUT}/hypervisors-lists.png`, fullPage: true })
  await page.evaluate(() => {
    localStorage.removeItem('janus-hypervisors-view')
    localStorage.removeItem('janus-machines-view')
  })
  await ctx.close()
}

await browser.close()
if (problems.length) {
  console.log(problems.join('\n'))
  process.exit(1)
}
console.log(`ui-preview check: ${PALETTES.length} palettes × 2 modes, ${NODE_PAGES.length + 7} pages each, the appearance, its shortcut, motion, the sidebar, a cluster, Settings and the hypervisors' lists - no problem`)
