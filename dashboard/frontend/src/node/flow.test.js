// node --test (npm test)
import assert from 'node:assert/strict'
import fs from 'node:fs'
import { test } from 'node:test'
import {
  pathLength,
  particles,
  FIT_MIN,
  MAX_SCALE,
  MIN_SCALE,
  apiPeers,
  arrange,
  buildFlow,
  figures,
  interfaces,
  layout,
  logTargets,
  mapCalls,
  parseTopology,
  protocol,
  pulse,
  reach,
  related,
  resolveLinks,
  risen,
  serverTone,
  statRows,
  tokenize,
  withRates,
} from './flow.js'

// examples/haproxy/web.cfg's proxies.
const WEB = `
global
    stats socket /run/janus/haproxy-admin.sock mode 660 level admin
defaults
    mode http
    timeout client 30s

frontend web
    bind :80
    http-request return status 200 content-type text/plain string "ok\\n" if { path /healthz }
    default_backend app

backend app
    balance roundrobin
    server app1 10.0.10.11:8080
    server app2 10.0.10.12:8080

frontend prometheus
    bind :8405
    http-request use-service prometheus-exporter if { path /metrics }
    no log
`

test('words, quotes, escapes and comments', () => {
  assert.deepEqual(tokenize('  use_backend api if { hdr(host) -i api.example.com }  # the API'), ['use_backend', 'api', 'if', '{', 'hdr(host)', '-i', 'api.example.com', '}'])
  assert.deepEqual(tokenize(`http-request return string "a \\"b\\" # c" 'd\\e'`), ['http-request', 'return', 'string', 'a "b" # c', 'd\\e'])
  assert.deepEqual(tokenize('acl a path_beg /a\\ b ""'), ['acl', 'a', 'path_beg', '/a b', ''])
  assert.deepEqual(tokenize('# only a comment'), [])
  assert.deepEqual(tokenize('\tbind :80\r'), ['bind', ':80'])
})

test('web.cfg: a default backend, a frontend that answers itself', () => {
  const t = parseTopology(WEB)
  assert.deepEqual(t.backends, ['app'])
  assert.deepEqual(t.frontends, [
    { name: 'web', kind: 'frontend', mode: 'http', binds: [':80'], answers: ['return'], rules: [{ type: 'default', backend: 'app', inherited: false }] },
    { name: 'prometheus', kind: 'frontend', mode: 'http', binds: [':8405'], answers: ['use-service prometheus-exporter'], rules: [] },
  ])
  assert.deepEqual(t.maps, [])
})

test('use_backend rules, defaults inherited, listen sections', () => {
  const t = parseTopology(`
defaults base
    mode http
    default_backend fallback
defaults tcp from base
    mode tcp
defaults
    mode http
frontend fe1 from base
    bind 10.0.0.1:443,10.0.0.1:8443 ssl crt /etc/haproxy/files/x.pem
    use_backend api if { req.hdr(host) -i api.example.com }
    use_backend "static" unless is_dynamic
frontend fe2 from tcp
    bind :5432
frontend fe3
    bind :81
.if defined(DEBUG)
    use_backend debug
.endif
listen stats :8404
    stats enable
listen pg
    bind :5433
    default_backend pg_replicas
backend api
backend static
backend fallback
backend debug
backend pg_replicas
`)
  const by = Object.fromEntries(t.frontends.map((f) => [f.name, f]))
  assert.deepEqual(by.fe1.rules, [
    { type: 'use', backend: 'api', cond: 'if { req.hdr(host) -i api.example.com }' },
    { type: 'use', backend: 'static', cond: 'unless is_dynamic' },
    { type: 'default', backend: 'fallback', inherited: true },
  ])
  assert.equal(by.fe1.binds[0], '10.0.0.1:443,10.0.0.1:8443')
  assert.equal(by.fe2.mode, 'tcp')
  assert.deepEqual(by.fe2.rules, [{ type: 'default', backend: 'fallback', inherited: true }]) // tcp from base
  assert.deepEqual(by.fe3.rules, [{ type: 'use', backend: 'debug', cond: '' }]) // the last defaults has none
  assert.deepEqual(by.stats.rules, [{ type: 'self', backend: 'stats' }])
  assert.deepEqual(by.stats.binds, [':8404'])
  assert.deepEqual(by.stats.answers, ['stats page'])
  assert.deepEqual(by.pg.rules, [{ type: 'default', backend: 'pg_replicas', inherited: false }])
  assert.deepEqual(t.backends, ['stats', 'pg', 'api', 'static', 'fallback', 'debug', 'pg_replicas'])
})

test('map converters', () => {
  assert.deepEqual(mapCalls('req.hdr(host),lower,map_dom(/etc/haproxy/files/hosts.map,be_default)'), [{ path: '/etc/haproxy/files/hosts.map', def: 'be_default' }])
  assert.deepEqual(mapCalls('base,map_beg("/etc/haproxy/files/paths.map")'), [{ path: '/etc/haproxy/files/paths.map', def: '' }])
  assert.deepEqual(mapCalls('req.hdr(host),bitmap(1)'), [])
})

