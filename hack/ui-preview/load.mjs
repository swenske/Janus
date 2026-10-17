// hack/ui-preview's traffic: streams of requests to the nodes, each on
// for a while and then quiet, out of step with each other - so the
// Traffic flow's numbers start from zero again and again (its flash),
// HTTP and TCP alike (node.cfg's postgres frontend is TCP: its requests go
// through it to the application servers' :8082). And lea, another
// account, with edge-1's page open a minute - its events streamed a
// while - then gone longer than the Traffic flow keeps a user listed
// (2 minutes). Run by hack/ui-preview.sh in the browser image (Node),
// until it's stopped.
const { NET, CONTROLLER, VISIT_NODE, VISITOR_PASSWORD } = process.env // NET: 10.0.30
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

// every: ms between two requests while on; on, off: seconds; after:
// seconds before the first.
const streams = [
  { url: `http://${NET}.21/`, every: 50, on: 40, off: 15, after: 0 },
  { url: `http://${NET}.21/api/items`, every: 150, on: 15, off: 20, after: 5 },
  { url: `http://${NET}.21:5432/`, every: 250, on: 10, off: 20, after: 12 },
  { url: `http://${NET}.22/`, every: 100, on: 30, off: 30, after: 3 },
  { url: `http://${NET}.22:5432/`, every: 400, on: 12, off: 18, after: 20 },
  { url: `http://${NET}.23/api/items`, every: 200, on: 20, off: 40, after: 8 },
  { url: `http://${NET}.23/`, every: 120, on: 25, off: 10, after: 30 },
]

async function run({ url, every, on, off, after }) {
  await sleep(after * 1000)
  for (;;) {
    const end = Date.now() + on * 1000
    while (Date.now() < end) {
      // Not awaited: a slow answer doesn't slow the stream.
      fetch(url, { signal: AbortSignal.timeout(5000) })
        .then((r) => r.arrayBuffer())
        .catch(() => {})
      await sleep(every)
    }
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
