// HAProxy's counters broken down for Metrics (breakdown.test.js): from
// show stat (/api/haproxy/stats: columns and rows), what its frontends
// answered, in which HTTP version, their TLS handshakes; what each
// backend moved and how fast it answers - counters since HAProxy
// started. And from its last log lines, the TLS versions and ciphers
// and the clients' IP versions.

function table(stats) {
  const idx = Object.fromEntries((stats?.columns || []).map((c, i) => [c, i]))
  const num = (r, c) => {
    const v = idx[c] == null ? '' : r[idx[c]]
    return v === '' || v == null ? 0 : Number(v) || 0
  }
  const str = (r, c) => (idx[c] == null ? '' : String(r[idx[c]] ?? ''))
  return { rows: stats?.rows || [], num, str }
}

const sum = (t, kind, col) => t.rows.filter((r) => t.str(r, 'svname') === kind).reduce((n, r) => n + t.num(r, col), 0)
const slices = (list) => list.filter((s) => s.value > 0)

// responses: the frontends' answers by status class.
export function responses(stats) {
  const t = table(stats)
  return slices([
    { label: '2xx', value: sum(t, 'FRONTEND', 'hrsp_2xx'), color: 'var(--ok)' },
    { label: '3xx', value: sum(t, 'FRONTEND', 'hrsp_3xx'), color: 'var(--info)' },
    { label: '4xx', value: sum(t, 'FRONTEND', 'hrsp_4xx'), color: 'var(--warn)' },
    { label: '5xx', value: sum(t, 'FRONTEND', 'hrsp_5xx'), color: 'var(--danger)' },
    { label: '1xx', value: sum(t, 'FRONTEND', 'hrsp_1xx'), color: 'var(--chart-5)' },
    { label: 'other', value: sum(t, 'FRONTEND', 'hrsp_other'), color: 'var(--muted)' },
  ])
}

// versions: the frontends' requests by HTTP version.
export function versions(stats) {
  const t = table(stats)
  return slices([
    { label: 'HTTP/1.x', value: sum(t, 'FRONTEND', 'h1req'), color: 'var(--chart-1)' },
    { label: 'HTTP/2', value: sum(t, 'FRONTEND', 'h2req'), color: 'var(--chart-2)' },
    { label: 'HTTP/3', value: sum(t, 'FRONTEND', 'h3req'), color: 'var(--chart-3)' },
    { label: 'other', value: sum(t, 'FRONTEND', 'req_other'), color: 'var(--muted)' },
  ])
}

// tls: the frontends' handshakes - full ones, sessions resumed, failed.
// ssl_sess counts the new sessions only, the resumed ones apart (3.4: a
// thousand resumed over one full one counted 1 and 1000, not 1001).
export function tls(stats) {
  const t = table(stats)
  return slices([
    { label: 'Full', value: sum(t, 'FRONTEND', 'ssl_sess'), color: 'var(--info)' },
    { label: 'Resumed', value: sum(t, 'FRONTEND', 'ssl_reused_sess'), color: 'var(--ok)' },
    { label: 'Failed', value: sum(t, 'FRONTEND', 'ssl_failed_handshake'), color: 'var(--danger)' },
  ])
}

const COLORS = ['var(--chart-1)', 'var(--chart-2)', 'var(--chart-3)', 'var(--chart-4)', 'var(--chart-5)']

// biggest keeps the top parts, a colour each, and sums the rest.
function biggest(all, top) {
  const sorted = all.filter((s) => s.value > 0).sort((a, b) => b.value - a.value)
  const out = sorted.slice(0, top).map((s, i) => ({ ...s, color: COLORS[i] }))
  const rest = sorted.slice(top).reduce((n, s) => n + s.value, 0)
  if (rest > 0) out.push({ label: `${sorted.length - top} more`, value: rest, color: 'var(--muted)' })
  return out
}

// traffic: the bytes each backend moved, the biggest five and the rest;
// frontendTraffic: each frontend's.
export function traffic(stats, top = 5) {
  return moved(stats, 'BACKEND', top)
}

export function frontendTraffic(stats, top = 5) {
  return moved(stats, 'FRONTEND', top)
}

function moved(stats, kind, top) {
  const t = table(stats)
  return biggest(
    t.rows.filter((r) => t.str(r, 'svname') === kind).map((r) => ({ label: t.str(r, 'pxname'), value: t.num(r, 'bin') + t.num(r, 'bout') })),
    top,
  )
}

