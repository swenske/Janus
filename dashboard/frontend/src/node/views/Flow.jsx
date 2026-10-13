import { ArrowDown, ArrowRight, Ellipsis, EthernetPort, Globe, Layers, Route, Scaling, Server, Shuffle, X } from 'lucide-react'
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { getJSON } from '../api.js'
import ServerStateButtons from '../components/ServerStateButtons.jsx'
import { Badge, Card, Empty, ErrorBox, Loading, PageHeader } from '../../shared/ui.jsx'
import { bytes, compact, duration, num, rate } from '../format.js'
import { NAME_CHARS, VALUE_CHARS, arrange, buildFlow, interfaces, layout, lineChars, parseTopology, pulse, reach, related, statRows, withRates } from '../flow.js'
import { useMetrics, usePoll } from '../hooks.jsx'
import { useMay } from '../may.js'

const ICONS = { iface: EthernetPort, hub: Shuffle, fe: Globe, be: Layers, srv: Server, more: Ellipsis }
const ICON_SIZE = { iface: 18, hub: 26, fe: 18, be: 18, srv: 13, more: 13 }
const KIND = { iface: 'Interface', hub: 'HAProxy', fe: 'Frontend', be: 'Backend', srv: 'Server', more: 'Servers' }
const SLOW = 30000 // the configuration, maps and interfaces' state change seldom
const ORIENTATIONS = ['auto', 'vertical', 'horizontal']
const SIDE_REM = 22 + 1 // the side panel beside the map, and the gap before it
const CARD_REM = 1.2 // the card's padding around the map
const BOTTOM_REM = 1 // under the card, at the window's bottom

// stored is what this browser remembers of a choice; def without it.
function stored(key, def) {
  try {
    return localStorage.getItem(key) ?? def
  } catch {
    return def
  }
}

function store(key, value) {
  try {
    localStorage.setItem(key, value)
  } catch {
    // not remembered, still applied
  }
}

