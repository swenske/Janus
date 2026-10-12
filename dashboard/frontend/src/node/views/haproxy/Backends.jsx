import { Server } from 'lucide-react'
import ServerStateButtons from '../../components/ServerStateButtons.jsx'
import { Badge, Card, Empty, ErrorBox, Loading, stateTone } from '../../../shared/ui.jsx'
import { usePoll } from '../../hooks.jsx'
import { useMay } from '../../may.js'

export default function Backends() {
  const { data, error, loading, reload } = usePoll('/api/haproxy/backends')
  const canSet = useMay()('HAProxyService/ServerSetState')

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
                            <ServerStateButtons backend={b.name} server={s.name} onDone={reload} />
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
