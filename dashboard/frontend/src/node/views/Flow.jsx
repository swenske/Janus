import { ArrowDown, ArrowRight, Database, Ellipsis, EthernetPort, Globe, Hourglass, LayoutDashboard, Layers, Route, Scaling, ScrollText, Server, SquareTerminal, Table2, Users, X, ZoomIn, ZoomOut } from 'lucide-react'
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { getJSON } from '../api.js'
import ServerStateButtons from '../components/ServerStateButtons.jsx'
import { Badge, Card, Empty, ErrorBox, Loading, PageHeader } from '../../shared/ui.jsx'
import { bytes, compact, duration, num, rate } from '../format.js'
import {
  NAME_CHARS,
  VALUE_CHARS,
  apiPeers,
  arrange,
  buildFlow,
  dotsShown,
  figures,
  inWindow,
  interfaceRates,
  interfaces,
  layout,
  lineChars,
  lingerFor,
  logTargets,
  maxDots,
  motionFrames,
  parseTopology,
  particles,
  protocol,
  pulse,
  reach,
  related,
  risen,
  speedAt,
  statRows,
  stickTables,
  windowRates,
} from '../flow.js'
import { useMetrics, usePoll, useRefresh } from '../hooks.jsx'
import { useMay } from '../may.js'
import { useNodeCharts } from '../nodeCharts.js'
import { useReducedMotion } from '../../shared/chart/motion.js'
import BrandMark from '../../shared/BrandMark.jsx'

// The middle node is Janus's: its symbol, in the palette's colours.
const ICONS = { api: SquareTerminal, log: ScrollText, iface: EthernetPort, hub: BrandMark, fe: Globe, be: Layers, srv: Server, more: Ellipsis }
const ICON_SIZE = { api: 18, log: 18, iface: 18, hub: 34, fe: 18, be: 18, srv: 13, more: 13 }
const KIND = { api: 'API', log: 'Log target', iface: 'Interface', hub: 'Janus', fe: 'Frontend', be: 'Backend', srv: 'Server', more: 'Servers' }
const PROTO = { http: 'HTTP', tcp: 'TCP' }
// The zoom (ctrl + scroll, the buttons): the map's drawn size, from
// arrange's to these; a step of the buttons.
const ZOOM_MIN = 0.3
const ZOOM_MAX = 4
const ZOOM_STEP = 1.25
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
      // Gone from the page (the map replaced by a message), or not laid
      // out: nothing to measure - the last room stays.
      if (!el.isConnected || !el.clientWidth) return
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

// useSampleWindow keeps a poll's answers over the rate window (flow.js
// inWindow), oldest first, each parsed once (parse, when given: a
// function that never changes) with when it came (usePoll's at - not
// when this effect ran, which can be later): what the numbers a second
// are averaged over.
function useSampleWindow({ data, at }, parse) {
  const [samples, setSamples] = useState([])
  useEffect(() => {
    if (!data) return
    const t = at ?? Date.now()
    const value = parse ? parse(data) : data
    setSamples((s) => (s.at(-1)?.t === t ? s : inWindow([...s, { t, data: value }])))
  }, [data, at, parse])
  return samples
}

// usePeerSamples is the peers of a poll's answers over the rate window
// - /api/log-forwarding read by logTargets, /api/system/clients by
// apiPeers -, their numbers a second from the window's first answer to
// its last.
function usePeerSamples(poll, peers) {
  const win = useSampleWindow(poll.error ? NO_ANSWER : poll)
  return useMemo(() => {
    const cur = win.at(-1)
    if (!cur) return []
    const base = win.length > 1 ? win[0] : null
    return peers(cur.data, base?.data, base ? (cur.t - base.t) / 1000 : 0)
  }, [win, peers])
}

// useLinger is active, kept true for ms after it last was: a link whose
// traffic touches 0 between two samples doesn't blink.
function useLinger(active, ms) {
  const [kept, setKept] = useState(active)
  useEffect(() => {
    if (active) {
      setKept(true)
      return undefined
    }
    const t = setTimeout(() => setKept(false), ms)
    return () => clearTimeout(t)
  }, [active, ms])
  return active || kept
}

