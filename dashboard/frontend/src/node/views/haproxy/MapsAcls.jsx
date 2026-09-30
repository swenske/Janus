import { ListTree, Plus, Trash2 } from 'lucide-react'
import { useEffect, useState } from 'react'
import { getJSON, postJSON } from '../../api.js'
import DataTable from '../../components/DataTable.jsx'
import { Card, Empty, ErrorBox, useAction } from '../../../shared/ui.jsx'
import { usePoll } from '../../hooks.jsx'

function MapEntries({ name }) {
  const [entries, setEntries] = useState(null)
  const [error, setError] = useState(null)
  const [key, setKey] = useState('')
  const [value, setValue] = useState('')
  const [busy, run] = useAction()
  const load = async () => {
    try {
      const r = await getJSON(`/api/haproxy/maps/${encodeURIComponent(name)}`)
      setEntries(r?.entries || {})
      setError(null)
    } catch (err) {
      setError(err)
    }
  }
  useEffect(() => {
    load()
  }, [name])
  const update = (k, v, del) =>
    run(() => postJSON(`/api/haproxy/maps/${encodeURIComponent(name)}`, { key: k, value: v, delete: del }), del ? `Deleted ${k}` : `Set ${k}`).then(load)

  return (
    <div className="stack">
      <ErrorBox error={error} />
      <form
        className="row"
        onSubmit={(e) => {
          e.preventDefault()
          if (key) update(key, value, false).then(() => {
            setKey('')
            setValue('')
          })
        }}
      >
        <input placeholder="key" value={key} onChange={(e) => setKey(e.target.value)} className="mono" />
        <input placeholder="value" value={value} onChange={(e) => setValue(e.target.value)} className="mono grow" />
        <button className="primary" disabled={busy || !key}>
          <Plus size={15} /> Set
        </button>
      </form>
      <DataTable
        rows={Object.entries(entries || {}).map(([k, v]) => ({ __key: k, key: k, value: v }))}
        empty="This map is empty."
        columns={[
          { key: 'key', label: 'Key', render: (r) => <span className="mono">{r.key}</span> },
          { key: 'value', label: 'Value', render: (r) => <span className="mono">{r.value}</span> },
          {
            key: 'actions',
            label: '',
            render: (r) => (
              <div className="row" style={{ justifyContent: 'flex-end' }}>
                <button className="small" onClick={() => {
                  setKey(r.key)
                  setValue(r.value)
                }}>
                  Edit
                </button>
                <button className="small danger" disabled={busy} onClick={() => update(r.key, '', true)}>
                  <Trash2 size={13} />
                </button>
              </div>
            ),
          },
        ]}
      />
      <p className="muted small">Runtime changes apply immediately but live in memory: they're lost at the next reload unless the map file is updated too.</p>
    </div>
  )
}

function Acls() {
  const [acl, setAcl] = useState('')
  const [value, setValue] = useState('')
  const [busy, run] = useAction()
  const act = (del) => run(() => postJSON(`/api/haproxy/acls/${encodeURIComponent(acl)}`, { value, delete: del }), del ? `Removed ${value} from ${acl}` : `Added ${value} to ${acl}`)
  return (
    <Card title="ACL patterns" icon={ListTree}>
      <p className="muted small" style={{ marginTop: 0 }}>
        Add or remove a value from a file-backed ACL (<code>acl name ... -f /path</code>), by its file path or <code>#id</code>.
      </p>
      <div className="row">
        <input className="mono" placeholder="/etc/haproxy/blocked.acl" value={acl} onChange={(e) => setAcl(e.target.value)} />
        <input className="mono grow" placeholder="value, e.g. 203.0.113.7" value={value} onChange={(e) => setValue(e.target.value)} />
        <button className="primary" disabled={busy || !acl || !value} onClick={() => act(false)}>
          <Plus size={15} /> Add
        </button>
        <button className="danger" disabled={busy || !acl || !value} onClick={() => act(true)}>
          <Trash2 size={15} /> Remove
        </button>
      </div>
    </Card>
  )
}

export default function MapsAcls() {
  const maps = usePoll('/api/haproxy/maps', { every: 0 })
  const [selected, setSelected] = useState('')
  const names = maps.data?.maps || []
  const current = selected || names[0] || ''
  return (
    <div className="stack">
      <Card title="Maps" icon={ListTree}>
        <ErrorBox error={maps.error} />
        {!names.length && !maps.error ? (
          <Empty>No file-backed map in the running configuration (only maps loaded from a file can be changed at runtime).</Empty>
        ) : (
          <>
            <div className="chips" style={{ marginBottom: '0.8rem' }}>
              {names.map((n) => (
                <button key={n} className={`small chip mono ${current === n ? 'active' : ''}`} onClick={() => setSelected(n)}>
                  {n}
                </button>
              ))}
            </div>
            {current && <MapEntries name={current} />}
          </>
        )}
      </Card>
      <Acls />
    </div>
  )
}