test('use_backend through a map, a variable or nothing known', () => {
  const t = parseTopology(`
frontend https
    bind :443
    use_backend be_%[req.hdr(host),lower,map(/etc/haproxy/files/hosts.map,default)]_pool
frontend vars
    bind :80
    http-request set-var(txn.be) req.hdr(host),lower,map_str(/etc/haproxy/files/vars.map) if { req.hdr(host) -m found }
    http-request set-var(txn.be) str(be_api) if { path_beg /api }
    use_backend %[var(txn.be)]
frontend opaque
    bind :82
    use_backend %[req.hdr(x-backend)]
backend be_www_pool
backend be_blog_pool
backend be_default_pool
backend be_api
backend be_a
`)
  const [https, vars, opaque] = t.frontends
  assert.deepEqual(https.rules[0], {
    type: 'dynamic',
    format: 'be_%[req.hdr(host),lower,map(/etc/haproxy/files/hosts.map,default)]_pool',
    cond: '',
    prefix: 'be_',
    suffix: '_pool',
    maps: [{ path: '/etc/haproxy/files/hosts.map', def: 'default' }],
    literals: [],
  })
  assert.deepEqual(vars.rules[0].maps, [{ path: '/etc/haproxy/files/vars.map', def: '' }])
  assert.deepEqual(vars.rules[0].literals, ['be_api'])
  assert.deepEqual(t.maps, ['/etc/haproxy/files/hosts.map', '/etc/haproxy/files/vars.map'])

  const backends = ['be_www_pool', 'be_blog_pool', 'be_default_pool', 'be_api', 'be_a']
  const maps = {
    '/etc/haproxy/files/hosts.map': { 'www.example.com': 'www', 'example.com': 'www', 'blog.example.com': 'blog', 'old.example.com': 'gone' },
  }
  const { links, unresolved } = resolveLinks(t, backends, maps)
  assert.deepEqual(
    links.map((l) => [l.frontend, l.backend, l.rules.map((r) => r.keys || (r.isDefault ? 'default' : r.text))]),
    [
      ['https', 'be_blog_pool', [['blog.example.com']]],
      ['https', 'be_www_pool', [['example.com', 'www.example.com']]],
      ['https', 'be_default_pool', ['default']],
      ['vars', 'be_api', ['use_backend %[var(txn.be)]']],
    ],
  )
  // vars.map isn't readable: the str() literal still resolves the rule.
  assert.deepEqual(unresolved, [{ frontend: 'opaque', format: '%[req.hdr(x-backend)]', maps: [] }])
  assert.equal(opaque.rules.length, 1)
  // Without the map: https can't be resolved.
  assert.deepEqual(resolveLinks(t, backends, {}).unresolved.map((u) => u.frontend), ['https', 'opaque'])
})

// janusctl reads configurations in Go (internal/haproxyflow): both are
// held to the same cases.
test('the cases janusctl shares', () => {
  const cases = JSON.parse(fs.readFileSync(new URL('../../../../internal/haproxyflow/testdata/cases.json', import.meta.url), 'utf8'))
  assert.ok(cases.length >= 6)
  for (const c of cases) {
    const t = parseTopology(c.config)
    assert.deepEqual(
      t.frontends.map(({ name, kind, mode, binds, answers }) => ({ name, kind, mode, binds, answers })),
      c.want.frontends,
      c.name,
    )
    assert.deepEqual(t.backends, c.want.backends, c.name)
    assert.deepEqual(t.maps, c.want.maps, c.name)
    const { links, unresolved } = resolveLinks(t, c.backends, c.maps)
    assert.deepEqual(links, c.want.links, c.name)
    assert.deepEqual(unresolved, c.want.unresolved, c.name)
  }
})

const COLUMNS = ['pxname', 'svname', 'qcur', 'scur', 'smax', 'slim', 'stot', 'bin', 'bout', 'status', 'act', 'bck', 'type', 'rate', 'check_status', 'req_rate', 'addr', 'mode']
const row = (px, sv, type, status, scur, bin, bout, extra = {}) =>
  COLUMNS.map((c) => String({ pxname: px, svname: sv, type, status, scur, bin, bout, qcur: 0, smax: scur, slim: '', stot: 10, act: '', bck: '', rate: 1, check_status: '', req_rate: '', addr: '', mode: 'http', ...extra }[c] ?? ''))

function sample(bin) {
  return statRows({
    columns: COLUMNS,
    rows: [
      row('web', 'FRONTEND', 0, 'OPEN', 3, bin, bin * 2),
      row('prometheus', 'FRONTEND', 0, 'OPEN', 0, 0, 0),
      row('app', 'app1', 2, 'UP', 2, bin / 2, bin, { addr: '10.0.10.11:8080', check_status: 'L7OK' }),
      row('app', 'app2', 2, 'DOWN', 0, 0, 0, { addr: '10.0.10.12:8080', check_status: '* L4CON' }),
      row('app', 'BACKEND', 1, 'UP', 2, bin / 2, bin),
      row('<HTTPCLIENT>', 'BACKEND', 1, 'UP', 0, 0, 0),
    ],
  })
}

test('counters, rates and a reload', () => {
  const a = sample(1000)
  assert.equal(a[0].proxy, 'web')
  assert.equal(a[0].type, 'frontend')
  assert.equal(a[0].scur, 3)
  assert.equal(a[0].slim, null)
  assert.equal(a[3].check, 'L4CON') // "* L4CON": a check under way
  const first = withRates(a, null, 0)
  assert.equal(first[0].traffic, undefined)
  const b = withRates(sample(3000), a, 2)
  assert.equal(b[0].inRate, 1000)
  assert.equal(b[0].outRate, 2000)
  assert.equal(b[0].traffic, 3000)
  // HAProxy reloaded: its counters start again - no rate, never a negative one.
  const c = withRates(sample(10), sample(3000), 2)
  assert.equal(c[0].inRate, undefined)
  assert.equal(c[0].traffic, undefined)
})

test('server tones', () => {
  assert.equal(serverTone('UP'), 'ok')
  assert.equal(serverTone('no check'), 'ok')
  assert.equal(serverTone('UP 1/3'), 'warn')
  assert.equal(serverTone('DRAIN'), 'warn')
  assert.equal(serverTone('MAINT (via app/app1)'), 'off')
  assert.equal(serverTone('DOWN'), 'danger')
  assert.equal(serverTone('DOWN 1/2'), 'danger')
})

