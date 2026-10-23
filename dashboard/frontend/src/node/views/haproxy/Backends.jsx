import { Server } from 'lucide-react'
import DataTable from '../../components/DataTable.jsx'
import ServerStateButtons from '../../components/ServerStateButtons.jsx'
import { Badge, Card, Empty, ErrorBox, Loading, stateTone } from '../../../shared/ui.jsx'
import ViewToggle from '../../../shared/ViewToggle.jsx'
import { useStoredView } from '../../../shared/storedView.js'
import { usePoll } from '../../hooks.jsx'
import { useMay } from '../../may.js'

// A tile per backend, a row per server - its name over its address, its
// state, the buttons setting it -, the buttons going under them when the
// tile is too narrow (node.css .servers): however many backends there
// are, and however long their servers' names, nothing is wider than its
// tile. Or, as a list (remembered per browser), a row per server of
// every backend, filtered and sorted.
export default function Backends() {
  const { data, error, loading, reload } = usePoll('/api/haproxy/backends')
  const canSet = useMay()('HAProxyService/ServerSetState')
  const [view, setView] = useStoredView('janus-backends-view')

  if (loading && !data) return <Loading />
  const backends = data?.backends || []
  return (
    <div className="stack">
      <ErrorBox error={error} />
      {!backends.length && !error && <Empty>No backend in the running configuration.</Empty>}
      {backends.length > 0 && (
        <div className="row">
          <span className="grow" />
          <ViewToggle value={view} onChange={setView} label="Show the backends as" />
        </div>
      )}
      {view === 'list' && backends.length > 0 ? (
        <Card>
          <BackendList backends={backends} canSet={canSet} reload={reload} />
        </Card>
      ) : (
        <div className="grid grid-2">
          {backends.map((b) => {
            const servers = b.servers || []
            return (
              <Card key={b.name} className="backend data-title" title={b.name} icon={Server} actions={<span className="muted small">{servers.length === 1 ? '1 server' : `${servers.length} servers`}</span>}>
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
      )}
    </div>
  )
}

// BackendList: a row per server, a backend without any on a row of its
// own.
function BackendList({ backends, canSet, reload }) {
  const rows = backends.flatMap((b) => {
    const servers = b.servers || []
    if (!servers.length) return [{ __key: b.name, backend: b.name, server: null }]
    return servers.map((s) => ({ __key: `${b.name}/${s.name}`, backend: b.name, server: s.name, address: s.address, state: s.state }))
  })
  const columns = [
    { key: 'backend', label: 'Backend', render: (r) => <span className="mono">{r.backend}</span> },
    { key: 'server', label: 'Server', render: (r) => (r.server ? <span className="mono">{r.server}</span> : <span className="muted">No servers</span>) },
    { key: 'address', label: 'Address', render: (r) => <span className="mono small">{r.address}</span> },
    {
      key: 'state',
      label: 'State',
      render: (r) =>
        r.state && (
          <Badge tone={stateTone(r.state)} dot>
            {r.state}
          </Badge>
        ),
    },
  ]
  if (canSet) columns.push({ key: 'actions', label: '', get: () => null, render: (r) => r.server && <ServerStateButtons backend={r.backend} server={r.server} state={r.state} onDone={reload} /> })
  return <DataTable rows={rows} columns={columns} initialSort={{ key: 'backend', dir: 1 }} empty="No server matches." />
}
