import { KeyRound, Trash2, Upload } from 'lucide-react'
import { useState } from 'react'
import { del, postJSON } from '../../api.js'
import DataTable from '../../components/DataTable.jsx'
import { Badge, Card, ErrorBox, useAction, useConfirm } from '../../../shared/ui.jsx'
import { usePoll } from '../../hooks.jsx'
import { useMay } from '../../may.js'

function UploadForm({ onDone }) {
  const [name, setName] = useState('')
  const [bundle, setBundle] = useState('')
  const [crtList, setCrtList] = useState('')
  const [sni, setSni] = useState('')
  const [busy, run] = useAction()
  const addFile = async (e) => {
    const texts = await Promise.all([...e.target.files].map((f) => f.text()))
    setBundle((b) => [b, ...texts].filter(Boolean).join('\n'))
    if (!name && e.target.files[0]) setName(e.target.files[0].name.replace(/\.(key|crt|pem)$/, '.pem'))
    e.target.value = ''
  }
  const submit = (e) => {
    e.preventDefault()
    run(
      () =>
        postJSON('/api/haproxy/certs', {
          name,
          pem_bundle: bundle.endsWith('\n') ? bundle : bundle + '\n',
          crt_list: crtList,
          sni: sni ? sni.split(',').map((s) => s.trim()).filter(Boolean) : [],
        }),
      `Uploaded ${name}`,
    ).then((r) => {
      if (r !== undefined) {
        setName('')
        setBundle('')
        onDone()
      }
    })
  }
  return (
    <form className="stack" onSubmit={submit}>
      <div className="grid grid-3">
        <label className="field">
          <span>Name in HAProxy's store</span>
          <input className="mono" placeholder="/etc/haproxy/certs/site.pem" value={name} onChange={(e) => setName(e.target.value)} required />
        </label>
        <label className="field">
          <span>Bind into crt-list (optional)</span>
          <input className="mono" placeholder="/etc/haproxy/crt-list.txt" value={crtList} onChange={(e) => setCrtList(e.target.value)} />
        </label>
        <label className="field">
          <span>SNI filter (optional, comma-separated)</span>
          <input className="mono" placeholder="example.com, *.example.com" value={sni} onChange={(e) => setSni(e.target.value)} disabled={!crtList} />
        </label>
      </div>
      <label className="field">
        <span>Certificate chain and private key (PEM)</span>
        <textarea rows={8} value={bundle} onChange={(e) => setBundle(e.target.value)} placeholder="-----BEGIN CERTIFICATE-----" required />
      </label>
      <div className="row">
        <label className="button small">
          Load PEM file(s)
          <input type="file" multiple hidden onChange={addFile} />
        </label>
        <span className="grow" />
        <button className="primary" disabled={busy || !name || !bundle}>
          <Upload size={15} /> Upload
        </button>
      </div>
    </form>
  )
}

export default function Certificates() {
  const certs = usePoll('/api/haproxy/certs', { every: 0 })
  const [crtList, setCrtList] = useState('')
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const may = useMay()
  const canDelete = may('HAProxyService/CertificateDelete')
  const remove = async (name) => {
    const ok = await confirm({
      title: `Delete ${name}?`,
      body: (
        <p>
          It's removed from HAProxy's certificate store{crtList ? <> after being unbound from <code>{crtList}</code></> : ''}, and from the node: it won't come back on a reload or a reboot. HAProxy refuses to delete a certificate that is still bound into a crt-list.
        </p>
      ),
      action: 'Delete',
      danger: true,
    })
    if (!ok) return
    await run(() => del(`/api/haproxy/certs/${encodeURIComponent(name)}${crtList ? `?crt_list=${encodeURIComponent(crtList)}` : ''}`), `Deleted ${name}`)
    certs.reload()
  }
  return (
    <div className="stack">
      <Card
        title="Certificate store"
        icon={KeyRound}
        actions={canDelete && <input className="mono small" style={{ width: '17rem' }} placeholder="crt-list to unbind from on delete" value={crtList} onChange={(e) => setCrtList(e.target.value)} />}
      >
        <ErrorBox error={certs.error} />
        <DataTable
          rows={(certs.data?.certificates || []).map((c) => ({ ...c, __key: c.name }))}
          empty="No certificates loaded."
          columns={[
            { key: 'name', label: 'Name', render: (r) => <span className="mono">{r.name}</span> },
            { key: 'not_after', label: 'Expires', render: (r) => r.not_after || '–' },
            { key: 'status', label: 'Status', render: (r) => <Badge tone={r.status === 'Used' ? 'ok' : ''}>{r.status || '–'}</Badge> },
            canDelete && {
              key: 'a',
              label: '',
              render: (r) => (
                <button className="small danger" disabled={busy} onClick={() => remove(r.name)}>
                  <Trash2 size={13} /> Delete
                </button>
              ),
            },
          ].filter(Boolean)}
        />
      </Card>
      {may('HAProxyService/CertificateUpload') && (
      <Card title="Upload a certificate" icon={Upload}>
        <p className="small muted" style={{ marginTop: 0 }}>
          Loaded into HAProxy at once and kept on the node: it's put back - crt-list binding included - after every reload, restart and reboot.
        </p>
        <UploadForm onDone={certs.reload} />
      </Card>
      )}
    </div>
  )
}
