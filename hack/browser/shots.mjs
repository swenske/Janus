// The Controller's screenshots for the docs and the landing page (make
// docs-screenshots - hack/docs-screenshots.sh starts the Controller and
// the nodes). The Controller is set up through its own API: the nodes
// added, labelled, in a fleet, running examples/haproxy/web.cfg, a few
// accounts and tokens. Then each shot of shots.yaml is taken in both
// themes and kept as WebP - rewritten only when it changed.
//
// An image mustn't depend on the machine or the moment that took it, or
// every run would rewrite them all:
// - the pages run on a paused clock (shots.yaml's `clock`), and every
//   time the Controller sends is moved with it, to the minute: the
//   setup happened two minutes before, a token made then expires 90
//   days after;
// - what a container shares with its host - CPUs, memory, kernel, boot
//   time - is shown as shots.yaml's small VM, and the charts' counters
//   are a fixed series, sampled every 5 s of the page's clock;
// - fingerprints and token IDs, new at every setup, are fixed ones.
// What might still change can be left out of the comparison (a shot's
// `ignore`); images.mjs says what the comparison tolerates.
import { chromium, request } from 'playwright'
import crypto from 'node:crypto'
import fs from 'node:fs'
import https from 'node:https'
import YAML from 'yaml'
import { diff, toWebp } from './images.mjs'

const CONTROLLER = process.env.CONTROLLER
const NODES = Object.fromEntries(process.env.NODES.split(',').map((kv) => kv.split('=')))
const MODE = process.env.SHOTS_MODE || 'write'
const THRESHOLD = Number(process.env.SHOTS_THRESHOLD || 0.00015)
const ONLY = (process.env.SHOTS_ONLY || '').split(',').filter(Boolean)
const PASSWORD = 'screenshots-admin-password'
const spec = YAML.parse(fs.readFileSync('shots.yaml', 'utf8'))
const T0 = Date.parse(spec.clock)
const KERNEL = fs.readFileSync('/versions.mk', 'utf8').match(/^KERNEL_VERSION\s*:=\s*(\S+)/m)[1]
const DPR = 2

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

// --- the Controller, set up through its API ---

const api = await request.newContext({ baseURL: CONTROLLER, ignoreHTTPSErrors: true })
async function call(method, path, body) {
  for (let i = 0; ; i++) {
    let resp
    try {
      resp = await api.fetch(path, { method, data: body })
    } catch (err) {
      if (i > 60) throw err
      await sleep(1000)
      continue
    }
    const text = await resp.text()
    if (!resp.ok()) throw new Error(`${method} ${path}: ${resp.status()} ${text}`)
    return text ? JSON.parse(text) : null
  }
}
async function until(what, fn, seconds = 120) {
  for (let i = 0; i < seconds; i++) {
    if (await fn()) return
    await sleep(1000)
  }
  throw new Error(`waited ${seconds} s for ${what}`)
}

const SETUP = Date.now()
await call('POST', '/api/auth/setup', { password: PASSWORD, mfa_required: 'nobody' })
const ids = {}
const labels = {
  'edge-par-1': { site: 'par', team: 'web' },
  'edge-par-2': { site: 'par', team: 'web' },
  'edge-par-3': { site: 'par', team: 'api' },
}
for (const [name, ip] of Object.entries(NODES)) {
  const admin = fs.readFileSync(`/creds/${name}.admin`, 'utf8')
  const node = await call('POST', '/api/nodes', {
    name,
    address: `${ip}:9505`,
    ca_cert_pem: fs.readFileSync(`/creds/${name}.ca`, 'utf8'),
    bootstrap_cert_pem: admin.match(/-----BEGIN CERTIFICATE-----[\s\S]+?-----END CERTIFICATE-----\n/)[0],
    bootstrap_key_pem: admin.match(/-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]+?-----END [A-Z ]*PRIVATE KEY-----\n/)[0],
  })
  ids[name] = node.id
  await call('PATCH', `/api/nodes/${node.id}`, { labels: labels[name] })
}
const fleet = await call('POST', '/api/fleet/setup')
const kit = await (await api.get('/api/fleet/recovery-kit')).text()
await call('POST', '/api/fleet/confirm', { kit, passphrase: fleet.passphrase })
await until('the nodes to trust the fleet', async () => {
  const f = await call('GET', '/api/fleet')
  return Object.values(ids).every((id) => f.nodes?.[id]?.state === 'trusted')
})
const web = fs.readFileSync('/examples/haproxy/web.cfg', 'utf8')
for (const id of Object.values(ids)) {
  await call('POST', `/nodes/${id}/api/haproxy/config`, { config: web })
  await until('the application servers to be up', async () => {
    const b = await call('GET', `/nodes/${id}/api/haproxy/backends`)
    const servers = (b.backends || []).flatMap((x) => x.servers || [])
    return servers.length > 0 && servers.every((s) => s.state.toLowerCase() === 'up')
  })
}
for (const user of [
  { name: 'alice', role: 'operator', password: 'alice-given-password' },
  { name: 'rita', role: 'reader', password: 'rita-given-password' },
  { name: 'web-dev', role: 'none', password: 'web-dev-given-password', grants: [{ role: 'operator', selector: { team: 'web' }, domains: ['haproxy'] }] },
])
  await call('POST', '/api/users', user)
