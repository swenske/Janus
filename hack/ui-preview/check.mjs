// hack/ui-preview's pages walked in a browser (hack/ui-preview.sh
// check): every palette in both modes - the sign-in screen, the node
// list, the hypervisors tab, and pages of edge-1 - with no page or
// console error, no failed request, no CSP report, and nothing serious
// for axe (WCAG 2 A/AA); a screenshot of each in $OUT. Then what the
// appearance and the sidebar do: a palette chosen in one tab followed by
// another, no animation frame under "reduce motion", the charts moving
// otherwise, the sidebar opening over the page and pinned. Run by
// hack/ui-preview.sh in the browser image, on the preview's network.
import AxeBuilder from '@axe-core/playwright'
import { chromium } from 'playwright'

const { CONTROLLER, PASSWORD, OUT } = process.env
const PALETTES = (process.env.PALETTES || 'janus,nord,abysse,tokyo-night,catppuccin').split(',')
const MODES = ['light', 'dark']
const NODE_PAGES = ['/', '/metrics', '/storage', '/flow', '/haproxy', '/system/update']
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
    for (const [name, url] of [
      ['nodes', '/'],
      ['hypervisors', '/#/hypervisors'],
      ...NODE_PAGES.map((p) => [`node${p.replace(/\//g, '-') || '-'}`, `/nodes/${edge1}/#${p}`]),
    ]) {
      at = name
      await page.goto(`${CONTROLLER}${url}`)
      await settle(page)
      await axe(page, `${palette}/${mode} ${name}`)
      await page.screenshot({ path: `${OUT}/${palette}-${mode}-${name}.png` })
    }
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
console.log(`ui-preview check: ${PALETTES.length} palettes × 2 modes, ${NODE_PAGES.length + 4} pages each, the appearance, motion and the sidebar - no problem`)
