import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { useInView, useReducedMotion } from './motion.js'
import { ServerClock, advanceEdge, area, curve, ease, nearest, niceMax, segments, timeStep, timeTicks } from './math.js'
import { subscribe } from './scheduler.js'

// Chart plots series over time - points: [{t, iv, ...}] (iv: the sampling
// interval then, 0 = sampling off), series: [{label, color, get(point) ->
// number|undefined, fill}]. Missing values and time gaps leave gaps.
//
// While sampling, the time axis scrolls continuously: its right edge
// stays about one interval behind the samples' clock (ServerClock), so
// the line grows at the edge instead of jumping a step per sample, and
// the value axis eases to a new scale. The curves are built once per
// sample, in the samples' own units; a frame only moves them - one
// transform per chart, set directly, no render. Still (redrawn per
// sample, the edge on the newest) under "reduce motion", with sampling
// off, or motion="static"; asleep off screen.
const PAD = { top: 10, right: 12, bottom: 22, left: 52 }
const COMPACT = { top: 3, right: 1, bottom: 2, left: 1 }
const MIN_SPAN = 30 * 1000
let nextId = 0

function useWidth() {
  const ref = useRef(null)
  const [width, setWidth] = useState(600)
  useEffect(() => {
    if (!ref.current) return undefined
    const ro = new ResizeObserver(([entry]) => setWidth(Math.max(80, Math.floor(entry.contentRect.width))))
    ro.observe(ref.current)
    return () => ro.disconnect()
  }, [])
  return [ref, width]
}

function tickLabel(t, step) {
  const opts = step >= 60000 ? { hour: '2-digit', minute: '2-digit' } : { hour: '2-digit', minute: '2-digit', second: '2-digit' }
  return new Date(t).toLocaleTimeString([], { ...opts, hour12: false })
}

// layout is where the axes stand for an edge and a scale: what a render
// draws, and what each frame moves.
function layout({ edge, yCur, first, tRef, yRef, windowMs, w, h, pad }) {
  const data = edge - first
  const span = Math.max(MIN_SPAN, windowMs ? Math.min(windowMs, data) : data)
  const t0 = edge - span
  const k = w / (span / 1000) // px a second
  const y = yCur || yRef
  const tx = pad.left + ((tRef - t0) / 1000) * k
  const sy = (h * yRef) / y
  return {
    t0,
    t1: edge,
    span,
    k,
    tx,
    sy,
    x: (t) => pad.left + ((t - t0) / 1000) * k,
    y: (v) => pad.top + h - (v / y) * h,
    series: `matrix(${k},0,0,${sy},${tx},${pad.top + h})`,
  }
}

const fade = (px, c) => Math.min(1, Math.max(0, (px - c.pad.left) / 24), Math.max(0, (c.pad.left + c.w - px) / 24))

// place moves what a frame moves: the curves, the time ticks (fading in
// and out at the ends), the hovered sample's marks.
function place(els, L, c) {
  els.series?.setAttribute('transform', L.series)
  c.ticks.forEach((t, i) => {
    const el = els.ticks[i]
    if (!el) return
    const px = L.x(t)
    el.setAttribute('transform', `translate(${px.toFixed(1)},0)`)
    el.setAttribute('opacity', fade(px, c).toFixed(2))
  })
  const p = c.points[c.hover]
  if (!p || !els.hover) return
  els.hover.setAttribute('transform', `translate(${L.x(p.t).toFixed(1)},0)`)
  c.series.forEach((s, i) => {
    const v = s.get(p)
    if (v != null && els.dots[i]) els.dots[i].setAttribute('cy', L.y(Math.min(v, c.yMax ?? Infinity)).toFixed(1))
  })
}

// startEdge is where a chart starting to move puts its right edge.
function startEdge(clk, first, last) {
  return Math.max(first, Math.min(last, clk.now(Date.now()) - clk.buffer()))
}

// hoverAt picks the sample under the pointer - the data scrolls under a
// pointer that stays still.
function hoverAt(els, L, c) {
  if (els.pointer == null || !c.points.length) return
  const t = L.t0 + ((els.pointer - c.pad.left) / L.k) * 1000
  let i = nearest(c.points, t)
  // Nothing sampled near the pointer (before the history, in a gap).
  if (Math.abs(c.points[i].t - t) > Math.max(2000, 1.5 * (c.points[i].iv || 0))) i = -1
  if (i !== c.hover) c.setHover(i)
}