await call('POST', '/api/tokens', { name: 'terraform', expires_in_days: 90, role: 'operator' })
await call('POST', '/api/tokens', { name: 'ci-haproxy', expires_in_days: 30, role: 'operator', scope: { selector: { team: 'web' }, domains: ['haproxy'] } })
await call('POST', '/api/enroll-tokens', { name: 'rack-3', max_uses: 10, expires_in_days: 7, labels: { site: 'par', team: 'web' } })
const session = await api.storageState()
const cookie = session.cookies.map((c) => `${c.name}=${c.value}`).join('; ')
console.log(`shots: the Controller is set up - ${Object.keys(ids).join(', ')} in its fleet, running web.cfg`)

// --- what the pages get: the Controller's answers, normalized ---

const ISO = /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:\d\d)$/
const fakeHex = (key) => crypto.createHash('sha256').update(`janus-shots/${key}`).digest('hex')
// moved maps a real time (ms) to the pages' clock: the setup's start is
// two minutes before T0, and what follows keeps its distance to it,
// floored to the minute - a run a few seconds slower changes nothing.
const moved = (ms) => T0 - 120000 + Math.floor((ms - SETUP) / 60000) * 60000

// normalize rewrites a JSON answer in place: times moved to the pages'
// clock, the host's values replaced by the VM's, fingerprints fixed.
function normalize(v, key = '') {
  if (Array.isArray(v)) return v.map((x) => normalize(x, key))
  if (v && typeof v === 'object') {
    for (const k of Object.keys(v)) v[k] = normalize(v[k], k)
    if (v.boot_time_unix) v.boot_time_unix = Math.round(T0 / 1000) - spec.node.uptime_seconds
    if (v.kernel_version) v.kernel_version = KERNEL
    if (v.models && v.count) {
      v.count = spec.node.cpus
      v.models = [{ name: spec.node.cpu_model, count: spec.node.cpus }]
      v.max_mhz = spec.node.cpu_mhz
      if (v.cores) Object.assign(v, { cores: spec.node.cpus, sockets: 1 })
    }
    return v
  }
  if (typeof v === 'string') {
    if (ISO.test(v) && key !== 'published_at') return new Date(moved(Date.parse(v))).toISOString()
    if (/fingerprint/i.test(key) && /^[0-9a-f]{64}$/.test(v)) return fakeHex(key)
    return v
  }
  if (typeof v === 'number') {
    if (key === 'time_ns') return moved(v / 1e6) * 1e6
    if (/_unix$/.test(key) && v > 1e9) return moved(v * 1000) / 1000
  }
  return v
}

// The charts' series: a node serving a steady trickle of requests.
// Sample k is at T0 + 5 k s; counters are the running sums of rates.
const SERIES = (() => {
  const out = []
  let ticks = 0
  let idle = 0
  let reqs = 0
  let rx = 0
  let tx = 0
  for (let k = 0; k < 200; k++) {
    const w = (a, p, ph = 0) => a * Math.sin((2 * Math.PI * k) / p + ph)
    const cpu = 0.07 + w(0.025, 17) + w(0.01, 5, 1)
    const rate = 160 + w(35, 23) + w(12, 7, 2)
    ticks += 5 * 100 * spec.node.cpus
    idle += 5 * 100 * spec.node.cpus * (1 - cpu)
    reqs += rate * 5
    rx += rate * 5 * 620
    tx += rate * 5 * 4100
    out.push({ ticks, idle: Math.round(idle), reqs: Math.round(reqs), rx: Math.round(rx), tx: Math.round(tx), conns: Math.round(38 + w(9, 11)) })
  }
  return out
})()

