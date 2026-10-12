import { postJSON } from '../api.js'
import { useAction } from '../../shared/ui.jsx'

const ACTIONS = [
  { state: 'ready', label: 'Ready', title: 'Take traffic normally' },
  { state: 'drain', label: 'Drain', title: 'Finish existing connections, take no new ones' },
  { state: 'maint', label: 'Maint', title: 'Maintenance: no traffic at all' },
]

// ServerStateButtons sets a server's administrative state (Ready, Drain,
// Maint) - HAProxy › Backends and the Traffic flow. Shown only to an
// account that may (HAProxyService/ServerSetState).
export default function ServerStateButtons({ backend, server, onDone }) {
  const [busy, run] = useAction()
  const set = (state) => run(() => postJSON('/api/haproxy/backends/state', { backend, server, state }), `${backend}/${server} → ${state}`).then(onDone)
  return (
    <div className="row" style={{ justifyContent: 'flex-end', flexWrap: 'nowrap' }}>
      {ACTIONS.map((a) => (
        <button key={a.state} className="small" title={a.title} disabled={busy} onClick={() => set(a.state)}>
          {a.label}
        </button>
      ))}
    </div>
  )
}
