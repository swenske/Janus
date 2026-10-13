// node --test (npm test)
import assert from 'node:assert/strict'
import fs from 'node:fs'
import { test } from 'node:test'
import { MIN_SCALE, arrange, buildFlow, interfaces, layout, mapCalls, parseTopology, pulse, reach, related, resolveLinks, serverTone, statRows, tokenize, withRates } from './flow.js'

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
  const bare = layout(buildFlow(rows, null, {}), ifs, { vertical: true })
  assert.ok(bare.nodes.every((n) => Number.isFinite(n.x) && Number.isFinite(n.y)))
  assert.ok(bare.links.every((l) => !/NaN|Infinity/.test(l.d)))
  assert.ok(Number.isFinite(bare.width))
})

test('arrange: the orientation and the side panel for the room there is', () => {
  const maps = { across: { width: 1000 }, down: { width: 600 } }
  const at = (beside, below) => ({ beside, below })
  // Auto: left to right first, the panel beside it when it still fits.
  assert.deepEqual(arrange('auto', maps, at(1100, 1500)), { vertical: false, beside: true })
  assert.deepEqual(arrange('auto', maps, at(800, 1200)), { vertical: false, beside: false })
  assert.deepEqual(arrange('auto', maps, at(1000 * MIN_SCALE, 1200)), { vertical: false, beside: true })
  // Top to bottom when only that fits.
  assert.deepEqual(arrange('auto', maps, at(300, 700)), { vertical: true, beside: false })
  assert.deepEqual(arrange('auto', maps, at(520, 800)), { vertical: true, beside: true })
  // Nothing fits: the one that overflows less.
  assert.deepEqual(arrange('auto', maps, at(-20, 340)), { vertical: true, beside: false })
  assert.deepEqual(arrange('auto', { across: { width: 1000 }, down: { width: 3600 } }, at(-20, 340)), { vertical: false, beside: false })
  // A choice is kept, whatever the room.
  assert.deepEqual(arrange('vertical', { down: { width: 3600 } }, at(1100, 1500)), { vertical: true, beside: false })
  assert.deepEqual(arrange('horizontal', { across: { width: 1000 } }, at(300, 700)), { vertical: false, beside: false })
  // Not measured yet: the first choice, the panel below.
  assert.deepEqual(arrange('auto', maps, null), { vertical: false, beside: false })
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
