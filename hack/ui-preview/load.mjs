// hack/ui-preview's traffic: streams of requests to the nodes, each on
// for a while and then quiet, out of step with each other - so the
// Traffic flow's numbers start from zero again and again (its flash),
// HTTP and TCP alike (node.cfg's postgres frontend is TCP: its requests go
// through it to the application servers' :8082). Run by
// hack/ui-preview.sh in the browser image (Node), until it's stopped.
const NET = process.env.NET // 10.0.30
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

console.log(`load: ${streams.length} streams`)
await Promise.all(streams.map(run))