// refusals: what went wrong, by kind - on the frontends' side what
// clients sent (bad requests, refused by a rule), on the backends' what
// their servers did (failed connections and responses, retries,
// redispatches). Each counted once: HAProxy counts a denial in the
// frontend and in the backend both, the frontend's is taken.
export function refusals(stats) {
  const t = table(stats)
  return slices([
    { label: 'Bad requests', value: sum(t, 'FRONTEND', 'ereq'), color: 'var(--danger)' },
    { label: 'Denied requests', value: sum(t, 'FRONTEND', 'dreq'), color: 'var(--warn)' },
    { label: 'Denied connections', value: sum(t, 'FRONTEND', 'dcon') + sum(t, 'FRONTEND', 'dses'), color: 'var(--chart-3)' },
    { label: 'Denied responses', value: sum(t, 'FRONTEND', 'dresp'), color: 'var(--chart-4)' },
    { label: 'Connection errors', value: sum(t, 'BACKEND', 'econ'), color: 'var(--chart-1)' },
    { label: 'Response errors', value: sum(t, 'BACKEND', 'eresp'), color: 'var(--chart-2)' },
    { label: 'Retries', value: sum(t, 'BACKEND', 'wretr'), color: 'var(--info)' },
    { label: 'Redispatches', value: sum(t, 'BACKEND', 'wredis'), color: 'var(--chart-5)' },
  ])
}

// The TLS versions and ciphers in HAProxy's last log lines
// (/api/haproxy/tls-log: {lines, tls, versions, ciphers} - the
// Controller counts them, no counter says them). A version's colour says
// its age: 1.3 fine, 1.2 still, older ones a warning.
const VERSION_COLORS = { 'TLSv1.3': 'var(--ok)', 'TLSv1.2': 'var(--info)', 'TLSv1.1': 'var(--warn)', TLSv1: 'var(--danger)', SSLv3: 'var(--danger)' }

export function tlsVersions(log) {
  return (log?.versions || [])
    .filter((v) => v.count > 0)
    .map((v) => ({ label: v.name, value: v.count, color: VERSION_COLORS[v.name] || 'var(--muted)' }))
}

export function ciphers(log, top = 5) {
  return biggest((log?.ciphers || []).map((c) => ({ label: c.name, value: c.count })), top)
}

// ipVersions: the IP version the clients of those log lines came over.
const FAMILY_COLORS = { IPv4: 'var(--chart-1)', IPv6: 'var(--chart-2)' }

export function ipVersions(log) {
  return (log?.families || []).filter((f) => f.count > 0).map((f) => ({ label: f.name, value: f.count, color: FAMILY_COLORS[f.name] || 'var(--muted)' }))
}

// hasIPv6 says whether the node has an IPv6 address of its own beyond
// its links' (/api/network/status): one clients can come to.
export function hasIPv6(status) {
  return (status?.interfaces || []).some((i) => i.kind !== 'loopback' && (i.addresses || []).some((a) => a.includes(':') && !/^fe80:/i.test(a) && !a.startsWith('::1/')))
}

// backends: each backend's answers - average response, connect and
// queue times over its last 1024 requests (ms), requests a second now,
// errors, servers up - slowest first.
export function backends(stats) {
  const t = table(stats)
  const up = {}
  for (const r of t.rows) {
    const sv = t.str(r, 'svname')
    if (sv === 'BACKEND' || sv === 'FRONTEND') continue
    const be = (up[t.str(r, 'pxname')] ||= { up: 0, all: 0 })
    be.all++
    if (/^UP|^no check/i.test(t.str(r, 'status'))) be.up++
  }
  return t.rows
    .filter((r) => t.str(r, 'svname') === 'BACKEND')
    .map((r) => {
      const name = t.str(r, 'pxname')
      return {
        name,
        mode: t.str(r, 'mode'),
        rtime: t.num(r, 'rtime'),
        ctime: t.num(r, 'ctime'),
        qtime: t.num(r, 'qtime'),
        ttime: t.num(r, 'ttime'),
        rate: t.num(r, 'req_rate') || t.num(r, 'rate'),
        errors: t.num(r, 'econ') + t.num(r, 'eresp'),
        servers: up[name] || { up: 0, all: 0 },
      }
    })
    .sort((a, b) => b.rtime - a.rtime || b.ttime - a.ttime || a.name.localeCompare(b.name))
}
