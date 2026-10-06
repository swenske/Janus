import { Activity, Archive, Cpu, Gauge, Server, Shuffle } from 'lucide-react'
import { useState } from 'react'
import Chart from '../components/Chart.jsx'
import { Badge, Card, Meter, PageHeader, Stat, stateTone } from '../../shared/ui.jsx'
import { bytes, compact, cpuModels, cpuShape, dateTime, duration, num, percent, rate } from '../format.js'
import { useMetrics, useNodeStatus, usePoll, useSSE } from '../hooks.jsx'
import { UpdateBadge } from './Update.jsx'
import { variantNote } from '../../variants.js'

const WINDOW = 5 * 60 * 1000

function netTotal(p, dir) {
  if (!p.net) return undefined
  let sum = 0
  let any = false
  for (const [name, d] of Object.entries(p.net)) {
    if (name === 'lo' || d[dir] == null) continue
    sum += d[dir]
    any = true
  }
  return any ? sum : undefined
}

function RecentEvents() {
  const [events, setEvents] = useState([])
  useSSE('/api/stream/events', {
    onMessage: (data) => {
      try {
        const e = JSON.parse(data)
        setEvents((all) => [e, ...all].slice(0, 8))
      } catch {
        // not an event payload
      }
    },
  })
  if (!events.length) return <div className="muted">No events yet.</div>
  return (
    <div className="stack" style={{ gap: '0.45rem' }}>
      {events.map((e) => (
        <div key={e.id} className="spread" style={{ flexWrap: 'nowrap' }}>
          <span className="event-type">{e.type}</span>
          <span className="muted small nowrap">{new Date(e.time_ns / 1e6).toLocaleTimeString()}</span>
        </div>
      ))}
    </div>
  )
}

