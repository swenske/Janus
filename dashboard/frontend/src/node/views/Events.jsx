import { Activity, ChevronDown, ChevronRight, Search, Trash2 } from 'lucide-react'
import { useMemo, useState } from 'react'
import { Badge, Card, PageHeader, stateTone } from '../../shared/ui.jsx'
import { useSSE } from '../hooks.jsx'

const MAX = 2000

function category(type) {
  return type.split('.')[0]
}

function tone(e) {
  if (e.type.includes('rejected') || e.type.includes('failed') || e.payload?.reason === 'exited on its own') return 'danger'
  if (e.type.startsWith('system.') || e.type.includes('reverted') || e.type.startsWith('lifecycle.')) return 'warn'
  if (e.type.includes('applied') || e.type.includes('confirmed') || e.type.includes('started')) return 'ok'
  return 'info'
}

function EventRow({ e }) {
  const [open, setOpen] = useState(false)
  const hasPayload = e.payload && Object.keys(e.payload).length > 0
  const summary = hasPayload
    ? Object.entries(e.payload)
        .filter(([, v]) => typeof v !== 'object')
        .slice(0, 4)
        .map(([k, v]) => `${k}=${v}`)
        .join('  ')
    : ''
  return (
    <div className="event">
      <div className="row" style={{ flexWrap: 'nowrap', cursor: hasPayload ? 'pointer' : 'default' }} onClick={() => hasPayload && setOpen(!open)}>
        {hasPayload ? open ? <ChevronDown size={14} /> : <ChevronRight size={14} /> : <span style={{ width: 14 }} />}
        <span className="muted small mono nowrap">{new Date(e.time_ns / 1e6).toLocaleTimeString()}</span>
        <Badge tone={tone(e)}>{e.type}</Badge>
        <span className="muted small mono grow" style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
          {summary}
        </span>
        <span className="muted small">#{e.id}</span>
      </div>
      {open && <pre className="code" style={{ marginTop: '0.4rem' }}>{JSON.stringify(e.payload, null, 2)}</pre>}
    </div>
  )
}

export default function Events() {
  const [events, setEvents] = useState([])
  const [cat, setCat] = useState('all')
  const [q, setQ] = useState('')
  const status = useSSE('/api/stream/events', {
    onMessage: (data) => {
      try {
        const e = JSON.parse(data)
        setEvents((all) => {
          if (all.some((x) => x.id === e.id && x.time_ns === e.time_ns)) return all
          const next = [e, ...all]
          return next.length > MAX ? next.slice(0, MAX) : next
        })
      } catch {
        // ignore
      }
    },
  })

  const cats = useMemo(() => {
    const c = {}
    for (const e of events) c[category(e.type)] = (c[category(e.type)] || 0) + 1
    return c
  }, [events])
  const shown = events.filter((e) => (cat === 'all' || category(e.type) === cat) && (!q || JSON.stringify(e).toLowerCase().includes(q.toLowerCase())))

  return (
    <>
      <PageHeader title="Events" subtitle="What the node did, as it happens - kept in the node's memory, reset when janusd restarts" />
      <Card
        title="Event log"
        icon={Activity}
        actions={
          <>
            <Badge tone={stateTone(status)} dot>
              {status}
            </Badge>
            <button className="small" onClick={() => setEvents([])}>
              <Trash2 size={14} /> Clear view
            </button>
          </>
        }
      >
        <div className="row" style={{ marginBottom: '0.8rem' }}>
          <div className="chips">
            <button className={`small chip ${cat === 'all' ? 'active' : ''}`} onClick={() => setCat('all')}>
              All ({events.length})
            </button>
            {Object.entries(cats)
              .sort()
              .map(([c, n]) => (
                <button key={c} className={`small chip ${cat === c ? 'active' : ''}`} onClick={() => setCat(c)}>
                  {c} ({n})
                </button>
              ))}
          </div>
          <span className="grow" />
          <label className="search">
            <Search size={14} />
            <input placeholder="Search…" value={q} onChange={(e) => setQ(e.target.value)} />
          </label>
        </div>
        <div className="stack events" style={{ gap: '0.35rem', maxHeight: '68vh', overflow: 'auto' }}>
          {shown.length ? shown.map((e) => <EventRow key={`${e.id}-${e.time_ns}`} e={e} />) : <div className="empty">No events yet.</div>}
        </div>
      </Card>
    </>
  )
}
