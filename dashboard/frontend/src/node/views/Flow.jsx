import { Ellipsis, EthernetPort, Globe, Layers, Route, Server, Shuffle } from 'lucide-react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { getJSON } from '../api.js'
import ServerStateButtons from '../components/ServerStateButtons.jsx'
import { Badge, Card, Empty, ErrorBox, Loading, PageHeader } from '../../shared/ui.jsx'
import { bytes, compact, duration, num, rate } from '../format.js'
import { buildFlow, interfaces, layout, parseTopology, pulse, related, statRows, withRates } from '../flow.js'
import { useMetrics, usePoll } from '../hooks.jsx'
import { useMay } from '../may.js'

const ICONS = { iface: EthernetPort, hub: Shuffle, fe: Globe, be: Layers, srv: Server, more: Ellipsis }
const ICON_SIZE = { iface: 18, hub: 26, fe: 18, be: 18, srv: 13, more: 13 }
const KIND = { iface: 'Interface', hub: 'HAProxy', fe: 'Frontend', be: 'Backend', srv: 'Server', more: 'Servers' }
const SLOW = 30000 // the configuration, maps and interfaces' state change seldom

function storedServers() {
  try {
    return localStorage.getItem('janus-flow-servers') !== 'off'
  } catch {
    return true
  }
}

function short(s, n = 22) {
  return s.length > n ? `${s.slice(0, n - 1)}…` : s
}

// useSamples keeps the last two answers of /api/haproxy/stats and when
// each came: the byte counters' difference is the traffic.
function useSamples(data) {
  const [s, setS] = useState({ cur: null, prev: null, seconds: 0 })
  useEffect(() => {
    if (!data) return
    const now = Date.now()
    const rows = statRows(data)
    setS((p) => ({ prev: p.cur, cur: { rows, t: now }, seconds: p.cur ? (now - p.cur.t) / 1000 : 0 }))
  }, [data])
  return s
}

// useMaps reads the maps a dynamic use_backend reads ({path: entries},
// a path missing when the node didn't answer for it), every 30 s.
function useMaps(paths, enabled, reloadKey) {
  const key = enabled ? paths.join('\n') : ''
  const [state, setState] = useState({ key: '', maps: {}, failed: {} })
  useEffect(() => {
    if (!key) return undefined
    let cancelled = false
    let timer
    const run = async () => {
      const list = key.split('\n')
      const got = await Promise.allSettled(list.map((p) => getJSON(`/api/haproxy/maps/${encodeURIComponent(p)}`)))
      if (cancelled) return
      const maps = {}
      const failed = {}
      got.forEach((g, i) => {
        if (g.status === 'fulfilled') maps[list[i]] = g.value?.entries || {}
        else failed[list[i]] = String(g.reason?.message || g.reason)
      })
      setState({ key, maps, failed })
      timer = setTimeout(run, SLOW)
    }
    run()
    return () => {
      cancelled = true
      clearTimeout(timer)
    }
  }, [key, reloadKey])
  return state.key === key ? state : NO_MAPS
}

const NO_MAPS = { key: '', maps: {}, failed: {} }

function activity(row) {
  if (!row) return 0
  if (row.traffic != null) return row.traffic
  return row.scur || row.rate ? 1 : 0 // the first sample: no rate yet
}

// sub is the line under a node's name: its traffic and sessions, or
// what's wrong with it.
function sub(n, hap) {
  const r = n.row
  switch (n.kind) {
    case 'iface':
      return n.up === false ? { text: n.status?.up ? 'No carrier' : 'Down' } : { text: rate(n.traffic) }
    case 'hub':
      if (!hap) return { text: '…' }
      if (!hap.up) return { text: 'not answering', tone: 'danger' }
      return { text: `${compact(hap.conns)} conns${hap.reqRate != null ? ` · ${num(hap.reqRate, hap.reqRate < 10 ? 1 : 0)} req/s` : ''}` }
    case 'fe':
      if (n.tone === 'off') return { text: r.status }
      return { text: `${rate(r.traffic)} · ${compact(r.scur)} sess` }
    case 'be':
      if (n.tone === 'danger') return { text: 'DOWN', tone: 'danger' }
      if (n.down) return { text: `${n.servers.length - n.down}/${n.servers.length} servers up`, tone: 'warn' }
      return { text: `${rate(r.traffic)} · ${compact(r.scur)} sess` }
    case 'srv':
      if (n.tone === 'danger' || n.tone === 'warn' || n.tone === 'off') return { text: [r.status, r.check].filter(Boolean).join(' · '), tone: n.tone === 'off' ? '' : n.tone }
      return { text: `${rate(r.traffic)} · ${compact(r.scur)} sess` }
    case 'more': {
      const down = n.hidden.filter((s) => s.tone === 'danger').length
      return down ? { text: `${down} down`, tone: 'danger' } : { text: `${n.hidden.length} servers` }
    }
    default:
      return { text: '' }
  }
}

