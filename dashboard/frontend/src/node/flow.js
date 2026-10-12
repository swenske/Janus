// The node page's Traffic flow: where HAProxy's traffic can go and how
// much goes there now. "show stat" counts each frontend, backend and
// server but never says which frontend sends to which backend: that's
// in the configuration (default_backend, use_backend, a listen section's
// own servers) and, when use_backend reads a map, in the map's values.
// Pure - no React - so npm test covers it (flow.test.js).

// Top-level keywords that open a section: any of them ends the proxy
// being read.
const SECTIONS = new Set([
  'global',
  'defaults',
  'frontend',
  'backend',
  'listen',
  'peers',
  'resolvers',
  'userlist',
  'mailers',
  'program',
  'http-errors',
  'ring',
  'cache',
  'crt-store',
  'log-forward',
  'traces',
  'acme',
  'fcgi-app',
])

// tokenize splits a configuration line into words as HAProxy does:
// blanks separate them, quotes group them ('...' as is, "..." with
// backslash escapes), a backslash escapes the next character, and an
// unquoted # starts a comment.
export function tokenize(line) {
  const words = []
  let cur = ''
  let started = false // "" is still a word
  let quote = ''
  for (let i = 0; i < line.length; i++) {
    const c = line[i]
    if (quote) {
      if (c === quote) quote = ''
      else if (c === '\\' && quote === '"' && i + 1 < line.length) cur += line[++i]
      else cur += c
      continue
    }
    if (c === '#') break
    if (c === ' ' || c === '\t' || c === '\r') {
      if (started) words.push(cur)
      cur = ''
      started = false
      continue
    }
    started = true
    if (c === '"' || c === "'") quote = c
    else if (c === '\\' && i + 1 < line.length) cur += line[++i]
    else cur += c
  }
  if (started) words.push(cur)
  return words
}

// sectionArgs reads "<name> [args...] [from <defaults>]".
function sectionArgs(args) {
  const from = args.indexOf('from')
  const own = from >= 0 ? args.slice(0, from) : args
  return { name: own[0] || '', extra: own.slice(1), from: from >= 0 ? args[from + 1] || '' : '' }
}

// varName is the variable a "set-var(txn.x[,cond])" or
// "set-var-fmt(txn.x)" word sets, or "".
function varName(word) {
  const m = /^set-var(?:-fmt)?\(([^,)]+)/.exec(word)
  return m ? m[1].trim() : ''
}

function unquote(s) {
  const t = s.trim()
  return t.length >= 2 && (t[0] === '"' || t[0] === "'") && t[t.length - 1] === t[0] ? t.slice(1, -1) : t
}

// mapCalls finds the map converters in a sample expression -
// map(file[,default]), map_str(...), map_dom(...), map_str_int(...) -
// as the file each reads and the value it gives when no key matches.
export function mapCalls(expr) {
  const out = []
  const re = /\bmap(?:_[a-z0-9_]+)?\(([^)]*)\)/g
  let m
  while ((m = re.exec(expr))) {
    const args = m[1].split(',')
    const path = unquote(args[0] || '')
    if (path) out.push({ path, def: args.length > 1 ? unquote(args.slice(1).join(',')) : '' })
  }
  return out
}

// splitFormat cuts a log-format string with exactly one %[...]
// expression into what comes before, the expression and what comes
// after; null for anything else.
function splitFormat(format) {
  const start = format.indexOf('%[')
  if (start < 0 || format.indexOf('%[', start + 2) >= 0) return null
  let depth = 0
  for (let i = start + 1; i < format.length; i++) {
    if (format[i] === '[') depth++
    else if (format[i] === ']' && --depth === 0) {
      return { prefix: format.slice(0, start), expr: format.slice(start + 2, i), suffix: format.slice(i + 1) }
    }
  }
  return null
}

