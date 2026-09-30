import { Cpu } from 'lucide-react'
import DataTable from '../components/DataTable.jsx'
import { Card, ErrorBox, Loading, PageHeader } from '../../shared/ui.jsx'
import { bytes, num } from '../format.js'
import { usePoll } from '../hooks.jsx'

export default function Processes() {
  const { data, error, loading } = usePoll('/api/system/processes')
  const rows = (data?.processes || []).map((p) => ({ ...p, __key: p.pid, cpu: p.cpu_percent || 0, mem: p.memory_bytes || 0 }))
  const kernel = rows.filter((r) => r.command?.startsWith('[')).length
  return (
    <>
      <PageHeader title="Processes" subtitle={data ? `${rows.length} processes, ${kernel} of them kernel threads` : ''} />
      <Card title="Process list" icon={Cpu}>
        <ErrorBox error={error} />
        {loading && !data ? (
          <Loading />
        ) : (
          <DataTable
            rows={rows}
            initialSort={{ key: 'cpu', dir: -1 }}
            columns={[
              { key: 'pid', label: 'PID', num: true, width: '5rem' },
              { key: 'command', label: 'Command', render: (r) => <span className="mono small">{r.command}</span> },
              { key: 'cpu', label: 'CPU (avg)', num: true, render: (r) => `${num(r.cpu, 1)}%` },
              { key: 'mem', label: 'Memory (RSS)', num: true, render: (r) => (r.mem ? bytes(r.mem) : '–') },
            ]}
          />
        )}
        <p className="muted small">CPU is the average over each process's lifetime, like ps. Live CPU usage is on the Metrics page.</p>
      </Card>
    </>
  )
}
