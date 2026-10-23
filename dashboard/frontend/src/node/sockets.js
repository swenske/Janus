// The sockets' throughput for Monitoring › Network (sockets.test.js):
// Netstat gives each TCP connection's bytes received and sent so far
// (rx_bytes, tx_bytes - absent for UDP, listeners, an older node); the
// difference between two answers, seconds apart, is its rate.

// socketKey: a socket's four-tuple and protocol.
export function socketKey(c) {
  return `${c.protocol}|${c.local_address}|${c.remote_address}`
}

// socketRates: by socketKey, {rx, tx} bytes a second - none for a
// connection the previous answer hadn't, nor one whose counter went
// back (its four-tuple reused by a new connection).
export function socketRates(prev, cur, seconds) {
  const out = new Map()
  if (!prev || !cur || !(seconds > 0)) return out
  const before = new Map((prev.connections || []).filter((c) => c.rx_bytes != null && c.tx_bytes != null).map((c) => [socketKey(c), c]))
  for (const c of cur.connections || []) {
    const p = before.get(socketKey(c))
    if (!p || c.rx_bytes == null || c.tx_bytes == null) continue
    const rx = Number(c.rx_bytes) - Number(p.rx_bytes)
    const tx = Number(c.tx_bytes) - Number(p.tx_bytes)
    if (rx < 0 || tx < 0) continue
    out.set(socketKey(c), { rx: rx / seconds, tx: tx / seconds })
  }
  return out
}