test('the flow: counters joined to the configuration', () => {
  const rows = withRates(sample(3000), sample(1000), 2)
  const g = buildFlow(rows, parseTopology(WEB), {})
  assert.deepEqual(g.frontends.map((f) => [f.id, f.tone, f.answers]), [
    ['fe:web', 'ok', ['return']],
    ['fe:prometheus', 'ok', ['use-service prometheus-exporter']],
  ])
  assert.deepEqual(g.backends.map((b) => [b.id, b.tone, b.down, b.servers.map((s) => `${s.name}:${s.tone}`)]), [['be:app', 'warn', 1, ['app1:ok', 'app2:danger']]])
  assert.deepEqual(g.links.map((l) => [l.from, l.to, l.rules[0].text]), [['fe:web', 'be:app', 'default_backend app']])
  // The configuration not readable: the same proxies, nothing linking them.
  const bare = buildFlow(rows, null, {})
  assert.equal(bare.frontends.length, 2)
  assert.equal(bare.links.length, 0)
  assert.equal(bare.backends[0].servers.length, 2)
  assert.equal(g.configKnown, true)
  assert.equal(bare.configKnown, false)
})

test('interfaces: the loopback left out, the state when known', () => {
  const net = { lo: { rx: 1, tx: 1 }, eth10: { rx: 5, tx: 5 }, eth2: { rx: undefined, tx: undefined } }
  assert.deepEqual(interfaces(net, null).map((i) => [i.name, i.up, i.traffic]), [
    ['eth2', null, undefined],
    ['eth10', null, 10],
  ])
  const status = { interfaces: [{ name: 'lo', kind: 'loopback', up: true, carrier: true }, { name: 'eth2', kind: 'physical', up: true, carrier: false }, { name: 'eth10', kind: 'physical', up: true, carrier: true }] }
  assert.deepEqual(interfaces(net, status).map((i) => [i.name, i.up, i.tone]), [
    ['eth2', false, 'off'],
    ['eth10', true, ''],
  ])
})

function overlaps(nodes, minGap) {
  const cols = {}
  for (const n of nodes) (cols[n.x] ||= []).push(n.y)
  for (const ys of Object.values(cols)) {
    ys.sort((a, b) => a - b)
    for (let i = 1; i < ys.length; i++) if (ys[i] - ys[i - 1] < minGap) return true
  }
  return false
}

