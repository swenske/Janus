import { RefreshCw, ScrollText } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { call } from './call.js'
import { Badge, Card, ErrorBox } from './shared/ui.jsx'

// The audit (admin): every change made on this Controller and every
// sign-in, newest first - dashboard/backend/internal/audit.

function statusTone(code) {
  if (code >= 500) return 'danger'
  if (code >= 400) return 'warn'
  return 'ok'
}

export default function AuditPage() {
  const [entries, setEntries] = useState(null)
  const [user, setUser] = useState('')
  const [only, setOnly] = useState('')
  const [error, setError] = useState(null)
  const load = useCallback(() => {
    const q = new URLSearchParams({ limit: '500' })
    if (only) q.set('user', only)
    call(`/api/audit?${q}`).then((e) => {
      setEntries(e || [])
      setError(null)
    }, setError)
  }, [only])
  useEffect(load, [load])

  return (
    <div className="stack">
      <div className="spread">
        <h1>Audit</h1>
      </div>
      <p className="muted" style={{ margin: 0 }}>
        Every change made on this Controller - from its pages or with an API token - and every sign-in, with who made it. What the Controller does on a node, the node logs too,
        with the same name.
      </p>
      <Card
        title="Recent"
        icon={ScrollText}
        actions={
          <form
            className="row"
            onSubmit={(e) => {
              e.preventDefault()
              if (user.trim() === only) load()
              else setOnly(user.trim())
            }}
          >
            <input value={user} onChange={(e) => setUser(e.target.value.toLowerCase())} placeholder="account" aria-label="Only this account" style={{ width: '10rem' }} />
            <button className="ghost icon" type="submit" title="Refresh">
              <RefreshCw size={15} />
            </button>
          </form>
        }
      >
        <ErrorBox error={error} />
        {entries && entries.length === 0 && <div className="muted">Nothing yet.</div>}
        {entries && entries.length > 0 && (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>When</th>
                  <th>Who</th>
                  <th>What</th>
                  <th>Result</th>
                  <th>From</th>
                </tr>
              </thead>
              <tbody>
                {entries.map((e, i) => (
                  <tr key={i}>
                    <td style={{ whiteSpace: 'nowrap' }}>{new Date(e.time).toLocaleString()}</td>
                    <td>
                      {e.user ? <strong>{e.user}</strong> : <span className="muted">–</span>} {e.via && e.via !== 'session' && <span className="muted small mono">{e.via}</span>}
                    </td>
                    <td className="mono small">
                      {e.method} {e.path}
                    </td>
                    <td>
                      <Badge tone={statusTone(e.status)}>{e.status}</Badge>
                    </td>
                    <td className="mono small">{e.client}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </div>
  )
}
