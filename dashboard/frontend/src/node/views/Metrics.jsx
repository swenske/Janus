import { Activity, ChartPie, Cpu, ExternalLink, Gauge, KeyRound, Lock, MemoryStick, Network, ShieldCheck, Shuffle, Timer, Waypoints } from 'lucide-react'
import { useMemo, useState } from 'react'
import Chart from '../../shared/chart/Chart.jsx'
import { Badge, Card, PageHeader, stateTone } from '../../shared/ui.jsx'
import Donut from '../../shared/chart/Donut.jsx'
import { backends, ciphers, responses, tls, tlsVersions, traffic, versions } from '../breakdown.js'
import { bytes, compact, duration, rate } from '../format.js'
import { useMetrics, usePoll, useRefresh } from '../hooks.jsx'
import { useMay } from '../may.js'
import { useChartWindow } from '../chartWindow.js'
import WindowPicker from '../WindowPicker.jsx'


export default function Metrics() {
  const { points, clear, clock } = useMetrics()
  const { interval } = useRefresh()
  const [windowMs, setWindowMs] = useChartWindow()
  const [iface, setIface] = useState('')
  const may = useMay()
  // HAProxy's own counters, for the breakdowns: since it started.
  const stats = usePoll('/api/haproxy/stats', { every: 5000, enabled: may('HAProxyService/Stats') })
  // The TLS versions and ciphers, from HAProxy's last log lines - its
  // log is the only place that says them.
  const mayLogs = may('SystemService/Logs')
  const tlsLog = usePoll('/api/haproxy/tls-log', { every: 30000, enabled: mayLogs })
  const hapUptime = points[points.length - 1]?.hap?.uptime

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
            <WindowPicker value={windowMs} onChange={setWindowMs} />
            <button className="small" onClick={clear}>
              Clear history
            </button>
          </>
        }
      />
      <div className="grid metrics-charts">
        <Card title="CPU" icon={Cpu}>
          <Chart
            points={points}
            clock={clock}
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
            clock={clock}
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
            clock={clock}
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
            <select aria-label="Interface" value={selected} onChange={(e) => setIface(e.target.value)}>
              {ifaces.map((n) => (
                <option key={n}>{n}</option>
              ))}
            </select>
          }
        >
          <Chart
            points={points}
            clock={clock}
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
            clock={clock}
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
            clock={clock}
            windowMs={win}
            format={(x) => compact(x)}
            series={[
              { label: 'Current connections', color: 'var(--chart-3)', get: (p) => p.hap?.conns },
              { label: 'Busy %', color: 'var(--chart-1)', get: (p) => (p.hap?.up ? 100 - p.hap.idle : undefined) },
            ]}
          />
        </Card>
      </div>
      {may('HAProxyService/Stats') && <Breakdown stats={stats.data} uptime={hapUptime} tlsLog={{ ...tlsLog, may: mayLogs }} />}
      <p className="muted small" style={{ marginTop: '1rem' }}>
        These charts are the Controller&apos;s, kept in your browser. For Prometheus, the node serves its own metrics: <a href="#/apps/janus-exporter">Apps › Janus exporter</a>, and the host&apos;s with{' '}
        <a href="#/apps/node-exporter">the node exporter</a> when the node has it.
      </p>
    </>
  )
}

// Breakdown: what HAProxy's counters say since it started - its
// frontends' answers, HTTP versions and TLS handshakes, its backends'
// traffic and response times (breakdown.js) - and what its last log
// lines say of TLS versions and ciphers.
// How to have HAProxy log what the TLS cards count.
const TLS_LOG_DOCS = 'https://janus.sw-servers.net/docs/haproxy-config.md#tls-versions-and-ciphers-on-metrics'

// tlsEmpty says why the TLS log cards have nothing to show - and, when
// it's the configuration's doing, links to how to log them.
function tlsEmpty(log, handshakes) {
  if (!log.may) return "Your role doesn't read the node's logs, where these are."
  if (log.error) return `HAProxy's log couldn't be read: ${log.error.message}`
  if (!log.data) return "Reading HAProxy's log…"
  if (handshakes === 0) return 'No TLS handshake on this node yet.'
  const how = (
    <a href={TLS_LOG_DOCS} target="_blank" rel="noreferrer">
      How to log them <ExternalLink size={11} />
    </a>
  )
  if (!log.data.access)
    return (
      <>
        HAProxy keeps no access log on the node: these come from it - <code>log stdout</code> in its global section. {how}
      </>
    )
  return (
    <>
      HAProxy&apos;s access log doesn&apos;t name TLS versions and ciphers - <code>option httpslog</code> does. {how}
    </>
  )
}

