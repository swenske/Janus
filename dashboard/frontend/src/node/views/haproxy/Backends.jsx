import { Server } from 'lucide-react'
import ServerStateButtons from '../../components/ServerStateButtons.jsx'
import { Badge, Card, Empty, ErrorBox, Loading, stateTone } from '../../../shared/ui.jsx'
import { usePoll } from '../../hooks.jsx'
import { useMay } from '../../may.js'

// A tile per backend, a row per server - its name over its address, its
// state, the buttons setting it -, the buttons going under them when the
// tile is too narrow (node.css .servers): however many backends there
// are, and however long their servers' names, nothing is wider than its
// tile.
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
        {backends.map((b) => {
          const servers = b.servers || []
          return (
            <Card key={b.name} className="backend" title={b.name} icon={Server} actions={<span className="muted small">{servers.length === 1 ? '1 server' : `${servers.length} servers`}</span>}>
              {servers.length === 0 ? (
                <div className="muted">No servers.</div>
              ) : (
                <ul className="servers" aria-label={`${b.name}: servers`}>
                  {servers.map((s) => (
                    <li key={s.name}>
                      <div className="server-id">
                        <span className="mono">{s.name}</span>
                        <span className="mono small muted">{s.address}</span>
                      </div>
                      <Badge tone={stateTone(s.state)} dot>
                        {s.state}
                      </Badge>
                      {canSet && <ServerStateButtons backend={b.name} server={s.name} state={s.state} onDone={reload} />}
                    </li>
                  ))}
                </ul>
              )}
            </Card>
          )
        })}
      </div>
    </div>
  )
}