// useFlashes is, by node or link id, how many times one of its numbers
// started from nothing (figures, risen): a new count replays its flash.
function useFlashes(cur) {
  const last = useRef(null)
  const [flash, setFlash] = useState({})
  useEffect(() => {
    const up = risen(last.current, cur)
    last.current = cur
    if (!up.size) return
    setFlash((f) => {
      const next = { ...f }
      for (const id of up) next[id] = (next[id] || 0) + 1
      return next
    })
  }, [cur])
  return flash
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
const NO_ANSWER = { data: null }

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
    case 'api':
      return { parts: [`${n.callRate == null ? '–' : num(n.callRate, n.callRate > 0 && n.callRate < 10 ? 1 : 0)} calls/s`, rate(n.byteRate)] }
    case 'log':
      if (n.tone === 'off') return { parts: ['disabled'] }
      if (n.tone === 'danger') return { parts: ['failing'], tone: 'danger' }
      return { parts: [`${n.msgRate == null ? '–' : num(n.msgRate, n.msgRate > 0 && n.msgRate < 10 ? 1 : 0)} msg/s`, ...(n.knowsBytes ? [rate(n.byteRate)] : [])] }
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
      if (n.stick) return { parts: [`${compact(n.table.used)} of ${compact(n.table.size)} entries`] }
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

// peerIcon is a node's icon: an Elasticsearch target's, the Controller's,
// a stick table's are their own.
function peerIcon(n) {
  if (n.kind === 'log' && n.es) return Database
  if (n.kind === 'api' && n.controller) return LayoutDashboard
  if (n.kind === 'be' && n.stick) return Table2
  return ICONS[n.kind]
}

// kindName: what a node is, in words.
function kindName(n) {
  return n.kind === 'be' && n.stick ? 'Stick table' : KIND[n.kind]
}

// apiCount is what an API client's badge counts: the users the
// Controller acts for, the other clients' connections.
function apiCount(n) {
  return n.controller ? n.userCount : n.conns.length
}

function FlowNode({ n, hap, configKnown, shape, state, flash, onSelect, onHover }) {
  const Icon = n.kind === 'log' && n.es ? Database : n.kind === 'api' && n.controller ? LayoutDashboard : n.kind === 'be' && n.stick ? Table2 : ICONS[n.kind] // peerIcon's
  const s = ICON_SIZE[n.kind]
  const v = values(n, hap)
  const text = v.parts.join(' · ')
  // A backend no frontend rule names - with the configuration read; a
  // peer no interface on the map says it goes by.
  const peer = n.kind === 'log' || n.kind === 'api'
  // (A stick table's backend: no rule sends to it, by design.)
  const unlinked = (n.kind === 'be' && configKnown && !n.linked && !n.stick) || (peer && !n.linked)
  const proto = (n.kind === 'fe' || (n.kind === 'be' && !n.stick)) && PROTO[n.mode]
  const count = n.kind === 'api' ? apiCount(n) : 0
  const who = n.kind === 'api' ? ` (${count} ${n.controller ? (count === 1 ? 'user' : 'users') : count === 1 ? 'client' : 'clients'})` : ''
  // A TCP frontend HAProxy counts only as its connections end (no
  // option contstats): a long one - a tunnel, a relay - shows nothing
  // until then.
  const late = lateCounted(n)
  const label = `${n.kind === 'api' ? '' : `${kindName(n)} `}${n.name}${proto ? ` (${proto})` : ''}${who}${text ? `: ${text}` : ''}${unlinked ? (peer ? ' - not known through which interface' : ' - no frontend rule names it') : ''}${COUNTS[n.kind] || ''}${late ? ` - ${lateWords(n)}` : ''}`
  // A number that started from nothing flashes: its key replays it.
  const sub = (extra = '') => `flow-sub ${v.tone || ''} ${flash ? 'flash' : ''} ${extra}`
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
        <tspan key={flash || 0} className={sub()} dx={10}>
          {short(text, lineChars(n))}
        </tspan>
      </text>
    ) : (
      <>
        <text className="flow-name" x={n.r + 8} y={-6} textAnchor="start">
          {short(n.name, NAME_CHARS)}
        </text>
        <text key={flash || 0} className={sub()} x={n.r + 8} y={15} textAnchor="start">
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
          <text key={`${i}.${flash || 0}`} className={sub()} x={n.r + 8} y={top + 14 + i * 13} textAnchor="start">
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
        <text key={flash || 0} className={sub()} x={n.r + 8} y={11} textAnchor="start">
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
        <text key={flash || 0} className={sub()} y={n.r + 29} textAnchor="middle">
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
      {flash > 0 && <circle key={flash} className="flow-flash" r={n.r} />}
      <Icon className="flow-icon" size={s} x={-s / 2} y={-s / 2} aria-hidden="true" />
      {proto && <ProtoBadge proto={n.mode} r={n.r} />}
      {count > 0 && <CountBadge count={count} r={n.r} />}
      {late && <LateBadge r={n.r} />}
      {labels}
      <title>{label}</title>
    </g>
  )
}

// ProtoBadge says a frontend's or backend's mode - HTTP or TCP - at its
// ring's top left, where no link comes in nor label goes.
function ProtoBadge({ proto, r }) {
  const label = PROTO[proto]
  const w = label.length * 5.6 + 7
  const cx = -r * 0.78
  const cy = -r * 0.78
  return (
    <g className={`flow-proto proto-${proto}`} transform={`translate(${cx} ${cy})`} aria-hidden="true">
      <rect x={-w / 2} y={-6} width={w} height={12} rx={6} />
      <text y={3} textAnchor="middle">
        {label}
      </text>
    </g>
  )
}

// CountBadge is how many an API client's node stands for - the users
// the Controller acts for, the other clients - at its ring's top left.
function CountBadge({ count, r }) {
  const label = count > 99 ? '99+' : String(count)
  const w = label.length * 5.6 + 17
  return (
    <g className="flow-count" transform={`translate(${-r * 0.78} ${-r * 0.78})`} aria-hidden="true">
      <rect x={-w / 2} y={-6} width={w} height={12} rx={6} />
      <Users size={8} x={-w / 2 + 4} y={-4} />
      <text x={-w / 2 + 14} y={3} textAnchor="start">
        {label}
      </text>
    </g>
  )
}

// lateCounted: a TCP frontend without option contstats, whose bytes
// HAProxy adds to its counters only as each connection ends - a stream
// is the whole connection in TCP, often a long one (a database's, a
// tunnel, a relay), where in HTTP it's a request (lateWords).
function lateCounted(n) {
  return n.kind === 'fe' && n.contstats === false && n.mode === 'tcp'
}

// LateBadge marks a frontend lateCounted - at its ring's bottom left,
// where no link comes in nor label goes.
function LateBadge({ r }) {
  return (
    <g className="flow-late" transform={`translate(${-r * 0.78} ${r * 0.78})`} aria-hidden="true">
      <circle r={6.5} />
      <Hourglass size={8} x={-4} y={-4} />
    </g>
  )
}

// lateWords says what a frontend without option contstats counts: a
// stream is a connection in TCP, a request in HTTP.
function lateWords(n) {
  return `HAProxy counts its bytes when each ${n.mode === 'tcp' ? 'connection' : 'request'} ends (no option contstats)`
}

// What a node's traffic counts, said after its numbers - the averages
// of the rate window (flow.js RATE_WINDOW_MS).
const COUNTS = {
  iface: ' - over the last 10 s, everything the interface carries both ways: what HAProxy forwards crosses it twice when its clients and servers are both on it',
  fe: ' - over the last 10 s, the bytes HAProxy forwarded',
}

