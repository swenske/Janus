import { Download, FileText, Lock, Pencil, Plus, Trash2, Upload } from 'lucide-react'
import { useState } from 'react'
import { download, getText, postJSON } from '../../api.js'
import { Editor } from '../../components/Editor.jsx'
import DataTable from '../../components/DataTable.jsx'
import { Badge, Card, ErrorBox, useAction, useConfirm, useToast } from '../../../shared/ui.jsx'
import { bytes, dateTime } from '../../format.js'
import { usePoll } from '../../hooks.jsx'

const NAME = /^[A-Za-z0-9][A-Za-z0-9._-]{0,99}(\/[A-Za-z0-9][A-Za-z0-9._-]{0,99})?$/

function base64(buf) {
  const u8 = new Uint8Array(buf)
  let s = ''
  for (let i = 0; i < u8.length; i += 0x8000) s += String.fromCharCode(...u8.subarray(i, i + 0x8000))
  return btoa(s)
}

// FileForm writes a file: new (name and content), or the content of an
// existing one. The node refuses it when haproxy.cfg wouldn't load with it.
function FileForm({ initialName = '', initialText = '', editing, dir, onDone, onCancel }) {
  const [name, setName] = useState(initialName)
  const [text, setText] = useState(initialText)
  const [binary, setBinary] = useState(null) // { name, base64 } from a file
  const [reload, setReload] = useState(true)
  const [errors, setErrors] = useState(null)
  const [busy, run] = useAction()
  const pick = async (e) => {
    const f = e.target.files[0]
    e.target.value = ''
    if (!f) return
    const buf = await f.arrayBuffer()
    setBinary({ name: f.name, base64: base64(buf) })
    setText('')
    if (!name) setName(f.name)
  }
  const save = () =>
    run(
      async () => {
        const content_base64 = binary ? binary.base64 : base64(new TextEncoder().encode(text))
        const r = await postJSON('/api/haproxy/files', { name, content_base64, reload })
        if (!r.accepted) {
          setErrors(r.errors || ['refused'])
          throw new Error('haproxy.cfg would not load with this file - nothing changed')
        }
        setErrors(null)
        onDone()
        return r
      },
      `${name} saved${reload ? ', HAProxy reloaded' : ''}`,
    )
  const valid = NAME.test(name) && (binary || text)
  return (
    <div className="stack">
      <div className="grid grid-2">
        <label className="field">
          <span>Name - haproxy.cfg references {dir || '/etc/haproxy/files'}/&lt;name&gt;</span>
          <input className="mono" value={name} disabled={editing} onChange={(e) => setName(e.target.value)} placeholder="errors/503.http" />
        </label>
        <div className="field">
          <span className="field-label">Content</span>
          <div className="row">
            <label className="button small">
              <Upload size={13} /> From a file…
              <input type="file" hidden onChange={pick} />
            </label>
            {binary && <Badge tone="info">{binary.name}</Badge>}
          </div>
        </div>
      </div>
      {!binary && <Editor value={text} onChange={setText} />}
      {name && !NAME.test(name) && <div className="notice">Letters, digits, '.', '-', '_', and at most one subdirectory (certs/site.pem).</div>}
      <label className="row small">
        <input type="checkbox" checked={reload} onChange={(e) => setReload(e.target.checked)} />
        Reload HAProxy afterwards, for it to use the file now
      </label>
      {errors && <div className="error-box">{errors.join('\n')}</div>}
      <div className="row">
        <button className="primary" disabled={busy || !valid} onClick={save}>
          <Upload size={15} /> Save
        </button>
        {onCancel && <button onClick={onCancel}>Cancel</button>}
      </div>
    </div>
  )
}

