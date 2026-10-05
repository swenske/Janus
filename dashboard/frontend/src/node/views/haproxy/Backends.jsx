import { Server } from 'lucide-react'
import { postJSON } from '../../api.js'
import { Badge, Card, Empty, ErrorBox, Loading, stateTone, useAction } from '../../../shared/ui.jsx'
import { usePoll } from '../../hooks.jsx'
import { useMay } from '../../may.js'

const ACTIONS = [
  { state: 'ready', label: 'Ready', title: 'Take traffic normally' },
  { state: 'drain', label: 'Drain', title: 'Finish existing connections, take no new ones' },
  { state: 'maint', label: 'Maint', title: 'Maintenance: no traffic at all' },
]

export default function Backends() {
  const { data, error, loading, reload } = usePoll('/api/haproxy/backends')
  const [busy, run] = useAction()
  const canSet = useMay()('HAProxyService/ServerSetState')
  const set = (backend, server, state) =>
    run(() => postJSON('/api/haproxy/backends/state', { backend, server, state }), `${backend}/${server} → ${state}`).then(reload)

  if (loading && !data) return <Loading />
  const backends = data?.backends || []
  return (
    <div className="stack">
      <ErrorBox error={error} />
      {!backends.length && !error && <Empty>No backend in the running configuration.</Empty>}
      <div className="grid grid-2">
        {backends.map((b) => (
          <Card key={b.name} title={b.name} icon={Server} actions={<span className="muted small">{(b.servers || []).length} servers</span>}>
            {(b.servers || []).length === 0 ? (
              <div className="muted">No servers.</div>
            ) : (
              <div className="table-wrap">
                <table>
                  <thead>
                    <tr>
                      <th>Server</th>
                      <th>Address</th>
                      <th>State</th>
                      {canSet && <th />}
                    </tr>
                  </thead>
                  <tbody>
                    {b.servers.map((s) => (
                      <tr key={s.name}>
                        <td className="mono">{s.name}</td>
                        <td className="mono small">{s.address}</td>
                        <td>
                          <Badge tone={stateTone(s.state)} dot>
                            {s.state}
                          </Badge>
                        </td>
                        {canSet && (
                          <td>
                            <div className="row" style={{ justifyContent: 'flex-end', flexWrap: 'nowrap' }}>
                              {ACTIONS.map((a) => (
                                <button key={a.state} className="small" title={a.title} disabled={busy} onClick={() => set(b.name, s.name, a.state)}>
                                  {a.label}
                                </button>
                              ))}
                            </div>
                          </td>
                        )}
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </Card>
        ))}
      </div>
    </div>
  )
}