function metrics(m, k) {
  const s = SERIES[Math.min(k, SERIES.length - 1)]
  m.time_ms = T0 + k * 5000
  if (m.system) Object.assign(m.system, { cpu_total_ticks: s.ticks, cpu_idle_ticks: s.idle, context_switches: 1000 * s.ticks, processes_created: 2817 })
  if (m.memory) Object.assign(m.memory, { total_bytes: spec.node.memory_bytes, available_bytes: 3221225472, cached_bytes: 805306368 })
  if (m.load) Object.assign(m.load, { load1: spec.node.load[0], load5: spec.node.load[1], load15: spec.node.load[2] })
  for (const d of m.network?.devices || []) if (d.name !== 'lo') Object.assign(d, { rx_bytes: s.rx, tx_bytes: s.tx })
  for (const p of m.services?.processes || [])
    Object.assign(p, p.id === 'haproxy' ? { cpu_seconds: s.ticks / 4000, memory_bytes: 41943040 } : { cpu_seconds: s.ticks / 9000, memory_bytes: 29360128 })
  if (m.haproxy)
    Object.assign(m.haproxy, { uptime_seconds: spec.node.uptime_seconds - 600, current_connections: s.conns, cumulative_requests: s.reqs, cumulative_connections: Math.round(s.reqs / 3), idle_percent: 97 })
  return m
}

// A Server-Sent Events stream: what the real one sends in its first
// second, normalized, then the stream ends - with a day's retry, so the
// page doesn't ask again.
function sseBacklog(path) {
  return new Promise((resolve) => {
    const chunks = []
    const req = https.get(CONTROLLER + path, { rejectUnauthorized: false, headers: { cookie, accept: 'text/event-stream' } }, (res) => {
      res.on('data', (c) => chunks.push(c))
      setTimeout(() => req.destroy(), 1200)
    })
    req.on('error', () => {})
    req.on('close', () => {
      const events = Buffer.concat(chunks)
        .toString('utf8')
        .split('\n\n')
        .filter((e) => e.startsWith('data:'))
        .map((e) => {
          try {
            return 'data: ' + JSON.stringify(normalize(JSON.parse(e.slice(5))))
          } catch {
            return e
          }
        })
      resolve('retry: 86400000\n\n' + events.map((e) => e + '\n\n').join(''))
    })
  })
}

async function newContext(browser, theme, size) {
  const ctx = await browser.newContext({
    ignoreHTTPSErrors: true,
    storageState: session,
    colorScheme: theme,
    reducedMotion: 'reduce',
    locale: 'en-US',
    timezoneId: 'UTC',
    viewport: size,
    deviceScaleFactor: DPR,
  })
  await ctx.clock.install({ time: T0 - 1000 })
  await ctx.clock.pauseAt(T0)
  ctx.metricsServed = 0
  await ctx.route(/\/api\//, async (route) => {
    const req = route.request()
    const path = new URL(req.url()).pathname
    if ((req.headers().accept || '').includes('text/event-stream')) {
      return route.fulfill({ status: 200, contentType: 'text/event-stream', body: await sseBacklog(path + new URL(req.url()).search) })
    }
    let resp
    try {
      resp = await route.fetch()
    } catch {
      return route.abort()
    }
    if (!(resp.headers()['content-type'] || '').includes('application/json')) return route.fulfill({ response: resp })
    let body = normalize(await resp.json())
    if (path.endsWith('/api/metrics')) body = metrics(body, ctx.metricsServed++)
    if (/\/api\/(enroll-)?tokens$/.test(path)) for (const t of body || []) t.id = fakeHex(`token/${t.name}`).slice(0, t.id.length)
    return route.fulfill({ response: resp, json: body })
  })
  return ctx
}

// --- the shots ---

// Text at whole pixels: with subpixel positioning, Chromium draws a
// glyph a fraction of a pixel aside from one launch to the next, and
// runs would differ. Each shot gets a browser of its own too: a second
// page in the same browser did the same. The first launch in a
// container builds the font cache - done here, before any shot.
const launch = () => chromium.launch({ args: ['--disable-font-subpixel-positioning'] })
const lab = await (await (await launch()).newContext()).newPage()
await lab.setContent('<p style="font-family: system-ui">Janus</p><code style="font-family: ui-monospace">janus</code>')

