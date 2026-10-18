import { useState } from 'react'

// Donut: a whole split into its parts - slices [{label, value, color}] -
// as a ring, its total (or the hovered part's) in the middle, and a
// legend with each part's value and share. Hovering a part, on the ring
// or in the legend, brings it forward.
const R = 46
const C = 2 * Math.PI * R
const GAP = 1.6 // between two parts, along the ring

export default function Donut({ slices, format = (v) => String(v), unit = '', empty = 'Nothing yet', label }) {
  const [hover, setHover] = useState(-1)
  const total = slices.reduce((n, s) => n + s.value, 0)
  if (!total) return <div className="donut-empty muted small">{empty}</div>
  const pct = (v) => `${((v / total) * 100).toFixed(v / total < 0.1 ? 1 : 0)}%`
  const many = slices.length > 1
  const arcs = []
  for (const s of slices) {
    const prev = arcs[arcs.length - 1]
    const len = (s.value / total) * C
    arcs.push({ ...s, full: len, len: Math.max(0, many ? len - GAP : len), start: prev ? prev.start + prev.full : 0 })
  }
  const shown = slices[hover]
  return (
    <div className="donut">
      <svg viewBox="0 0 120 120" role="img" aria-label={`${label}: ${slices.map((s) => `${s.label} ${pct(s.value)}`).join(', ')}`}>
        <circle cx="60" cy="60" r={R} className="donut-track" />
        <g transform="rotate(-90 60 60)">
          {arcs.map((a, i) => (
            <circle
              key={a.label}
              cx="60"
              cy="60"
              r={R}
              className={`donut-arc${hover >= 0 && hover !== i ? ' faded' : ''}`}
              style={{ stroke: a.color, strokeDasharray: `${a.len.toFixed(2)} ${(C - a.len).toFixed(2)}`, strokeDashoffset: (-a.start).toFixed(2) }}
              onPointerEnter={() => setHover(i)}
              onPointerLeave={() => setHover(-1)}
            />
          ))}
        </g>
        <text x="60" y="58" textAnchor="middle" className="donut-total">
          {format(shown ? shown.value : total)}
        </text>
        <text x="60" y="74" textAnchor="middle" className="donut-caption">
          {shown ? `${shown.label} · ${pct(shown.value)}` : unit}
        </text>
      </svg>
      <ul className="donut-legend">
        {slices.map((s, i) => (
          <li key={s.label} className={hover >= 0 && hover !== i ? 'faded' : undefined} onPointerEnter={() => setHover(i)} onPointerLeave={() => setHover(-1)}>
            <span className="chart-swatch" style={{ background: s.color }} />
            <span className="donut-label">{s.label}</span>
            <span className="donut-value">{format(s.value)}</span>
            <span className="donut-pct">{pct(s.value)}</span>
          </li>
        ))}
      </ul>
    </div>
  )
}