function FlowNode({ n, hap, configKnown, state, onSelect, onHover }) {
  const Icon = ICONS[n.kind]
  const s = ICON_SIZE[n.kind]
  const line = sub(n, hap)
  const beside = n.kind === 'srv' || n.kind === 'more'
  // A backend no frontend rule names - with the configuration read.
  const unlinked = n.kind === 'be' && configKnown && !n.linked
  const label = `${KIND[n.kind]} ${n.name}${line.text ? `: ${line.text}` : ''}${unlinked ? ' - no frontend rule names it' : ''}`
  return (
    <g
      className={`flow-node kind-${n.kind} ${n.tone ? `tone-${n.tone}` : ''} ${unlinked ? 'unlinked' : ''} ${state}`}
      data-id={n.id}
      transform={`translate(${n.x} ${n.y})`}
      tabIndex={0}
      role="button"
      aria-label={label}
      onClick={(e) => {
        e.stopPropagation()
        onSelect(n.id)
      }}
      onKeyDown={(e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault()
          onSelect(n.id)
        }
      }}
      onMouseEnter={() => onHover(n.id)}
      onMouseLeave={() => onHover(null)}
      onFocus={() => onHover(n.id)}
      onBlur={() => onHover(null)}
    >
      <circle className="flow-disc" r={n.r} />
      <Icon className="flow-icon" size={s} x={-s / 2} y={-s / 2} aria-hidden="true" />
      {beside ? (
        <>
          <text className="flow-name" x={n.r + 8} y={-3} textAnchor="start">
            {short(n.name, 20)}
          </text>
          <text className={`flow-sub ${line.tone || ''}`} x={n.r + 8} y={11} textAnchor="start">
            {short(line.text, 30)}
          </text>
        </>
      ) : (
        <>
          <text className="flow-name" y={n.r + 15} textAnchor="middle">
            {short(n.name)}
          </text>
          <text className={`flow-sub ${line.tone || ''}`} y={n.r + 29} textAnchor="middle">
            {line.text}
          </text>
        </>
      )}
      <title>{label}</title>
    </g>
  )
}

// linkLook is how a link shows: its pulse when traffic goes through, or
// down (a server that fails its checks) or off (stopped, in maintenance).
function linkLook(l, map, incoming) {
  const a = map.byId[l.from]
  const b = map.byId[l.to]
  let traffic = 0
  let cls = ''
  if (l.kind === 'iface') {
    traffic = a.up === false ? 0 : (a.traffic ?? 0)
    if (a.up === false) cls = 'off'
  } else if (l.kind === 'fe') {
    traffic = activity(b.row)
    if (b.tone === 'off') cls = 'off'
  } else if (l.kind === 'be') {
    // HAProxy doesn't count how a backend's traffic splits between the
    // frontends that send to it: with one, it's the backend's; with
    // several, at most the smaller side's.
    const fe = activity(a.row)
    const be = activity(b.row)
    traffic = fe > 0 && be > 0 ? (incoming[b.id] > 1 ? Math.min(fe, be) : be) : 0
  } else if (b.kind === 'more') {
    traffic = b.hidden.reduce((t, s) => t + activity(s.row), 0)
  } else {
    traffic = activity(b.row)
    if (b.tone === 'danger') cls = 'down'
    else if (b.tone === 'off') cls = 'off'
  }
  return { cls, pulse: cls ? null : pulse(traffic) }
}