test('layout: columns, bands, no overlap', () => {
  const cfg = ['frontend a', 'use_backend b2 if x', 'default_backend b1', 'frontend c', 'default_backend b1', 'frontend d', 'default_backend b3', 'frontend lonely']
  const backends = ['b1', 'b2', 'b3', 'b4']
  const rows = []
  for (const f of ['a', 'c', 'd', 'lonely']) rows.push(...statRows({ columns: COLUMNS, rows: [row(f, 'FRONTEND', 0, 'OPEN', 0, 0, 0)] }))
  for (const b of backends) {
    const n = b === 'b1' ? 12 : b === 'b2' ? 3 : 1
    for (let i = 0; i < n; i++) rows.push(...statRows({ columns: COLUMNS, rows: [row(b, `s${i}`, 2, 'UP', 0, 0, 0)] }))
    rows.push(...statRows({ columns: COLUMNS, rows: [row(b, 'BACKEND', 1, 'UP', 0, 0, 0)] }))
  }
  const g = buildFlow(rows, parseTopology(cfg.join('\n')), {})
  const ifs = interfaces({ eth0: { rx: 1, tx: 1 }, eth1: { rx: 0, tx: 0 } }, null)
  const m = layout(g, ifs)

  // Backends in the order the frontends name them, the unnamed one last.
  const bes = m.nodes.filter((n) => n.kind === 'be').sort((p, q) => p.y - q.y)
  assert.deepEqual(bes.map((b) => [b.name, b.linked]), [['b2', true], ['b1', true], ['b3', true], ['b4', false]])
  // b1: 7 servers shown and "+5 more".
  const b1 = m.nodes.filter((n) => n.backend === 'b1')
  assert.equal(b1.filter((n) => n.kind === 'srv').length, 7)
  assert.deepEqual(b1.filter((n) => n.kind === 'more').map((n) => n.name), ['+5 more'])
  assert.equal(overlaps(m.nodes.filter((n) => n.kind !== 'srv' && n.kind !== 'more'), 92), false)
  assert.equal(overlaps(m.nodes.filter((n) => n.kind === 'srv' || n.kind === 'more'), 46), false)
  // a sits between b2 and b1, c level with b1, the lonely frontend last.
  const y = (id) => m.byId[id].y
  assert.ok(y('fe:a') > y('be:b2') && y('fe:a') < y('be:b1'))
  assert.ok(y('fe:lonely') > y('fe:d'))
  assert.ok(m.nodes.every((n) => n.y - n.r >= 0 && n.y + n.r <= m.height))
  // Every link drawn from one node to another.
  assert.equal(m.links.length, 2 + 4 + 4 + (7 + 1) + 3 + 1 + 1) // interfaces, frontends, a->b2 a->b1 c->b1 d->b3, servers
  assert.ok(m.links.every((l) => /^M[\d.]+ [\d.]+C/.test(l.d)))

  // Without servers: one row per backend.
  const slim = layout(g, ifs, { servers: false })
  assert.equal(slim.nodes.filter((n) => n.kind === 'srv').length, 0)
  assert.ok(slim.width < m.width)

  // Top to bottom: rows, the servers hanging below their backend.
  const v = layout(g, ifs, { vertical: true })
  const level = (kind) => v.nodes.filter((n) => n.kind === kind)
  const ys = ['iface', 'hub', 'fe', 'be'].map((k) => [...new Set(level(k).map((n) => n.y))])
  assert.ok(ys.every((r) => r.length === 1), 'one row per kind')
  assert.ok(ys[0][0] < ys[1][0] && ys[1][0] < ys[2][0] && ys[2][0] < ys[3][0])
  // A label never reaches the next node of its row; a backend's
  // servers' labels never reach the next backend.
  for (const k of ['iface', 'fe', 'be']) {
    const ns = level(k).sort((p, q) => p.x - q.x)
    assert.ok(
      ns.every((n, i) => i === 0 || n.x - n.r >= ns[i - 1].x + reach(ns[i - 1])),
      `${k}: labels apart`,
    )
  }
  const bx = level('be').sort((p, q) => p.x - q.x)
  for (const [i, b] of bx.entries()) {
    const next = bx[i + 1]
    if (next) assert.ok(v.nodes.filter((n) => n.backend === b.name).every((n) => n.x + reach(n) <= next.x - next.r))
  }
  assert.deepEqual(
    level('be')
      .sort((p, q) => p.x - q.x)
      .map((b) => b.name),
    ['b2', 'b1', 'b3', 'b4'],
  )
  const vb1 = v.byId['be:b1']
  const under = v.nodes.filter((n) => n.backend === 'b1')
  assert.equal(under.length, 8)
  assert.ok(under.every((n, i) => n.x > vb1.x && n.y > vb1.y && (i === 0 || n.y > under[i - 1].y)))
  // a above the middle of b2 and b1; HAProxy above its frontends.
  assert.equal(v.byId['fe:a'].x, (v.byId['be:b2'].x + vb1.x) / 2)
  const fx = level('fe').map((n) => n.x)
  assert.equal(v.byId.hub.x, (Math.min(...fx) + Math.max(...fx)) / 2)
  // The same links, and a trunk per backend with servers.
  assert.equal(v.links.length, m.links.length + 4)
  assert.equal(v.links.filter((l) => l.kind === 'trunk').length, 4)
  assert.ok(
    v.links.every((l) => /^M[\d.]+ [\d.]+[CVQ]/.test(l.d)),
    v.links.map((l) => l.d).join(' '),
  )
  assert.ok(v.nodes.every((n) => n.x - n.r >= 0 && n.x <= v.width && n.y + n.r <= v.height))
  // The configuration not readable: no frontend sends anywhere known.
  for (const shape of ['across', 'dense', 'down']) {
    const bare = layout(buildFlow(rows, null, {}), ifs, { shape })
    assert.ok(bare.nodes.every((n) => Number.isFinite(n.x) && Number.isFinite(n.y)), shape)
    assert.ok(bare.links.every((l) => !/NaN|Infinity/.test(l.d)), shape)
    assert.ok(Number.isFinite(bare.width) && Number.isFinite(bare.height), shape)
    // The middle node is Janus in every shape - dense, the one a big
    // configuration gets, kept "HAProxy" once.
    assert.equal(bare.byId.hub.name, 'Janus', shape)
  }

  // Dense: columns as across's, the labels beside the nodes, rows half
  // as tall.
  const d = layout(g, ifs, { shape: 'dense' })
  const col = (kind) => d.nodes.filter((n) => n.kind === kind)
  const xs = ['iface', 'hub', 'fe', 'be', 'srv'].map((k) => [...new Set(col(k).map((n) => n.x))])
  assert.ok(xs.every((c) => c.length === 1), 'one column per kind')
  assert.ok(xs.every((c, i) => i === 0 || c[0] > xs[i - 1][0]))
  // A column starts past the widest label of the one before.
  for (const [a, b] of [['iface', 'hub'], ['hub', 'fe'], ['fe', 'be'], ['be', 'srv']]) {
    assert.ok(col(b)[0].x - col(b)[0].r > Math.max(...col(a).map((n) => n.x + reach(n))), `${a} -> ${b}`)
  }
  assert.deepEqual(
    col('be')
      .sort((p, q) => p.y - q.y)
      .map((b) => b.name),
    ['b2', 'b1', 'b3', 'b4'],
  )
  assert.equal(overlaps(d.nodes.filter((n) => n.kind !== 'srv' && n.kind !== 'more'), 44), false)
  assert.equal(overlaps(d.nodes.filter((n) => n.kind === 'srv' || n.kind === 'more'), 28), false)
  assert.ok(d.height < m.height * 0.75, `${d.height} vs ${m.height}`)
  assert.ok(d.nodes.every((n) => n.y - n.r >= 0 && n.y + n.r <= d.height && n.x + reach(n) <= d.width))
  // Links leave past their node's label, under its name.
  assert.equal(d.links.length, m.links.length)
  for (const l of d.links) {
    const a = d.byId[l.from]
    const [, x1, y1, x2] = /^M([\d.]+) ([\d.]+)H([\d.]+)C/.exec(l.d).map(Number)
    assert.ok(x1 === a.x + a.r && y1 === a.y && x2 > a.x + reach(a), l.d)
  }
  // Its size doesn't follow the numbers: a refresh never moves it.
  const busy = buildFlow(
    rows.map((r) => ({ ...r, traffic: 9e9, inRate: 5e9, outRate: 4e9, scur: 99999 })),
    parseTopology(cfg.join('\n')),
    {},
  )
  const d2 = layout(busy, ifs, { shape: 'dense' })
  assert.deepEqual([d2.width, d2.height], [d.width, d.height])
})

