// hack/ui-preview's pages walked in a browser (hack/ui-preview.sh
// check): every palette in both modes - the sign-in screen, the node
// list (cards and rows), the hypervisors tab, the clusters and cluster
// edge, and pages of edge-1 - with no page or
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
const NODE_PAGES = ['/', '/metrics', '/storage', '/flow', '/network', '/haproxy', '/haproxy/config', '/system/network', '/system/update']
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

function watch(page, where) {
  page.on('pageerror', (e) => problems.push(`${where()}: ${e.message}`))
  page.on('console', (m) => {
    if (m.type() === 'error' || m.text().includes('[Report Only]')) problems.push(`${where()}: console ${m.type()}: ${m.text()}`)
  })
  page.on('response', (r) => {
    if (r.status() >= 400 && !r.url().includes('/api/auth/status')) problems.push(`${where()}: ${r.status()} ${r.url()}`)
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
  const apply = await page.getByRole('button', { name: /Apply this version/ }).count()
  if (versions < 2 || !diff || apply !== 1) problems.push(`config history: ${versions} versions, a diff ${diff}, Apply offered ${apply} times`)
  await page.screenshot({ path: `${OUT}/config-history.png`, fullPage: true })
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

await browser.close()
if (problems.length) {
  console.log(problems.join('\n'))
  process.exit(1)
}
console.log(`ui-preview check: ${PALETTES.length} palettes × 2 modes, ${NODE_PAGES.length + 7} pages each, the appearance, its shortcut, motion, the sidebar and a cluster - no problem`)