function FlowLink({ l, look, state, onSelect, onHover }) {
  const clickable = l.kind === 'be'
  return (
    <g className={`flow-link ${look.cls} ${state}`} data-id={l.id}>
      <path className="flow-track" d={l.d} />
      {look.pulse && <path className="flow-pulse" d={l.d} style={{ strokeWidth: look.pulse.width, animationDuration: `${look.pulse.seconds}s` }} />}
      {clickable && (
        <path
          className="flow-hit"
          d={l.d}
          onClick={(e) => {
            e.stopPropagation()
            onSelect(l.id)
          }}
          onMouseEnter={() => onHover(l.id)}
          onMouseLeave={() => onHover(null)}
        >
          <title>{l.rules.map((r) => r.text).join('\n')}</title>
        </path>
      )}
    </g>
  )
}

function Row({ k, children }) {
  if (children == null || children === '' || children === false) return null
  return (
    <>
      <dt>{k}</dt>
      <dd>{children}</dd>
    </>
  )
}

function traffic(r) {
  return r.inRate == null ? `in ${bytes(r.bin)} · out ${bytes(r.bout)} in total` : `${rate(r.inRate)} in · ${rate(r.outRate)} out`
}

function sessions(r) {
  return `${num(r.scur)} now · ${num(r.smax)} max${r.slim ? ` · limit ${num(r.slim)}` : ''}`
}

function responses(r) {
  if (r.hrsp_2xx == null) return null
  return `${compact(r.hrsp_2xx)} 2xx · ${compact(r.hrsp_3xx)} 3xx · ${compact(r.hrsp_4xx)} 4xx · ${compact(r.hrsp_5xx)} 5xx`
}

const KEYS_SHOWN = 40

// Rules lists what sends a frontend to a backend: its use_backend or
// default_backend line, or the map keys that pick it (the line itself,
// the same for every backend it picks, in the tooltip).
function Rules({ rules }) {
  return (
    <ul className="flow-rules">
      {rules.map((r, i) => (
        <li key={i} title={r.map ? r.text : undefined}>
          {!r.map ? (
            <code>{r.text}</code>
          ) : r.isDefault ? (
            <>
              when no key of <code>{r.map}</code> matches
            </>
          ) : (
            <>
              {r.keys.length === 1 ? 'the key' : `${r.keys.length} keys`} of <code>{r.map}</code>: <span className="mono">{r.keys.slice(0, KEYS_SHOWN).join(', ')}</span>
              {r.keys.length > KEYS_SHOWN && ` and ${r.keys.length - KEYS_SHOWN} more`}
            </>
          )}
          {r.via && <div className="muted small">named in a set-var</div>}
        </li>
      ))}
    </ul>
  )
}