export default function Overview() {
  const { points, latest } = useMetrics()
  const { overview, check } = useNodeStatus()
  const services = usePoll('/api/system/services')

  const v = overview.data?.version
  const cpu = overview.data?.cpu
  const hap = latest?.hap
  const uptime = latest?.bootTime ? Date.now() / 1000 - latest.bootTime : undefined
  const uc = check.data

  return (
    <>
      <PageHeader title="Overview" subtitle="Live state of this node" />
      <div className="grid grid-4" style={{ marginBottom: '1rem' }}>
        <Stat
          label="CPU"
          value={percent(latest?.cpu)}
          sub={latest ? `${cpu ? `${cpu.count} CPU${cpu.count === 1 ? '' : 's'} · ` : ''}load ${num(latest.load1, 2)} · ${num(latest.load5, 2)} · ${num(latest.load15, 2)}` : ''}
        />
        <Stat label="Memory" value={percent(latest?.memPct)} sub={latest?.memTotal ? `${bytes(latest.memUsed)} of ${bytes(latest.memTotal)}` : ''} />
        <Stat
          label="HAProxy connections"
          value={hap?.up ? num(hap.conns) : '–'}
          sub={hap?.up ? `${hap.reqRate != null ? `${num(hap.reqRate, 1)} req/s · ` : ''}max ${compact(hap.maxConns)}` : hap ? <span title={hap.error}>not answering on its stats socket</span> : ''}
          tone={hap && !hap.up ? 'danger' : undefined}
        />
        <Stat label="Uptime" value={duration(uptime)} sub={latest?.bootTime ? `since ${dateTime(latest.bootTime * 1000)}` : ''} />
      </div>

      <div className="grid grid-2" style={{ marginBottom: '1rem' }}>
        <Card title="CPU & memory" icon={Cpu}>
          <Chart
            points={points}
            windowMs={WINDOW}
            yMax={100}
            format={(x) => `${x.toFixed(0)}%`}
            series={[
              { label: 'CPU', color: 'var(--chart-1)', get: (p) => p.cpu },
              { label: 'Memory', color: 'var(--chart-2)', get: (p) => p.memPct },
            ]}
          />
        </Card>
        <Card title="HAProxy traffic" icon={Shuffle}>
          <Chart
            points={points}
            windowMs={WINDOW}
            format={(x) => compact(x)}
            series={[
              { label: 'Requests/s', color: 'var(--chart-1)', get: (p) => p.hap?.reqRate },
              { label: 'Connections', color: 'var(--chart-3)', get: (p) => p.hap?.conns },
            ]}
          />
        </Card>
        <Card title="Network (all interfaces)" icon={Activity}>
          <Chart
            points={points}
            windowMs={WINDOW}
            format={(x) => rate(x)}
            binary
            series={[
              { label: 'Received', color: 'var(--chart-2)', get: (p) => netTotal(p, 'rx') },
              { label: 'Sent', color: 'var(--chart-4)', get: (p) => netTotal(p, 'tx') },
            ]}
          />
        </Card>
        <Card title="Node" icon={Gauge}>
          <dl className="kv">
            <dt>Hostname</dt>
            <dd className="mono">{overview.data?.hostname || '…'}</dd>
            <dt>Janus</dt>
            <dd>
              <span className="mono">{v?.version || '…'}</span>
            </dd>
            <dt>Boot slot</dt>
            <dd>{v?.active_slot ? <Badge tone="info">slot {v.active_slot}</Badge> : <span className="muted">not an A/B boot</span>}</dd>
            <dt>Kernel</dt>
            <dd>
              <span className="mono">{v?.kernel_version || '…'}</span>
              {v?.kernel?.variant && <span className="muted small"> · {variantNote(v.kernel, 'track')}</span>}
            </dd>
            <dt>CPU</dt>
            <dd>
              {cpu ? (
                <>
                  <div>{cpuModels(cpu)}</div>
                  <div className="muted small">{cpuShape(cpu)}</div>
                </>
              ) : (
                <span className="muted">{overview.data ? '–' : '…'}</span>
              )}
            </dd>
            <dt>Architecture</dt>
            <dd className="mono">{v ? v.arch || '–' : '…'}</dd>
            <dt>Go</dt>
            <dd className="mono">{v?.go_version || '…'}</dd>
            <dt>HAProxy</dt>
            <dd>
              <span className="mono">{v?.haproxy?.version || hap?.version || '–'}</span>
              {v?.haproxy?.variant && <span className="muted small"> · {variantNote(v.haproxy, 'branch')}</span>}
            </dd>
            <dt>Memory</dt>
            <dd>
              <Meter value={latest?.memUsed || 0} max={latest?.memTotal || 1} />
            </dd>
          </dl>
        </Card>
      </div>

      <div className="grid grid-3">
        <Card title="Services" icon={Server} actions={<a href="#/system/services">Manage</a>}>
          <div className="stack" style={{ gap: '0.5rem' }}>
            {(services.data?.services || []).map((s) => (
              <div key={s.id} className="spread">
                <span className="mono">{s.id}</span>
                <span className="row">
                  <Badge tone={stateTone(s.state)} dot>
                    {s.state}
                  </Badge>
                  <Badge tone={stateTone(s.health)}>{s.health}</Badge>
                </span>
              </div>
            ))}
          </div>
        </Card>
        <Card title="Release" icon={Archive} actions={<a href="#/system/update">Update</a>}>
          {check.error ? (
            <div className="muted small">Update check unavailable: {String(check.error.message)}</div>
          ) : (
            <div className="stack" style={{ gap: '0.5rem' }}>
              <div className="spread">
                <span className="muted">Running</span>
                <span className="mono">{v?.version || '…'}</span>
              </div>
              <div className="spread">
                <span className="muted">Latest</span>
                <span className="mono">{uc ? uc.latest || '–' : '…'}</span>
              </div>
              {uc?.extensions?.length > 0 && (
                <div className="spread">
                  <span className="muted">Extensions</span>
                  <span className="small">{uc.extensions.join(', ')}</span>
                </div>
              )}
              <div>
                <UpdateBadge uc={uc} />
              </div>
            </div>
          )}
        </Card>
        <Card title="Recent events" icon={Activity} actions={<a href="#/events">All events</a>}>
          <RecentEvents />
        </Card>
      </div>
    </>
  )
}