test('arrange: the shape, the side panel and the size for the room there is', () => {
  const maps = { across: { width: 1000, height: 400 }, dense: { width: 1100, height: 300 }, down: { width: 600, height: 800 } }
  const room = (width, height, side = 368) => ({ width, side, height })
  // Whole in the window, as big as it gets: left to right first, its
  // labels under the nodes, the panel beside the map.
  assert.deepEqual(arrange('auto', maps, room(1500, 900)), { shape: 'across', beside: true, scale: 1132 / 1000 })
  // A big screen: bigger, up to MAX_SCALE.
  assert.deepEqual(arrange('auto', maps, room(2400, 1400)), { shape: 'across', beside: true, scale: MAX_SCALE })
  // Below it when that's what makes it whole.
  assert.deepEqual(arrange('auto', maps, room(1000, 900)), { shape: 'across', beside: false, scale: 1 })
  // Too tall left to right with its labels under the nodes: beside them.
  const tall = { ...maps, across: { width: 1000, height: 1600 }, dense: { width: 1100, height: 800 } }
  assert.deepEqual(arrange('auto', tall, room(1900, 880)), { shape: 'dense', beside: true, scale: 1.1 })
  // Nearly as big comes first: across at 1.0 before dense at 1.05.
  const close = { ...maps, dense: { width: 950, height: 380 } }
  assert.equal(arrange('auto', close, room(1000, 2000)).shape, 'across')
  // Top to bottom when only that is whole.
  assert.deepEqual(arrange('auto', maps, room(600, 900)), { shape: 'down', beside: false, scale: 1 })
  // Never smaller than FIT_MIN to be whole: as wide as the card instead,
  // the page scrolling down it, the first that is.
  assert.deepEqual(arrange('auto', tall, room(1050, 300)), { shape: 'across', beside: false, scale: 1 })
  assert.ok(arrange('auto', tall, room(1050, 1600 * FIT_MIN)).scale >= FIT_MIN)
  // Wider than the card at MIN_SCALE: the one overflowing less.
  assert.deepEqual(arrange('auto', maps, room(340, 700)), { shape: 'down', beside: false, scale: MIN_SCALE })
  // A choice is kept, whatever the room: horizontal takes either way of
  // labelling, vertical only top to bottom.
  assert.equal(arrange('horizontal', tall, room(1900, 880)).shape, 'dense')
  assert.deepEqual(arrange('vertical', { down: { width: 3600, height: 400 } }, room(1500, 900)), { shape: 'down', beside: false, scale: MIN_SCALE })
  // Not measured yet: the first shape, the panel below, its own size.
  assert.deepEqual(arrange('auto', maps, null), { shape: 'across', beside: false, scale: 1 })
})

test('related: the paths through a node', () => {
  const rows = withRates(sample(3000), sample(1000), 2)
  const m = layout(buildFlow(rows, parseTopology(WEB), {}), interfaces({ eth0: { rx: 1, tx: 1 } }, null))
  assert.equal(related(m, 'hub'), null)
  assert.equal(related(m, 'if:eth0'), null)
  const always = ['hub', 'if:eth0', 'if:eth0>hub']
  assert.deepEqual([...related(m, 'srv:app/app1')].sort(), ['be:app', 'be:app>srv:app/app1', 'fe:web', 'fe:web>be:app', 'hub>fe:web', ...always, 'srv:app/app1'].sort())
  assert.deepEqual(
    [...related(m, 'fe:web>be:app')].sort(),
    ['be:app', 'be:app>srv:app/app1', 'be:app>srv:app/app2', 'fe:web', 'fe:web>be:app', 'hub>fe:web', ...always, 'srv:app/app1', 'srv:app/app2'].sort(),
  )
  // prometheus sends nowhere: only its own path.
  assert.ok(!related(m, 'fe:prometheus').has('be:app'))
  // Top to bottom, a backend's trunk goes with it.
  const v = layout(buildFlow(rows, parseTopology(WEB), {}), interfaces({ eth0: { rx: 1, tx: 1 } }, null), { vertical: true })
  assert.ok(related(v, 'srv:app/app2').has('be:app>trunk'))
  assert.ok(related(v, 'fe:web').has('be:app>trunk'))
  assert.ok(!related(v, 'fe:prometheus').has('be:app>trunk'))
})

test('pulse', () => {
  assert.equal(pulse(0), null)
  assert.equal(pulse(undefined), null)
  const slow = pulse(1000)
  const fast = pulse(100e6)
  assert.ok(fast.width > slow.width && fast.seconds < slow.seconds)
  assert.ok(fast.width <= 5 && fast.seconds >= 0.35)
})

// A frontend in each mode, and the backends they send to.
const MODES = `
defaults
    mode http
frontend web
    bind :80
    default_backend app
frontend db
    mode tcp
    bind :5432
    default_backend pg
backend app
    server a1 10.0.0.1:80
backend pg
    mode tcp
    server p1 10.0.0.2:5432
`
function modeRows() {
  return statRows({
    columns: COLUMNS,
    rows: [
      row('web', 'FRONTEND', 0, 'OPEN', 1, 10, 10),
      row('db', 'FRONTEND', 0, 'OPEN', 1, 10, 10, { mode: 'tcp' }),
      row('app', 'a1', 2, 'UP', 1, 10, 10),
      row('app', 'BACKEND', 1, 'UP', 1, 10, 10),
      row('pg', 'p1', 2, 'UP', 1, 10, 10, { mode: 'tcp' }),
      row('pg', 'BACKEND', 1, 'UP', 1, 10, 10, { mode: 'tcp' }),
    ],
  })
}

test('protocol: a link is its frontend\'s or backend\'s mode', () => {
  const g = buildFlow(modeRows(), parseTopology(MODES), {})
  assert.deepEqual(g.backends.map((b) => [b.name, b.mode, b.servers.map((s) => s.mode)]), [
    ['app', 'http', ['http']],
    ['pg', 'tcp', ['tcp']],
  ])
  const m = layout(g, interfaces({ eth0: { rx: 1, tx: 1 } }, null))
  const by = Object.fromEntries(m.links.map((l) => [l.id, protocol(l, m.byId)]))
  assert.deepEqual(by, {
    'if:eth0>hub': '',
    'hub>fe:web': 'http',
    'hub>fe:db': 'tcp',
    'fe:web>be:app': 'http',
    'fe:db>be:pg': 'tcp',
    'be:app>srv:app/a1': 'http',
    'be:pg>srv:pg/p1': 'tcp',
  })
  // A TCP frontend handing its connections to an HTTP backend: HTTP.
  const tcpToHttp = buildFlow(
    modeRows().map((r) => (r.proxy === 'web' ? { ...r, mode: 'tcp' } : r)),
    parseTopology(MODES),
    {},
  )
  const t = layout(tcpToHttp, [])
  assert.equal(protocol(t.links.find((l) => l.id === 'fe:web>be:app'), t.byId), 'http')
  assert.equal(protocol(t.links.find((l) => l.id === 'hub>fe:web'), t.byId), 'tcp')
})