// linkLook is how a link shows: its pulse when traffic goes through, or
// down (a server that fails its checks, a log target failing) or off
// (stopped, in maintenance, a log target disabled) - and its protocol's
// colour.
function linkLook(l, map, incoming) {
  const a = map.byId[l.from]
  const b = map.byId[l.to]
  let traffic = 0
  let cls = ''
  if (l.kind === 'api') {
    traffic = a.byteRate ?? (a.callRate > 0 ? 1 : 0)
  } else if (l.kind === 'log') {
    traffic = b.knowsBytes ? (b.byteRate ?? 0) : b.msgRate > 0 ? 1 : 0
    if (b.tone === 'danger') cls = 'down'
    else if (b.tone === 'off') cls = 'off'
  } else if (l.kind === 'iface') {
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
  const proto = protocol(l, map.byId)
  return { cls, proto, traffic: cls ? 0 : traffic, pulse: cls ? null : pulse(traffic) }
}

// --- the particles' motion ---

// A dot's animation lasts CYCLE_MS a crossing: its playbackRate is the
// crossings it makes a second.
const CYCLE_MS = 1000
// node.css' fades: a link's dots going or coming back, a set of them
// giving way to another.
const FADE_MS = 300

// The speed changes under way, one loop for every link: a frame sets the
// changing links' playbackRate - which keeps each dot where it is - and
// the loop stops when none changes.
const changing = new Set()
let frame = 0
function stepSpeeds(now) {
  for (const m of changing) if (m.step(now)) changing.delete(m)
  frame = changing.size ? requestAnimationFrame(stepSpeeds) : 0
}

// LinkMotion is a link's dots: a Web Animation each along the link,
// made once and kept from one refresh to the next - their speed eased
// to a new one (speedAt), never restarted; paused while they're hidden.
class LinkMotion {
  sets = new Map() // a set of dots' id: their animations
  rate = 0 // crossings a second now
  from = 0
  to = 0
  start = 0
  paused = false

  all() {
    return [...this.sets.values()].flat()
  }

  // phase is where the newest set's first dot is along the link, 0 to 1.
  phase() {
    const t = [...this.sets.values()].at(-1)?.[0]?.currentTime
    return typeof t === 'number' ? (t % CYCLE_MS) / CYCLE_MS : 0
  }

  add(id, anims) {
    for (const a of anims) {
      a.playbackRate = this.rate
      if (this.paused) a.pause()
    }
    this.sets.set(id, anims)
  }

  remove(id) {
    for (const a of this.sets.get(id) || []) a.cancel()
    this.sets.delete(id)
  }

  // speed eases the dots to crossings a second.
  speed(to) {
    if (!(to > 0)) return
    if (!this.rate) {
      this.rate = this.to = to
      for (const a of this.all()) a.playbackRate = to
      return
    }
    if (to === this.to) return
    this.from = this.rate
    this.to = to
    this.start = performance.now()
    changing.add(this)
    if (!frame) frame = requestAnimationFrame(stepSpeeds)
  }

  step(now) {
    this.rate = speedAt(this.from, this.to, now - this.start)
    for (const a of this.all()) a.playbackRate = this.rate
    return this.rate === this.to
  }

  pause(paused) {
    if (this.paused === paused) return
    this.paused = paused
    for (const a of this.all()) {
      if (paused) a.pause()
      else a.play()
    }
  }

  stop() {
    changing.delete(this)
    for (const id of [...this.sets.keys()]) this.remove(id)
  }
}

// A dot's trail, behind it on its own x axis (motionFrames turns it the
// way it goes): the dot in its glow, two fainter ones following.
function dotParts(r) {
  const gap = r * 2.6
  return [
    { x: 0, r: r * 3, cls: 'flow-particle-halo' },
    { x: -2 * gap, r: r * 0.55, cls: 'flow-particle-tail far2' },
    { x: -gap, r: r * 0.75, cls: 'flow-particle-tail' },
    { x: 0, r, cls: 'flow-particle-core' },
  ]
}

// ParticleSet is a link's dots, spread evenly along it - the first
// where the set before it had its own (motion.phase), when the link's
// length asked for another number of them -, those shown lit (shown):
// more traffic lights more of the same dots, fading in where they are.
function ParticleSet({ id, shown, r, frames, motion, leaving, onGone }) {
  const dots = useRef([])
  const shape = useRef(frames)
  const total = shown.length
  useLayoutEffect(() => {
    const phase = motion.phase()
    const anims = []
    for (let i = 0; i < total; i++) {
      const el = dots.current[i]
      if (!el?.animate) continue
      const a = el.animate(shape.current, { duration: CYCLE_MS, iterations: Infinity, easing: 'linear' })
      a.currentTime = ((phase + i / total) % 1) * CYCLE_MS
      anims.push(a)
    }
    motion.add(id, anims)
    return () => motion.remove(id)
  }, [id, total, motion])
  // The path changed (the map laid out again): the same dots follow it.
  useEffect(() => {
    if (shape.current === frames) return
    shape.current = frames
    for (const a of motion.sets.get(id) || []) a.effect?.setKeyframes(frames)
  }, [frames, id, motion])
  useEffect(() => {
    if (!leaving) return undefined
    const t = setTimeout(() => onGone(id), FADE_MS + 50)
    return () => clearTimeout(t)
  }, [leaving, id, onGone])
  const parts = dotParts(r)
  return (
    <g className={`flow-particle-set ${leaving ? 'leaving' : ''}`}>
      {shown.map((lit, i) => (
        <g key={i} className={`flow-particle ${lit ? '' : 'unlit'}`} ref={(el) => (dots.current[i] = el)}>
          {parts.map((part, j) => (
            <circle key={j} cx={part.x} r={part.r} className={part.cls} />
          ))}
        </g>
      ))}
    </g>
  )
}

// Particles run along a link while traffic goes through: glowing dots,
// closer together and faster the more bytes (flow.js particles), spread
// along it from the start - a log target's from it, out of the node. A
// Web Animation each, made once: a refresh changes their speed, eased,
// and how many of them are lit, faded - never restarts them; when the
// traffic stops they fade out (on: false, after useLinger's grace),
// paused, and fade in again where they were when it's back. None under
// "reduce motion" or with the account's node charts off: the link is
// only lit.
function Particles({ l, traffic, width, on, track }) {
  const motion = useRef(null)
  if (!motion.current) motion.current = new LinkMotion()
  const back = l.kind === 'log'
  // The path as the page draws it: its length, the keyframes along it.
  const [geom, setGeom] = useState(null)
  useLayoutEffect(() => {
    const el = track.current
    if (!el?.getTotalLength) return
    const len = el.getTotalLength()
    const k = Math.max(8, Math.min(64, Math.ceil(len / 12)))
    const pts = []
    for (let i = 0; i <= k; i++) {
      const q = el.getPointAtLength((len * i) / k)
      pts.push({ x: q.x, y: q.y })
    }
    if (back) pts.reverse()
    setGeom({ len, frames: motionFrames(pts) })
  }, [l.d, back, track])
  const p = geom ? particles(traffic, geom.len) : null
  const total = geom ? maxDots(geom.len) : 0
  const crossings = p ? 1 / p.seconds : 0
  useEffect(() => {
    motion.current.speed(crossings)
  }, [crossings])
  // Hidden: paused once faded out; back: moving again as it fades in.
  useEffect(() => {
    const m = motion.current
    if (on) {
      m.pause(false)
      return undefined
    }
    const t = setTimeout(() => m.pause(true), FADE_MS + 50)
    return () => clearTimeout(t)
  }, [on])
  useEffect(() => () => motion.current.stop(), [])
  // The sets of dots: the one moving, and one fading away a moment when
  // the path's length changed their number (the map laid out again).
  const [sets, setSets] = useState([])
  useEffect(() => {
    if (!total) return
    setSets((s) => {
      const cur = s.find((x) => !x.leaving)
      if (cur?.total === total) return s
      return [...s.map((x) => (x.leaving ? x : { ...x, leaving: true })), { id: (s.at(-1)?.id || 0) + 1, total, leaving: false }]
    })
  }, [total])
  const gone = useCallback((id) => setSets((s) => s.filter((x) => x.id !== id)), [])
  if (!geom || !sets.length) return null
  const r = Math.max(2, width * 0.6)
  return (
    <g className={`flow-particles ${on ? 'on' : ''}`}>
      {sets.map((s) => (
        <ParticleSet key={s.id} id={s.id} shown={dotsShown(Math.min(p?.count || 0, s.total), s.total)} r={r} frames={geom.frames} motion={motion.current} leaving={s.leaving} onGone={gone} />
      ))}
    </g>
  )
}

function FlowLink({ l, look, state, flash, onSelect, onHover, moving, linger }) {
  const clickable = l.kind === 'be'
  const track = useRef(null)
  // Lit while traffic goes through, and a moment after (linger): then
  // the light and the dots fade out; a server down, a target disabled
  // at once.
  const active = !look.cls && look.traffic > 0
  const on = useLinger(active, linger) && !look.cls
  // The traffic last seen: what the light and dots keep as they fade.
  const last = useRef(0)
  if (active) last.current = look.traffic
  const lit = pulse(last.current)
  // The dots come once there's been traffic, and stay - hidden, paused -
  // while there isn't.
  const seen = useRef(false)
  if (active) seen.current = true
  return (
    <g className={`flow-link kind-${l.kind} ${look.proto ? `proto-${look.proto}` : ''} ${look.cls} ${state}`} data-id={l.id}>
      <path className="flow-track" d={l.d} ref={track} />
      {flash > 0 && active && <path key={flash} className="flow-flash-link" d={l.d} />}
      {lit && !look.cls && <path className={`flow-pulse ${on ? 'on' : ''}`} d={l.d} style={{ strokeWidth: lit.width }} />}
      {moving && seen.current && lit && <Particles l={l} traffic={last.current} width={lit.width} on={on} track={track} />}
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
  return r.inRate == null ? `in ${bytes(r.bin)} · out ${bytes(r.bout)} in total` : `${rate(r.inRate)} in · ${rate(r.outRate)} out (last 10 s)`
}

// counted is when HAProxy adds a frontend's bytes to its counters - what
// its traffic can say; null without the configuration.
function counted(n) {
  if (n.contstats == null) return null
  const tcp = n.mode === 'tcp'
  if (n.contstats) return `along each ${tcp ? 'connection' : 'request'}, every 5 s (option contstats)`
  if (!tcp) {
    return (
      <>
        when each request ends (no <code>option contstats</code>): a long one - a big download, a WebSocket - shows nothing until it ends, then all at once
      </>
    )
  }
  return (
    <>
      <Hourglass size={12} aria-hidden="true" className="flow-late-icon" /> when each connection ends (no <code>option contstats</code>): one open long - a tunnel, a relay, a
      database&apos;s - shows nothing until it closes, then all at once
    </>
  )
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
      <Card className="data-title" title={`${link.from.slice(3)} → ${link.to.slice(3)}`} icon={Route}>
        <p className="muted small">The rules that send this frontend&apos;s traffic to this backend:</p>
        <Rules rules={link.rules} />
      </Card>
    )
  }
  const n = map.byId[id]
  if (!n) return null
  if (n.kind === 'api') return <ApiDetails n={n} />
  const Icon = n.kind === 'be' && n.stick ? Table2 : ICONS[n.kind]
  const r = n.row
  const title = (
    <>
      {n.name} <Badge>{kindName(n).toLowerCase()}</Badge> {(n.kind === 'fe' || (n.kind === 'be' && !n.stick)) && PROTO[n.mode] && <span className={`badge flow-proto-badge proto-${n.mode}`}>{PROTO[n.mode]}</span>}
    </>
  )
  const go = (other) => (
    <button key={other} className="flow-goto" onClick={() => onSelect(other)}>
      {other.replace(/^(fe|be):/, '')}
    </button>
  )

  if (n.kind === 'hub') {
    return (
      <Card className="data-title" title={title} icon={Icon}>
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
  if (n.kind === 'log') return <LogDetails n={n} title={title} />
  if (n.kind === 'iface') {
    const s = n.status
    return (
      <Card className="data-title" title={title} icon={Icon}>
        <dl className="kv">
          <Row k="State">{s ? (!s.up ? 'down' : s.carrier ? 'up' : 'no carrier') : null}</Row>
          <Row k="Kind">{s ? (s.vlan_parent ? `VLAN ${s.vlan_id} on ${s.vlan_parent}` : s.kind) : null}</Row>
          <Row k="Addresses">{s?.addresses?.length ? <span className="mono">{s.addresses.join(', ')}</span> : null}</Row>
          <Row k="MTU">{s?.mtu || null}</Row>
          <Row k="Receiving">{rate(n.rx)}</Row>
          <Row k="Sending">{rate(n.tx)}</Row>
        </dl>
        <p className="muted small">
          Everything the interface carries, both ways, over the last 10 s - the kernel&apos;s count, each packet as it goes: what HAProxy forwards crosses it twice when its
          clients and its servers are both on it (in from one side, out to the other), with the API, the logs, health checks, packet headers, TLS and retransmissions.
          HAProxy&apos;s figures are the bytes it forwarded, once, as it counted them.
        </p>
      </Card>
    )
  }
  if (n.kind === 'fe') {
    const out = graph.links.filter((l) => l.from === n.id)
    const open = graph.unresolved.filter((u) => u.frontend === n.name)
    return (
      <Card className="data-title" title={title} icon={Icon}>
        <dl className="kv">
          <Row k="Status">{r.status}</Row>
          <Row k={n.listen ? 'Listen' : 'Mode'}>{n.listen ? `a listen section · ${n.mode}` : n.mode}</Row>
          <Row k="Binds">{n.binds.length ? <span className="mono">{n.binds.join(' ')}</span> : null}</Row>
          <Row k="Sessions">{sessions(r)}</Row>
          <Row k="Sessions/s">{`${num(r.rate)} · max ${num(r.rate_max)}`}</Row>
          <Row k="Requests/s">{r.req_rate != null ? num(r.req_rate) : null}</Row>
          <Row k="Traffic">{traffic(r)}</Row>
          <Row k="Counted">{counted(n)}</Row>
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
  if (n.kind === 'be' && n.stick) {
    return (
      <Card className="data-title" title={title} icon={Icon}>
        <dl className="kv">
          <Row k="Type">{n.table.type}</Row>
          <Row k="Entries">{`${num(n.table.used)} of ${num(n.table.size)}`}</Row>
        </dl>
        <p className="muted small">A backend holding a stick table and no server: frontend rules track their clients into it (track-sc, table) - no traffic goes to it.</p>
        <p className="small">
          <a href="#/haproxy/tables">HAProxy › Stick tables</a>
        </p>
      </Card>
    )
  }
  if (n.kind === 'be') {
    const from = graph.links.filter((l) => l.to === n.id)
    return (
      <Card className="data-title" title={title} icon={Icon}>
        <dl className="kv">
          <Row k="Status">{r.status}</Row>
          <Row k="Mode">{n.mode}</Row>
          <Row k="Servers">{`${n.servers.length - n.down} of ${n.servers.length} up${r.act != null ? ` · ${num(r.act)} active, ${num(r.bck)} backup` : ''}`}</Row>
          <Row k="Sessions">{sessions(r)}</Row>
          <Row k="Queued">{r.qcur ? num(r.qcur) : null}</Row>
          <Row k="Sessions/s">{`${num(r.rate)} · max ${num(r.rate_max)}`}</Row>
          <Row k="Traffic">{traffic(r)}</Row>
          <Row k="Response time">{r.rtime != null ? `${num(r.rtime)} ms (last 1024)` : null}</Row>
          <Row k="Responses">{responses(r)}</Row>
          <Row k="Errors">{r.econ || r.eresp ? `${num(r.econ)} connection · ${num(r.eresp)} response` : null}</Row>
          <Row k="Stick table">{n.table ? `${n.table.type} · ${num(n.table.used)} of ${num(n.table.size)} entries` : null}</Row>
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
      <Card className="data-title" title={`${n.backend}: ${n.hidden.length} more servers`} icon={Icon}>
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
    <Card className="data-title" title={title} icon={Icon} actions={may('HAProxyService/ServerSetState') ? <ServerStateButtons backend={n.backend} server={n.name} state={r.status} onDone={onChanged} /> : null}>
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

// logKind is how a log target sends: "syslog over UDP · RFC 5424",
// "Elasticsearch · gzip".
function logKind(t) {
  if (t.elasticsearch) return `Elasticsearch${t.elasticsearch.gzip ? ' · gzip' : ''}`
  const s = t.syslog || {}
  const framing = s.transport === 'tcp' || s.transport === 'tls' ? ` · ${s.framing || (s.format === 'rfc3164' ? 'lf' : 'octet-counting')}` : ''
  return `syslog over ${(s.transport || 'udp').toUpperCase()} · ${s.format === 'rfc3164' ? 'RFC 3164' : 'RFC 5424'}${framing}`
}

function LogDetails({ n, title }) {
  const st = n.status
  const t = n.target
  const Icon = n.es ? Database : ScrollText
  const lastSent = Number(st.last_sent_unix_ns || 0)
  let via
  if (n.via == null) via = <span className="muted">not said - the node runs an older Janus</span>
  else if (n.via) via = n.linked ? <span className="mono">{n.via}</span> : <span className="mono">{n.via} (not on the map)</span>
  else via = <span className="muted">{st.remote_address ? 'no route says' : 'not connected yet'}</span>
  return (
    <Card className="data-title" title={title} icon={Icon}>
      <dl className="kv">
        <Row k="State">
          <Badge tone={n.tone === 'off' ? '' : n.tone} dot>
            {st.state}
          </Badge>
        </Row>
        <Row k="Sends">{logKind(t)}</Row>
        <Row k="To">{t.syslog?.address || t.elasticsearch?.url ? <span className="mono">{t.syslog?.address || t.elasticsearch?.url}</span> : null}</Row>
        <Row k="Through">{via}</Row>
        <Row k="Connection">{st.remote_address ? <span className="mono">{`${st.local_address || '?'} → ${st.remote_address}`}</span> : null}</Row>
        <Row k="Records/s">{n.msgRate != null ? num(n.msgRate, 1) : null}</Row>
        <Row k="Throughput">{n.knowsBytes ? rate(n.byteRate) : null}</Row>
        <Row k="Sent">{`${num(Number(st.sent || 0))} records${n.knowsBytes ? ` · ${bytes(Number(st.bytes_sent || 0))}` : ''}`}</Row>
        <Row k="Waiting">{num(Number(st.queued || 0))}</Row>
        <Row k="Dropped">{Number(st.dropped || 0) ? num(Number(st.dropped)) : null}</Row>
        <Row k="Failed sends">{Number(st.errors || 0) ? num(Number(st.errors)) : null}</Row>
        <Row k="Last sent">{lastSent ? `${duration(Math.max(0, (Date.now() - lastSent / 1e6) / 1000))} ago` : null}</Row>
        <Row k="Sources">{t.sources?.length ? t.sources.join(', ') : 'all but the access logs'}</Row>
      </dl>
      {st.last_error && <div className="notice danger small">{st.last_error}</div>}
      <p className="muted small">What janusd sends: its own lines, HAProxy&apos;s, the kernel&apos;s, the events and audit - through the interface the node&apos;s routes pick.</p>
      <p className="small">
        <a href="#/log-forwarding">Logs › Forwarding</a>
      </p>
    </Card>
  )
}

// The Controller's own work, as it names it to a node (nodeproxy).
const CONTROLLER_WORK = { automation: 'the Controller’s own work', 'auto-scaling': 'the Controller’s auto-scaling' }

const role = (roles) => (roles || []).map((r) => r.replace(/^os:/, '')).join(', ')

// ago is how long ago a node's time was (unix ns) - null for none.
function ago(ns, now) {
  const t = Number(ns || 0)
  return t ? `${duration(Math.max(0, (now - t / 1e6) / 1000))} ago` : null
}

// ApiDetails is who is connected to the node's API: the Controller -
// the users its calls are for -, or the other clients, each
// connection.
function ApiDetails({ n }) {
  const now = Date.now()
  const Icon = peerIcon(n)
  const opened = Math.min(...n.conns.map((c) => Number(c.opened_unix_ns || 0) || Infinity))
  const streams = n.conns.reduce((t, c) => t + Number(c.open_streams || 0), 0)
  const calls = n.conns.reduce((t, c) => t + Number(c.calls || 0), 0)
  let via
  if (n.via) via = n.linked ? <span className="mono">{n.via}</span> : <span className="mono">{n.via} (not on the map)</span>
  else via = <span className="muted">no route says</span>
  const hidden = n.userCount - n.users.length
  return (
    <Card
      title={
        <>
          {n.name} <Badge>{n.controller ? 'API client' : n.conns.length === 1 ? 'API client' : 'API clients'}</Badge>
        </>
      }
      icon={Icon}
    >
      <dl className="kv">
        <Row k="Through">{via}</Row>
        <Row k="From">{n.controller ? <span className="mono">{[...new Set(n.conns.map((c) => c.remote_address?.replace(/:\d+$/, '')).filter(Boolean))].join(', ')}</span> : null}</Row>
        <Row k="Certificate">
          {n.controller
            ? [...new Set(n.conns.map((c) => `${c.name}${c.fleet ? ' · the fleet’s' : ' · the node’s own CA'}`))].join(', ')
            : null}
        </Row>
        <Row k="Connected">{Number.isFinite(opened) ? `${duration((now - opened / 1e6) / 1000)}${n.conns.length > 1 ? ` · ${n.conns.length} connections` : ''}` : null}</Row>
        <Row k="Calls/s">{n.callRate != null ? num(n.callRate, 1) : null}</Row>
        <Row k="Receiving">{n.inRate != null ? rate(n.inRate) : null}</Row>
        <Row k="Sending">{n.outRate != null ? rate(n.outRate) : null}</Row>
        <Row k="Calls">{num(calls)}</Row>
        <Row k="Streams open">{streams ? num(streams) : null}</Row>
      </dl>
      {n.controller ? (
        <>
          <h3 className="flow-h">Users</h3>
          {n.users.length > 0 && (
            <ul className="flow-users">
              {n.users.map((u) => (
                <li key={u.name}>
                  <span className="mono">{u.name}</span> {u.self && <Badge tone="ok">you</Badge>} <Badge>{role(u.roles)}</Badge>
                  <div className="muted small">
                    {[CONTROLLER_WORK[u.name], u.open_streams ? `${num(Number(u.open_streams))} ${Number(u.open_streams) === 1 ? 'stream' : 'streams'} open` : '', `last call ${ago(u.last_call_unix_ns, now)}`].filter(Boolean).join(' · ')}
                  </div>
                </li>
              ))}
            </ul>
          )}
          {hidden > 0 && (
            <p className="muted small">
              {n.users.length ? `And ${hidden} more` : `${hidden} ${hidden === 1 ? 'user' : 'users'}`} - their names are shown to admins.
            </p>
          )}
          {!n.userCount && <p className="muted small">None in the last 2 minutes.</p>}
          <p className="muted small">Whom the Controller&apos;s calls were for in the last 2 minutes: those with a page of this node open, or who just acted on it.</p>
        </>
      ) : (
        <>
          <h3 className="flow-h">Connections</h3>
          <ul className="flow-users">
            {n.conns.map((c) => (
              <li key={c.id}>
                {c.name ? <span className="mono">{c.name}</span> : <span className="muted">a client</span>} {c.self && <Badge tone="ok">this one</Badge>} {c.roles?.length > 0 && <Badge>{role(c.roles)}</Badge>}
                <div className="muted small">
                  {[c.remote_address ? `from ${c.remote_address}` : '', c.fleet ? 'a fleet certificate' : '', `connected ${duration((now - Number(c.opened_unix_ns) / 1e6) / 1000)}`, Number(c.open_streams) ? `${num(Number(c.open_streams))} open` : '', ago(c.last_call_unix_ns, now) ? `last call ${ago(c.last_call_unix_ns, now)}` : 'no call yet']
                    .filter(Boolean)
                    .join(' · ')}
                </div>
              </li>
            ))}
          </ul>
          {n.conns.some((c) => !c.name) && <p className="muted small">The other clients&apos; names and addresses are shown to admins.</p>}
          <p className="muted small">Clients reaching the node&apos;s API with a certificate of their own - janusctl, its tui, a script -, not through the Controller.</p>
        </>
      )}
    </Card>
  )
}

function Legend({ stick, late }) {
  return (
    <div className="flow-legend">
      <div className="flow-legend-row">
        {[
          ['iface', 'Interface · Janus'],
          ['fe', 'Frontend'],
          ['be', 'Backend'],
          ['srv', 'Server'],
        ].map(([kind, label]) => (
          <span key={kind} className="flow-legend-item">
            <span className={`flow-dot kind-${kind}`} /> {label}
          </span>
        ))}
        {stick && (
          <span className="flow-legend-item">
            <Table2 size={12} aria-hidden="true" /> Stick table: a backend holding one and no server
          </span>
        )}
        {late && (
          <span className="flow-legend-item wraps">
            <Hourglass size={12} aria-hidden="true" className="flow-late-icon" /> Counted as connections close: TCP without <code>option contstats</code>
          </span>
        )}
      </div>
      <div className="flow-legend-row">
        {[
          ['pulse proto-http', 'HTTP'],
          ['pulse proto-tcp', 'TCP'],
          ['pulse proto-log', 'logs'],
          ['pulse proto-api', 'the API: the Controller, janusctl'],
          ['pulse', 'an interface’s, all of it'],
        ].map(([cls, label]) => (
          <span key={cls} className="flow-legend-item">
            <svg width="26" height="8" aria-hidden="true">
              <path className={cls.split(' ').map((c, i) => (i ? c : `flow-legend-${c}`)).join(' ')} d="M1 4H25" />
            </svg>
            {label}
          </span>
        ))}
      </div>
      <div className="flow-legend-row">
        {[
          ['track', 'none'],
          ['down', 'server down, log target failing'],
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
        Links come from the configuration - <code>default_backend</code>, <code>use_backend</code>, a map&apos;s values when <code>use_backend</code> reads one. Traffic is a
        second&apos;s average over the last 10 s. An interface&apos;s is everything it carries, both ways: what HAProxy forwards crosses it twice when the clients and the
        servers are both on it. HAProxy&apos;s are its counters, which add a stream&apos;s bytes as it handles it - every 5 s with <code>option contstats</code>, else when it
        ends - and don&apos;t say how a backend&apos;s traffic splits between the frontends that send to it. A number that starts from zero flashes. Ctrl + scroll zooms; drag
        to move a zoomed map.
      </p>
    </div>
  )
}

export default function Flow() {
  const may = useMay()
  // Particles: not under "reduce motion", nor with the account's node
  // charts off (the Controller's preferences).
  const nodeCharts = useNodeCharts()
  const moving = !useReducedMotion() && nodeCharts
  const mayConfig = may('HAProxyService/GetConfig')
  const { latest, points } = useMetrics()
  const { interval } = useRefresh()
  const linger = lingerFor(interval)
  const hap = latest?.hap
  const stats = usePoll('/api/haproxy/stats')
  const config = usePoll('/api/haproxy/config', { every: SLOW, enabled: mayConfig })
  const net = usePoll('/api/network/status', { every: SLOW, enabled: may('NetworkService/NetworkStatus') })
  // The stick tables (their headers only): a backend holding one and no
  // server is drawn as the table it is.
  const tablesPoll = usePoll('/api/haproxy/tables', { every: SLOW, enabled: may('HAProxyService/TableList') })
  const tables = useMemo(() => stickTables(tablesPoll.data), [tablesPoll.data])
  // Where the node's logs go, at the stats' pace: their records and
  // bytes a second (a node without a log shipper answers an error - no
  // targets then).
  const logPoll = usePoll('/api/log-forwarding', { enabled: may('SystemService/LogForwardingGet') })
  // Who is connected to the node's API - the Controller this page comes
  // through, janusctl -, at the same pace (an older node doesn't say).
  const clientPoll = usePoll('/api/system/clients', { enabled: may('SystemService/ClientList') })
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

  // HAProxy's counters over the rate window: their traffic, a second's
  // average over it (flow.js windowRates).
  const statWindow = useSampleWindow(stats, statRows)
  const configText = config.data?.config
  // A string: parsed again only when the configuration changed.
  const topology = useMemo(() => (configText != null ? parseTopology(configText) : null), [configText])
  const mapPaths = topology?.maps || []
  const maps = useMaps(mapPaths, mayConfig && may('HAProxyService/MapGet'), generation)
  const rows = useMemo(() => windowRates(statWindow.map((s) => ({ t: s.t, rows: s.data }))), [statWindow])
  const graph = useMemo(() => buildFlow(rows, topology, maps.maps, tables), [rows, topology, maps.maps, tables])
  // The interfaces' over the same window, from the metrics' samples.
  const ifaces = useMemo(() => interfaces(interfaceRates(points), net.data), [points, net.data])
  const logs = usePeerSamples(logPoll, logTargets)
  const apis = usePeerSamples(clientPoll, apiPeers)
  const peers = useMemo(() => [...apis, ...logs], [apis, logs])
  // The layouts the orientation may take, then the one that shows.
  const across = useMemo(() => (orientation !== 'vertical' ? layout(graph, ifaces, { servers, shape: 'across', peers }) : null), [graph, ifaces, servers, orientation, peers])
  const dense = useMemo(() => (orientation !== 'vertical' ? layout(graph, ifaces, { servers, shape: 'dense', peers }) : null), [graph, ifaces, servers, orientation, peers])
  const down = useMemo(() => (orientation !== 'horizontal' ? layout(graph, ifaces, { servers, shape: 'down', peers }) : null), [graph, ifaces, servers, orientation, peers])
  const shown = arrange(orientation, { across, dense, down }, room)
  const vertical = shown.shape === 'down'
  const map = { across, dense, down }[shown.shape]
  const incoming = useMemo(() => {
    const c = {}
    for (const l of graph.links) c[l.to] = (c[l.to] || 0) + 1
    return c
  }, [graph])
  const looks = useMemo(() => Object.fromEntries(map.links.map((l) => [l.id, linkLook(l, map, incoming)])), [map, incoming])
  // What started from nothing since the last refresh flashes.
  const now = useMemo(
    () =>
      figures(
        map,
        hap,
        Object.fromEntries(Object.entries(looks).map(([id, look]) => [id, look.traffic])),
      ),
    [map, hap, looks],
  )
  const flash = useFlashes(now)

  const scroller = useRef(null) // the map's card, its scrolling element

  // The zoom: a factor of arrange's scale, 1 the map whole in the window.
  // The point under the pointer (or the view's middle, from the buttons)
  // stays where it is: anchor is it, in the map's units, until the new
  // size is laid out.
  const [zoom, setZoom] = useState(1)
  const scale = shown.scale * zoom
  const anchor = useRef(null)
  const sizes = useRef({ fit: shown.scale, width: map.width })
  sizes.current = { fit: shown.scale, width: map.width }
  const zoomBy = useCallback((factor, cx, cy) => {
    const el = scroller.current
    const svg = el?.querySelector('svg')
    if (!svg) return
    const { fit, width } = sizes.current
    const r = svg.getBoundingClientRect()
    const k = r.width / width
    const box = el.getBoundingClientRect()
    const x = cx ?? box.left + box.width / 2
    const y = cy ?? (Math.max(box.top, 0) + Math.min(box.bottom, window.innerHeight)) / 2
    anchor.current = { px: (x - r.left) / k, py: (y - r.top) / k, cx: x, cy: y }
    setZoom((z) => {
      const next = Math.min(ZOOM_MAX, Math.max(ZOOM_MIN, fit * z * factor)) / fit
      return Math.abs(next - 1) < 0.03 ? 1 : next
    })
  }, [])
  useLayoutEffect(() => {
    const a = anchor.current
    anchor.current = null
    const el = scroller.current
    const svg = el?.querySelector('svg')
    if (!a || !svg) return
    const r = svg.getBoundingClientRect()
    const k = r.width / sizes.current.width
    el.scrollLeft += r.left + a.px * k - a.cx
    const dy = r.top + a.py * k - a.cy
    const before = el.scrollTop
    el.scrollTop += dy
    const rest = dy - (el.scrollTop - before) // what the card's own scroll didn't take
    if (Math.abs(rest) > 0.5) window.scrollBy(0, rest)
  }, [zoom])

  // A map wider than its card keeps HAProxy in the middle - as the
  // configuration arrives and moves it - until the user scrolls or
  // zooms it.
  const centred = useRef({ key: '', left: null })
  const drawn = graph.frontends.length + graph.backends.length > 0
  useEffect(() => {
    const el = scroller.current
    if (!el || !drawn || zoom !== 1) return
    const key = shown.shape
    const last = centred.current
    if (last.key === key && last.left != null && Math.abs(el.scrollLeft - last.left) > 1) return // scrolled by hand
    const svg = el.querySelector('svg')
    const scale = svg ? svg.getBoundingClientRect().width / map.width : 1
    el.scrollLeft = vertical ? Math.max(0, map.byId.hub.x * scale - el.clientWidth / 2) : 0
    centred.current = { key, left: el.scrollLeft }
  }, [vertical, shown.shape, drawn, map, zoom])

  // Ctrl (or ⌘) + scroll zooms - a trackpad's pinch comes as one too -,
  // not the page; a scroll alone still scrolls it. The listener can't
  // be React's: those are passive, and preventDefault needs not.
  const [scrollEl, setScrollEl] = useState(null)
  const scrollRef = useCallback((el) => {
    scroller.current = el
    setScrollEl(el)
  }, [])
  useEffect(() => {
    if (!scrollEl) return undefined
    const onWheel = (e) => {
      if (!e.ctrlKey && !e.metaKey) return
      e.preventDefault()
      const unit = e.deltaMode === 1 ? 33 : e.deltaMode === 2 ? 400 : 1 // lines, pages: in pixels
      zoomBy(Math.exp(-e.deltaY * unit * 0.0025), e.clientX, e.clientY)
    }
    scrollEl.addEventListener('wheel', onWheel, { passive: false })
    return () => scrollEl.removeEventListener('wheel', onWheel)
  }, [scrollEl, zoomBy])

  // A zoomed map is dragged with the mouse, from anywhere but a node.
  const drag = useRef(null)
  const dragged = useRef(false)
  const [dragging, setDragging] = useState(false)
  const pointer = {
    onPointerDown: (e) => {
      const el = scroller.current
      if (zoom === 1 || e.pointerType !== 'mouse' || e.button !== 0 || e.target.closest('.flow-node, .flow-hit')) return
      drag.current = { x: e.clientX, y: e.clientY, left: el.scrollLeft, top: el.scrollTop, id: e.pointerId }
      dragged.current = false
    },
    onPointerMove: (e) => {
      const d = drag.current
      if (!d) return
      const dx = e.clientX - d.x
      const dy = e.clientY - d.y
      if (!dragged.current && Math.abs(dx) + Math.abs(dy) < 4) return
      if (!dragged.current) {
        dragged.current = true
        setDragging(true)
        scroller.current.setPointerCapture(d.id)
      }
      scroller.current.scrollLeft = d.left - dx
      scroller.current.scrollTop = d.top - dy
    },
    onPointerUp: () => {
      drag.current = null
      setDragging(false)
    },
  }
  pointer.onPointerCancel = pointer.onPointerUp

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
    setZoom(1)
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
          <div className="chips flow-zoom" role="group" aria-label="Zoom">
            <button className="small chip" onClick={() => zoomBy(1 / ZOOM_STEP)} disabled={scale <= ZOOM_MIN + 0.001} aria-label="Zoom out" title="Zoom out (Ctrl + scroll)">
              <ZoomOut size={14} aria-hidden="true" />
            </button>
            <span className="flow-zoom-level small mono" aria-live="polite" title="The map's size">
              {Math.round(scale * 100)}%
            </span>
            <button className="small chip" onClick={() => zoomBy(ZOOM_STEP)} disabled={scale >= ZOOM_MAX - 0.001} aria-label="Zoom in" title="Zoom in (Ctrl + scroll)">
              <ZoomIn size={14} aria-hidden="true" />
            </button>
            <button className={`small chip ${zoom === 1 ? 'active' : ''}`} aria-pressed={zoom === 1} onClick={() => setZoom(1)} title="The map whole in the window, as big as it allows">
              Fit
            </button>
          </div>
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
  const empty = statWindow.length > 0 && !graph.frontends.length && !graph.backends.length
  const picked = selected && (map.byId[selected] || map.links.some((l) => l.id === selected))
  const summary = `Traffic flow: ${graph.frontends.length} frontends, ${graph.backends.length} backends, ${graph.backends.reduce((t, b) => t + b.servers.length, 0)} servers${apis.length ? `, ${apis.map((a) => a.name.toLowerCase()).join(', ')}` : ''}${logs.length ? `, ${logs.length} log targets` : ''}`

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
              <div
                className={`flow-scroll ${zoom !== 1 ? 'zoomed' : ''} ${dragging ? 'dragging' : ''}`}
                ref={scrollRef}
                {...pointer}
                onClick={() => {
                  if (dragged.current) {
                    dragged.current = false // the end of a drag, not a click
                    return
                  }
                  setSelected(null)
                }}
              >
                <svg
                  className={`flow-map ${vertical ? 'vertical' : ''} ${shown.shape} ${near ? 'focused' : ''} ${nodeCharts ? '' : 'still'}`}
                  viewBox={`0 0 ${map.width} ${map.height}`}
                  style={{ width: Math.round(map.width * scale) }}
                  role="group"
                  aria-label={summary}
                >
                  <g>
                    {map.links.map((l) => (
                      <FlowLink key={l.id} l={l} look={looks[l.id]} state={stateOf(l.id)} flash={flash[l.id]} onSelect={select} onHover={setHover} moving={moving} linger={linger} />
                    ))}
                  </g>
                  <g>
                    {map.nodes.map((n) => (
                      <FlowNode
                        key={n.id}
                        n={n}
                        hap={hap}
                        configKnown={graph.configKnown}
                        shape={shown.shape}
                        state={`${stateOf(n.id)} ${selected === n.id ? 'selected' : ''}`}
                        flash={flash[n.id]}
                        onSelect={select}
                        onHover={setHover}
                      />
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
                  <Legend stick={graph.backends.some((b) => b.stick)} late={graph.frontends.some(lateCounted)} />
                </Card>
              )}
            </div>
          </div>
        )}
      </div>
    </>
  )
}

