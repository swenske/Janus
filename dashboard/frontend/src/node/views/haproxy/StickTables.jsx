import { Eraser, Plus, Table2, X } from 'lucide-react'
import { useState } from 'react'
import { postJSON } from '../../api.js'
import DataTable from '../../components/DataTable.jsx'
import { duration } from '../../format.js'
import { Card, Empty, useAction, useConfirm } from '../../../shared/ui.jsx'
import { usePoll } from '../../hooks.jsx'
import { useMay } from '../../may.js'
import { RuntimeError } from './RuntimeError.jsx'

const OPS = ['gt', 'ge', 'eq', 'ne', 'le', 'lt']
const OP_LABEL = { gt: '>', ge: '≥', eq: '=', ne: '≠', le: '≤', lt: '<' }

// "http_req_rate(10000)" is filtered as "http_req_rate" and shown with its
// period: "http_req_rate /10s".
const dataType = (name) => name.replace(/\(\d+\)$/, '')
function dataLabel(name) {
  const m = name.match(/^(.*)\((\d+)\)$/)
  return m ? `${m[1]} /${duration(Number(m[2]) / 1000)}` : name
}

function Entries({ table }) {
  const [key, setKey] = useState('')
  const [filters, setFilters] = useState([])
  const [draft, setDraft] = useState({ data: '', op: 'gt', value: '' })
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const may = useMay()
  const canClear = may('HAProxyService/TableClear')
  const query = new URLSearchParams()
  if (key) query.set('key', key)
  for (const f of filters) query.append('filter', `${f.data}:${f.op}:${f.value}`)
  const shown = usePoll(`/api/haproxy/tables/${encodeURIComponent(table.name)}?${query}`)
  const entries = shown.data?.entries || []
  const names = (entries[0]?.data || []).map((d) => d.name)

  const clear = async (body, what, typed) => {
    const ok = await confirm({
      title: `Clear ${what}?`,
      body: <p>HAProxy forgets {what}: its counters start again from zero - a client a rate limit blocked is let through until it reaches the limit again.</p>,
      action: 'Clear',
      danger: true,
      typeToConfirm: typed,
    })
    if (!ok) return
    await run(() => postJSON(`/api/haproxy/tables/${encodeURIComponent(table.name)}/clear`, body), `Cleared ${what}`)
    shown.reload()
  }
  const addFilter = () => {
    setFilters([...filters, { ...draft, value: Number(draft.value) }])
    setDraft({ data: '', op: 'gt', value: '' })
  }

  return (
    <div className="stack">
      <div className="row">
        <input className="mono" placeholder="key, e.g. 203.0.113.7" value={key} onChange={(e) => setKey(e.target.value.trim())} disabled={filters.length > 0} />
        <span className="muted small">or entries whose</span>
        <select value={draft.data} onChange={(e) => setDraft({ ...draft, data: e.target.value })} disabled={!!key || filters.length >= 4}>
          <option value="">data…</option>
          {[...new Set(names.map(dataType))].map((n) => (
            <option key={n} value={n}>
              {n}
            </option>
          ))}
        </select>
        <select value={draft.op} onChange={(e) => setDraft({ ...draft, op: e.target.value })} disabled={!!key}>
          {OPS.map((o) => (
            <option key={o} value={o}>
              {OP_LABEL[o]}
            </option>
          ))}
        </select>
        <input type="number" style={{ width: '7rem' }} placeholder="value" value={draft.value} onChange={(e) => setDraft({ ...draft, value: e.target.value })} disabled={!!key} />
        <button className="small" disabled={!draft.data || draft.value === '' || filters.length >= 4} onClick={addFilter}>
          <Plus size={13} /> Filter
        </button>
      </div>
      {filters.length > 0 && (
        <div className="chips">
          {filters.map((f, i) => (
            <button key={i} className="small chip mono active" title="Remove this filter" onClick={() => setFilters(filters.filter((_, j) => j !== i))}>
              {f.data} {OP_LABEL[f.op]} {f.value} <X size={12} />
            </button>
          ))}
        </div>
      )}
      <RuntimeError error={shown.error} what="stick tables" />
      {shown.data?.truncated && (
        <div className="notice">The first {entries.length} entries are shown: more match. Narrow them with a key or filters.</div>
      )}
      <DataTable
        rows={entries.map((e) => ({ ...e, __key: e.key, use: Number(e.use), expires: Number(e.expires_ms) / 1000, ...Object.fromEntries((e.data || []).map((d) => [`d:${d.name}`, Number.isNaN(Number(d.value)) ? d.value : Number(d.value)])) }))}
        empty={key || filters.length ? 'No entry matches.' : 'This table is empty.'}
        initialSort={{ key: names[0] ? `d:${names[0]}` : 'key', dir: -1 }}
        toolbar={
          canClear && (
            <>
              {filters.length > 0 && entries.length > 0 && (
                <button className="small danger" disabled={busy} onClick={() => clear({ filters }, 'the entries matching these filters', table.name)}>
                  <Eraser size={13} /> Clear matching…
                </button>
              )}
              {!key && !filters.length && Number(table.used) > 0 && (
                <button className="small danger" disabled={busy} onClick={() => clear({ all: true }, `every entry of ${table.name}`, table.name)}>
                  <Eraser size={13} /> Clear all…
                </button>
              )}
            </>
          )
        }
        columns={[
          { key: 'key', label: 'Key', render: (r) => <span className="mono">{r.key}</span> },
          { key: 'use', label: 'In use', num: true, render: (r) => (r.use ? r.use : <span className="muted">0</span>) },
          { key: 'expires', label: 'Expires in', num: true, render: (r) => duration(r.expires) },
          ...names.map((n) => ({ key: `d:${n}`, label: <span className="mono">{dataLabel(n)}</span>, num: true })),
          canClear && {
            key: 'actions',
            label: '',
            render: (r) => (
              <div className="row" style={{ justifyContent: 'flex-end' }}>
                <button className="small" disabled={busy} onClick={() => clear({ key: r.key }, `entry ${r.key}`)} title="Forget this entry: its counters start again from zero">
                  <Eraser size={13} /> Clear
                </button>
              </div>
            ),
          },
        ].filter(Boolean)}
      />
    </div>
  )
}

export default function StickTables() {
  const tables = usePoll('/api/haproxy/tables')
  const [selected, setSelected] = useState('')
  const list = tables.data?.tables || []
  const current = list.find((t) => t.name === selected) || list[0]
  return (
    <Card title="Stick tables" icon={Table2}>
      <RuntimeError error={tables.error} what="stick tables" />
      {!list.length && !tables.error ? (
        <Empty>The running configuration has no stick table.</Empty>
      ) : (
        <div className="stack">
          <div className="chips">
            {list.map((t) => (
              <button key={t.name} className={`small chip ${current?.name === t.name ? 'active' : ''}`} onClick={() => setSelected(t.name)}>
                <span className="mono">{t.name}</span>{' '}
                <span className="muted">
                  {t.type} · {Number(t.used)} / {Number(t.size)}
                </span>
              </button>
            ))}
          </div>
          {current && <Entries key={current.name} table={current} />}
        </div>
      )}
    </Card>
  )
}
