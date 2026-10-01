import { FileCode } from 'lucide-react'
import ExporterCard from './Exporter.jsx'
import { Card, PageHeader } from '../../shared/ui.jsx'
import { usePoll } from '../hooks.jsx'

// JanusExporter is the exporter janusd itself runs - Janus's own metrics,
// always there, unlike the extensions' apps.
export default function JanusExporter() {
  const cfg = usePoll('/api/system/metrics-config', { every: 0 })
  const node = usePoll('/api/node', { every: 0 })
  const host = (node.data?.address || '').replace(/:\d+$/, '').replace(/^\[|\]$/g, '') || '<node>'
  const port = cfg.data?.config?.port || 10056
  const target = `${host.includes(':') ? `[${host}]` : host}:${port}`
  return (
    <>
      <PageHeader title="Janus exporter" subtitle="prometheus-janus-exporter · Janus's own metrics for Prometheus, built into janusd" />
      <div className="stack">
        <ExporterCard />
        <Card title="Prometheus configuration" icon={FileCode}>
          <pre className="mono small" style={{ margin: 0 }}>{`scrape_configs:
  - job_name: janus
    static_configs:
      - targets: ['${target}']`}</pre>
          <p className="muted small" style={{ marginBottom: 0 }}>
            One target per node. With node_exporter (Apps › Node exporter) and HAProxy's own exporter, a node has three: Janus, the host, HAProxy.
          </p>
        </Card>
      </div>
    </>
  )
}