export default function Chart({ points, series, format = (v) => v.toFixed(1), height = 170, yMax, windowMs, binary = false, clock, motion = 'auto', compact = false, label }) {
  const [ref, width] = useWidth()
  const inView = useInView(ref)
  const reduced = useReducedMotion()
  const [hover, setHover] = useState(-1)
  // While it moves: the time span the ticks are drawn for, renewed by a
  // frame when a tick is about to enter.
  const [moving, setMoving] = useState(null)
  const [id] = useState(() => ++nextId)
  const [ownClock] = useState(() => new ServerClock())
  const clk = clock || ownClock

  const last = points[points.length - 1]
  const first = points[0]
  const live = motion !== 'static' && !reduced && last?.iv > 0 && points.length >= 2

  useEffect(() => {
    if (!clock && last) ownClock.update(last.t, Date.now(), last.iv || 0)
  }, [clock, ownClock, last])

  // The value axis: a nice top over what the window shows.
  const yTarget = useMemo(() => {
    if (yMax) return yMax
    let maxV = 0
    const from = last && windowMs ? last.t - windowMs - (last.iv || 0) * 2 : -Infinity
    for (const p of points) {
      if (p.t < from) continue
      for (const s of series) {
        const v = s.get(p)
        if (v != null && v > maxV) maxV = v
      }
    }
    return niceMax(maxV, binary)
  }, [points, series, windowMs, yMax, binary, last])

  // The value axis' labels set the left margin: the widest, in the
  // monospace face (0.6 em a character, 10 px).
  const yTicks = useMemo(() => (compact ? [] : [0, 0.25, 0.5, 0.75, 1].map((f) => f * yTarget)), [compact, yTarget])
  const labels = yTicks.map((v) => format(v))
  const left = compact ? COMPACT.left : Math.max(34, Math.ceil(Math.max(0, ...labels.map((l) => l.length)) * 6) + 12)
  const pad = useMemo(() => (compact ? COMPACT : { ...PAD, left }), [compact, left])
  const w = width - pad.left - pad.right
  const h = height - pad.top - pad.bottom

  // The curves, in the samples' own units: seconds before the newest
  // sample across, the value over yRef up (negative: SVG's y goes down).
  // Kept from a little before the window: the edge stays up to 32 s
  // behind the newest sample, and a line runs out by the left.
  const tRef = last?.t ?? 0
  const yRef = yTarget
  const drawn = useMemo(() => {
    if (!last) return []
    const keep = windowMs ? last.t - windowMs - 33000 - (last.iv || 0) * 3 : -Infinity
    let from = 0
    while (from < points.length - 1 && points[from + 1].t < keep) from++
    const pts = points.slice(from)
    const X = (t) => (t - tRef) / 1000
    const Y = (v) => -Math.min(v, yMax ?? Infinity) / yRef
    return series.map((s, i) => {
      const runs = segments(pts, s.get)
      return {
        line: runs.map((r) => curve(r, X, Y)).join(''),
        area: (s.fill ?? i === 0) ? runs.map((r) => area(r, X, Y, 0)).join('') : '',
      }
    })
  }, [points, series, windowMs, yMax, yRef, tRef, last])

  // A render draws the still chart - the edge on the newest sample; while
  // it moves, a layout effect puts it where its frames have it before the
  // page paints.
  const firstT = first?.t ?? 0
  const shape = useMemo(() => ({ first: firstT, tRef, yRef, windowMs, w, h, pad }), [firstT, tRef, yRef, windowMs, w, h, pad])
  const S = last ? layout({ ...shape, edge: last.t, yCur: yTarget }) : null
  const span = live && moving ? moving : S
  const step = span ? timeStep(span.span, w) : 0
  const [from, to] = span ? [span.t0 - step, span.t1 + step] : [0, -1]
  const ticks = useMemo(() => (compact || !step ? [] : timeTicks(from, to, step)), [from, to, step, compact])

  const els = useRef({ series: null, ticks: [], hover: null, dots: [], pointer: null })
  const anim = useRef({ edge: null, yCur: null, key: '' })
  const now = useRef(null)
  useLayoutEffect(() => {
    now.current = { last, yTarget, yMax, points, series, hover, setHover, ticks, ...shape }
    if (!live || !last) {
      anim.current = { edge: null, yCur: null, key: '' }
      return
    }
    const a = anim.current
    if (a.edge == null) {
      a.edge = startEdge(clk, firstT, last.t)
      a.yCur = yTarget
    }
    const L = layout({ ...shape, edge: a.edge, yCur: a.yCur })
    place(els.current, L, now.current)
    Object.assign(a, { tx: L.tx, k: L.k, sy: L.sy })
  }, [live, last, firstT, yTarget, yMax, points, series, hover, ticks, shape, clk])

  useEffect(() => {
    if (!live || !inView) return undefined
    return subscribe((_, dt) => {
      const c = now.current
      const a = anim.current
      if (!c?.last || a.edge == null) return
      a.edge = Math.max(c.first, advanceEdge(a.edge, clk.now(Date.now()) - clk.buffer(), dt, c.last.t, Math.max(15000, 3 * (c.last.iv || 1000))))
      a.yCur = ease(a.yCur, c.yTarget, dt)
      const L = layout({ ...c, edge: a.edge, yCur: a.yCur })
      // A tick about to enter: a render brings it, with its label.
      const s = timeStep(L.span, c.w)
      const key = `${s}:${Math.ceil((L.t0 - s) / s)}:${Math.floor((L.t1 + s) / s)}`
      if (key !== a.key) {
        a.key = key
        setMoving({ t0: L.t0, t1: L.t1, span: L.span })
      }
      // Nothing moved by a quarter of a pixel (a sparkline over five
      // minutes moves a third of one a second): the frame is skipped -
      // what costs is the browser painting the chart again. The scale
      // changes while the window fills: by its effect at the far end.
      const moved = Math.abs(L.tx - a.tx) >= 0.25 || Math.abs(L.k - a.k) * (c.w / L.k) >= 0.25 || Math.abs(L.sy - a.sy) > 0.01 || c.hover >= 0
      if (!moved) return
      Object.assign(a, { tx: L.tx, k: L.k, sy: L.sy })
      place(els.current, L, c)
      hoverAt(els.current, L, c)
    }, compact ? 15 : 30)
  }, [live, inView, clk, compact])

  const onMove = (e) => {
    if (compact || !S) return
    const px = e.clientX - e.currentTarget.getBoundingClientRect().left
    els.current.pointer = px < pad.left ? null : px
    if (els.current.pointer == null) setHover(-1)
    else if (!live) hoverAt(els.current, S, { points, pad, hover, setHover })
  }
  const onLeave = () => {
    els.current.pointer = null
    setHover(-1)
  }

  const hovered = hover >= 0 ? points[hover] : null
  const shown = hovered || last
  const clipId = `chart-clip-${id}`

  return (
    <div className={`chart${compact ? ' compact' : ''}`} ref={ref}>
      {!compact && (
        <div className="chart-legend">
          {series.map((s) => {
            const v = shown && s.get(shown)
            return (
              <span key={s.label} className="chart-legend-item">
                <span className="chart-swatch" style={{ background: s.color }} />
                {s.label}
                <strong>{v == null ? '–' : format(v)}</strong>
              </span>
            )
          })}
          {hovered && <span className="muted small">{new Date(hovered.t).toLocaleTimeString()}</span>}
        </div>
      )}
      <svg width={width} height={height} onPointerMove={onMove} onPointerLeave={onLeave} role="img" aria-label={label || series.map((s) => s.label).join(', ')}>
        <defs>
          <clipPath id={clipId}>
            <rect x={pad.left} y={pad.top - 3} width={Math.max(0, w)} height={h + 4} />
          </clipPath>
          {series.map((s, i) =>
            drawn[i]?.area ? (
              <linearGradient key={i} id={`${clipId}-g${i}`} gradientUnits="userSpaceOnUse" x1="0" y1="-1" x2="0" y2="0">
                <stop offset="0" className="chart-fill-top" style={{ stopColor: s.color }} />
                <stop offset="1" className="chart-fill-bottom" style={{ stopColor: s.color }} />
              </linearGradient>
            ) : null,
          )}
        </defs>
        {yTicks.map((v, i) => (
          <g key={i} transform={`translate(0,${(pad.top + h - (i / 4) * h).toFixed(1)})`}>
            <line x1={pad.left} x2={pad.left + w} className="chart-grid" />
            <text x={pad.left - 6} y={4} textAnchor="end" className="chart-axis">
              {labels[i]}
            </text>
          </g>
        ))}
        {S &&
          ticks.map((t, i) => {
            const px = S.x(t)
            return (
              <g key={t} ref={(el) => (els.current.ticks[i] = el)} transform={`translate(${px.toFixed(1)},0)`} opacity={fade(px, { pad, w }).toFixed(2)}>
                <line y1={pad.top} y2={pad.top + h} className="chart-grid chart-grid-x" />
                <text y={height - 5} textAnchor="middle" className="chart-axis">
                  {tickLabel(t, step)}
                </text>
              </g>
            )
          })}
        <g clipPath={`url(#${clipId})`}>
          <g ref={(el) => (els.current.series = el)} transform={S?.series}>
            {drawn.map((d, i) => d.area && <path key={`a${i}`} d={d.area} fill={`url(#${clipId}-g${i})`} className="chart-area" />)}
            {drawn.map((d, i) => (
              <path key={`l${i}`} d={d.line} className="chart-line" style={{ stroke: series[i].color }} vectorEffect="non-scaling-stroke" />
            ))}
          </g>
        </g>
        {hovered && S && (
          <g ref={(el) => (els.current.hover = el)} transform={`translate(${S.x(hovered.t).toFixed(1)},0)`}>
            <line y1={pad.top} y2={pad.top + h} className="chart-crosshair" />
            {series.map((s, i) => {
              const v = s.get(hovered)
              return v == null ? null : <circle key={s.label} ref={(el) => (els.current.dots[i] = el)} cx="0" cy={S.y(Math.min(v, yMax ?? Infinity)).toFixed(1)} r="3.5" style={{ fill: s.color }} />
            })}
          </g>
        )}
        {!last && !compact && (
          <text x={pad.left + w / 2} y={pad.top + h / 2} textAnchor="middle" className="chart-axis">
            collecting data…
          </text>
        )}
      </svg>
    </div>
  )
}
