// The OpenGraph cards (1200x630) a link to the site or the docs shows
// when it's shared: the landing page's, the image builder's, the docs'
// home's and one per docs audience (site/docs/structure.yaml's topics),
// drawn in the brand's colours with its symbol (brand/favicon). make
// docs-og writes them: site/frontend/public/og/ and site/docs/public/og/.
import { chromium } from 'playwright'
import fs from 'node:fs'
import YAML from 'yaml'

const { topics } = YAML.parse(fs.readFileSync('/structure.yaml', 'utf8'))
// The symbol, in the dark theme's colours.
const symbol = fs
  .readFileSync('/favicon.svg', 'utf8')
  .replace(/<style>.*<\/style>/, '')
  .replace(/class="t"/g, 'fill="#F3EFE6"')
  .replace(/class="b"/g, 'fill="#D8643C"')

const cards = [
  {
    out: '/out/site/og/janus.png',
    title: 'An immutable Linux for your HAProxy load balancers',
    text: 'No shell, no SSH, signed A/B updates with rollback, SELinux enforcing - every change through an mTLS API.',
    url: 'janus.sw-servers.net',
  },
  {
    out: '/out/site/og/builder.png',
    kicker: 'Image builder',
    title: 'Build your Janus image',
    text: 'Proxmox, KVM, VMware, an installer ISO or a Raspberry Pi - with the extensions you need, signed, kept up to date.',
    url: 'janus.sw-servers.net/builder',
  },
  {
    out: '/out/docs/og/docs.png',
    kicker: 'Documentation',
    title: 'Janus docs',
    text: 'Install it, run it, build on it: guides, how it works inside, and how to contribute.',
    url: 'janus.sw-servers.net/docs',
  },
  ...topics.map((t) => ({ out: `/out/docs/og/${t.id}.png`, kicker: 'Documentation', title: t.label, text: t.description, url: 'janus.sw-servers.net/docs' })),
]

const esc = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;')
const page = (c) => `<!doctype html><html><head><meta charset="utf-8"><style>
  html, body { margin: 0; }
  body { width: 1200px; height: 630px; box-sizing: border-box; padding: 70px 84px 64px 96px; background: #131211; color: #F3EFE6;
         font-family: Inter, sans-serif; display: flex; flex-direction: column; justify-content: space-between; position: relative; }
  .bar { position: absolute; left: 0; top: 0; bottom: 0; width: 16px; background: #D8643C; }
  .brand { display: flex; align-items: center; gap: 20px; font-size: 54px; font-weight: 700; letter-spacing: -0.02em; }
  .brand svg { width: 78px; height: 78px; }
  .kicker { color: #D8643C; font-size: 28px; font-weight: 600; letter-spacing: 0.06em; text-transform: uppercase; margin-bottom: 14px; }
  h1 { margin: 0; font-size: 66px; line-height: 1.06; font-weight: 700; letter-spacing: -0.025em; max-width: 1000px; }
  p { margin: 24px 0 0; font-size: 31px; line-height: 1.32; color: #B5AFA3; max-width: 980px; }
  .url { font-size: 26px; color: #8F897E; }
</style></head><body>
  <div class="bar"></div>
  <div class="brand">${symbol}<span>Janus</span></div>
  <div>${c.kicker ? `<div class="kicker">${esc(c.kicker)}</div>` : ''}<h1>${esc(c.title)}</h1><p>${esc(c.text)}</p></div>
  <div class="url">${esc(c.url)}</div>
</body></html>`

const browser = await chromium.launch({ args: ['--disable-font-subpixel-positioning'] })
const tab = await (await browser.newContext({ viewport: { width: 1200, height: 630 }, deviceScaleFactor: 1 })).newPage()
for (const c of cards) {
  await tab.setContent(page(c))
  await tab.evaluate(() => document.fonts.ready)
  fs.mkdirSync(c.out.slice(0, c.out.lastIndexOf('/')), { recursive: true })
  fs.writeFileSync(c.out, await tab.screenshot({ type: 'png' }))
  console.log(`og: ${c.out.replace('/out/', '')}`)
}
await browser.close()
