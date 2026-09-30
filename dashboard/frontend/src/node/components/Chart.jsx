import { useEffect, useMemo, useRef, useState } from 'react'

const PAD = { top: 10, right: 12, bottom: 22, left: 52 }

function niceMax(v, binary) {
  if (!(v > 0)) return 1
  if (binary) {
    // Bytes: round to powers of two of a 1024 unit (1 GiB, 512 MiB, ...),
    // so the quarter ticks land on round values too.
    let top = 1
    while (top < v) top *= 2
    return top
  }
  const exp = Math.pow(10, Math.floor(Math.log10(v)))
  for (const m of [1, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10]) {
    if (v <= m * exp) return m * exp
  }
  return 10 * exp
}

function useWidth() {
  const ref = useRef(null)
  const [width, setWidth] = useState(600)
  useEffect(() => {
    if (!ref.current) return undefined
    const ro = new ResizeObserver(([entry]) => setWidth(Math.max(200, Math.floor(entry.contentRect.width))))
    ro.observe(ref.current)
    return () => ro.disconnect()
  }, [])
  return [ref, width]
}

// Chart plots series over time. points: [{t, ...}]; series: [{label,
// color, get(point) -> number|undefined}]. Missing values leave gaps
// instead of drawing a line through a period with no data.
const MIN_SPAN = 30 * 1000

function tickLabel(t) {
  return new Date(t).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false })
}

export default function Chart({ points, series, format = (v) => v.toFixed(1), height = 170, yMax, windowMs, binary = false }) {
  const [ref, width] = useWidth()
  const [hover, setHover] = useState(null)

  const now = points.length ? points[points.length - 1].t : Date.now()
  const visible = useMemo(() => (windowMs ? points.filter((p) => p.t >= now - windowMs) : points), [points, windowMs, now])

  // Until the window is full, the time axis follows the data instead of
  // squeezing a few samples against the right edge.
  const dataSpan = visible.length ? now - visible[0].t : 0
  const span = Math.max(MIN_SPAN, windowMs ? Math.min(windowMs, dataSpan) : dataSpan)
  const t1 = now
  const t0 = now - span
  let maxV = 0
  for (const p of visible) {
    for (const s of series) {
      const v = s.get(p)
      if (v != null && v > maxV) maxV = v
    }
  }
  const y1 = yMax ?? niceMax(maxV, binary)
  const w = width - PAD.left - PAD.right
  const h = height - PAD.top - PAD.bottom
  const x = (t) => PAD.left + ((t - t0) / (t1 - t0)) * w
  const y = (v) => PAD.top + h - (Math.min(v, y1) / y1) * h

  const paths = series.map((s) => {
    let d = ''
    let pen = false
    for (const p of visible) {
      const v = s.get(p)
      if (v == null || Number.isNaN(v)) {
        pen = false
        continue
      }
      d += `${pen ? 'L' : 'M'}${x(p.t).toFixed(1)},${y(v).toFixed(1)}`
      pen = true
    }
    return d
  })

  const yTicks = [0, 0.25, 0.5, 0.75, 1].map((f) => f * y1)
  const xCount = Math.max(2, Math.min(6, Math.floor(w / 95)))
  const xTicks = Array.from({ length: xCount }, (_, i) => t0 + (i / (xCount - 1)) * (t1 - t0))

  const onMove = (e) => {
    const rect = e.currentTarget.getBoundingClientRect()
    const px = e.clientX - rect.left
    if (!visible.length || px < PAD.left) return setHover(null)
    const t = t0 + ((px - PAD.left) / w) * (t1 - t0)
    let best = visible[0]
    for (const p of visible) if (Math.abs(p.t - t) < Math.abs(best.t - t)) best = p
    setHover(best)
  }

  const last = visible[visible.length - 1]
  return (
    <div className="chart" ref={ref}>
      <div className="chart-legend">
        {series.map((s) => {
          const v = (hover || last) && s.get(hover || last)
          return (
            <span key={s.label} className="chart-legend-item">
              <span className="chart-swatch" style={{ background: s.color }} />
              {s.label}
              <strong>{v == null ? '–' : format(v)}</strong>
            </span>
          )
        })}
        {hover && <span className="muted small">{new Date(hover.t).toLocaleTimeString()}</span>}
      </div>
      <svg width={width} height={height} onMouseMove={onMove} onMouseLeave={() => setHover(null)} role="img">
        {yTicks.map((v) => (
          <g key={v}>
            <line x1={PAD.left} x2={PAD.left + w} y1={y(v)} y2={y(v)} stroke="var(--chart-grid)" />
            <text x={PAD.left - 6} y={y(v) + 4} textAnchor="end" className="chart-axis">
              {format(v)}
            </text>
          </g>
        ))}
        {xTicks.map((t, i) => (
          <text key={i} x={x(t)} y={height - 5} textAnchor={i === 0 ? 'start' : i === xCount - 1 ? 'end' : 'middle'} className="chart-axis">
            {tickLabel(t)}
          </text>
        ))}
        {paths.map((d, i) => (
          <g key={series[i].label}>
            {i === 0 && d && <path d={`${d}`} fill="none" stroke={series[i].color} strokeOpacity="0.12" strokeWidth="6" strokeLinejoin="round" />}
            <path d={d} fill="none" stroke={series[i].color} strokeWidth="1.8" strokeLinejoin="round" strokeLinecap="round" />
          </g>
        ))}
        {hover && (
          <g>
            <line x1={x(hover.t)} x2={x(hover.t)} y1={PAD.top} y2={PAD.top + h} stroke="var(--muted)" strokeDasharray="3 3" />
            {series.map((s) => {
              const v = s.get(hover)
              return v == null ? null : <circle key={s.label} cx={x(hover.t)} cy={y(v)} r="3.5" fill={s.color} />
            })}
          </g>
        )}
        {!visible.length && (
          <text x={PAD.left + w / 2} y={PAD.top + h / 2} textAnchor="middle" className="chart-axis">
            collecting data…
          </text>
        )}
      </svg>
    </div>
  )
}
