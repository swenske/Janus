// The docs and the site against their budget: Lighthouse, on a desktop
// and on a phone, on pages of each kind - the docs' home, a plain page,
// one with diagrams, one with screenshots; the landing page and the image
// builder when they're served with their data (a deployed site).
//
//   node lighthouse.mjs <site URL> [pages...]
//
// Prints each page's scores and the JavaScript it loads (gzipped), and
// fails on a page under its budget.
import { chromium } from 'playwright'
import lighthouse from 'lighthouse'
import desktopConfig from 'lighthouse/core/config/desktop-config.js'

const site = (process.argv[2] ?? 'http://127.0.0.1:18600').replace(/\/$/, '')
const pages = process.argv.length > 3 ? process.argv.slice(3) : ['/docs/', '/docs/guide/quickstart/', '/docs/internals/boot/', '/docs/guide/controller/']

// The budget: scores out of 100, by form factor - and the JavaScript a
// docs page without diagrams may load (the landing page and the builder
// are a React application, 80 KB). Mermaid's renderer, loaded only where
// there's a diagram, is 250 KB: such a page measured 89 to 92 on a phone
// where the others measure 97 to 100, hence its own floor.
const BUDGET = {
  desktop: { performance: 95, accessibility: 100, 'best-practices': 100, seo: 100 },
  mobile: { performance: 90, accessibility: 100, 'best-practices': 100, seo: 100 },
}
const MOBILE_WITH_DIAGRAMS = 85
const JS_KB = 50
const PORT = 9333

const browser = await chromium.launch({ args: [`--remote-debugging-port=${PORT}`] })
const problems = []
for (const path of pages) {
  for (const form of ['desktop', 'mobile']) {
    const { lhr } = await lighthouse(
      site + path,
      { port: PORT, output: 'json', logLevel: 'error', onlyCategories: Object.keys(BUDGET[form]) },
      form === 'desktop' ? desktopConfig : undefined,
    )
    const scores = Object.fromEntries(Object.entries(lhr.categories).map(([k, c]) => [k, Math.round((c.score ?? 0) * 100)]))
    const js = lhr.audits['resource-summary']?.details?.items?.find((i) => i.resourceType === 'script')?.transferSize ?? 0
    const diagrams = lhr.audits['network-requests']?.details?.items?.some((i) => /mermaid/i.test(i.url))
    console.log(`${path} (${form}): ${Object.entries(scores).map(([k, v]) => `${k} ${v}`).join(', ')} - JavaScript ${Math.round(js / 1024)} KB${diagrams ? ' (diagrams)' : ''}`)
    for (const [k, floor] of Object.entries(BUDGET[form])) {
      const min = form === 'mobile' && k === 'performance' && diagrams ? MOBILE_WITH_DIAGRAMS : floor
      if (scores[k] < min) {
        const failing = Object.values(lhr.audits).filter((a) => a.score !== null && a.score < 1 && lhr.categories[k].auditRefs.some((r) => r.id === a.id && r.weight > 0))
        problems.push(`${path} (${form}): ${k} ${scores[k]}, under ${min} - ${failing.map((a) => a.id).join(', ')}`)
      }
    }
    if (path.startsWith('/docs/') && !diagrams && js > JS_KB * 1024) problems.push(`${path} (${form}): ${Math.round(js / 1024)} KB of JavaScript, over ${JS_KB}`)
  }
}
await browser.close()
if (problems.length) {
  console.log(`lighthouse: ${problems.length} page(s) over budget:\n  ${problems.join('\n  ')}`)
  process.exit(1)
}
console.log(`lighthouse: ${pages.length} pages within budget, on a desktop and a phone`)
