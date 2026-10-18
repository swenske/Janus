// HAProxy's counters broken down for Metrics (breakdown.test.js): from
// show stat (/api/haproxy/stats: columns and rows), what its frontends
// answered, in which HTTP version, their TLS handshakes; what each
// backend moved and how fast it answers. Counters since HAProxy started.

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

// traffic: the bytes each backend moved, the biggest five and the rest.
export function traffic(stats, top = 5) {
  const t = table(stats)
  const colors = ['var(--chart-1)', 'var(--chart-2)', 'var(--chart-3)', 'var(--chart-4)', 'var(--chart-5)']
  const all = t.rows
    .filter((r) => t.str(r, 'svname') === 'BACKEND')
    .map((r) => ({ label: t.str(r, 'pxname'), value: t.num(r, 'bin') + t.num(r, 'bout') }))
    .filter((s) => s.value > 0)
    .sort((a, b) => b.value - a.value)
  const out = all.slice(0, top).map((s, i) => ({ ...s, color: colors[i] }))
  const rest = all.slice(top).reduce((n, s) => n + s.value, 0)
  if (rest > 0) out.push({ label: `${all.length - top} more`, value: rest, color: 'var(--muted)' })
  return out
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