// parseTopology reads which backends each frontend (or listen) section
// may send to. Conditional blocks (.if/.endif) are read as if they all
// held: a link that may exist is shown.
export function parseTopology(cfg) {
  const named = new Map() // named defaults sections
  let last = { defaultBackend: '', mode: '' } // the last defaults section: what a proxy without "from" inherits
  const frontends = []
  const backends = []
  let cur = null

  for (const raw of String(cfg || '').split('\n')) {
    const w = tokenize(raw)
    if (!w.length || w[0].startsWith('.')) continue
    if (SECTIONS.has(w[0])) {
      const { name, extra, from } = sectionArgs(w.slice(1))
      const base = from ? named.get(from) || last : last
      if (w[0] === 'defaults') {
        // A defaults section starts over, or from the one it names.
        cur = { kind: 'defaults', defaultBackend: from ? base.defaultBackend : '', mode: from ? base.mode : '' }
        if (name) named.set(name, cur)
        last = cur
      } else if (w[0] === 'frontend' || w[0] === 'listen') {
        cur = {
          kind: w[0],
          name,
          mode: base.mode || '',
          binds: extra.slice(0, 1), // "listen name :80": the old inline bind
          answers: [],
          uses: [],
          defaultBackend: '',
          inherited: base.defaultBackend || '',
          vars: {},
        }
        frontends.push(cur)
        if (w[0] === 'listen') backends.push(name)
      } else if (w[0] === 'backend') {
        cur = { kind: 'backend', name }
        backends.push(name)
      } else {
        cur = null
      }
      continue
    }
    if (!cur || cur.kind === 'backend') continue
    const kw = w[0]
    if (kw === 'mode' && w[1]) cur.mode = w[1]
    else if (kw === 'default_backend' && w[1]) cur.defaultBackend = w[1]
    if (cur.kind === 'defaults') continue

    if (kw === 'bind' && w[1]) cur.binds.push(w[1])
    else if (kw === 'use_backend' && w[1]) {
      const rest = w.slice(2)
      cur.uses.push({ target: w[1], cond: rest[0] === 'if' || rest[0] === 'unless' ? rest.join(' ') : '' })
    } else if (kw === 'http-request' && ['return', 'redirect', 'use-service'].includes(w[1])) {
      cur.answers.push(w[1] === 'use-service' ? `use-service ${w[2] || ''}`.trim() : w[1])
    } else if (kw === 'tcp-request' && w[2] === 'use-service') {
      cur.answers.push(`use-service ${w[3] || ''}`.trim())
    } else if (kw === 'stats' && (w[1] === 'enable' || w[1] === 'uri')) {
      cur.answers.push('stats page')
    }
    // Variables a dynamic use_backend may read: set-var(txn.be) <expr>.
    if (kw === 'http-request' || kw === 'tcp-request' || kw === 'http-response') {
      for (let i = 1; i < w.length - 1; i++) {
        const v = varName(w[i])
        if (v) (cur.vars[v] ||= []).push(w[i + 1])
      }
    }
  }

  const maps = new Set()
  const out = frontends.map((f) => {
    const rules = f.uses.map(({ target, cond }) => {
      if (!target.includes('%[')) return { type: 'use', backend: target, cond }
      const parts = splitFormat(target)
      const rule = { type: 'dynamic', format: target, cond, prefix: '', suffix: '', maps: [], literals: [] }
      if (!parts) return rule
      rule.prefix = parts.prefix
      rule.suffix = parts.suffix
      const exprs = [parts.expr]
      const re = /\bvar\(([^,)]+)/g
      let m
      while ((m = re.exec(parts.expr))) exprs.push(...(f.vars[m[1].trim()] || []))
      for (const e of exprs) {
        rule.maps.push(...mapCalls(e))
        // set-var(txn.be) str(be_api): a backend named as is.
        const lit = /^str\(([^)]*)\)$/.exec(e)
        if (lit) rule.literals.push(unquote(lit[1]))
      }
      for (const mc of rule.maps) maps.add(mc.path)
      return rule
    })
    const def = f.defaultBackend || f.inherited
    if (def) rules.push({ type: 'default', backend: def, inherited: !f.defaultBackend })
    else if (f.kind === 'listen') rules.push({ type: 'self', backend: f.name })
    return { name: f.name, kind: f.kind, mode: f.mode, binds: f.binds, answers: f.answers, rules }
  })
  return { frontends: out, backends, maps: [...maps] }
}