function Details({ id, map, graph, hap, onSelect, onChanged }) {
  const may = useMay()
  const link = map.links.find((l) => l.id === id && l.kind === 'be')
  if (link) {
    return (
      <Card title={`${link.from.slice(3)} → ${link.to.slice(3)}`} icon={Route}>
        <p className="muted small">The rules that send this frontend&apos;s traffic to this backend:</p>
        <Rules rules={link.rules} />
      </Card>
    )
  }
  const n = map.byId[id]
  if (!n) return null
  const Icon = ICONS[n.kind]
  const r = n.row
  const title = (
    <>
      {n.name} <Badge>{KIND[n.kind].toLowerCase()}</Badge>
    </>
  )
  const go = (other) => (
    <button key={other} className="flow-goto" onClick={() => onSelect(other)}>
      {other.replace(/^(fe|be):/, '')}
    </button>
  )

  if (n.kind === 'hub') {
    return (
      <Card title={title} icon={Icon}>
        {hap?.up ? (
          <dl className="kv">
            <Row k="Version">{hap.version}</Row>
            <Row k="Connections">{`${num(hap.conns)} · max ${compact(hap.maxConns)}`}</Row>
            <Row k="Requests/s">{hap.reqRate != null ? num(hap.reqRate, 1) : null}</Row>
            <Row k="New connections/s">{num(hap.connRate)}</Row>
            <Row k="Busy">{`${num(100 - hap.idle)}%`}</Row>
            <Row k="Uptime">{duration(hap.uptime)}</Row>
          </dl>
        ) : (
          <div className="notice danger">HAProxy isn&apos;t answering on its stats socket{hap?.error ? `: ${hap.error}` : '.'}</div>
        )}
        <p className="small">
          <a href="#/haproxy/status">HAProxy › Status</a> has every counter.
        </p>
      </Card>
    )
  }
  if (n.kind === 'iface') {
    const s = n.status
    return (
      <Card title={title} icon={Icon}>
        <dl className="kv">
          <Row k="State">{s ? (!s.up ? 'down' : s.carrier ? 'up' : 'no carrier') : null}</Row>
          <Row k="Kind">{s ? (s.vlan_parent ? `VLAN ${s.vlan_id} on ${s.vlan_parent}` : s.kind) : null}</Row>
          <Row k="Addresses">{s?.addresses?.length ? <span className="mono">{s.addresses.join(', ')}</span> : null}</Row>
          <Row k="MTU">{s?.mtu || null}</Row>
          <Row k="Receiving">{rate(n.rx)}</Row>
          <Row k="Sending">{rate(n.tx)}</Row>
        </dl>
        <p className="muted small">All of the interface&apos;s traffic, not only HAProxy&apos;s.</p>
      </Card>
    )
  }
  if (n.kind === 'fe') {
    const out = graph.links.filter((l) => l.from === n.id)
    const open = graph.unresolved.filter((u) => u.frontend === n.name)
    return (
      <Card title={title} icon={Icon}>
        <dl className="kv">
          <Row k="Status">{r.status}</Row>
          <Row k={n.listen ? 'Listen' : 'Mode'}>{n.listen ? `a listen section · ${n.mode}` : n.mode}</Row>
          <Row k="Binds">{n.binds.length ? <span className="mono">{n.binds.join(' ')}</span> : null}</Row>
          <Row k="Sessions">{sessions(r)}</Row>
          <Row k="Sessions/s">{`${num(r.rate)} · max ${num(r.rate_max)}`}</Row>
          <Row k="Requests/s">{r.req_rate != null ? num(r.req_rate) : null}</Row>
          <Row k="Traffic">{traffic(r)}</Row>
          <Row k="Responses">{responses(r)}</Row>
          <Row k="Errors">{r.ereq ? num(r.ereq) : null}</Row>
          <Row k="Denied">{r.dreq ? num(r.dreq) : null}</Row>
          <Row k="Answers itself">{n.answers.length ? n.answers.join(', ') : null}</Row>
        </dl>
        {out.length > 0 && (
          <>
            <h3 className="flow-h">Sends to</h3>
            {out.map((l) => (
              <div key={l.id} className="flow-target">
                {go(l.to)}
                <Rules rules={l.rules} />
              </div>
            ))}
          </>
        )}
        {open.map((u) => (
          <div key={u.format} className="notice small">
            <code>use_backend {u.format}</code>: {u.maps.length ? `reads ${u.maps.join(', ')}, which couldn't be read` : 'the backend is chosen per request'} - not on the map.
          </div>
        ))}
      </Card>
    )
  }
  if (n.kind === 'be') {
    const from = graph.links.filter((l) => l.to === n.id)
    return (
      <Card title={title} icon={Icon}>
        <dl className="kv">
          <Row k="Status">{r.status}</Row>
          <Row k="Servers">{`${n.servers.length - n.down} of ${n.servers.length} up${r.act != null ? ` · ${num(r.act)} active, ${num(r.bck)} backup` : ''}`}</Row>
          <Row k="Sessions">{sessions(r)}</Row>
          <Row k="Queued">{r.qcur ? num(r.qcur) : null}</Row>
          <Row k="Sessions/s">{`${num(r.rate)} · max ${num(r.rate_max)}`}</Row>
          <Row k="Traffic">{traffic(r)}</Row>
          <Row k="Response time">{r.rtime != null ? `${num(r.rtime)} ms (last 1024)` : null}</Row>
          <Row k="Responses">{responses(r)}</Row>
          <Row k="Errors">{r.econ || r.eresp ? `${num(r.econ)} connection · ${num(r.eresp)} response` : null}</Row>
        </dl>
        <h3 className="flow-h">Reached from</h3>
        {from.length ? (
          from.map((l) => (
            <div key={l.id} className="flow-target">
              {go(l.from)}
              <Rules rules={l.rules} />
            </div>
          ))
        ) : (
          <p className="muted small">No frontend rule names it{graph.unresolved.length ? ' - unless a use_backend chosen per request does' : ''}.</p>
        )}
        <p className="small">
          <a href="#/haproxy/backends">HAProxy › Backends</a>
        </p>
      </Card>
    )
  }
  if (n.kind === 'more') {
    return (
      <Card title={`${n.backend}: ${n.hidden.length} more servers`} icon={Icon}>
        <ul className="flow-rules">
          {n.hidden.map((s) => (
            <li key={s.id}>
              <span className="mono">{s.name}</span> <Badge tone={s.tone === 'off' ? '' : s.tone}>{s.row.status}</Badge>{' '}
              <span className="muted small">
                {rate(s.row.traffic)} · {compact(s.row.scur)} sess
              </span>
            </li>
          ))}
        </ul>
      </Card>
    )
  }
  // A server.
  return (
    <Card title={title} icon={Icon} actions={may('HAProxyService/ServerSetState') ? <ServerStateButtons backend={n.backend} server={n.name} onDone={onChanged} /> : null}>
      <dl className="kv">
        <Row k="Backend">{go(`be:${n.backend}`)}</Row>
        <Row k="Status">
          <Badge tone={n.tone === 'off' ? '' : n.tone} dot>
            {r.status}
          </Badge>
        </Row>
        <Row k="Check">{r.check ? [r.check, r.checkDesc, r.check_duration != null ? `${r.check_duration} ms` : ''].filter(Boolean).join(' · ') : null}</Row>
        <Row k="Address">{r.addr ? <span className="mono">{r.addr}</span> : null}</Row>
        <Row k="Weight">{r.weight != null ? num(r.weight) : null}</Row>
        <Row k="Sessions">{sessions(r)}</Row>
        <Row k="Queued">{r.qcur ? num(r.qcur) : null}</Row>
        <Row k="Sessions/s">{`${num(r.rate)} · max ${num(r.rate_max)}`}</Row>
        <Row k="Traffic">{traffic(r)}</Row>
        <Row k="Response time">{r.rtime != null ? `${num(r.rtime)} ms (last 1024)` : null}</Row>
        <Row k="Responses">{responses(r)}</Row>
        <Row k="State for">{r.lastchg != null ? duration(r.lastchg) : null}</Row>
        <Row k="Down for">{r.downtime ? `${duration(r.downtime)} in all` : null}</Row>
      </dl>
    </Card>
  )
}

