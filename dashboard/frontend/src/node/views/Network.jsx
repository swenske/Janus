import { Cable, Network as NetworkIcon } from 'lucide-react'
import { useMemo, useState } from 'react'
import DataTable from '../components/DataTable.jsx'
import { Badge, Card, ErrorBox, PageHeader, stateTone } from '../../shared/ui.jsx'
import { bytes, num, rate } from '../format.js'
import { useMetrics, usePoll } from '../hooks.jsx'

const STATE_FILTERS = ['all', 'LISTEN', 'ESTABLISHED', 'TIME_WAIT', 'other']

export default function Network() {
  const { latest } = useMetrics()
  const devices = usePoll('/api/system/netdev')
  const sockets = usePoll('/api/system/netstat')
  const [stateFilter, setStateFilter] = useState('all')
  const [proto, setProto] = useState('all')

  const ifRows = (devices.data?.devices || []).map((d) => ({
    ...d,
    __key: d.name,
    rxRate: latest?.net?.[d.name]?.rx,
    txRate: latest?.net?.[d.name]?.tx,
  }))

  const all = sockets.data?.connections || []
  const counts = useMemo(() => {
    const c = {}
    for (const s of all) c[s.state] = (c[s.state] || 0) + 1
    return c
  }, [all])
  const sockRows = all
    .filter((s) => proto === 'all' || s.protocol.startsWith(proto))
    .filter((s) => {
      if (stateFilter === 'all') return true
      if (stateFilter === 'other') return !['LISTEN', 'ESTABLISHED', 'TIME_WAIT'].includes(s.state)
      return s.state === stateFilter
    })
    .map((s, i) => ({ ...s, __key: i, local_port: Number((s.local_address || '').split(':').pop()) }))

  return (
    <>
      <PageHeader title="Network" subtitle="Interfaces and sockets" />
      <div className="stack">
        <Card title="Interfaces" icon={NetworkIcon}>
          <ErrorBox error={devices.error} />
          <DataTable
            filterable={false}
            rows={ifRows}
            columns={[
              { key: 'name', label: 'Interface', render: (r) => <span className="mono">{r.name}</span> },
              { key: 'rxRate', label: 'Receiving', num: true, render: (r) => rate(r.rxRate) },
              { key: 'txRate', label: 'Sending', num: true, render: (r) => rate(r.txRate) },
              { key: 'rx_bytes', label: 'Received', num: true, render: (r) => bytes(r.rx_bytes || 0) },
              { key: 'tx_bytes', label: 'Sent', num: true, render: (r) => bytes(r.tx_bytes || 0) },
              { key: 'rx_errors', label: 'RX errors', num: true, render: (r) => <span style={r.rx_errors ? { color: 'var(--danger)' } : undefined}>{num(r.rx_errors || 0)}</span> },
              { key: 'tx_errors', label: 'TX errors', num: true, render: (r) => <span style={r.tx_errors ? { color: 'var(--danger)' } : undefined}>{num(r.tx_errors || 0)}</span> },
            ]}
          />
        </Card>
        <Card
          title="Sockets"
          icon={Cable}
          actions={
            <>
              <select value={proto} onChange={(e) => setProto(e.target.value)}>
                <option value="all">TCP + UDP</option>
                <option value="tcp">TCP</option>
                <option value="udp">UDP</option>
              </select>
              <div className="chips">
                {STATE_FILTERS.map((f) => (
                  <button key={f} className={`small chip ${stateFilter === f ? 'active' : ''}`} onClick={() => setStateFilter(f)}>
                    {f === 'all' ? `All (${all.length})` : f === 'other' ? 'Other' : `${f} (${counts[f] || 0})`}
                  </button>
                ))}
              </div>
            </>
          }
        >
          <ErrorBox error={sockets.error} />
          <DataTable
            rows={sockRows}
            maxHeight="60vh"
            initialSort={{ key: 'local_port', dir: 1 }}
            columns={[
              { key: 'protocol', label: 'Proto', width: '5rem' },
              { key: 'local_address', label: 'Local', render: (r) => <span className="mono">{r.local_address}</span> },
              { key: 'remote_address', label: 'Remote', render: (r) => <span className="mono">{r.remote_address}</span> },
              { key: 'state', label: 'State', render: (r) => <Badge tone={stateTone(r.state)}>{r.state}</Badge> },
              { key: 'local_port', label: 'Port', num: true },
            ]}
          />
        </Card>
      </div>
    </>
  )
}
