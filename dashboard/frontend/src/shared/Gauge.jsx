// Gauge: how full something is, as a half circle of segments lit from the
// left - in its colour (a chart colour), amber past the first threshold,
// red past the second (Meter's 75 and 90 %) - the figure in its middle.
const SEGMENTS = 28
const R_IN = 31
const R_OUT = 42

const ticks = Array.from({ length: SEGMENTS }, (_, i) => {
  const a = Math.PI + ((i + 0.5) * Math.PI) / SEGMENTS
  const at = (r) => [50 + r * Math.cos(a), 50 + r * Math.sin(a)].map((v) => v.toFixed(2))
  const [x1, y1] = at(R_IN)
  const [x2, y2] = at(R_OUT)
  return `M${x1} ${y1}L${x2} ${y2}`
})

export function gaugeTone(pct, thresholds = [75, 90]) {
  return pct > thresholds[1] ? 'danger' : pct > thresholds[0] ? 'warn' : ''
}

export default function Gauge({ value, max = 100, label, display, color = 'var(--chart-1)', thresholds }) {
  const known = value != null && Number.isFinite(value) && max > 0
  const pct = known ? Math.max(0, Math.min(100, (value / max) * 100)) : 0
  const lit = known ? Math.round((pct / 100) * SEGMENTS) : 0
  return (
    <div
      className={`gauge ${gaugeTone(pct, thresholds)}`}
      style={{ '--gauge': color }}
      role="meter"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={Math.round(pct)}
      aria-valuetext={known ? undefined : 'unknown'}
    >
      <svg viewBox="4 4 92 50" aria-hidden="true" focusable="false">
        {ticks.map((d, i) => (
          <path key={i} d={d} className={i < lit ? 'on' : undefined} />
        ))}
      </svg>
      <div className="gauge-value">{known ? (display ?? `${pct.toFixed(0)}%`) : '–'}</div>
    </div>
  )
}

// GaugeStat is a stat tile around a gauge: its label above, a line under.
export function GaugeStat({ label, sub, ...gauge }) {
  return (
    <div className="card stat gauge-stat">
      <div className="stat-label">{label}</div>
      <Gauge label={label} {...gauge} />
      {sub && <div className="stat-sub">{sub}</div>}
    </div>
  )
}