// /api/log-forwarding's answer (proto names, 64-bit counters as strings).
function logAnswer(sent, bytes, extra = {}) {
  return {
    config: {
      targets: [
        { name: 'rsyslog', syslog: { address: '10.0.31.20:601', transport: 'tcp' }, elasticsearch: null },
        { name: 'elastic', syslog: null, elasticsearch: { url: 'https://10.0.30.30:9200' } },
        { name: 'old', syslog: { address: '10.0.30.20:514' }, elasticsearch: null },
        { name: 'off', disabled: true, syslog: { address: '10.0.30.20:514' }, elasticsearch: null },
      ],
    },
    targets: [
      { name: 'rsyslog', kind: 'syslog', state: 'ok', sent: String(sent), bytes_sent: String(bytes), interface: 'eth1', remote_address: '10.0.31.20:601', ...extra },
      { name: 'elastic', kind: 'elasticsearch', state: 'failing', sent: '7', bytes_sent: '900', interface: 'eth0' },
      { name: 'old', kind: 'syslog', state: 'ok', sent: String(sent) }, // a node older than bytes and interfaces
      { name: 'off', kind: 'syslog', state: 'disabled', sent: '0', bytes_sent: '0', interface: '' },
    ],
  }
}

test('log targets: records and bytes a second, the interface, the state', () => {
  const first = logTargets(logAnswer(100, 10000), null, 0)
  assert.deepEqual(first.map((t) => [t.id, t.es, t.via, t.msgRate, t.byteRate, t.knowsBytes, t.tone]), [
    ['log:rsyslog', false, 'eth1', undefined, undefined, true, 'ok'],
    ['log:elastic', true, 'eth0', undefined, undefined, true, 'danger'],
    ['log:old', false, null, undefined, undefined, false, 'ok'],
    ['log:off', false, '', undefined, undefined, true, 'off'],
  ])
  const t = logTargets(logAnswer(150, 16000), logAnswer(100, 10000), 5)
  assert.deepEqual([t[0].msgRate, t[0].byteRate], [10, 1200])
  assert.deepEqual([t[2].msgRate, t[2].byteRate], [10, undefined])
  // janusd restarted: its counters start again - no rate.
  const back = logTargets(logAnswer(3, 300), logAnswer(150, 16000), 5)
  assert.deepEqual([back[0].msgRate, back[0].byteRate], [undefined, undefined])
  assert.deepEqual(logTargets(null, null, 0), [])
})

test('layout with log targets: beyond the interfaces they leave by', () => {
  const g = buildFlow(modeRows(), parseTopology(MODES), {})
  const ifs = interfaces({ eth0: { rx: 1, tx: 1 }, eth1: { rx: 1, tx: 1 } }, null)
  const logs = logTargets(logAnswer(150, 16000), logAnswer(100, 10000), 5)
  for (const shape of ['across', 'dense', 'down']) {
    const plain = layout(g, ifs, { shape })
    const m = layout(g, ifs, { shape, peers: logs })
    const lg = m.nodes.filter((n) => n.kind === 'log')
    assert.equal(lg.length, 4, shape)
    // Linked to their interface when it's on the map, not otherwise.
    assert.deepEqual(
      m.links.filter((l) => l.kind === 'log').map((l) => l.id),
      ['if:eth0>log:elastic', 'if:eth1>log:rsyslog'],
      shape,
    )
    assert.deepEqual(lg.filter((n) => !n.linked).map((n) => n.name), ['old', 'off'], shape)
    assert.ok(m.links.every((l) => !/NaN|Infinity|undefined/.test(l.d)), shape)
    // Beyond the interfaces: left of them, or above them top to bottom.
    const iface = m.nodes.filter((n) => n.kind === 'iface')
    if (shape === 'down') {
      assert.ok(lg.every((n) => n.y < Math.min(...iface.map((i) => i.y))), shape)
      const row = [...lg].sort((p, q) => p.x - q.x)
      assert.ok(row.every((n, i) => i === 0 || n.x - n.r >= row[i - 1].x + reach(row[i - 1])), `${shape}: labels apart`)
      assert.equal(m.byId['log:rsyslog'].x, m.byId['if:eth1'].x)
    } else {
      assert.ok(lg.every((n) => n.x + reach(n) < Math.min(...iface.map((i) => i.x - i.r)) || shape === 'across'), shape)
      assert.ok(lg.every((n) => n.x < Math.min(...iface.map((i) => i.x))), shape)
      assert.equal(overlaps(lg, shape === 'dense' ? 44 : 92), false, shape)
      // Level with their interface; the unlinked ones after.
      assert.equal(m.byId['log:rsyslog'].y, m.byId['if:eth1'].y, shape)
      assert.equal(m.byId['log:elastic'].y, m.byId['if:eth0'].y, shape)
      assert.ok(m.byId['log:old'].y > m.byId['log:rsyslog'].y, shape)
    }
    assert.ok(m.nodes.every((n) => n.x - n.r >= 0 && n.x + reach(n) <= m.width + 1 && n.y - n.r >= 0 && n.y + n.r <= m.height), shape)
    // Across, a label is centred under its node: a log target's whole.
    if (shape === 'across') assert.ok(lg.every((n) => n.x >= (22 * 6.7) / 2), shape)
    // Without log targets, the map is what it was.
    assert.deepEqual(layout(g, ifs, { shape, peers: [] }), plain, shape)
  }
})