// resolveLinks turns a topology's rules into links between proxies that
// exist (backendNames), each with the rules that make it - a map's keys
// included, from mapEntries ({path: {key: value}}, the maps read). A
// dynamic rule nothing resolves is listed in unresolved.
export function resolveLinks(topology, backendNames, mapEntries = {}) {
  const known = new Set(backendNames)
  const links = new Map()
  const unresolved = []
  const add = (fe, be, rule) => {
    if (!known.has(be)) return
    const key = `${fe}\t${be}`
    if (!links.has(key)) links.set(key, { frontend: fe, backend: be, rules: [] })
    links.get(key).rules.push(rule)
  }
  for (const f of topology?.frontends || []) {
    for (const r of f.rules) {
      if (r.type === 'use') add(f.name, r.backend, { text: `use_backend ${r.backend}${r.cond ? ` ${r.cond}` : ''}` })
      else if (r.type === 'default') add(f.name, r.backend, { text: `default_backend ${r.backend}${r.inherited ? ' (from defaults)' : ''}` })
      else if (r.type === 'self') add(f.name, r.backend, { text: 'its own servers (listen)' })
      else if (r.type === 'dynamic') {
        let resolved = r.literals.length > 0
        for (const lit of r.literals) add(f.name, r.prefix + lit + r.suffix, { text: `use_backend ${r.format}${r.cond ? ` ${r.cond}` : ''}`, via: 'set-var' })
        for (const m of r.maps) {
          const entries = mapEntries[m.path]
          if (!entries) continue
          resolved = true
          const byValue = new Map()
          for (const [k, v] of Object.entries(entries)) {
            const be = r.prefix + v + r.suffix
            if (!byValue.has(be)) byValue.set(be, [])
            byValue.get(be).push(k)
          }
          for (const [be, keys] of byValue) add(f.name, be, { text: `use_backend ${r.format}`, map: m.path, keys: keys.sort() })
          if (m.def) add(f.name, r.prefix + m.def + r.suffix, { text: `use_backend ${r.format}`, map: m.path, isDefault: true })
        }
        if (!resolved) unresolved.push({ frontend: f.name, format: r.format, maps: r.maps.map((m) => m.path) })
      }
    }
  }
  return { links: [...links.values()], unresolved }
}

// --- show stat ---

const TYPE = { 0: 'frontend', 1: 'backend', 2: 'server', 3: 'listener' }
const NUMBERS = ['scur', 'smax', 'slim', 'stot', 'rate', 'rate_max', 'req_rate', 'bin', 'bout', 'qcur', 'act', 'bck', 'weight', 'ereq', 'econ', 'eresp', 'dreq', 'hrsp_2xx', 'hrsp_3xx', 'hrsp_4xx', 'hrsp_5xx', 'rtime', 'ttime', 'lastchg', 'downtime', 'check_duration']

// statRows reads /api/haproxy/stats ({columns, rows}) by column name.
export function statRows(table) {
  const cols = table?.columns || []
  const idx = Object.fromEntries(cols.map((c, i) => [c, i]))
  const val = (r, c) => (idx[c] != null && r[idx[c]] != null ? r[idx[c]] : '')
  return (table?.rows || []).map((r) => {
    const row = {
      proxy: val(r, 'pxname'),
      name: val(r, 'svname'),
      type: TYPE[val(r, 'type')] || val(r, 'type'),
      status: val(r, 'status'),
      mode: val(r, 'mode'),
      addr: val(r, 'addr'),
      check: val(r, 'check_status').replace(/^\* /, ''), // "* L4TOUT": a check under way
      checkDesc: val(r, 'check_desc') || val(r, 'last_chk'),
    }
    for (const c of NUMBERS) row[c] = val(r, c) === '' ? null : Number(val(r, c))
    return row
  })
}

