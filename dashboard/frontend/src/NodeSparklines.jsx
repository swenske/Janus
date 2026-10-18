import Sparkline from './shared/chart/Sparkline.jsx'
import { compact, percent, rate } from './node/format.js'

// NodeSparklines: a node card's live trends - its CPU, HAProxy's
// requests, its traffic in and out - over the last five minutes
// (nodeMetrics.js). Room is kept before the first readings come, so the
// card doesn't grow under the pointer.
const WINDOW = 5 * 60 * 1000

const CPU = [{ label: 'CPU', color: 'var(--chart-1)', get: (p) => p.cpu }]
const REQ = [{ label: 'Requests/s', color: 'var(--chart-3)', get: (p) => p.req }]
const NET = [
  { label: 'Received', color: 'var(--chart-2)', get: (p) => p.rx },
  { label: 'Sent', color: 'var(--chart-4)', get: (p) => p.tx },
]

function latest(points, get) {
  for (let i = points.length - 1; i >= 0; i--) {
    const v = get(points[i])
    if (v != null) return v
  }
  return null
}

function Cell({ label, value, children }) {
  return (
    <div className="spark-cell">
      <div className="spark-head">
        <span className="spark-label">{label}</span>
        <span className="spark-value">{value ?? '–'}</span>
      </div>
      {children}
    </div>
  )
}

export default function NodeSparklines({ points = [], clock }) {
  const cpu = latest(points, CPU[0].get)
  const req = latest(points, REQ[0].get)
  const rx = latest(points, NET[0].get)
  const tx = latest(points, NET[1].get)
  return (
    <div className="sparks">
      <Cell label="CPU" value={cpu == null ? null : percent(cpu, 0)}>
        <Sparkline points={points} clock={clock} series={CPU} yMax={100} windowMs={WINDOW} label="CPU, the last five minutes" />
      </Cell>
      <Cell label="Req/s" value={req == null ? null : compact(req)}>
        <Sparkline points={points} clock={clock} series={REQ} windowMs={WINDOW} label="HAProxy requests a second, the last five minutes" />
      </Cell>
      <Cell label="Traffic" value={rx == null && tx == null ? null : `${rate((rx || 0) + (tx || 0))}`}>
        <Sparkline points={points} clock={clock} series={NET} windowMs={WINDOW} binary label="Bytes in and out a second, the last five minutes" />
      </Cell>
    </div>
  )
}
