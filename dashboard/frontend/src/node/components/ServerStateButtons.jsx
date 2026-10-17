import { postJSON } from '../api.js'
import { useAction } from '../../shared/ui.jsx'

const ACTIONS = [
  { state: 'ready', label: 'Ready', title: 'Take traffic normally' },
  { state: 'drain', label: 'Drain', title: 'Finish existing connections, take no new ones' },
  { state: 'maint', label: 'Maint', title: 'Maintenance: no traffic at all' },
]

// adminState is the administrative state a server's state says - drain
// or maint as HAProxy names them (BackendList's "maint", show stat's
// "MAINT (resolution)"), ready otherwise -, '' without one.
function adminState(state) {
  const s = String(state || '').toLowerCase()
  if (!s) return ''
  if (s.startsWith('maint')) return 'maint'
  if (s.startsWith('drain')) return 'drain'
  return 'ready'
}

// ServerStateButtons sets a server's administrative state (Ready, Drain,
// Maint) - HAProxy › Backends and the Traffic flow -, the one it has
// marked when its state is given. Shown only to an account that may
// (HAProxyService/ServerSetState).
export default function ServerStateButtons({ backend, server, state, onDone }) {
  const [busy, run] = useAction()
  const now = adminState(state)
  const set = (to) => run(() => postJSON('/api/haproxy/backends/state', { backend, server, state: to }), `${backend}/${server} → ${to}`).then(onDone)
  return (
    <div className="chips server-state" role="group" aria-label={`${server}: administrative state`}>
      {ACTIONS.map((a) => (
        <button
          key={a.state}
          className={`small chip ${now === a.state ? 'active' : ''}`}
          aria-pressed={now ? now === a.state : undefined}
          title={a.title}
          disabled={busy}
          onClick={() => set(a.state)}
        >
          {a.label}
        </button>
      ))}
    </div>
  )
}