// Files is HAProxy's own files: error pages, maps, ACL lists, Lua, the
// certificates the letsencrypt extension doesn't obtain.
export default function Files() {
  const files = usePoll('/api/haproxy/files', { every: 0 })
  const [editing, setEditing] = useState(null) // { name, text } | 'new'
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const toast = useToast()
  const dir = files.data?.dir

  const edit = (f) =>
    run(async () => {
      const text = await getText(`/api/haproxy/files/content?name=${encodeURIComponent(f.name)}`)
      setEditing({ name: f.name, text })
    })
  const remove = async (f) => {
    const ok = await confirm({
      title: `Remove ${f.name}?`,
      body: <p>The node refuses while haproxy.cfg needs it. HAProxy is reloaded afterwards.</p>,
      action: 'Remove',
      danger: true,
    })
    if (!ok) return
    const r = await run(() => postJSON('/api/haproxy/files/delete', { name: f.name, reload: true }))
    if (!r) return
    if (!r.accepted) {
      const errs = r.errors || []
      const why = errs.find((e) => e.includes(f.name)) || errs.find((e) => e.includes('[ALERT]')) || errs[0] || 'refused'
      toast(`haproxy.cfg still needs ${f.name}: ${why}`, 'danger')
      return
    }
    toast(`${f.name} removed`)
    files.reload()
  }
  const done = () => {
    setEditing(null)
    files.reload()
  }

  return (
    <div className="stack">
      <Card
        title="Files"
        icon={FileText}
        actions={
          <button className="small primary" onClick={() => setEditing('new')}>
            <Plus size={13} /> Add a file
          </button>
        }
      >
        <p className="small muted" style={{ marginTop: 0 }}>
          What haproxy.cfg references besides the Let's Encrypt certificates - error pages, maps, ACL lists, Lua, other certificates - as{' '}
          <span className="mono">{dir || '/etc/haproxy/files'}/&lt;name&gt;</span>, kept on the node. A change is refused when haproxy.cfg wouldn't load with
          it. A file holding a private key is never shown again.
        </p>
        <ErrorBox error={files.error} />
        <DataTable
          rows={(files.data?.files || []).map((f) => ({ ...f, __key: f.name }))}
          empty="No file yet."
          columns={[
            { key: 'name', label: 'Name', render: (r) => <span className="mono">{r.name}</span> },
            { key: 'size', label: 'Size', render: (r) => bytes(r.size) },
            { key: 'modified_unix', label: 'Modified', render: (r) => dateTime(r.modified_unix * 1000) },
            {
              key: 'secret',
              label: '',
              render: (r) =>
                r.secret ? (
                  <Badge tone="warn">
                    <Lock size={11} /> private key
                  </Badge>
                ) : null,
            },
            {
              key: 'a',
              label: '',
              render: (r) => (
                <span className="row nowrap" style={{ justifyContent: 'flex-end' }}>
                  {!r.secret && (
                    <>
                      <button className="small" disabled={busy} title="Edit" onClick={() => edit(r)}>
                        <Pencil size={13} />
                      </button>
                      <button
                        className="small"
                        disabled={busy}
                        title="Download"
                        onClick={() => run(() => download(`/api/haproxy/files/content?name=${encodeURIComponent(r.name)}`, r.name.split('/').pop()))}
                      >
                        <Download size={13} />
                      </button>
                    </>
                  )}
                  {r.secret && (
                    <button className="small" disabled={busy} title="Replace" onClick={() => setEditing({ name: r.name, text: '' })}>
                      <Upload size={13} />
                    </button>
                  )}
                  <button className="small danger" disabled={busy} title="Remove" onClick={() => remove(r)}>
                    <Trash2 size={13} />
                  </button>
                </span>
              ),
            },
          ]}
        />
      </Card>
      {editing && (
        <Card title={editing === 'new' ? 'Add a file' : `Edit ${editing.name}`} icon={editing === 'new' ? Plus : Pencil}>
          <FileForm
            key={editing === 'new' ? 'new' : editing.name}
            dir={dir}
            initialName={editing === 'new' ? '' : editing.name}
            initialText={editing === 'new' ? '' : editing.text}
            editing={editing !== 'new'}
            onDone={done}
            onCancel={() => setEditing(null)}
          />
        </Card>
      )}
    </div>
  )
}