function Breakdown({ stats, uptime, tlsLog }) {
  // Whether any TLS handshake happened (unknown until stats come).
  const handshakes = stats ? tls(stats).reduce((n, s) => n + s.value, 0) : null
  const since = uptime ? `since HAProxy started, ${duration(uptime)} ago` : 'since HAProxy started'
  const rows = backends(stats)
  // A bar's scale: the slowest backend's, a tenth of a second at least -
  // a millisecond never fills one.
  const slowest = Math.max(100, ...rows.map((b) => b.rtime))
  return (
    <>
      <h2 className="metrics-h">
        HAProxy <span className="muted small">{since}</span>
      </h2>
      <div className="grid grid-3 metrics-donuts">
        <Card title="Responses" icon={ChartPie}>
          <Donut slices={responses(stats)} format={compact} unit="responses" empty="No HTTP response yet" label="Responses by status" />
        </Card>
        <Card title="HTTP versions" icon={Waypoints}>
          <Donut slices={versions(stats)} format={compact} unit="requests" empty="No HTTP request yet" label="Requests by HTTP version" />
        </Card>
        <Card title="TLS handshakes" icon={Lock}>
          <Donut slices={tls(stats)} format={compact} unit="handshakes" empty="No TLS frontend, or no handshake yet" label="TLS handshakes" />
        </Card>
        <Card title="TLS versions" icon={ShieldCheck}>
          <Donut slices={tlsVersions(tlsLog.data)} format={compact} unit="requests" empty={tlsEmpty(tlsLog, handshakes)} label="Requests by TLS version" />
          <TLSLogNote log={tlsLog.data} />
        </Card>
        <Card title="Ciphers" icon={KeyRound}>
          <Donut slices={ciphers(tlsLog.data)} format={compact} unit="requests" empty={tlsEmpty(tlsLog, handshakes)} label="Requests by cipher" />
          <TLSLogNote log={tlsLog.data} />
        </Card>
        <Card title="Traffic by backend" icon={Shuffle}>
          <Donut slices={traffic(stats)} format={(v) => bytes(v)} unit="in and out" empty="No traffic yet" label="Bytes by backend" />
        </Card>
      </div>
      {rows.length > 0 && (
        <Card title="Backends: how fast they answer" icon={Timer} className="metrics-backends">
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Backend</th>
                  <th>Servers up</th>
                  <th className="num">Req/s</th>
                  <th>Response time</th>
                  <th className="num">Connect</th>
                  <th className="num">Queue</th>
                  <th className="num">Total</th>
                  <th className="num">Errors</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((b) => (
                  <tr key={b.name}>
                    <td>
                      <span className="mono">{b.name}</span> <Badge>{b.mode}</Badge>
                    </td>
                    <td>
                      <Badge tone={stateTone(b.servers.up === b.servers.all ? 'up' : b.servers.up ? 'unknown' : 'down')} dot>
                        {b.servers.up}/{b.servers.all}
                      </Badge>
                    </td>
                    <td className="num">{compact(b.rate)}</td>
                    <td className="rtime">
                      <span className="rtime-bar" style={{ width: `${(b.rtime / slowest) * 100}%` }} />
                      <span className="mono small">{b.mode === 'tcp' ? '–' : `${b.rtime} ms`}</span>
                    </td>
                    <td className="num">{b.ctime} ms</td>
                    <td className="num">{b.qtime} ms</td>
                    <td className="num">{b.ttime} ms</td>
                    <td className="num">{b.errors ? <span style={{ color: 'var(--danger)' }}>{compact(b.errors)}</span> : 0}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p className="muted small">Averages over each backend&apos;s last 1024 requests or connections; errors are failed connections and responses.</p>
        </Card>
      )}
    </>
  )
}

// TLSLogNote says what the TLS log cards counted: a sample of what
// HAProxy logged, not every request since it started.
function TLSLogNote({ log }) {
  if (!log?.tls) return null
  return (
    <p className="muted small donut-note">
      The last {compact(log.tls)} TLS requests in HAProxy&apos;s log ({compact(log.lines)} lines read)
    </p>
  )
}
