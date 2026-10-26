import { Activity, ChevronDown, ChevronRight, Search, Trash2 } from 'lucide-react'
import { useMemo, useState } from 'react'
import { Badge, Card, PageHeader, stateTone } from '../../shared/ui.jsx'
import { useSSE } from '../hooks.jsx'
import { fmtDateTime, fmtTime, sameDay } from '../../shared/units.js'

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

// when is an event's time: the clock alone today, the date too before -
// in the account's zone.
function when(ns) {
  const t = ns / 1e6
  return sameDay(t) ? fmtTime(t) : fmtDateTime(t)
}

// BootRow marks where janusd started - with a new boot, a reboot or an
// update before it: the events below came before.
function BootRow({ e }) {
  const p = e.payload || {}
  return (
    <div className="event boot">
      <span className="muted small mono nowrap">{when(e.time_ns)}</span>
      <span className="small">
        janusd started{p.version ? <> - <span className="mono">{p.version}</span></> : null}
        {p.slot ? `, slot ${p.slot}` : ''}
      </span>
      <span className="grow" />
      <span className="muted small">#{e.id}</span>
    </div>
  )
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
        <span className="muted small mono nowrap">{when(e.time_ns)}</span>
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
      <PageHeader title="Events" subtitle="What the node did, as it happens - its last thousand events, kept across reboots and updates" />
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
        <div className="stack events" style={{ gap: '0.35rem', maxHeight: '68vh', overflow: 'auto' }} tabIndex={0} role="log" aria-label="Events">
          {shown.length ? (
            shown.map((e) => (e.type === 'janusd.started' ? <BootRow key={`${e.id}-${e.time_ns}`} e={e} /> : <EventRow key={`${e.id}-${e.time_ns}`} e={e} />))
          ) : (
            <div className="empty">No events yet.</div>
          )}
        </div>
      </Card>
    </>
  )
}
