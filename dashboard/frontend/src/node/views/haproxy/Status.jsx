import { Table2 } from 'lucide-react'
import { useState } from 'react'
import DataTable from '../../components/DataTable.jsx'
import { Badge, Card, ErrorBox, Stat, stateTone } from '../../../shared/ui.jsx'
import { bytes, compact, duration, num } from '../../format.js'
import { useMetrics, usePoll } from '../../hooks.jsx'

const TYPES = { 0: 'frontend', 1: 'backend', 2: 'server', 3: 'listener' }

export default function Status() {
  const { latest } = useMetrics()
  const stats = usePoll('/api/haproxy/stats')
  const [type, setType] = useState('all')
  const hap = latest?.hap

  const cols = stats.data?.columns || []
  const idx = Object.fromEntries(cols.map((c, i) => [c, i]))
  const val = (r, c) => (idx[c] != null ? r[idx[c]] : '')
  const n = (r, c) => Number(val(r, c)) || 0
  const rows = (stats.data?.rows || []).map((r, i) => ({
    __key: i,
    proxy: val(r, 'pxname'),
    name: val(r, 'svname'),
    type: TYPES[val(r, 'type')] || val(r, 'type'),
    status: val(r, 'status'),
    cur: n(r, 'scur'),
    max: n(r, 'smax'),
    limit: val(r, 'slim'),
    total: n(r, 'stot'),
    rate: n(r, 'rate'),
    bin: n(r, 'bin'),
    bout: n(r, 'bout'),
    h2xx: n(r, 'hrsp_2xx'),
    h4xx: n(r, 'hrsp_4xx'),
    h5xx: n(r, 'hrsp_5xx'),
    errors: n(r, 'ereq') + n(r, 'econ') + n(r, 'eresp'),
    check: val(r, 'check_status'),
    addr: val(r, 'addr'),
  }))
  const shown = type === 'all' ? rows : rows.filter((r) => r.type === type)

  return (
    <div className="stack">
      <div className="grid grid-4">
        <Stat label="Connections" value={hap?.up ? num(hap.conns) : '–'} sub={hap?.up ? `max ${compact(hap.maxConns)}` : hap ? <span title={hap.error}>not answering on its stats socket</span> : ''} tone={hap && !hap.up ? 'danger' : undefined} />
        <Stat label="Requests/s" value={hap?.reqRate != null ? num(hap.reqRate, 1) : '–'} sub={hap?.up ? `${compact(hap.cumReq)} since start` : ''} />
        <Stat label="New connections/s" value={hap?.up ? num(hap.connRate) : '–'} sub={hap?.up ? `${num(hap.sessRate)} sessions/s` : ''} />
        <Stat label="Uptime" value={hap?.up ? duration(hap.uptime) : '–'} sub={hap?.up ? `${num(100 - hap.idle)}% busy · ${hap.version}` : ''} />
      </div>
      <Card
        title="Proxies"
        icon={Table2}
        actions={
          <div className="chips">
            {['all', 'frontend', 'backend', 'server'].map((t) => (
              <button key={t} className={`small chip ${type === t ? 'active' : ''}`} onClick={() => setType(t)}>
                {t === 'all' ? 'All' : `${t}s`}
              </button>
            ))}
          </div>
        }
      >
        <ErrorBox error={stats.error} />
        <DataTable
          rows={shown}
          columns={[
            { key: 'proxy', label: 'Proxy', render: (r) => <strong>{r.proxy}</strong> },
            { key: 'name', label: 'Name', render: (r) => <span className="mono">{r.name}</span> },
            { key: 'type', label: 'Type', render: (r) => <Badge>{r.type}</Badge> },
            { key: 'status', label: 'Status', render: (r) => <Badge tone={stateTone(r.status)}>{r.status || '–'}</Badge> },
            { key: 'cur', label: 'Sessions', num: true, render: (r) => `${num(r.cur)} / ${num(r.max)}` },
            { key: 'rate', label: 'Rate', num: true, render: (r) => `${num(r.rate)}/s` },
            { key: 'total', label: 'Total', num: true, render: (r) => compact(r.total) },
            { key: 'bin', label: 'In', num: true, render: (r) => bytes(r.bin) },
            { key: 'bout', label: 'Out', num: true, render: (r) => bytes(r.bout) },
            { key: 'h2xx', label: '2xx', num: true, render: (r) => compact(r.h2xx) },
            { key: 'h4xx', label: '4xx', num: true, render: (r) => <span style={r.h4xx ? { color: 'var(--warn)' } : undefined}>{compact(r.h4xx)}</span> },
            { key: 'h5xx', label: '5xx', num: true, render: (r) => <span style={r.h5xx ? { color: 'var(--danger)' } : undefined}>{compact(r.h5xx)}</span> },
            { key: 'errors', label: 'Errors', num: true, render: (r) => <span style={r.errors ? { color: 'var(--danger)' } : undefined}>{num(r.errors)}</span> },
            { key: 'check', label: 'Check', render: (r) => <span className="mono small">{r.check}</span> },
            { key: 'addr', label: 'Address', render: (r) => <span className="mono small">{r.addr}</span> },
          ]}
        />
      </Card>
    </div>
  )
}
