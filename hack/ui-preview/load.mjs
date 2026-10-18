// hack/ui-preview's traffic: streams of requests to the nodes, each on
// for a while and then quiet, out of step with each other - so the
// Traffic flow's numbers start from zero again and again (its flash),
// HTTP and TCP alike (node.cfg's postgres frontend is TCP: its requests go
// through it to the application servers' :8082). And lea, another
// account, with edge-1's page open a minute - its events streamed a
// while - then gone longer than the Traffic flow keeps a user listed
// (2 minutes). HTTPS too (node.cfg's web-tls): new connections whose
// TLS sessions are resumed, full handshakes now and then, HTTP/2 over one
// connection a while, plain text at :443 - a failed handshake - every few
// seconds; and requests refused (403), not found (404), without a server
// (503). Run by hack/ui-preview.sh in the browser image (Node), until it's
// stopped.
import http2 from 'node:http2'
import https from 'node:https'
import net from 'node:net'

const { NET, CONTROLLER, VISIT_NODE, VISITOR_PASSWORD } = process.env // NET: 10.0.30
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

// every: ms between two requests while on; on, off: seconds; after:
// seconds before the first.
const streams = [
  { url: `http://${NET}.21/`, every: 50, on: 40, off: 15, after: 0 },
  { url: `https://${NET}.21/`, kind: 'https', every: 120, on: 35, off: 15, after: 2 },
  { url: `https://${NET}.21/api/items`, kind: 'h2', every: 90, on: 25, off: 20, after: 6 },
  { url: `https://${NET}.22/`, kind: 'https-full', every: 700, on: 30, off: 20, after: 4 },
  { url: `https://${NET}.22/`, kind: 'h2', every: 150, on: 20, off: 25, after: 10 },
  { url: `https://${NET}.22/broken`, kind: 'https', every: 2500, on: 40, off: 20, after: 7 },
  { url: `https://${NET}.21/`, kind: 'garbage', every: 4000, on: 60, off: 30, after: 15 },
  { url: `http://${NET}.21/admin`, every: 1500, on: 30, off: 30, after: 9 },
  { url: `http://${NET}.21/broken`, every: 3000, on: 40, off: 20, after: 11 },
  { url: `http://${NET}.22/missing`, every: 1200, on: 25, off: 35, after: 14 },
  { url: `http://${NET}.21/api/items`, every: 150, on: 15, off: 20, after: 5 },
  { url: `http://${NET}.21:5432/`, every: 250, on: 10, off: 20, after: 12 },
  { url: `http://${NET}.22/`, every: 100, on: 30, off: 30, after: 3 },
  { url: `http://${NET}.22:5432/`, every: 400, on: 12, off: 18, after: 20 },
  { url: `http://${NET}.23/api/items`, every: 200, on: 20, off: 40, after: 8 },
  { url: `http://${NET}.23/`, every: 120, on: 25, off: 10, after: 30 },
]

// HTTPS: sessions resumed from this agent's cache; agent false = a
// full handshake each time. Self-signed: not checked.
const resumed = new https.Agent({ keepAlive: false, rejectUnauthorized: false })
function httpsGet(url, agent) {
  https
    .get(url, { agent, rejectUnauthorized: false, timeout: 5000 }, (r) => r.resume())
    .on('timeout', function () {
      this.destroy()
    })
    .on('error', () => {})
}

async function run({ url, kind, every, on, off, after }) {
  await sleep(after * 1000)
  for (;;) {
    const end = Date.now() + on * 1000
    const u = new URL(url)
    // HTTP/2: one connection for the whole while.
    const h2 = kind === 'h2' ? http2.connect(u.origin, { rejectUnauthorized: false }).on('error', () => {}) : null
    while (Date.now() < end) {
      // Not awaited: a slow answer doesn't slow the stream.
      if (kind === 'https') httpsGet(url, resumed)
      else if (kind === 'https-full') httpsGet(url, false)
      else if (kind === 'h2') {
        if (!h2.closed && !h2.destroyed) {
          const req = h2.request({ ':path': u.pathname }).on('error', () => {})
          req.on('response', () => req.resume())
          req.end()
        }
      } else if (kind === 'garbage') {
        net
          .connect(Number(u.port || 443), u.hostname, function () {
            this.end('hello\r\n')
          })
          .on('error', () => {})
      } else
        fetch(url, { signal: AbortSignal.timeout(5000) })
          .then((r) => r.arrayBuffer())
          .catch(() => {})
      await sleep(every)
    }
    h2?.close()
    await sleep(off * 1000)
  }
}

// visit is lea on edge-1's page, as the browser asks it: its overview,
// stats and clients every few seconds, its events streamed for the
// first 20 s - on 60 s, then off 150 s.
async function visit() {
  let cookie = ''
  const signIn = async () => {
    const r = await fetch(`${CONTROLLER}/api/auth/login`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: 'lea', password: VISITOR_PASSWORD }),
    })
    cookie = r.headers
      .getSetCookie()
      .map((c) => c.split(';')[0])
      .join('; ')
  }
  const get = async (path, ms = 5000) => {
    const r = await fetch(`${CONTROLLER}/nodes/${VISIT_NODE}${path}`, { headers: { Cookie: cookie }, signal: AbortSignal.timeout(ms) })
    if (r.status === 401) cookie = ''
    await r.arrayBuffer()
  }
  await sleep(20000)
  for (;;) {
    try {
      if (!cookie) await signIn()
      get('/api/stream/events', 20000).catch(() => {}) // held open, then dropped
      const end = Date.now() + 60000
      while (Date.now() < end) {
        await Promise.allSettled(['/api/system/overview', '/api/haproxy/stats', '/api/system/clients'].map((p) => get(p)))
        await sleep(3000)
      }
    } catch {
      cookie = ''
    }
    await sleep(150000)
  }
}

console.log(`load: ${streams.length} streams, lea on edge-1 now and then`)
await Promise.all([...streams.map(run), visit()])