function Legend() {
  return (
    <div className="flow-legend">
      <div className="flow-legend-row">
        {[
          ['iface', 'Interface · HAProxy'],
          ['fe', 'Frontend'],
          ['be', 'Backend'],
          ['srv', 'Server'],
        ].map(([kind, label]) => (
          <span key={kind} className="flow-legend-item">
            <span className={`flow-dot kind-${kind}`} /> {label}
          </span>
        ))}
      </div>
      <div className="flow-legend-row">
        {[
          ['pulse', 'traffic'],
          ['track', 'none'],
          ['down', 'server down'],
        ].map(([cls, label]) => (
          <span key={cls} className="flow-legend-item">
            <svg width="26" height="8" aria-hidden="true">
              <path className={`flow-legend-${cls}`} d="M1 4H25" />
            </svg>
            {label}
          </span>
        ))}
      </div>
      <p className="muted small">
        Links come from the configuration - <code>default_backend</code>, <code>use_backend</code>, a map&apos;s values when <code>use_backend</code> reads one. The numbers are
        HAProxy&apos;s counters at each refresh: it doesn&apos;t count how a backend&apos;s traffic splits between the frontends that send to it.
      </p>
    </div>
  )
}

export default function Flow() {
  const may = useMay()
  const mayConfig = may('HAProxyService/GetConfig')
  const { latest } = useMetrics()
  const hap = latest?.hap
  const stats = usePoll('/api/haproxy/stats')
  const config = usePoll('/api/haproxy/config', { every: SLOW, enabled: mayConfig })
  const net = usePoll('/api/network/status', { every: SLOW, enabled: may('NetworkService/NetworkStatus') })
  const [servers, setServers] = useState(storedServers)
  const [selected, setSelected] = useState(null)
  const [hover, setHover] = useState(null)

  // HAProxy reloads with each new configuration: read it again then.
  const reloadConfig = config.reload
  const uptime = hap?.uptime
  const lastUptime = useRef(null)
  const [generation, setGeneration] = useState(0)
  useEffect(() => {
    if (uptime == null) return
    if (lastUptime.current != null && uptime < lastUptime.current) {
      reloadConfig()
      setGeneration((g) => g + 1)
    }
    lastUptime.current = uptime
  }, [uptime, reloadConfig])

  const samples = useSamples(stats.data)
  const configText = config.data?.config
  // A string: parsed again only when the configuration changed.
  const topology = useMemo(() => (configText != null ? parseTopology(configText) : null), [configText])
  const mapPaths = topology?.maps || []
  const maps = useMaps(mapPaths, mayConfig && may('HAProxyService/MapGet'), generation)
  const rows = useMemo(() => (samples.cur ? withRates(samples.cur.rows, samples.prev?.rows, samples.seconds) : []), [samples])
  const graph = useMemo(() => buildFlow(rows, topology, maps.maps), [rows, topology, maps.maps])
  const ifaces = useMemo(() => interfaces(latest?.net, net.data), [latest?.net, net.data])
  const map = useMemo(() => layout(graph, ifaces, { servers }), [graph, ifaces, servers])
  const incoming = useMemo(() => {
    const c = {}
    for (const l of graph.links) c[l.to] = (c[l.to] || 0) + 1
    return c
  }, [graph])

  const focus = hover || selected
  const near = useMemo(() => (focus ? related(map, focus) : null), [map, focus])
  const stateOf = (id) => (near ? (near.has(id) ? 'near' : 'far') : '')
  const select = useCallback((id) => setSelected((cur) => (cur === id ? null : id)), [])
  const toggleServers = () => {
    setServers((v) => {
      try {
        localStorage.setItem('janus-flow-servers', v ? 'off' : 'on')
      } catch {
        // not remembered, still applied
      }
      return !v
    })
  }
  useEffect(() => {
    const onKey = (e) => e.key === 'Escape' && setSelected(null)
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  const header = (
    <PageHeader
      title="Traffic flow"
      subtitle="From the node's interfaces through HAProxy's frontends to their backends and servers - live"
      actions={
        <button className={`small chip ${servers ? 'active' : ''}`} aria-pressed={servers} onClick={toggleServers}>
          Servers
        </button>
      }
    />
  )
  if (stats.loading && !stats.data) {
    return (
      <>
        {header}
        <Loading />
      </>
    )
  }
  const empty = samples.cur && !graph.frontends.length && !graph.backends.length
  const summary = `Traffic flow: ${graph.frontends.length} frontends, ${graph.backends.length} backends, ${graph.backends.reduce((t, b) => t + b.servers.length, 0)} servers`

  return (
    <>
      {header}
      <div className="stack">
        <ErrorBox error={stats.error} />
        {!mayConfig && (
          <div className="notice">Your role doesn&apos;t read HAProxy&apos;s configuration on this node: the map shows its frontends, backends and servers, not which frontend sends to which backend.</div>
        )}
        {config.error && <ErrorBox error={`The configuration couldn't be read: ${config.error.message || config.error}`} />}
        {Object.entries(maps.failed).map(([path, err]) => (
          <div key={path} className="notice warn">
            The map {path} couldn&apos;t be read ({err}): the backends it routes to aren&apos;t linked.
          </div>
        ))}
        {empty ? (
          <Empty>HAProxy has no frontend or backend.</Empty>
        ) : (
          <div className="flow-layout">
            <Card className="flow-card">
              <div className="flow-scroll" onClick={() => setSelected(null)}>
                <svg
                  className={`flow-map ${near ? 'focused' : ''}`}
                  viewBox={`0 0 ${map.width} ${map.height}`}
                  style={{ maxWidth: map.width, minWidth: Math.round(map.width * 0.72) }}
                  role="group"
                  aria-label={summary}
                >
                  <g>
                    {map.links.map((l) => (
                      <FlowLink key={l.id} l={l} look={linkLook(l, map, incoming)} state={stateOf(l.id)} onSelect={select} onHover={setHover} />
                    ))}
                  </g>
                  <g>
                    {map.nodes.map((n) => (
                      <FlowNode key={n.id} n={n} hap={hap} configKnown={graph.configKnown} state={`${stateOf(n.id)} ${selected === n.id ? 'selected' : ''}`} onSelect={select} onHover={setHover} />
                    ))}
                  </g>
                </svg>
              </div>
            </Card>
            <div className="flow-side">
              {selected && (map.byId[selected] || map.links.some((l) => l.id === selected)) ? (
                <Details id={selected} map={map} graph={graph} hap={hap} onSelect={setSelected} onChanged={stats.reload} />
              ) : (
                <Card>
                  <p className="muted">Select a node, or a link between a frontend and a backend, to see its counters and rules.</p>
                  <Legend />
                </Card>
              )}
            </div>
          </div>
        )}
      </div>
    </>
  )
}

