import { Activity, Search, Unplug } from 'lucide-react'
import { useState } from 'react'
import { postJSON } from '../../api.js'
import DataTable from '../../components/DataTable.jsx'
import { duration } from '../../format.js'
import { Card, useAction, useConfirm } from '../../../shared/ui.jsx'
import { usePoll } from '../../hooks.jsx'
import { useMay } from '../../may.js'
import { RuntimeError } from './RuntimeError.jsx'

// The streams HAProxy is processing: a request it hasn't answered yet,
// a TCP connection it relays. An idle keep-alive connection has none.
export default function Sessions() {
  const [draft, setDraft] = useState({ frontend: '', backend: '', source: '' })
  const [filter, setFilter] = useState(draft)
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const may = useMay()
  const query = new URLSearchParams(Object.entries(filter).filter(([, v]) => v))
  const sessions = usePoll(`/api/haproxy/sessions?${query}`)
  const list = sessions.data?.sessions || []

  const end = async (s) => {
    const ok = await confirm({
      title: 'End this stream?',
      body: (
        <p>
          HAProxy closes the connection of <span className="mono">{s.source}</span> on {s.frontend}
          {s.server ? ` and the one to ${s.backend}/${s.server}` : ''}: the client sees it cut.
        </p>
      ),
      action: 'End it',
      danger: true,
    })
    if (!ok) return
    await run(() => postJSON(`/api/haproxy/sessions/${encodeURIComponent(s.id)}/shutdown`), 'Stream ended')
    sessions.reload()
  }

  return (
    <Card title="Sessions in progress" icon={Activity}>
      <form
        className="row"
        style={{ marginBottom: '0.8rem' }}
        onSubmit={(e) => {
          e.preventDefault()
          setFilter(draft)
        }}
      >
        <input placeholder="frontend" value={draft.frontend} onChange={(e) => setDraft({ ...draft, frontend: e.target.value.trim() })} />
        <input placeholder="backend" value={draft.backend} onChange={(e) => setDraft({ ...draft, backend: e.target.value.trim() })} />
        <input className="mono" placeholder="client address" value={draft.source} onChange={(e) => setDraft({ ...draft, source: e.target.value.trim() })} />
        <button>
          <Search size={15} /> Show
        </button>
      </form>
      <RuntimeError error={sessions.error} what="streams" />
      {sessions.data?.truncated && <div className="notice">The first {list.length} streams are shown: more match. Narrow them by frontend, backend or client.</div>}
      <DataTable
        rows={list.map((s) => ({ ...s, __key: s.id, age: Number(s.age_seconds) }))}
        empty="No stream in progress: HAProxy is answering everything it receives."
        initialSort={{ key: 'age', dir: -1 }}
        columns={[
          { key: 'source', label: 'Client', render: (s) => <span className="mono">{s.source}</span> },
          { key: 'frontend', label: 'Frontend' },
          { key: 'backend', label: 'Backend', render: (s) => s.backend || <span className="muted">not chosen yet</span> },
          { key: 'server', label: 'Server', render: (s) => s.server || <span className="muted">–</span> },
          { key: 'age', label: 'Age', num: true, render: (s) => duration(s.age) },
          { key: 'expires', label: 'Next timeout', render: (s) => s.expires || <span className="muted">–</span> },
          { key: 'id', label: 'Stream', render: (s) => <span className="mono muted small" title={s.raw}>{s.id}</span> },
          may('HAProxyService/SessionShutdown') && {
            key: 'actions',
            label: '',
            render: (s) => (
              <div className="row" style={{ justifyContent: 'flex-end' }}>
                <button className="small danger" disabled={busy} onClick={() => end(s)}>
                  <Unplug size={13} /> End
                </button>
              </div>
            ),
          },
        ].filter(Boolean)}
      />
    </Card>
  )
}