test('related: a log target is its link and its interface', () => {
  const g = buildFlow(modeRows(), parseTopology(MODES), {})
  const ifs = interfaces({ eth0: { rx: 1, tx: 1 }, eth1: { rx: 1, tx: 1 } }, null)
  const m = layout(g, ifs, { peers: logTargets(logAnswer(1, 1), null, 0) })
  assert.deepEqual([...related(m, 'log:rsyslog')].sort(), ['if:eth1', 'if:eth1>log:rsyslog', 'log:rsyslog'])
  assert.deepEqual([...related(m, 'if:eth1>log:rsyslog')].sort(), ['if:eth1', 'if:eth1>log:rsyslog', 'log:rsyslog'])
  // A frontend's paths leave the log targets out.
  assert.ok(!related(m, 'fe:web').has('log:rsyslog'))
})

test('arrange: no room at all - a card not laid out - is the first shape, not a crash', () => {
  const g = buildFlow(modeRows(), parseTopology(MODES), {})
  const maps = Object.fromEntries(['across', 'dense', 'down'].map((shape) => [shape, layout(g, [], { shape })]))
  for (const width of [0, -21]) {
    assert.deepEqual(arrange('auto', maps, { width, side: 368, height: 800 }), { shape: 'across', beside: false, scale: MIN_SCALE })
    assert.deepEqual(arrange('vertical', maps, { width, side: 368, height: 800 }), { shape: 'down', beside: false, scale: MIN_SCALE })
  }
})

test('risen: what started from nothing', () => {
  const g = buildFlow(withRates(modeRows(), modeRows(), 2), parseTopology(MODES), {})
  const m = layout(g, [], { peers: [...apiPeers(clientAnswer(20, 9000), clientAnswer(10, 4000), 5), ...logTargets(logAnswer(150, 16000), logAnswer(100, 10000), 5)] })
  const a = figures(m, { conns: 0, reqRate: 3 }, { 'hub>fe:web': 0 })
  assert.equal(a['fe:web'].traffic, 0)
  assert.equal(a.hub.conns, 0)
  assert.equal(a['log:rsyslog'].msgs, 10)
  assert.deepEqual(a['api:controller:eth0'], { calls: 2, bytes: 1000 })
  const busier = buildFlow(
    withRates(
      modeRows().map((r) => (r.proxy === 'web' ? { ...r, bin: r.bin + 100 } : r)),
      modeRows(),
      2,
    ),
    parseTopology(MODES),
    {},
  )
  const b = figures(layout(busier, [], {}), { conns: 2, reqRate: 3 }, { 'hub>fe:web': 50 })
  assert.deepEqual([...risen(a, b)].sort(), ['fe:web', 'hub', 'hub>fe:web'])
  // The first figures, or a number unknown before: nothing flashes.
  assert.equal(risen(null, b).size, 0)
  assert.equal(risen({ 'fe:web': { traffic: undefined, sess: 1 } }, b).size, 0)
  // Back to nothing: nothing flashes either.
  assert.equal(risen(b, a).size, 0)
})

// /api/system/clients's answer (proto names, 64-bit counters as
// strings): the Controller - a connection on eth0, its users -, two
// clients of their own on eth0, one on lo (not on the map).
function clientAnswer(calls, bytes, { gone = false } = {}) {
  const c = (id, extra) => ({ id: String(id), calls: String(calls), bytes_received: String(bytes / 2), bytes_sent: String(bytes / 2), opened_unix_ns: '1792000000000000000', ...extra })
  return {
    connections: [
      c(1, {
        controller: true,
        name: 'controller',
        roles: ['janus:controller'],
        fleet: true,
        interface: 'eth0',
        remote_address: '10.0.30.10:51000',
        self: true,
        users: [
          { name: 'alice', roles: ['os:admin'], calls: '9', last_call_unix_ns: '1792000000900000000', self: true },
          { name: 'automation', roles: ['os:admin'], calls: '2', last_call_unix_ns: '1792000000100000000' },
        ],
        user_count: 3,
      }),
      c(2, { name: 'bob', roles: ['os:reader'], interface: 'eth0', remote_address: '10.0.30.99:41000' }),
      ...(gone ? [] : [c(3, { name: '', roles: [], interface: 'eth0' })]),
      c(4, { name: 'local', interface: 'lo' }),
    ],
  }
}

test('API clients: the Controller, the others by interface - their calls and bytes a second', () => {
  const first = apiPeers(clientAnswer(10, 4000), null, 0)
  assert.deepEqual(
    first.map((p) => [p.id, p.controller, p.name, p.via, p.conns.length, p.callRate, p.byteRate, p.userCount, p.self]),
    [
      ['api:controller:eth0', true, 'Controller', 'eth0', 1, undefined, undefined, 3, true],
      ['api:clients:eth0', false, 'Direct clients', 'eth0', 2, undefined, undefined, 0, false],
      ['api:clients:lo', false, 'Direct clients', 'lo', 1, undefined, undefined, 0, false],
    ],
  )
  assert.deepEqual(
    first[0].users.map((u) => u.name),
    ['alice', 'automation'],
  )
  const p = apiPeers(clientAnswer(20, 9000), clientAnswer(10, 4000), 5)
  assert.deepEqual([p[0].callRate, p[0].inRate, p[0].outRate, p[0].byteRate], [2, 500, 500, 1000])
  assert.deepEqual([p[1].callRate, p[1].byteRate], [4, 2000])
  // A connection new since the last answer counts from the next one; one
  // gone, not at all.
  const fresh = apiPeers(clientAnswer(20, 9000), clientAnswer(10, 4000, { gone: true }), 5)
  assert.deepEqual([fresh[1].callRate, fresh[1].byteRate], [2, 1000])
  // Counters that went back (janusd restarted): no rate.
  const back = apiPeers(clientAnswer(1, 100), clientAnswer(20, 9000), 5)
  assert.deepEqual([back[0].callRate, back[0].byteRate], [undefined, undefined])
  assert.deepEqual(apiPeers(null, null, 0), [])
})