function rowKey(r) {
  return `${r.type}\t${r.proxy}\t${r.name}`
}

// withRates adds inRate/outRate (bytes per second) to each row from the
// previous sample, seconds earlier - none for a counter that went back
// (a reload restarts them) or without a previous sample.
export function withRates(rows, prevRows, seconds) {
  const prev = new Map((prevRows || []).map((r) => [rowKey(r), r]))
  return rows.map((r) => {
    const p = prev.get(rowKey(r))
    const rate = (k) => (p && seconds > 0 && r[k] != null && p[k] != null && r[k] >= p[k] ? (r[k] - p[k]) / seconds : undefined)
    const inRate = rate('bin')
    const outRate = rate('bout')
    return { ...r, inRate, outRate, traffic: inRate == null || outRate == null ? undefined : inRate + outRate }
  })
}

// Tones: '' (the kind's own colour), ok, warn, danger, off (greyed out).
export function serverTone(status) {
  const s = String(status || '').toUpperCase()
  if (s.startsWith('MAINT')) return 'off'
  if (s.startsWith('DRAIN') || s.startsWith('NOLB') || /^UP \d/.test(s)) return 'warn' // "UP 1/3": going down
  if (s.startsWith('DOWN')) return 'danger'
  if (s.startsWith('UP') || s === 'NO CHECK') return 'ok'
  return ''
}

function frontendTone(status) {
  const s = String(status || '').toUpperCase()
  if (s === 'OPEN') return 'ok'
  if (s === 'FULL') return 'warn'
  if (s === 'STOP') return 'off'
  return ''
}

// buildFlow joins the counters (rows, with their rates) to the links
// the configuration gives. Without a topology (the configuration isn't
// readable) nothing links frontends to backends; servers always belong
// to their backend.
export function buildFlow(rows, topology, mapEntries) {
  const internal = (r) => r.proxy.startsWith('<') // HAProxy's own (<HTTPCLIENT>...)
  const fes = rows.filter((r) => r.type === 'frontend' && !internal(r))
  const bes = rows.filter((r) => r.type === 'backend' && !internal(r))
  const servers = rows.filter((r) => r.type === 'server' && !internal(r))
  const conf = new Map((topology?.frontends || []).map((f) => [f.name, f]))
  const order = new Map((topology?.frontends || []).map((f, i) => [f.name, i]))

  const frontends = fes
    .map((r, i) => ({ r, i }))
    .sort((a, b) => (order.get(a.r.proxy) ?? 1e6 + a.i) - (order.get(b.r.proxy) ?? 1e6 + b.i))
    .map(({ r }) => {
      const c = conf.get(r.proxy)
      return {
        id: `fe:${r.proxy}`,
        kind: 'fe',
        name: r.proxy,
        listen: c?.kind === 'listen',
        binds: c?.binds || [],
        answers: c?.answers || [],
        mode: r.mode || c?.mode || '',
        tone: frontendTone(r.status),
        row: r,
      }
    })

  const { links, unresolved } = topology ? resolveLinks(topology, bes.map((b) => b.proxy), mapEntries) : { links: [], unresolved: [] }
  const backends = bes.map((r) => {
    const own = servers.filter((s) => s.proxy === r.proxy)
    const srv = own.map((s) => ({ id: `srv:${r.proxy}/${s.name}`, kind: 'srv', name: s.name, backend: r.proxy, tone: serverTone(s.status), row: s }))
    const down = srv.filter((s) => s.tone === 'danger').length
    let tone = String(r.status).toUpperCase().startsWith('DOWN') ? 'danger' : down ? 'warn' : String(r.status).toUpperCase() === 'UP' ? 'ok' : ''
    return { id: `be:${r.proxy}`, kind: 'be', name: r.proxy, tone, servers: srv, down, row: r }
  })
  return {
    configKnown: !!topology,
    frontends,
    backends,
    links: links.map((l) => ({ id: `fe:${l.frontend}>be:${l.backend}`, from: `fe:${l.frontend}`, to: `be:${l.backend}`, rules: l.rules })),
    unresolved,
  }
}

