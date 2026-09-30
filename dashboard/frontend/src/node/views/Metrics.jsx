import { Activity, Cpu, Gauge, MemoryStick, Network, Shuffle } from 'lucide-react'
import { useMemo, useState } from 'react'
import Chart from '../components/Chart.jsx'
import ExporterCard from './Exporter.jsx'
import { Card, PageHeader } from '../../shared/ui.jsx'
import { bytes, compact, rate } from '../format.js'
import { useMetrics, useRefresh } from '../hooks.jsx'

const WINDOWS = [
  { ms: 60 * 1000, label: '1 min' },
  { ms: 5 * 60 * 1000, label: '5 min' },
  { ms: 15 * 60 * 1000, label: '15 min' },
  { ms: 0, label: 'All' },
]

export default function Metrics() {
  const { points, clear } = useMetrics()
  const { interval } = useRefresh()
  const [windowMs, setWindowMs] = useState(5 * 60 * 1000)
  const [iface, setIface] = useState('')

  const ifaces = useMemo(() => {
    const last = points[points.length - 1]
    return last?.net ? Object.keys(last.net).sort() : []
  }, [points])
  const selected = iface || ifaces.find((n) => n !== 'lo') || ifaces[0] || ''
  const win = windowMs || undefined

  return (
    <>
      <PageHeader
        title="Metrics"
        subtitle={
          interval
            ? `Sampled every ${interval / 1000}s while this page is open - history is kept in the browser (${points.length} samples)`
            : 'Auto-refresh is off - pick an interval in the top bar to collect samples'
        }
        actions={
          <>
            <div className="row" role="group">
              {WINDOWS.map((w) => (
                <button key={w.label} className={`small ${windowMs === w.ms ? 'active' : ''}`} onClick={() => setWindowMs(w.ms)}>
                  {w.label}
                </button>
              ))}
            </div>
            <button className="small" onClick={clear}>
              Clear history
            </button>
          </>
        }
      />
      <div className="grid grid-2">
        <Card title="CPU" icon={Cpu}>
          <Chart
            points={points}
            windowMs={win}
            yMax={100}
            format={(x) => `${x.toFixed(0)}%`}
            series={[
              { label: 'System', color: 'var(--chart-1)', get: (p) => p.cpu },
              { label: 'janusd', color: 'var(--chart-2)', get: (p) => p.svc?.janusd?.cpu },
              { label: 'haproxy', color: 'var(--chart-3)', get: (p) => p.svc?.haproxy?.cpu },
            ]}
          />
        </Card>
        <Card title="Memory" icon={MemoryStick}>
          <Chart
            points={points}
            windowMs={win}
            format={(x) => bytes(x, 0)}
            binary
            series={[
              { label: 'Used', color: 'var(--chart-1)', get: (p) => p.memUsed },
              { label: 'Cached', color: 'var(--chart-2)', get: (p) => p.memCached },
              { label: 'haproxy', color: 'var(--chart-3)', get: (p) => p.svc?.haproxy?.mem },
              { label: 'janusd', color: 'var(--chart-4)', get: (p) => p.svc?.janusd?.mem },
            ]}
          />
        </Card>
        <Card title="Load average" icon={Gauge}>
          <Chart
            points={points}
            windowMs={win}
            format={(x) => x.toFixed(2)}
            series={[
              { label: '1 min', color: 'var(--chart-1)', get: (p) => p.load1 },
              { label: '5 min', color: 'var(--chart-2)', get: (p) => p.load5 },
              { label: '15 min', color: 'var(--chart-3)', get: (p) => p.load15 },
            ]}
          />
        </Card>
        <Card
          title="Network"
          icon={Network}
          actions={
            <select value={selected} onChange={(e) => setIface(e.target.value)}>
              {ifaces.map((n) => (
                <option key={n}>{n}</option>
              ))}
            </select>
          }
        >
          <Chart
            points={points}
            windowMs={win}
            format={(x) => rate(x)}
            binary
            series={[
              { label: 'Received', color: 'var(--chart-2)', get: (p) => p.net?.[selected]?.rx },
              { label: 'Sent', color: 'var(--chart-4)', get: (p) => p.net?.[selected]?.tx },
            ]}
          />
        </Card>
        <Card title="HAProxy requests & connections" icon={Shuffle}>
          <Chart
            points={points}
            windowMs={win}
            format={(x) => compact(x)}
            series={[
              { label: 'Requests/s', color: 'var(--chart-1)', get: (p) => p.hap?.reqRate },
              { label: 'New connections/s', color: 'var(--chart-2)', get: (p) => p.hap?.connRate },
              { label: 'Sessions/s', color: 'var(--chart-4)', get: (p) => p.hap?.sessRate },
            ]}
          />
        </Card>
        <Card title="HAProxy load" icon={Activity}>
          <Chart
            points={points}
            windowMs={win}
            format={(x) => compact(x)}
            series={[
              { label: 'Current connections', color: 'var(--chart-3)', get: (p) => p.hap?.conns },
              { label: 'Busy %', color: 'var(--chart-1)', get: (p) => (p.hap?.up ? 100 - p.hap.idle : undefined) },
            ]}
          />
        </Card>
      </div>
      <div style={{ marginTop: '1rem' }}>
        <ExporterCard />
      </div>
    </>
  )
}
