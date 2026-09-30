import { ScrollText } from 'lucide-react'
import { useState } from 'react'
import LogViewer from '../components/LogViewer.jsx'
import { Card, PageHeader, Tabs } from '../../shared/ui.jsx'

const SERVICES = [
  { id: 'haproxy', label: 'HAProxy' },
  { id: 'janusd', label: 'janusd' },
]
const TAILS = [
  { n: 200, label: 'last 200 lines' },
  { n: 1000, label: 'last 1000 lines' },
  { n: 0, label: 'everything kept' },
]

export default function Logs() {
  const [service, setService] = useState('haproxy')
  const [tail, setTail] = useState(200)
  return (
    <>
      <PageHeader
        title="Service logs"
        subtitle="Live output of the node's managed services (the node keeps the last 5000 lines of each, in memory)"
        actions={
          <select value={tail} onChange={(e) => setTail(Number(e.target.value))}>
            {TAILS.map((t) => (
              <option key={t.n} value={t.n}>
                Start from {t.label}
              </option>
            ))}
          </select>
        }
      />
      <Tabs tabs={SERVICES} active={service} onChange={setService} />
      <Card title={service === 'haproxy' ? 'HAProxy output (stdout/stderr)' : 'janusd log'} icon={ScrollText}>
        <LogViewer key={`${service}-${tail}`} url={`/api/stream/logs?id=${service}&tail=${tail}`} name={service} />
      </Card>
    </>
  )
}