// interfaces lists the node's network interfaces for the map: every
// one the metrics count but the loopback, with its state when the
// network status says it (null when it isn't known).
export function interfaces(net, status) {
  const st = new Map((status?.interfaces || []).map((i) => [i.name, i]))
  const names = new Set(Object.keys(net || {}))
  for (const i of status?.interfaces || []) names.add(i.name)
  return [...names]
    .filter((n) => n !== 'lo' && st.get(n)?.kind !== 'loopback')
    .sort((a, b) => a.localeCompare(b, undefined, { numeric: true }))
    .map((n) => {
      const s = st.get(n)
      const rx = net?.[n]?.rx
      const tx = net?.[n]?.tx
      const up = s ? !!s.up && !!s.carrier : null
      return { id: `if:${n}`, kind: 'iface', name: n, up, status: s || null, rx, tx, traffic: rx == null || tx == null ? undefined : rx + tx, tone: up === false ? 'off' : '' }
    })
}

// --- layout ---

export const MAX_SERVERS = 8
const ROW = 92 // a frontend's, backend's or interface's height
const SRV_ROW = 46 // a server's: its label is beside it
const TOP = 24
const R = { iface: 22, hub: 32, fe: 22, be: 22, srv: 14, more: 14 }
const X = { iface: 60, hub: 240, fe: 440, be: 650, srv: 850 }

function curve(a, b) {
  const x1 = a.x + a.r
  const x2 = b.x - b.r
  const xm = (x1 + x2) / 2
  return `M${x1} ${a.y}C${xm} ${a.y} ${xm} ${b.y} ${x2} ${b.y}`
}

// layout places the map's nodes in columns - interfaces, HAProxy,
// frontends, backends, servers (when shown) - and draws its links.
// Backends follow the first frontend that names them, each as tall as
// its servers; frontends sit level with their backends, never closer
// than a row.
export function layout(graph, ifaces, { servers = true } = {}) {
  const nodes = []
  const byId = {}
  const put = (n) => {
    nodes.push(n)
    byId[n.id] = n
    return n
  }
  const links = []

  // Backends: in the order of the frontends that reach them, then the rest.
  const rank = new Map()
  graph.frontends.forEach((f, fi) => {
    graph.links.filter((l) => l.from === f.id).forEach((l, li) => {
      if (!rank.has(l.to)) rank.set(l.to, fi * 1e4 + li)
    })
  })
  const bes = graph.backends.map((b, i) => ({ b, i })).sort((p, q) => (rank.get(p.b.id) ?? 1e9 + p.i) - (rank.get(q.b.id) ?? 1e9 + q.i))

  let y = TOP
  for (const { b } of bes) {
    const shown = servers ? b.servers.slice(0, b.servers.length > MAX_SERVERS ? MAX_SERVERS - 1 : MAX_SERVERS) : []
    const more = servers ? b.servers.length - shown.length : 0
    const count = shown.length + (more ? 1 : 0)
    const band = Math.max(ROW, count * SRV_ROW)
    const be = put({ ...b, x: X.be, y: y + band / 2, r: R.be, linked: rank.has(b.id) })
    let sy = y + (band - count * SRV_ROW) / 2 + SRV_ROW / 2
    for (const s of shown) {
      const n = put({ ...s, x: X.srv, y: sy, r: R.srv })
      links.push({ id: `${be.id}>${n.id}`, from: be.id, to: n.id, kind: 'srv' })
      sy += SRV_ROW
    }
    if (more) {
      const hidden = b.servers.slice(shown.length)
      const n = put({ id: `more:${b.name}`, kind: 'more', name: `+${more} more`, backend: b.name, x: X.srv, y: sy, r: R.more, tone: hidden.some((s) => s.tone === 'danger') ? 'danger' : '', hidden })
      links.push({ id: `${be.id}>${n.id}`, from: be.id, to: n.id, kind: 'srv' })
    }
    y += band
  }
  const beBottom = y

  // Frontends: level with the middle of their backends, a row apart.
  const want = graph.frontends.map((f, i) => {
    const ys = graph.links.filter((l) => l.from === f.id).map((l) => byId[l.to]?.y).filter((v) => v != null)
    return { f, i, y: ys.length ? ys.reduce((a, v) => a + v, 0) / ys.length : Infinity }
  })
  want.sort((a, b) => a.y - b.y || a.i - b.i)
  let prev = TOP + ROW / 2 - ROW
  for (const w of want) {
    const fy = Math.max(Number.isFinite(w.y) ? w.y : -Infinity, prev + ROW)
    put({ ...w.f, x: X.fe, y: fy, r: R.fe })
    prev = fy
  }
  const feYs = graph.frontends.map((f) => byId[f.id].y)

  // HAProxy in the middle of its frontends, the interfaces around it.
  const mid = feYs.length ? (Math.min(...feYs) + Math.max(...feYs)) / 2 : Math.max(TOP + ROW / 2, (TOP + beBottom) / 2)
  const span = (ifaces.length - 1) * ROW
  const first = Math.max(TOP + ROW / 2, mid - span / 2)
  const hubY = Math.max(mid, TOP + R.hub + 4)
  const hub = put({ id: 'hub', kind: 'hub', name: 'HAProxy', x: X.hub, y: hubY, r: R.hub })
  ifaces.forEach((it, i) => {
    const n = put({ ...it, x: X.iface, y: first + i * ROW, r: R.iface })
    links.push({ id: `${n.id}>hub`, from: n.id, to: 'hub', kind: 'iface' })
  })
  for (const f of graph.frontends) links.push({ id: `hub>${f.id}`, from: 'hub', to: f.id, kind: 'fe' })
  for (const l of graph.links) links.push({ ...l, kind: 'be' })

  for (const l of links) l.d = curve(byId[l.from], byId[l.to])

  const bottom = Math.max(beBottom, prev + ROW / 2, first + span + ROW / 2, hub.y + ROW / 2)
  const width = servers && graph.backends.some((b) => b.servers.length) ? X.srv + 200 : X.be + 120
  return { nodes, links, byId, width, height: bottom + TOP, hubY: hub.y }
}