let drifted = 0
let written = 0
for (const shot of spec.shots) {
  if (ONLY.length && !ONLY.includes(shot.id)) continue
  for (const theme of ['light', 'dark']) {
    const browser = await launch()
    const ctx = await newContext(browser, theme, shot.size || spec.size)
    const page = await ctx.newPage()
    const problems = []
    page.on('pageerror', (e) => problems.push(e.message))
    page.on('console', (m) => m.type() === 'error' && problems.push(m.text()))
    const base = shot.node ? `/nodes/${ids[shot.node]}/` : '/'
    await page.goto(CONTROLLER + base + shot.page)
    await page.getByText(shot.wait, { exact: false }).first().waitFor({ timeout: 30000 })
    for (let k = 1; k <= (shot.samples || 0); k++) {
      await page.clock.runFor(5000)
      for (let i = 0; ctx.metricsServed <= k && i < 100; i++) await sleep(50)
    }
    await page.clock.runFor(100)
    await page.waitForLoadState('networkidle')
    await page.evaluate(() => document.activeElement?.blur())
    await page.mouse.move(0, 0)
    await sleep(300)
    if (problems.length) throw new Error(`${shot.id} (${theme}): ${problems.join('; ')}`)

    const target = shot.crop ? page.locator(shot.crop).first() : page
    const origin = shot.crop ? await target.boundingBox() : { x: 0, y: 0 }
    const ignore = []
    for (const sel of shot.ignore || [])
      for (const box of await Promise.all((await page.locator(sel).all()).map((l) => l.boundingBox())))
        if (box) ignore.push({ x: Math.floor((box.x - origin.x) * DPR), y: Math.floor((box.y - origin.y) * DPR), width: Math.ceil(box.width * DPR), height: Math.ceil(box.height * DPR) })
    const webp = await toWebp(lab, await target.screenshot({ type: 'png', animations: 'disabled', caret: 'hide' }))
    await browser.close()

    const name = `${shot.id}-${theme}.webp`
    const committed = fs.existsSync(`/screenshots/${name}`) ? fs.readFileSync(`/screenshots/${name}`) : null
    const d = committed ? await diff(lab, committed, webp, { ignore }) : { ratio: 1 }
    const pct = (d.ratio * 100).toFixed(3)
    if (d.ratio <= THRESHOLD) {
      console.log(`  ${name}: unchanged (${pct} % of pixels)`)
      continue
    }
    if (MODE === 'check') {
      drifted++
      fs.writeFileSync(`/drift/${name}`, webp)
      if (d.png) fs.writeFileSync(`/drift/${shot.id}-${theme}.diff.png`, Buffer.from(d.png, 'base64'))
      console.log(`  ${name}: ${committed ? `${pct} % of its pixels changed` : 'new'}`)
    } else {
      written++
      fs.writeFileSync(`/screenshots/${name}`, webp)
      console.log(`  ${name}: ${committed ? `rewritten, ${pct} % of its pixels changed` : 'new'} (${Math.round(webp.length / 1024)} KiB)`)
    }
  }
}
// A shot gone from shots.yaml leaves no image behind.
const wanted = new Set(spec.shots.flatMap((s) => [`${s.id}-light.webp`, `${s.id}-dark.webp`]))
for (const f of fs.readdirSync('/screenshots').filter((f) => f.endsWith('.webp') && !wanted.has(f))) {
  if (MODE === 'check') {
    drifted++
    console.log(`  ${f}: no shot makes it any more`)
  } else if (!ONLY.length) {
    fs.unlinkSync(`/screenshots/${f}`)
    console.log(`  ${f}: removed - no shot makes it any more`)
  }
}
await lab.context().browser().close()
if (MODE === 'check' && drifted) {
  console.log(`shots: ${drifted} screenshot(s) differ from the committed ones - make docs-screenshots, and look at them`)
  process.exit(1)
}
console.log(MODE === 'check' ? 'shots: every screenshot matches the committed one' : `shots: ${written} written`)
// The site's landing page embeds some of them (site/frontend, @screens).
if (written) console.log('shots: the landing page shows some of them - make site-frontend-build, and commit site/backend/static with them')
