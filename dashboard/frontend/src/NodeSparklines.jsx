import Sparkline from './shared/chart/Sparkline.jsx'
import { compact, percent, rate } from './node/format.js'

// A node's live trends - its CPU, HAProxy's requests, its traffic in and
// out - over the last two minutes, a reading every 2 s (nodeMetrics.js):
// three on a node's card (NodeSparklines), one per column in the node
// list's rows (NodeSpark). Room is kept before the first readings come,
// so nothing grows under the pointer.
export const WINDOW = 2 * 60 * 1000

const latest = (points, get) => {
  for (let i = points.length - 1; i >= 0; i--) {
    const v = get(points[i])
    if (v != null) return v
  }
  return null
}

const KINDS = {
  cpu: {
    label: 'CPU',
    series: [{ label: 'CPU', color: 'var(--chart-1)', get: (p) => p.cpu }],
    props: { yMax: 100 },
    value: (pts) => {
      const v = latest(pts, (p) => p.cpu)
      return v == null ? null : percent(v, 0)
    },
    aria: 'CPU, the last five minutes',
  },
  req: {
    label: 'Req/s',
    series: [{ label: 'Requests/s', color: 'var(--chart-3)', get: (p) => p.req }],
    props: {},
    value: (pts) => {
      const v = latest(pts, (p) => p.req)
      return v == null ? null : compact(v)
    },
    aria: 'HAProxy requests a second, the last five minutes',
  },
  net: {
    label: 'Traffic',
    series: [
      { label: 'Received', color: 'var(--chart-2)', get: (p) => p.rx },
      { label: 'Sent', color: 'var(--chart-4)', get: (p) => p.tx },
    ],
    props: { binary: true },
    value: (pts) => {
      const rx = latest(pts, (p) => p.rx)
      const tx = latest(pts, (p) => p.tx)
      return rx == null && tx == null ? null : rate((rx || 0) + (tx || 0))
    },
    aria: 'Bytes in and out a second, the last five minutes',
  },
}

export function NodeSpark({ kind, points = [], clock, head = true }) {
  const k = KINDS[kind]
  return (
    <div className="spark-cell">
      {head ? (
        <div className="spark-head">
          <span className="spark-label">{k.label}</span>
          <span className="spark-value">{k.value(points) ?? '–'}</span>
        </div>
      ) : (
        <span className="spark-value">{k.value(points) ?? '–'}</span>
      )}
      <Sparkline points={points} clock={clock} series={k.series} windowMs={WINDOW} label={k.aria} {...k.props} />
    </div>
  )
}

export default function NodeSparklines({ points, clock }) {
  return (
    <div className="sparks">
      <NodeSpark kind="cpu" points={points} clock={clock} />
      <NodeSpark kind="req" points={points} clock={clock} />
      <NodeSpark kind="net" points={points} clock={clock} />
    </div>
  )
}