// related is what a node or link is part of, to bring forward: from a
// frontend, backend or server, the paths through it - up to HAProxy,
// down to the servers - and HAProxy and the interfaces, which every path
// crosses; null for HAProxy and the interfaces (everything).
export function related(map, id) {
  const link = map.links.find((l) => l.id === id)
  const start = link ? [link.from, link.to] : [id]
  if (start.some((n) => !map.byId[n] || map.byId[n].kind === 'hub' || map.byId[n].kind === 'iface')) return null
  const out = new Set([...start, ...(link ? [link.id] : [])])
  const down = (n) => {
    for (const l of map.links) {
      if (l.from === n && !out.has(l.id)) {
        out.add(l.id)
        out.add(l.to)
        down(l.to)
      }
    }
  }
  const up = (n) => {
    for (const l of map.links) {
      if (l.to === n && !out.has(l.id) && map.byId[l.from].kind !== 'iface') {
        out.add(l.id)
        out.add(l.from)
        up(l.from)
      }
    }
  }
  if (link) {
    down(link.to)
    up(link.from)
  } else {
    down(id)
    up(id)
  }
  out.add('hub')
  for (const n of map.nodes) if (n.kind === 'iface') out.add(n.id)
  for (const l of map.links) if (l.kind === 'iface') out.add(l.id)
  return out
}

// pulse is how a link with traffic looks: thicker and faster the more
// bytes per second (log scale); null when nothing flows.
export function pulse(bytesPerSecond) {
  if (!(bytesPerSecond > 0)) return null
  const l = Math.log10(Math.max(1, bytesPerSecond))
  return {
    width: Math.min(5, Math.max(1.5, 1.2 + 0.45 * l)),
    seconds: Math.round(Math.min(2.4, Math.max(0.35, 2.6 - 0.3 * l)) * 100) / 100,
  }
}
