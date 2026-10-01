import { ChartLine, ExternalLink, ScrollText, SlidersHorizontal } from 'lucide-react'
import { useState } from 'react'
import { postJSON } from '../api.js'
import { Badge, Card, ErrorBox, Loading, PageHeader, useAction } from '../../shared/ui.jsx'
import { usePoll } from '../hooks.jsx'

const DEFAULT_PORT = 9100
const DOCS = 'https://github.com/swenske/Janus/blob/main/docs/metrics.md'

// NodeExporter is the prometheus-node-exporter extension: node_exporter's
// state, and its settings - whether it runs, where it listens, which
// collectors (from the fixed list the node offers).
export default function NodeExporter() {
  const { data, error, reload } = usePoll('/api/system/node-exporter', { every: 0 })
  const node = usePoll('/api/node', { every: 0 })
  // The form's edits; null until something changes.
  const [draft, setDraft] = useState(null)
  const [busy, run] = useAction()

  if (error) {
    return (
      <>
        <PageHeader title="Node exporter" subtitle="prometheus-node-exporter · the node's host metrics for Prometheus" />
        {error.status === 501 ? (
          <div className="notice">
            This node's image is older than node_exporter's settings: it runs with the defaults - every address, port {DEFAULT_PORT}. Update the node (System › Update) to change them.
          </div>
        ) : (
          <ErrorBox error={error} />
        )}
      </>
    )
  }
  if (!data) return <Loading />

  const cfg = data.config || {}
  const saved = { enabled: !!cfg.enabled, address: cfg.address || '', port: String(cfg.port || DEFAULT_PORT), collectors: cfg.collectors || [] }
  const form = draft || saved
  const edit = (change) => setDraft({ ...form, ...change })
  const toggle = (name, on) => edit({ collectors: on ? [...form.collectors, name].sort() : form.collectors.filter((c) => c !== name) })
  const available = data.available_collectors || []
  const defaults = available.filter((c) => c.default).map((c) => c.name)

  const portValid = /^\d+$/.test(form.port) && Number(form.port) >= 1 && Number(form.port) <= 65535
  const addressValid = form.address === '' || /^[0-9a-fA-F.:]+$/.test(form.address)
  const changed = JSON.stringify(form) !== JSON.stringify(saved)
  const save = () =>
    run(async () => {
      await postJSON('/api/system/node-exporter', { enabled: form.enabled, address: form.address.trim(), port: Number(form.port), collectors: form.collectors })
      setDraft(null)
      reload()
    }, form.enabled ? 'node_exporter restarted with the new settings' : 'node_exporter stopped')

  const host = cfg.address || (node.data?.address || '').replace(/:\d+$/, '').replace(/^\[|\]$/g, '') || '<node>'
  const url = `http://${host.includes(':') ? `[${host}]` : host}:${cfg.port || DEFAULT_PORT}/metrics`
  let badge = <Badge tone="warn">{data.state || 'unknown'}</Badge>
  if (data.state === 'running') badge = <Badge tone="ok" dot>Running</Badge>
  else if (data.state === 'disabled') badge = <Badge>Disabled</Badge>

  const group = (title, names) => (
    <div className="stack" style={{ gap: '0.35rem' }}>
      <div className="small muted">{title}</div>
      <div className="collector-grid">
        {available
          .filter((c) => names.includes(c.name))
          .map((c) => (
            <label key={c.name} className="check collector">
              <input type="checkbox" checked={form.collectors.includes(c.name)} onChange={(e) => toggle(c.name, e.target.checked)} disabled={!form.enabled} />
              <span>
                <span className="mono">{c.name}</span> <span className="muted small">{c.description}</span>
              </span>
            </label>
          ))}
      </div>
    </div>
  )

  return (
    <>
      <PageHeader
        title="Node exporter"
        subtitle="prometheus-node-exporter · the node's host metrics for Prometheus"
        actions={
          <a href={DOCS} target="_blank" rel="noreferrer">
            Metrics reference <ExternalLink size={12} />
          </a>
        }
      />
      <div className="stack">
        <Card
          title="node_exporter"
          icon={ChartLine}
          actions={
            <a href="#/logs?service=prometheus-node-exporter" className="small">
              <ScrollText size={12} /> Logs
            </a>
          }
        >
          <div className="spread">
            {cfg.enabled ? (
              <span>
                Prometheus scrapes <span className="mono">{url}</span>
              </span>
            ) : (
              <span className="muted">Not running: turned off in its settings.</span>
            )}
            {badge}
          </div>
          {data.error && <div className="small" style={{ marginTop: '0.5rem' }}>{data.error}</div>}
          <p className="muted small" style={{ marginBottom: 0 }}>
            Like a stock node_exporter, it has no authentication: restrict who reaches the port - with the firewall app when the node has it.
          </p>
        </Card>
        <Card title="Settings" icon={SlidersHorizontal}>
          <div className="stack">
            <label className="check">
              <input type="checkbox" checked={form.enabled} onChange={(e) => edit({ enabled: e.target.checked })} /> Run node_exporter
            </label>
            <div className="row" style={{ flexWrap: 'wrap', alignItems: 'flex-end' }}>
              <label className="field" style={{ width: '16rem' }}>
                <span>Listen address</span>
                <input className="mono" value={form.address} placeholder="every address" onChange={(e) => edit({ address: e.target.value.trim() })} disabled={!form.enabled} />
              </label>
              <label className="field" style={{ width: '9rem' }}>
                <span>Port</span>
                <input className="mono" inputMode="numeric" value={form.port} onChange={(e) => edit({ port: e.target.value.trim() })} disabled={!form.enabled} />
              </label>
            </div>
            {!addressValid && <div className="small">The address must be one of the node's IP addresses, or empty for every address.</div>}
            {group('Collected by default', defaults)}
            {group('More collectors', available.filter((c) => !c.default).map((c) => c.name))}
            <div className="row">
              <button className="primary" disabled={busy || !changed || !portValid || !addressValid || (form.enabled && form.collectors.length === 0)} onClick={save}>
                {busy ? 'Saving…' : 'Save and restart'}
              </button>
              <button disabled={busy || JSON.stringify(form.collectors) === JSON.stringify(defaults)} onClick={() => edit({ collectors: defaults })}>
                Default collectors
              </button>
              {draft && (
                <button className="ghost" onClick={() => setDraft(null)}>
                  Cancel
                </button>
              )}
            </div>
            {data.is_default && !draft && <div className="muted small">The defaults: nothing was changed on this node.</div>}
          </div>
        </Card>
      </div>
    </>
  )
}
