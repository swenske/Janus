import { ChartPie, RefreshCw, Send } from 'lucide-react'
import { useEffect, useState } from 'react'
import { call, postJSON } from './call.js'
import { fmtDateTime } from './shared/units.js'
import { Badge, Card, ErrorBox, useAction, useConfirm } from './shared/ui.jsx'
import { TELEMETRY_WHY } from './telemetry.js'

// Telemetry is the Settings card of the usage report (/api/telemetry,
// admins): on or off, its ID - made again on request, which asks the
// receiver to forget the old one's reports - and exactly what it sends.
export default function Telemetry() {
  const [view, setView] = useState(null)
  const [error, setError] = useState(null)
  const [busy, run] = useAction()
  const confirm = useConfirm()
  useEffect(() => {
    call('/api/telemetry').then(setView, setError)
  }, [])
  const act = (fn, done) =>
    run(async () => {
      setView(await fn())
    }, done)
  const toggle = (on) => act(() => postJSON('/api/telemetry', { enabled: on }, 'PUT'), on ? 'The usage report is on' : 'The usage report is off')
  const regenerate = async () => {
    const ok = await confirm({
      title: 'Make a new ID?',
      body: <p>Reports go under a new ID from now on, and the receiver is asked to delete those sent under the current one.</p>,
      action: 'New ID',
    })
    if (ok) act(() => postJSON('/api/telemetry/regenerate'), 'New ID made')
  }
  const send = () => act(() => postJSON('/api/telemetry/send'), 'Report sent')
  return (
    <Card title="Usage report" icon={ChartPie} actions={view?.enabled ? <Badge tone="ok">on</Badge> : <Badge>off</Badge>}>
      <p className="muted small" style={{ marginTop: 0 }}>
        {TELEMETRY_WHY} <a href="https://janus.sw-servers.net/docs/guide/telemetry/">What it holds, field by field</a>.
      </p>
      {error && <ErrorBox error={error} />}
      {view && (
        <div className="stack">
          {view.locked ? (
            <div className="notice">
              Off: this Controller&apos;s environment turns it off (<span className="mono">JANUS_CONTROLLER_TELEMETRY=off</span>).
            </div>
          ) : (
            <label className="check">
              <input type="checkbox" checked={view.enabled} disabled={busy} onChange={(e) => toggle(e.target.checked)} />
              <span>Send the usage report once a day</span>
            </label>
          )}
          {view.install_id && (
            <div className="row">
              <span className="muted small">ID</span>
              <span className="mono small">{view.install_id}</span>
              <button className="ghost small" disabled={busy} onClick={regenerate} title="Make a new ID">
                <RefreshCw size={13} /> New ID
              </button>
            </div>
          )}
          {view.enabled && (
            <div className="muted small">
              {view.last_sent ? `Last sent ${fmtDateTime(view.last_sent)}.` : 'Not sent yet.'}
              {view.next ? ` Next ${fmtDateTime(view.next)}.` : ''}
            </div>
          )}
          {view.enabled && view.last_error && <div className="notice warn small">The last try failed: {view.last_error}</div>}
          <details className="small">
            <summary>Exactly what is sent, to {view.url}</summary>
            <pre className="mono small">{JSON.stringify(view.report, null, 2)}</pre>
          </details>
          {view.enabled && (
            <div>
              <button className="small" disabled={busy} onClick={send}>
                <Send size={13} /> Send now
              </button>
            </div>
          )}
        </div>
      )}
    </Card>
  )
}
