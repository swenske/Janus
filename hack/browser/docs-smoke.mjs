// The docs site in a real browser (make docs-smoke): every page of a
// channel, reached by following the site's own links from its home, in
// both themes - no console error or warning, no page error, no CSP
// violation, every image loaded, every Mermaid diagram drawn; no
// horizontal scroll on a phone; search answering; axe finding nothing
// serious.
//   node docs-smoke.mjs <site URL> <channel path: /docs/ or /docs/next/>
import AxeBuilder from '@axe-core/playwright'
import { chromium } from 'playwright'

const [site, root] = process.argv.slice(2)
const problems = []
const browser = await chromium.launch()

// Queries a reader would type, and a page title the results must hold.
const searches = [
  ['keepal', 'VRRP: the keepalived extension'],
  ['nftables', 'Firewall: the nftables extension'],
  ['opentofu', 'Terraform: Janus nodes as code'],
]

async function newPage(scheme, viewport = { width: 1440, height: 900 }) {
  const ctx = await browser.newContext({ colorScheme: scheme, viewport, reducedMotion: 'reduce' })
  const page = await ctx.newPage()
  page.on('console', (m) => {
    if (m.type() === 'error' || m.type() === 'warning') problems.push(`${page.url()} (${scheme}): console ${m.type()}: ${m.text()}`)
  })
  page.on('pageerror', (e) => problems.push(`${page.url()} (${scheme}): ${e.message}`))
  page.on('response', (r) => {
    if (r.status() >= 400) problems.push(`${page.url()} (${scheme}): ${r.status()} for ${r.url()}`)
  })
  await page.addInitScript(() =>
    document.addEventListener('securitypolicyviolation', (e) =>
      console.error(`CSP violation: ${e.violatedDirective} blocked ${e.blockedURI || 'an inline script'}`),
    ),
  )
  return { ctx, page }
}

// visit loads a page, checks it, and returns the docs links it holds.
async function visit(page, url, scheme) {
  const resp = await page.goto(url, { waitUntil: 'networkidle' })
  if (resp.status() !== 200) problems.push(`${url}: ${resp.status()}`)
  const diagrams = await page.locator('pre.mermaid').count()
  if (diagrams) {
    await page
      .waitForFunction(() => [...document.querySelectorAll('pre.mermaid')].every((p) => p.querySelector('svg')), null, { timeout: 15000 })
      .catch(() => problems.push(`${url} (${scheme}): a Mermaid diagram wasn't drawn`))
    if (await page.locator('pre.mermaid :text("Syntax error")').count()) problems.push(`${url}: a Mermaid syntax error`)
  }
  const broken = await page.evaluate(() => [...document.images].filter((i) => !i.complete || i.naturalWidth === 0).map((i) => i.src))
  for (const src of broken) problems.push(`${url}: image not loaded: ${src}`)
  return page.evaluate(
    (root) => [...document.querySelectorAll('a[href]')].map((a) => new URL(a.href, location.href)).filter((u) => u.origin === location.origin && u.pathname.startsWith(root)).map((u) => u.origin + u.pathname),
    root,
  )
}

// Every page: from the home, follow the links (sidebar included).
const pages = new Set()
for (const scheme of ['light', 'dark']) {
  const { ctx, page } = await newPage(scheme)
  const queue = [site + root]
  const seen = new Set(queue)
  while (queue.length) {
    const url = queue.shift()
    for (const next of await visit(page, url, scheme)) {
      if (/\/pagefind\/|\.(xml|txt|json)$/.test(next) || seen.has(next)) continue
      seen.add(next)
      queue.push(next)
    }
  }
  for (const u of seen) pages.add(u)
  console.log(`${scheme}: ${seen.size} pages`)

  // Search: Ctrl+K, a query, the expected page among the results.
  await page.goto(site + root, { waitUntil: 'networkidle' })
  for (const [query, title] of searches) {
    await page.keyboard.press('Control+k')
    const input = page.locator('dialog[open] input[type="search"], dialog[open] input').first()
    await input.waitFor()
    await input.fill(query)
    const found = await page
      .locator('dialog[open] .pagefind-ui__result-link', { hasText: title })
      .first()
      .waitFor({ timeout: 10000 })
      .then(() => true)
      .catch(() => false)
    if (!found) problems.push(`search (${scheme}): "${query}" didn't find "${title}"`)
    await page.keyboard.press('Escape')
  }
  await ctx.close()
}

// A phone: nothing wider than the screen.
{
  const { ctx, page } = await newPage('light', { width: 390, height: 844 })
  for (const url of pages) {
    await page.goto(url, { waitUntil: 'networkidle' })
    const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)
    if (over > 1) problems.push(`${url} (phone): ${over}px wider than the screen`)
  }
  await ctx.close()
}

// Accessibility: axe on every page, serious and critical issues only.
{
  const { ctx, page } = await newPage('light')
  for (const url of pages) {
    await page.goto(url, { waitUntil: 'networkidle' })
    const { violations } = await new AxeBuilder({ page }).analyze()
    for (const v of violations.filter((v) => v.impact === 'serious' || v.impact === 'critical')) {
      problems.push(`${url}: axe ${v.impact} ${v.id} (${v.help}) - ${v.nodes.map((n) => n.target.join(' ')).slice(0, 3).join(', ')}`)
    }
  }
  await ctx.close()
}

await browser.close()
if (problems.length) {
  console.error(`docs smoke: ${problems.length} problem(s):\n  ${[...new Set(problems)].join('\n  ')}`)
  process.exit(1)
}
console.log(`docs smoke: ${pages.size} pages of ${root} - both themes, a phone, search, axe: all good`)
