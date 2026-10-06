import { ExternalLink, RadioTower } from 'lucide-react'
import { useState } from 'react'
import { postJSON } from '../api.js'
import { Badge, Card, ErrorBox, useAction } from '../../shared/ui.jsx'
import { usePoll } from '../hooks.jsx'
import { useMay } from '../may.js'

const DEFAULT_PORT = 10056
// Its page on the docs site, by the file's permalink.
const DOCS = 'https://janus.sw-servers.net/docs/metrics.md'

// ExporterCard is the node's own Prometheus exporter: Janus's metrics
// (certificates, boot slot, HAProxy as janusd runs it, extensions, time,
// SELinux), plain HTTP on its own port.
export default function ExporterCard() {
  const { data, error, reload } = usePoll('/api/system/metrics-config', { every: 0 })
  const node = usePoll('/api/node', { every: 0 })
  // The form's edits; null until something is changed, the node's own
  // configuration showing until then.
  const [draft, setDraft] = useState(null)
  const [busy, run] = useAction()
  const canSet = useMay()('SystemService/MetricsConfigSet')

  const cfg = data?.config
  const enabled = draft ? draft.enabled : !!cfg?.enabled
  const port = draft ? draft.port : String(cfg?.port || DEFAULT_PORT)
  const edit = (change) => setDraft({ enabled, port, ...change })

  const host = (node.data?.address || '').replace(/:\d+$/, '').replace(/^\[|\]$/g, '') || '<node>'
  const shownPort = cfg?.port || DEFAULT_PORT
  const target = `${host.includes(':') ? `[${host}]` : host}:${shownPort}`
  const changed = cfg && (enabled !== !!cfg.enabled || Number(port || DEFAULT_PORT) !== (cfg.port || DEFAULT_PORT))
  const portValid = /^\d+$/.test(port || String(DEFAULT_PORT)) && Number(port || DEFAULT_PORT) >= 1 && Number(port || DEFAULT_PORT) <= 65535

  const save = () =>
    run(async () => {
      await postJSON('/api/system/metrics-config', { enabled, port: Number(port || DEFAULT_PORT) })
      setDraft(null)
      reload()
    }, enabled ? `Exporter serving on port ${port || DEFAULT_PORT}` : 'Exporter disabled')

  let badge = <Badge>…</Badge>
  if (cfg && !cfg.enabled) badge = <Badge>Disabled</Badge>
  else if (data?.listening) badge = <Badge tone="ok" dot>Serving on :{shownPort}</Badge>
  else if (cfg) badge = <Badge tone="danger">Not listening</Badge>

  return (
    <Card
      title="Prometheus exporter"
      icon={RadioTower}
      actions={
        <a href={DOCS} target="_blank" rel="noreferrer">
          Metrics reference <ExternalLink size={12} />
        </a>
      }
    >
      {error ? (
        <ErrorBox error={error} />
      ) : (
        <div className="stack">
          <div className="spread">
            <span className="muted small">
              Janus's own metrics - certificate expiry, boot slot and pending upgrades, HAProxy as janusd runs it, extension services, time sync, SELinux - over plain HTTP. HAProxy's and node_exporter's metrics
              aren't repeated.
            </span>
            {badge}
          </div>
          {data?.error && <div className="small">{data.error}</div>}
          <fieldset className="plain row" style={{ alignItems: 'flex-end', flexWrap: 'wrap' }} disabled={!canSet}>
            <label className="check">
              <input type="checkbox" checked={enabled} onChange={(e) => edit({ enabled: e.target.checked })} /> Enabled
            </label>
            <label className="field" style={{ width: '9rem' }}>
              <span>Port</span>
              <input className="mono" inputMode="numeric" value={port} placeholder={String(DEFAULT_PORT)} onChange={(e) => edit({ port: e.target.value.trim() })} disabled={!enabled} />
            </label>
            {canSet && (
              <button className="primary" disabled={busy || !changed || !portValid} onClick={save}>
                {busy ? 'Saving…' : 'Save'}
              </button>
            )}
          </fieldset>
          {cfg?.enabled && (
            <>
              <div className="small">
                Scrape <span className="mono">http://{target}/metrics</span> - the node's firewall, if any, must let Prometheus reach port {shownPort}.
              </div>
              <pre className="code">{`scrape_configs:
  - job_name: janus
    static_configs:
      - targets: ['${target}']`}</pre>
            </>
          )}
        </div>
      )}
    </Card>
  )
}