// useRoom is the room the map has, measured on the element given to the
// ref it returns and followed as the window changes - arrange's room:
// its width in its card with the side panel below it, the panel's, and
// the window's height below the card's top; null before.
function useRoom() {
  const [el, setEl] = useState(null)
  const [room, setRoom] = useState(null)
  useLayoutEffect(() => {
    if (!el) return
    const measure = () => {
      const rem = parseFloat(getComputedStyle(document.documentElement).fontSize) || 16
      const card = CARD_REM * rem + 2
      const top = el.getBoundingClientRect().top + window.scrollY
      const next = { width: el.clientWidth - card, side: SIDE_REM * rem, height: window.innerHeight - top - card - BOTTOM_REM * rem }
      setRoom((r) => (r && r.width === next.width && r.height === next.height ? r : next))
    }
    measure()
    const watch = new ResizeObserver(measure)
    watch.observe(el)
    watch.observe(document.documentElement) // what's above the map changing its place
    window.addEventListener('resize', measure)
    return () => {
      watch.disconnect()
      window.removeEventListener('resize', measure)
    }
  }, [el])
  return [room, setEl]
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

// values are what a node's label says under its name - its traffic
// and sessions, or what's wrong with it -, read on one line across or
// a line each top to bottom.
function values(n, hap) {
  const r = n.row
  switch (n.kind) {
    case 'iface':
      return n.up === false ? { parts: [n.status?.up ? 'No carrier' : 'Down'] } : { parts: [rate(n.traffic)] }
    case 'hub':
      if (!hap) return { parts: ['…'] }
      if (!hap.up) return { parts: ['not answering'], tone: 'danger' }
      return { parts: [`${compact(hap.conns)} conns`, ...(hap.reqRate != null ? [`${num(hap.reqRate, hap.reqRate < 10 ? 1 : 0)} req/s`] : [])] }
    case 'fe':
      if (n.tone === 'off') return { parts: [r.status] }
      return { parts: [rate(r.traffic), `${compact(r.scur)} sess`] }
    case 'be':
      if (n.tone === 'danger') return { parts: ['DOWN'], tone: 'danger' }
      if (n.down) return { parts: [`${n.servers.length - n.down}/${n.servers.length} servers up`], tone: 'warn' }
      return { parts: [rate(r.traffic), `${compact(r.scur)} sess`] }
    case 'srv':
      if (n.tone === 'danger' || n.tone === 'warn' || n.tone === 'off') return { parts: [r.status, r.check].filter(Boolean), tone: n.tone === 'off' ? '' : n.tone }
      return { parts: [rate(r.traffic), `${compact(r.scur)} sess`] }
    case 'more': {
      const down = n.hidden.filter((s) => s.tone === 'danger').length
      return down ? { parts: [`${down} down`], tone: 'danger' } : { parts: [`${n.hidden.length} servers`] }
    }
    default:
      return { parts: [] }
  }
}

function FlowNode({ n, hap, configKnown, shape, state, onSelect, onHover }) {
  const Icon = ICONS[n.kind]
  const s = ICON_SIZE[n.kind]
  const v = values(n, hap)
  const text = v.parts.join(' · ')
  // A backend no frontend rule names - with the configuration read.
  const unlinked = n.kind === 'be' && configKnown && !n.linked
  const label = `${KIND[n.kind]} ${n.name}${text ? `: ${text}` : ''}${unlinked ? ' - no frontend rule names it' : ''}`
  // A label beside its node is part of it: hovering or clicking it too.
  const leaf = n.kind === 'srv' || n.kind === 'more'
  const beside = shape !== 'across' || leaf
  let labels
  if (shape === 'dense') {
    // Its name above the line its links leave by, its numbers below it;
    // a server's on one line.
    labels = leaf ? (
      <text x={n.r + 8} y={4} textAnchor="start">
        <tspan className="flow-name">{short(n.name, NAME_CHARS)}</tspan>
        <tspan className={`flow-sub ${v.tone || ''}`} dx={10}>
          {short(text, lineChars(n))}
        </tspan>
      </text>
    ) : (
      <>
        <text className="flow-name" x={n.r + 8} y={-6} textAnchor="start">
          {short(n.name, NAME_CHARS)}
        </text>
        <text className={`flow-sub ${v.tone || ''}`} x={n.r + 8} y={15} textAnchor="start">
          {short(text, lineChars(n))}
        </text>
      </>
    )
  } else if (shape === 'down') {
    // Beside the node, a line each: the links come in above it and
    // leave below.
    const top = v.parts.length > 1 ? -9 : -3
    labels = (
      <>
        <text className="flow-name" x={n.r + 8} y={top} textAnchor="start">
          {short(n.name, NAME_CHARS)}
        </text>
        {v.parts.map((p, i) => (
          <text key={i} className={`flow-sub ${v.tone || ''}`} x={n.r + 8} y={top + 14 + i * 13} textAnchor="start">
            {short(p, VALUE_CHARS)}
          </text>
        ))}
      </>
    )
  } else if (n.kind === 'srv' || n.kind === 'more') {
    labels = (
      <>
        <text className="flow-name" x={n.r + 8} y={-3} textAnchor="start">
          {short(n.name, 20)}
        </text>
        <text className={`flow-sub ${v.tone || ''}`} x={n.r + 8} y={11} textAnchor="start">
          {short(text, 30)}
        </text>
      </>
    )
  } else {
    labels = (
      <>
        <text className="flow-name" y={n.r + 15} textAnchor="middle">
          {short(n.name)}
        </text>
        <text className={`flow-sub ${v.tone || ''}`} y={n.r + 29} textAnchor="middle">
          {text}
        </text>
      </>
    )
  }
  // The box a label beside its node takes: not over its neighbours'.
  const half = shape === 'dense' ? (leaf ? 13 : 20) : Math.max(n.r, 20)
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
      {beside && <rect className="flow-box" x={-n.r} y={-half} width={n.r + reach(n)} height={2 * half} />}
      <circle className="flow-disc" r={n.r} />
      <Icon className="flow-icon" size={s} x={-s / 2} y={-s / 2} aria-hidden="true" />
      {labels}
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
  } else if (l.kind === 'trunk') {
    traffic = activity(a.row) // the backend's: what its servers' links share
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
          <Row k="Answers itself">{n.answers.length ? n.answers.map((a) => (a === 'return' || a === 'redirect' ? `http-request ${a}` : a)).join(', ') : null}</Row>
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
  const [servers, setServers] = useState(() => stored('janus-flow-servers', 'on') !== 'off')
  const [orientation, setOrientation] = useState(() => {
    const o = stored('janus-flow-orientation', 'auto')
    return ORIENTATIONS.includes(o) ? o : 'auto'
  })
  const [room, measured] = useRoom()
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
  // The layouts the orientation may take, then the one that shows.
  const across = useMemo(() => (orientation !== 'vertical' ? layout(graph, ifaces, { servers, shape: 'across' }) : null), [graph, ifaces, servers, orientation])
  const dense = useMemo(() => (orientation !== 'vertical' ? layout(graph, ifaces, { servers, shape: 'dense' }) : null), [graph, ifaces, servers, orientation])
  const down = useMemo(() => (orientation !== 'horizontal' ? layout(graph, ifaces, { servers, shape: 'down' }) : null), [graph, ifaces, servers, orientation])
  const shown = arrange(orientation, { across, dense, down }, room)
  const vertical = shown.shape === 'down'
  const map = { across, dense, down }[shown.shape]
  const incoming = useMemo(() => {
    const c = {}
    for (const l of graph.links) c[l.to] = (c[l.to] || 0) + 1
    return c
  }, [graph])

  // A map wider than its card keeps HAProxy in the middle - as the
  // configuration arrives and moves it - until the user scrolls it.
  const scroller = useRef(null)
  const centred = useRef({ key: '', left: null })
  const drawn = graph.frontends.length + graph.backends.length > 0
  useEffect(() => {
    const el = scroller.current
    if (!el || !drawn) return
    const key = shown.shape
    const last = centred.current
    if (last.key === key && last.left != null && Math.abs(el.scrollLeft - last.left) > 1) return // scrolled by hand
    const svg = el.querySelector('svg')
    const scale = svg ? svg.getBoundingClientRect().width / map.width : 1
    el.scrollLeft = vertical ? Math.max(0, map.byId.hub.x * scale - el.clientWidth / 2) : 0
    centred.current = { key, left: el.scrollLeft }
  }, [vertical, shown.shape, drawn, map])

  const focus = hover || selected
  const near = useMemo(() => (focus ? related(map, focus) : null), [map, focus])
  const stateOf = (id) => (near ? (near.has(id) ? 'near' : 'far') : '')
  const select = useCallback((id) => setSelected((cur) => (cur === id ? null : id)), [])
  const toggleServers = () => {
    store('janus-flow-servers', servers ? 'off' : 'on')
    setServers(!servers)
  }
  const orient = (o) => {
    store('janus-flow-orientation', o)
    setOrientation(o)
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
        <>
          <div className="chips" role="group" aria-label="Orientation">
            <button
              className={`small chip ${orientation === 'auto' ? 'active' : ''}`}
              aria-pressed={orientation === 'auto'}
              onClick={() => orient('auto')}
              title={`The way that shows the map whole in the window, the biggest: left to right, its labels under or beside the nodes, or top to bottom${orientation === 'auto' ? ` - ${vertical ? 'top to bottom' : 'left to right'} now` : ''}`}
            >
              <Scaling size={14} aria-hidden="true" /> Auto
            </button>
            <button className={`small chip ${orientation === 'vertical' ? 'active' : ''}`} aria-pressed={orientation === 'vertical'} onClick={() => orient('vertical')} title="Top to bottom">
              <ArrowDown size={14} aria-hidden="true" /> Vertical
            </button>
            <button className={`small chip ${orientation === 'horizontal' ? 'active' : ''}`} aria-pressed={orientation === 'horizontal'} onClick={() => orient('horizontal')} title="Left to right">
              <ArrowRight size={14} aria-hidden="true" /> Horizontal
            </button>
          </div>
          <button className={`small chip ${servers ? 'active' : ''}`} aria-pressed={servers} onClick={toggleServers}>
            Servers
          </button>
        </>
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
  const picked = selected && (map.byId[selected] || map.links.some((l) => l.id === selected))
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
          <div className={`flow-layout ${shown.beside ? 'beside' : ''}`} ref={measured}>
            <Card className="flow-card">
              <div className="flow-scroll" ref={scroller} onClick={() => setSelected(null)}>
                <svg
                  className={`flow-map ${vertical ? 'vertical' : ''} ${shown.shape} ${near ? 'focused' : ''}`}
                  viewBox={`0 0 ${map.width} ${map.height}`}
                  style={{ width: Math.round(map.width * shown.scale) }}
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
                      <FlowNode key={n.id} n={n} hap={hap} configKnown={graph.configKnown} shape={shown.shape} state={`${stateOf(n.id)} ${selected === n.id ? 'selected' : ''}`} onSelect={select} onHover={setHover} />
                    ))}
                  </g>
                </svg>
              </div>
            </Card>
            <div className={`flow-side ${picked ? 'picked' : ''}`}>
              {picked ? (
                <>
                  <button className="flow-close icon" onClick={() => setSelected(null)} aria-label="Close the details" title="Close (Esc)">
                    <X size={14} aria-hidden="true" />
                  </button>
                  <Details id={selected} map={map} graph={graph} hap={hap} onSelect={setSelected} onChanged={stats.reload} />
                </>
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

