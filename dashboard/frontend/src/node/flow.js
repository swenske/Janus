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

    const answer = (a) => cur.answers.includes(a) || cur.answers.push(a)
    if (kw === 'bind' && w[1]) cur.binds.push(w[1])
    else if (kw === 'use_backend' && w[1]) {
      const rest = w.slice(2)
      cur.uses.push({ target: w[1], cond: rest[0] === 'if' || rest[0] === 'unless' ? rest.join(' ') : '' })
    } else if (kw === 'http-request' && ['return', 'redirect', 'use-service'].includes(w[1])) {
      answer(w[1] === 'use-service' ? `use-service ${w[2] || ''}`.trim() : w[1])
    } else if (kw === 'tcp-request' && w[2] === 'use-service') {
      answer(`use-service ${w[3] || ''}`.trim())
    } else if (kw === 'stats' && (w[1] === 'enable' || w[1] === 'uri')) {
      answer('stats page')
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
          // By key: the order the Controller's JSON gives them, and janusctl's.
          for (const [k, v] of Object.entries(entries).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))) {
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
    const mode = r.mode || ''
    const srv = own.map((s) => ({ id: `srv:${r.proxy}/${s.name}`, kind: 'srv', name: s.name, backend: r.proxy, mode, tone: serverTone(s.status), row: s }))
    const down = srv.filter((s) => s.tone === 'danger').length
    let tone = String(r.status).toUpperCase().startsWith('DOWN') ? 'danger' : down ? 'warn' : String(r.status).toUpperCase() === 'UP' ? 'ok' : ''
    return { id: `be:${r.proxy}`, kind: 'be', name: r.proxy, mode, tone, servers: srv, down, row: r }
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

// protocol is what a link's traffic is - http or tcp, as HAProxy's mode
// says (a backend's: a TCP frontend may hand its connections to an HTTP
// backend, never the other way), log to a log target, api from a client
// of the node's API, '' for an interface's (all of its traffic, whatever
// it is).
export function protocol(link, byId) {
  if (link.kind === 'log' || link.kind === 'api') return link.kind
  if (link.kind === 'iface') return ''
  const node = link.kind === 'fe' ? byId[link.to] : link.kind === 'be' ? byId[link.to] : byId[link.from] // srv, trunk: the backend
  const mode = node?.mode || (link.kind === 'be' ? byId[link.from]?.mode : '')
  return mode === 'http' || mode === 'tcp' ? mode : ''
}

const toNumber = (v) => (v == null || v === '' ? undefined : Number(v))

// logTargets are the node's log targets for the map, from
// /api/log-forwarding (its status, proto names): each with its records
// and bytes a second from the previous answer, seconds earlier (none
// without one, or for a counter that went back - janusd restarted), and
// the interface its traffic leaves by. A node older than that interface
// doesn't say it nor its bytes: via null, bytes unknown.
export function logTargets(resp, prevResp, seconds) {
  const cfg = new Map((resp?.config?.targets || []).map((t) => [t.name, t]))
  const prev = new Map((prevResp?.targets || []).map((t) => [t.name, t]))
  return (resp?.targets || []).map((st) => {
    const p = prev.get(st.name)
    const knowsBytes = 'bytes_sent' in st
    const rate = (k) => {
      const a = toNumber(st[k])
      const b = toNumber(p?.[k])
      return p && seconds > 0 && a != null && b != null && a >= b ? (a - b) / seconds : undefined
    }
    const c = cfg.get(st.name) || {}
    const state = st.state || ''
    return {
      id: `log:${st.name}`,
      kind: 'log',
      name: st.name,
      es: !!c.elasticsearch || st.kind === 'elasticsearch',
      target: c,
      status: st,
      via: 'interface' in st ? st.interface || '' : null,
      msgRate: rate('sent'),
      byteRate: knowsBytes ? rate('bytes_sent') : undefined,
      knowsBytes,
      tone: state === 'disabled' ? 'off' : state === 'failing' ? 'danger' : state === 'ok' ? 'ok' : '',
    }
  })
}

// apiPeers are who is connected to the node's API, for the map, from
// /api/system/clients (ClientList, proto names): the Controller's
// connections, and the other clients' (janusctl...), each by the
// interface they come by - their calls and bytes a second over the
// connections open at both answers, seconds apart (none before a second
// answer); the users the Controller acts for, the latest first.
export function apiPeers(resp, prevResp, seconds) {
  const prev = new Map((prevResp?.connections || []).map((c) => [String(c.id), c]))
  const groups = new Map()
  for (const c of resp?.connections || []) {
    const controller = !!c.controller
    const via = c.interface || ''
    const id = `api:${controller ? 'controller' : 'clients'}${via ? `:${via}` : ''}`
    let g = groups.get(id)
    if (!g) {
      g = { id, kind: 'api', controller, name: controller ? 'Controller' : 'Direct clients', via, conns: [], users: [], userCount: 0, self: false, tone: '' }
      groups.set(id, g)
    }
    const p = prev.get(String(c.id))
    const rate = (k) => {
      const a = toNumber(c[k])
      const b = toNumber(p?.[k])
      return p && seconds > 0 && a != null && b != null && a >= b ? (a - b) / seconds : undefined
    }
    for (const [k, v] of [
      ['callRate', rate('calls')],
      ['inRate', rate('bytes_received')],
      ['outRate', rate('bytes_sent')],
    ]) {
      if (v != null) g[k] = (g[k] ?? 0) + v
    }
    g.conns.push(c)
    g.self ||= !!c.self
    g.userCount += Number(c.user_count || 0)
    for (const u of c.users || []) {
      const had = g.users.find((x) => x.name === u.name)
      if (!had) g.users.push({ ...u })
      else if (Number(u.last_call_unix_ns) > Number(had.last_call_unix_ns)) Object.assign(had, u)
    }
  }
  for (const g of groups.values()) {
    g.byteRate = g.inRate != null && g.outRate != null ? g.inRate + g.outRate : undefined
    g.users.sort((a, b) => Number(b.last_call_unix_ns) - Number(a.last_call_unix_ns) || a.name.localeCompare(b.name))
  }
  // The Controller first.
  return [...groups.values()].sort((a, b) => b.controller - a.controller)
}

// peerPlaces is where the peers - API clients, log targets - go along
// the interfaces' column (or row), a step apart at least: those of one
// interface together around it, in their order, the interfaces' order
// kept - no two links cross -, all as near to that as the room they take
// allows (the least moved overall: pooling adjacent violators); those no
// interface on the map says after the others. at is an interface's
// place (undefined when it isn't on the map); none before start.
function peerPlaces(peers, at, step, start) {
  const linked = []
  const rest = []
  peers.forEach((t, i) => {
    const p = t.via ? at(t.via) : undefined
    if (p == null) rest.push(t)
    else linked.push({ t, i, p })
  })
  for (const w of linked) {
    const same = linked.filter((o) => o.p === w.p)
    w.want = w.p + (same.indexOf(w) - (same.length - 1) / 2) * step
  }
  linked.sort((a, b) => a.p - b.p || a.i - b.i)
  // Each k-th wants want - k·step: those in order, the same for a block.
  const blocks = []
  linked.forEach((w, k) => {
    let b = { sum: w.want - k * step, n: 1 }
    while (blocks.length && blocks.at(-1).sum / blocks.at(-1).n > b.sum / b.n) {
      const prev = blocks.pop()
      b = { sum: prev.sum + b.sum, n: prev.n + b.n }
    }
    blocks.push(b)
  })
  const out = []
  for (const b of blocks) {
    const q = Math.max(b.sum / b.n, start)
    for (let j = 0; j < b.n; j++) out.push({ t: linked[out.length].t, p: q + out.length * step })
  }
  let prev = out.length ? out.at(-1).p : start - step
  for (const t of rest) out.push({ t, p: (prev += step) })
  return out
}

// linkPeer joins a peer to its interface, when it's on the map: to a
// log target from it, from an API client to it.
function linkPeer(n, byId, links) {
  const iface = n.via ? byId[`if:${n.via}`] : null
  n.linked = !!iface
  if (!iface) return
  if (n.kind === 'log') links.push({ id: `${iface.id}>${n.id}`, from: iface.id, to: n.id, kind: 'log' })
  else links.push({ id: `${n.id}>${iface.id}`, from: n.id, to: iface.id, kind: 'api' })
}

// --- layout ---

export const MAX_SERVERS = 8
const ROW = 92 // a frontend's, backend's or interface's height
const SRV_ROW = 46 // a server's: its label is beside it
const TOP = 24
const R = { log: 22, api: 22, iface: 22, hub: 32, fe: 22, be: 22, srv: 14, more: 14 }
const COLUMNS = { iface: 60, hub: 240, fe: 440, be: 650, srv: 850 }
const shifted = (dx) => Object.fromEntries(Object.entries(COLUMNS).map(([k, x]) => [k, x + dx]))
// The peers' column (log targets, API clients), its labels centred
// under it whole - "15 msg/s · 13.0 KiB/s", "12 calls/s · 3.4 KiB/s" -,
// the others moved right of it.
const PEER_X = 95
const PEER_SHIFT = 220

function curve(a, b) {
  const x1 = a.x + a.r
  const x2 = b.x - b.r
  const xm = (x1 + x2) / 2
  return `M${x1} ${a.y}C${xm} ${a.y} ${xm} ${b.y} ${x2} ${b.y}`
}

// curveDown is curve from a's bottom to b's top.
function curveDown(a, b) {
  const y1 = a.y + a.r
  const y2 = b.y - b.r
  const ym = (y1 + y2) / 2
  return `M${a.x} ${y1}C${a.x} ${ym} ${b.x} ${ym} ${b.x} ${y2}`
}

// rankBackends is the backends in the order of the frontends that reach
// them, then the rest, and each linked one's rank.
function rankBackends(graph) {
  const rank = new Map()
  graph.frontends.forEach((f, fi) => {
    graph.links.filter((l) => l.from === f.id).forEach((l, li) => {
      if (!rank.has(l.to)) rank.set(l.to, fi * 1e4 + li)
    })
  })
  const bes = graph.backends.map((b, i) => ({ b, i })).sort((p, q) => (rank.get(p.b.id) ?? 1e9 + p.i) - (rank.get(q.b.id) ?? 1e9 + q.i))
  return { rank, ordered: bes.map(({ b }) => b) }
}

// serverSlots is what a backend shows of its servers: up to
// MAX_SERVERS, the last slot "+N more" when they don't all fit.
function serverSlots(b, servers) {
  if (!servers) return []
  const shown = b.servers.slice(0, b.servers.length > MAX_SERVERS ? MAX_SERVERS - 1 : MAX_SERVERS)
  const slots = shown.map((s) => ({ ...s }))
  if (shown.length < b.servers.length) {
    const hidden = b.servers.slice(shown.length)
    slots.push({ id: `more:${b.name}`, kind: 'more', name: `+${hidden.length} more`, backend: b.name, tone: hidden.some((s) => s.tone === 'danger') ? 'danger' : '', hidden })
  }
  return slots
}

// layout places the map's nodes and draws its links in a shape: across
// (left to right, labels under the nodes), dense (left to right, labels
// beside them) or down (top to bottom) - vertical is down. Peers - API
// clients (apiPeers), log targets (logTargets) -, when there are, go
// beyond the interfaces: a column left of them, a row above them.
export function layout(graph, ifaces, { servers = true, vertical = false, shape = vertical ? 'down' : 'across', peers = [] } = {}) {
  if (shape === 'down') return layoutDown(graph, ifaces, servers, peers)
  if (shape === 'dense') return layoutDense(graph, ifaces, servers, peers)
  return layoutAcross(graph, ifaces, servers, peers)
}

// layoutAcross places the nodes in columns - interfaces, HAProxy,
// frontends, backends, servers (when shown). Backends follow the first
// frontend that names them, each as tall as its servers; frontends sit
// level with their backends, never closer than a row.
function layoutAcross(graph, ifaces, servers, peers) {
  const X = peers.length ? shifted(PEER_SHIFT) : COLUMNS
  const nodes = []
  const byId = {}
  const put = (n) => {
    nodes.push(n)
    byId[n.id] = n
    return n
  }
  const links = []
  const { rank, ordered } = rankBackends(graph)

  let y = TOP
  for (const b of ordered) {
    const slots = serverSlots(b, servers)
    const band = Math.max(ROW, slots.length * SRV_ROW)
    const be = put({ ...b, x: X.be, y: y + band / 2, r: R.be, linked: rank.has(b.id) })
    let sy = y + (band - slots.length * SRV_ROW) / 2 + SRV_ROW / 2
    for (const s of slots) {
      const n = put({ ...s, x: X.srv, y: sy, r: R[s.kind] })
      links.push({ id: `${be.id}>${n.id}`, from: be.id, to: n.id, kind: 'srv' })
      sy += SRV_ROW
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
  const hub = put({ id: 'hub', kind: 'hub', name: 'Janus', x: X.hub, y: hubY, r: R.hub })
  ifaces.forEach((it, i) => {
    const n = put({ ...it, x: X.iface, y: first + i * ROW, r: R.iface })
    links.push({ id: `${n.id}>hub`, from: n.id, to: 'hub', kind: 'iface' })
  })
  for (const f of graph.frontends) links.push({ id: `hub>${f.id}`, from: 'hub', to: f.id, kind: 'fe' })
  for (const l of graph.links) links.push({ ...l, kind: 'be' })
  let peerBottom = 0
  for (const { t, p } of peerPlaces(peers, (name) => byId[`if:${name}`]?.y, ROW, TOP + ROW / 2)) {
    linkPeer(put({ ...t, x: PEER_X, y: p, r: R[t.kind] }), byId, links)
    peerBottom = p + ROW / 2
  }

  // A log link is drawn from its target: left to right too.
  for (const l of links) l.d = l.kind === 'log' ? curve(byId[l.to], byId[l.from]) : curve(byId[l.from], byId[l.to])

  const bottom = Math.max(beBottom, prev + ROW / 2, first + span + ROW / 2, hub.y + ROW / 2, peerBottom)
  const width = servers && graph.backends.some((b) => b.servers.length) ? X.srv + 200 : X.be + 120
  return { nodes, links, byId, width, height: bottom + TOP, hubY: hub.y }
}

// Top to bottom: each node's label is beside it - its name, then its
// numbers a line each - so a column is as wide as its label; a
// backend's servers hang below it, along a trunk.
export const NAME_CHARS = 20 // a name's longest, cut with an ellipsis
export const VALUE_CHARS = 15 // a number's line: "999.9 KiB/s", "12/20 servers up"
const NAME_PX = 7.3 // a character's width: 12px and 11px monospace
const VALUE_PX = 6.7
const GAP = 22 // between a label and the next node
const LEVEL = 100 // between two rows
const FAN = 130 // frontends to backends: room for the links to cross
const SRV_DOWN = 46 // between two servers: three lines
const SRV_IN = 30 // a server's centre right of its backend's
const ELBOW = 12 // a server link's turn off the trunk

// labelWidth is how far a node's label reaches right of its circle -
// never by its numbers, which change at each refresh: by room kept for
// them.
function labelWidth(n) {
  const name = Math.min(String(n.name).length, NAME_CHARS) * NAME_PX
  if (n.dense && (n.kind === 'srv' || n.kind === 'more')) return 8 + name + DENSE.space + lineChars(n) * VALUE_PX
  return 8 + Math.max(name, (n.dense ? lineChars(n) : VALUE_CHARS) * VALUE_PX)
}

// reach is how far right of its centre a node and its label go.
export function reach(n) {
  return n.r + labelWidth(n)
}

// layoutDown places the nodes in rows - interfaces, HAProxy, frontends,
// backends with their servers below them. Backends follow the first
// frontend that names them; frontends sit above the middle of their
// backends, never closer than their labels allow.
function layoutDown(graph, ifaces, servers, peers) {
  const nodes = []
  const byId = {}
  const put = (n) => {
    nodes.push(n)
    byId[n.id] = n
    return n
  }
  const links = []
  const { rank, ordered } = rankBackends(graph)

  const peerY = TOP + R.log
  const ifY = peers.length ? peerY + LEVEL : TOP + R.iface
  const hubY = ifY + LEVEL
  const feY = hubY + LEVEL
  const beY = feY + FAN
  let bottom = beY + R.be + 30

  let x = 0
  for (const b of ordered) {
    const be = put({ ...b, x, y: beY, r: R.be, linked: rank.has(b.id) })
    let right = reach(be)
    let sy = beY + R.be + 32
    for (const s of serverSlots(b, servers)) {
      const n = put({ ...s, x: be.x + SRV_IN, y: sy, r: R[s.kind] })
      links.push({ id: `${be.id}>${n.id}`, from: be.id, to: n.id, kind: 'srv' })
      right = Math.max(right, SRV_IN + reach(n))
      bottom = Math.max(bottom, sy + 30)
      sy += SRV_DOWN
    }
    if (sy > beY + R.be + 32) {
      // One trunk the servers' links branch off, drawn once.
      links.push({ id: `${be.id}>trunk`, from: be.id, to: be.id, kind: 'trunk', end: sy - SRV_DOWN - ELBOW })
    }
    x += right + GAP + R.be
  }

  // Frontends: above the middle of their backends, their labels apart.
  const want = graph.frontends.map((f, i) => {
    const xs = graph.links.filter((l) => l.from === f.id).map((l) => byId[l.to]?.x).filter((v) => v != null)
    return { f, i, x: xs.length ? xs.reduce((a, v) => a + v, 0) / xs.length : Infinity }
  })
  want.sort((a, b) => a.x - b.x || a.i - b.i)
  let prev = null
  for (const w of want) {
    // One that sends nowhere known goes right of the others - from the
    // left edge when none does (the configuration isn't readable).
    const after = prev ? prev.x + reach(prev) + GAP + R.fe : 0
    prev = put({ ...w.f, x: Number.isFinite(w.x) ? Math.max(w.x, after) : after, y: feY, r: R.fe })
  }

  // HAProxy above the middle of its frontends, the interfaces above it.
  const xs = (graph.frontends.length ? graph.frontends : ordered).map((n) => byId[n.id].x)
  const mid = xs.length ? (Math.min(...xs) + Math.max(...xs)) / 2 : 0
  const hub = put({ id: 'hub', kind: 'hub', name: 'Janus', x: mid, y: hubY, r: R.hub })
  const step = ifaces.length ? Math.max(...ifaces.map((it) => reach({ ...it, r: R.iface }))) + GAP + R.iface : 0
  ifaces.forEach((it, i) => {
    const n = put({ ...it, x: mid + (i - (ifaces.length - 1) / 2) * step, y: ifY, r: R.iface })
    links.push({ id: `${n.id}>hub`, from: n.id, to: 'hub', kind: 'iface' })
  })
  for (const f of graph.frontends) links.push({ id: `hub>${f.id}`, from: 'hub', to: f.id, kind: 'fe' })
  for (const l of graph.links) links.push({ ...l, kind: 'be' })
  // Peers above their interfaces, their labels apart.
  if (peers.length) {
    const peerStep = Math.max(...peers.map((t) => reach({ ...t, r: R[t.kind] }))) + GAP + R.log
    const left = ifaces.length ? mid - ((ifaces.length - 1) / 2) * step : mid
    for (const { t, p } of peerPlaces(peers, (name) => byId[`if:${name}`]?.x, peerStep, left)) linkPeer(put({ ...t, x: p, y: peerY, r: R[t.kind] }), byId, links)
  }

  // Everything moved right of the margin, then the links drawn.
  const shift = TOP - Math.min(...nodes.map((n) => n.x - n.r))
  for (const n of nodes) n.x += shift
  for (const l of links) {
    const a = byId[l.from]
    const b = byId[l.to]
    if (l.kind === 'trunk') l.d = `M${a.x} ${a.y + a.r}V${l.end}`
    else if (l.kind === 'srv') l.d = `M${a.x} ${b.y - ELBOW}Q${a.x} ${b.y} ${a.x + ELBOW} ${b.y}H${b.x - b.r}`
    else if (l.kind === 'log') l.d = curveDown(b, a) // from the target above
    else l.d = curveDown(a, b)
  }
  const width = Math.max(...nodes.map((n) => n.x + reach(n))) + TOP
  return { nodes, links, byId, width, height: bottom + TOP, hubY: hub.y }
}

// Left to right, dense: every label beside its node - its name above the
// links that leave it, its numbers on a line below them; a server's name
// and numbers on one line - so a row is as tall as a node: half the
// across layout's, for a configuration with many backends.
const LINE_CHARS = { iface: 11, hub: 20, log: 23, api: 25 } // a dense label's numbers on one line: "999.9 KiB/s", "999 conns · 99 req/s", "999 msg/s · 999.9 KiB/s", "999 calls/s · 999.9 KiB/s"

// lineChars is the room a dense label keeps for its numbers: "999.9
// KiB/s · 999 sess" but for an interface's and HAProxy's.
export function lineChars(n) {
  return LINE_CHARS[n.kind] ?? 22
}

const DENSE = {
  r: { log: 18, api: 18, iface: 18, hub: 24, fe: 18, be: 18, srv: 11, more: 11 },
  row: 44, // a row of interfaces, frontends or backends
  srv: 28, // a server's
  gap: 44, // between a label and the next column
  fan: 110, // frontends to backends: room for the links to cross
  space: 10, // a server's name to its numbers
}

// stem is where a dense node's links leave it: past its label, the line
// running under its name.
function stem(n) {
  return n.x + reach(n) + 6
}

function layoutDense(graph, ifaces, servers, peers) {
  const nodes = []
  const byId = {}
  const put = (n) => {
    nodes.push(n)
    byId[n.id] = n
    return n
  }
  const links = []
  const { rank, ordered } = rankBackends(graph)
  const R = DENSE.r

  let y = TOP
  for (const b of ordered) {
    const slots = serverSlots(b, servers)
    const band = Math.max(DENSE.row, slots.length * DENSE.srv)
    const be = put({ ...b, y: y + band / 2, r: R.be, dense: true, linked: rank.has(b.id) })
    let sy = y + (band - slots.length * DENSE.srv) / 2 + DENSE.srv / 2
    for (const s of slots) {
      const n = put({ ...s, y: sy, r: R[s.kind], dense: true })
      links.push({ id: `${be.id}>${n.id}`, from: be.id, to: n.id, kind: 'srv' })
      sy += DENSE.srv
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
  let prev = TOP + DENSE.row / 2 - DENSE.row
  for (const w of want) {
    const fy = Math.max(Number.isFinite(w.y) ? w.y : -Infinity, prev + DENSE.row)
    put({ ...w.f, y: fy, r: R.fe, dense: true })
    prev = fy
  }
  const feYs = graph.frontends.map((f) => byId[f.id].y)

  // HAProxy in the middle of its frontends, the interfaces around it.
  const mid = feYs.length ? (Math.min(...feYs) + Math.max(...feYs)) / 2 : Math.max(TOP + DENSE.row / 2, (TOP + beBottom) / 2)
  const span = (ifaces.length - 1) * DENSE.row
  const first = Math.max(TOP + DENSE.row / 2, mid - span / 2)
  const hub = put({ id: 'hub', kind: 'hub', name: 'HAProxy', y: Math.max(mid, TOP + R.hub), r: R.hub, dense: true })
  ifaces.forEach((it, i) => {
    const n = put({ ...it, y: first + i * DENSE.row, r: R.iface, dense: true })
    links.push({ id: `${n.id}>hub`, from: n.id, to: 'hub', kind: 'iface' })
  })
  for (const f of graph.frontends) links.push({ id: `hub>${f.id}`, from: 'hub', to: f.id, kind: 'fe' })
  for (const l of graph.links) links.push({ ...l, kind: 'be' })
  let peerBottom = 0
  for (const { t, p } of peerPlaces(peers, (name) => byId[`if:${name}`]?.y, DENSE.row, TOP + DENSE.row / 2)) {
    linkPeer(put({ ...t, y: p, r: R[t.kind], dense: true }), byId, links)
    peerBottom = p + DENSE.row / 2
  }

  // Columns as wide as their labels.
  const widest = (...kinds) => Math.max(0, ...nodes.filter((n) => kinds.includes(n.kind)).map((n) => reach(n)))
  const x = { log: TOP + R.log }
  x.api = x.log
  x.iface = (peers.length ? x.log + widest('log', 'api') + DENSE.gap : TOP) + R.iface
  x.hub = (ifaces.length ? x.iface + widest('iface') + DENSE.gap : peers.length ? x.log + widest('log', 'api') + DENSE.gap : TOP) + R.hub
  x.fe = x.hub + reach(hub) + DENSE.gap + R.fe
  x.be = x.fe + widest('fe') + DENSE.fan + R.be
  x.srv = x.be + widest('be') + DENSE.gap + R.srv
  for (const n of nodes) n.x = x[n.kind === 'more' ? 'srv' : n.kind]

  for (const l of links) {
    // A log link is drawn from its target: left to right too.
    const a = byId[l.kind === 'log' ? l.to : l.from]
    const b = byId[l.kind === 'log' ? l.from : l.to]
    const from = stem(a)
    const to = b.x - b.r
    const xm = (from + to) / 2
    l.d = `M${a.x + a.r} ${a.y}H${from}C${xm} ${a.y} ${xm} ${b.y} ${to} ${b.y}`
  }
  const bottom = Math.max(beBottom, prev + DENSE.row / 2, first + span + DENSE.row / 2, hub.y + DENSE.row / 2, peerBottom)
  const width = Math.max(...nodes.map((n) => n.x + reach(n))) + TOP
  return { nodes, links, byId, width, height: bottom + TOP, hubY: hub.y }
}

// How big a map is drawn: whole in the window down to FIT_MIN of its
// size - its labels still readable - and up to MAX_SCALE on a big screen;
// when it can't be whole, as wide as its card down to MIN_SCALE, the page
// scrolling down it; wider still, it scrolls in its card.
export const FIT_MIN = 0.75
export const MIN_SCALE = 0.85
export const MAX_SCALE = 1.5

// arrange is how the map shows in the room it has, its layouts given
// (across, dense, down: those the choice may take). The shapes the
// choice allows - auto: left to right with its labels under the nodes,
// then beside them, then top to bottom - each with the side panel beside
// the map or below it: the one that shows the map whole in the window
// the biggest - any at full size or more counting as one -, or nearly as
// big and earlier in that order; else the first as wide as its card;
// else the one overflowing it less.
// room.width is the map's with the panel below it, room.side the
// panel's with its gap, room.height the window's below the map's top;
// null before they're measured.
export function arrange(choice, maps, room) {
  const order = choice === 'vertical' ? ['down'] : choice === 'horizontal' ? ['across', 'dense'] : ['across', 'dense', 'down']
  if (!room) return { shape: order[0], beside: false, scale: 1 }
  const options = []
  for (const shape of order) {
    const m = maps[shape]
    for (const beside of [true, false]) {
      const w = beside ? room.width - room.side : room.width
      if (w <= 0) continue
      options.push({ shape, beside, wide: w / m.width, whole: Math.min(w / m.width, room.height / m.height, MAX_SCALE) })
    }
  }
  const pick = (o, scale) => ({ shape: o.shape, beside: o.beside, scale })
  const whole = options.filter((o) => o.whole >= FIT_MIN)
  if (whole.length) {
    const size = (o) => Math.min(o.whole, 1)
    const best = Math.max(...whole.map(size))
    const o = whole.find((o) => size(o) >= best * 0.9)
    return pick(o, o.whole)
  }
  const wide = options.find((o) => o.wide >= MIN_SCALE)
  if (wide) return pick(wide, Math.min(wide.wide, 1))
  // No room at all (a card not laid out): the first shape, as when not
  // measured yet.
  if (!options.length) return { shape: order[0], beside: false, scale: MIN_SCALE }
  return pick(options.reduce((a, o) => (o.wide > a.wide ? o : a)), MIN_SCALE)
}

// related is what a node or link is part of, to bring forward: from a
// frontend, backend or server, the paths through it - up to HAProxy,
// down to the servers - and HAProxy and the interfaces, which every path
// crosses; null for HAProxy and the interfaces (everything).
export function related(map, id) {
  const link = map.links.find((l) => l.id === id)
  // A peer - a log target, an API client: its link and the interface it
  // goes by.
  const isPeer = (n) => n?.kind === 'log' || n?.kind === 'api'
  const peer = link?.kind === 'log' ? map.byId[link.to] : link?.kind === 'api' ? map.byId[link.from] : isPeer(map.byId[id]) ? map.byId[id] : null
  if (peer) {
    const out = new Set([peer.id])
    for (const l of map.links) {
      if (l.to === peer.id || l.from === peer.id) {
        out.add(l.id)
        out.add(l.from)
        out.add(l.to)
      }
    }
    return out
  }
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

// pathLength is the length of a link's path (curve, curveDown: a move
// then cubics), its curves measured on 24 points each.
export function pathLength(d) {
  const n = (d.match(/-?\d+(\.\d+)?/g) || []).map(Number)
  if (n.length < 2) return 0
  let [x, y] = n
  let len = 0
  for (let i = 2; i + 5 < n.length; i += 6) {
    const [x1, y1, x2, y2, x3, y3] = n.slice(i, i + 6)
    let px = x
    let py = y
    for (let k = 1; k <= 24; k++) {
      const u = k / 24
      const a = (1 - u) ** 3
      const b = 3 * (1 - u) ** 2 * u
      const c = 3 * (1 - u) * u ** 2
      const e = u ** 3
      const qx = a * x + b * x1 + c * x2 + e * x3
      const qy = a * y + b * y1 + c * y2 + e * y3
      len += Math.hypot(qx - px, qy - py)
      px = qx
      py = qy
    }
    x = x3
    y = y3
  }
  return len
}

// particles is how a link's traffic shows moving: glowing dots running
// along it, closer together and faster the more bytes go through - as
// many as fit its length at that spacing (one at least, eight at most),
// each taking seconds to cross it (between 0.6 and 8 s, in tenths, so a
// small change of traffic doesn't restart them).
export function particles(bytesPerSecond, length) {
  if (!(bytesPerSecond > 0) || !(length > 0)) return null
  const l = Math.log10(Math.max(1, bytesPerSecond))
  const spacing = Math.min(170, Math.max(45, 170 - 20 * l))
  const speed = 45 + 30 * l // px a second
  return {
    count: Math.min(8, Math.max(1, Math.round(length / spacing))),
    seconds: Math.round(Math.min(8, Math.max(0.6, length / speed)) * 10) / 10,
  }
}

// figures are the numbers the map shows, by node or link id - a node's
// traffic and sessions, HAProxy's connections and requests, a log
// target's records and bytes a second, an API client's calls and bytes
// a second, a link's traffic (traffic, by link id) - what a flash
// watches.
export function figures(map, hap, traffic = {}) {
  const f = {}
  for (const n of map.nodes) {
    const r = n.row
    if (n.kind === 'iface') f[n.id] = { traffic: n.traffic }
    else if (n.kind === 'hub') f[n.id] = { conns: hap?.conns, req: hap?.reqRate }
    else if (n.kind === 'log') f[n.id] = { msgs: n.msgRate, bytes: n.byteRate }
    else if (n.kind === 'api') f[n.id] = { calls: n.callRate, bytes: n.byteRate }
    else if (r) f[n.id] = { traffic: r.traffic, sess: r.scur }
  }
  for (const [id, t] of Object.entries(traffic)) f[id] = { traffic: t }
  return f
}

// risen is what started from nothing between two figures: the ids one
// of whose numbers was 0 and is more now - not one that wasn't known
// (the first sample, a counter that went back).
export function risen(prev, cur) {
  const out = new Set()
  if (!prev) return out
  for (const [id, v] of Object.entries(cur)) {
    const p = prev[id]
    if (!p) continue
    for (const [k, x] of Object.entries(v)) {
      if (p[k] === 0 && x > 0) {
        out.add(id)
        break
      }
    }
  }
  return out
}