test('layout with API clients: beyond the interfaces they come by, with the log targets', () => {
  const g = buildFlow(modeRows(), parseTopology(MODES), {})
  const ifs = interfaces({ eth0: { rx: 1, tx: 1 }, eth1: { rx: 1, tx: 1 } }, null)
  const peers = [...apiPeers(clientAnswer(20, 9000), clientAnswer(10, 4000), 5), ...logTargets(logAnswer(150, 16000), logAnswer(100, 10000), 5)]
  for (const shape of ['across', 'dense', 'down']) {
    const m = layout(g, ifs, { shape, peers })
    // From the client to its interface; the log targets' the other way.
    assert.deepEqual(
      m.links.filter((l) => l.kind === 'api').map((l) => [l.id, l.from, l.to]),
      [
        ['api:controller:eth0>if:eth0', 'api:controller:eth0', 'if:eth0'],
        ['api:clients:eth0>if:eth0', 'api:clients:eth0', 'if:eth0'],
      ],
      shape,
    )
    assert.equal(m.byId['api:clients:lo'].linked, false, shape)
    assert.equal(protocol(m.links.find((l) => l.kind === 'api'), m.byId), 'api')
    const ps = m.nodes.filter((n) => n.kind === 'api' || n.kind === 'log')
    assert.equal(ps.length, 7, shape)
    assert.ok(m.links.every((l) => !/NaN|Infinity|undefined/.test(l.d)), shape)
    if (shape === 'down') {
      const row = [...ps].sort((p, q) => p.x - q.x)
      assert.ok(row.every((n, i) => i === 0 || n.x - n.r >= row[i - 1].x + reach(row[i - 1])), `${shape}: labels apart`)
      // Those of eth0 around it, in their order, the Controller first,
      // then eth1's: no links cross.
      const [c, d, e] = ['api:controller:eth0', 'api:clients:eth0', 'log:elastic'].map((id) => m.byId[id].x)
      assert.ok(c < d && d < e && e < m.byId['log:rsyslog'].x, `${shape}: ${c} ${d} ${e}`)
      assert.ok(Math.abs(d - m.byId['if:eth0'].x) <= d - c, `${shape}: the direct clients near eth0`)
    } else {
      const row = shape === 'dense' ? 44 : 92
      assert.equal(overlaps(ps, row), false, shape)
      // Those of eth0 around it, in their order - the Controller first -,
      // rsyslog near eth1: no further than the map's top pushes them.
      const [c, d, e] = ['api:controller:eth0', 'api:clients:eth0', 'log:elastic'].map((id) => m.byId[id].y)
      assert.ok(c < d && d < e, `${shape}: ${c} ${d} ${e}`)
      assert.ok(Math.abs(d - m.byId['if:eth0'].y) <= row, shape)
      assert.ok(Math.abs(m.byId['log:rsyslog'].y - m.byId['if:eth1'].y) <= 2 * row, `${shape}: rsyslog at ${m.byId['log:rsyslog'].y}, eth1 at ${m.byId['if:eth1'].y}`)
      // eth0's together, then eth1's: no links cross.
      assert.ok(e < m.byId['log:rsyslog'].y, shape)
      // Those no interface on the map says, after.
      assert.ok(m.byId['api:clients:lo'].y > m.byId['log:rsyslog'].y, shape)
      assert.ok(ps.every((n) => n.x < Math.min(...m.nodes.filter((i) => i.kind === 'iface').map((i) => i.x))), shape)
    }
    assert.ok(m.nodes.every((n) => n.x - n.r >= 0 && n.x + reach(n) <= m.width + 1 && n.y - n.r >= 0 && n.y + n.r <= m.height), shape)
    // Across, a label is centred under its node: "999 calls/s · 999.9 KiB/s" whole.
    if (shape === 'across') assert.ok(ps.every((n) => n.x >= (25 * 6.7) / 2), shape)
  }
})

test('related: an API client is its link and its interface', () => {
  const g = buildFlow(modeRows(), parseTopology(MODES), {})
  const ifs = interfaces({ eth0: { rx: 1, tx: 1 } }, null)
  const m = layout(g, ifs, { peers: apiPeers(clientAnswer(1, 2), null, 0) })
  const want = ['api:controller:eth0', 'api:controller:eth0>if:eth0', 'if:eth0']
  assert.deepEqual([...related(m, 'api:controller:eth0')].sort(), want)
  assert.deepEqual([...related(m, 'api:controller:eth0>if:eth0')].sort(), want)
  assert.ok(!related(m, 'fe:web').has('api:controller:eth0'))
})

test('a link path is measured along its curve', () => {
  assert.equal(Math.round(pathLength('M0 0C10 0 20 0 30 0')), 30)
  // an S curve is longer than its chord
  const len = pathLength('M0 0C50 0 50 100 100 100')
  assert.ok(len > Math.hypot(100, 100) && len < 200, String(len))
  assert.equal(pathLength(''), 0)
})

test('particles: closer and faster with more traffic, within bounds', () => {
  assert.equal(particles(0, 200), null)
  assert.equal(particles(100, 0), null)
  const slow = particles(100, 300)
  const fast = particles(1e6, 300)
  assert.ok(fast.count > slow.count && fast.seconds < slow.seconds, JSON.stringify({ slow, fast }))
  for (const b of [1, 1e3, 1e6, 1e9]) {
    for (const len of [10, 100, 1000, 5000]) {
      const p = particles(b, len)
      assert.ok(p.count >= 1 && p.count <= 8 && p.seconds >= 0.6 && p.seconds <= 8, JSON.stringify({ b, len, p }))
      assert.equal(p.seconds, Math.round(p.seconds * 10) / 10)
    }
  }
})
