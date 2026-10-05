import { ArchiveRestore, List } from 'lucide-react'
import { useState } from 'react'
import { postJSON } from './call.js'
import { ErrorBox } from './shared/ui.jsx'

// RestoreForm is a new Controller's other first step: a backup - uploaded
// or from its bucket - with the backup kit and its passphrase. The
// Controller then starts again as the one backed up: its accounts, its
// fleet, its nodes.
export function RestoreForm({ onCancel }) {
  const [source, setSource] = useState('s3')
  const [s3, setS3] = useState({ endpoint: '', region: 'us-east-1', bucket: '', prefix: 'janus/', access_key: '', secret_key: '', path_style: true })
  const [backups, setBackups] = useState(null)
  const [key, setKey] = useState('')
  const [file, setFile] = useState(null)
  const [kit, setKit] = useState(null)
  const [passphrase, setPassphrase] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [done, setDone] = useState(null)
  const set = (k) => (e) => setS3({ ...s3, [k]: e.target.type === 'checkbox' ? e.target.checked : e.target.value })

  const list = async () => {
    setBusy(true)
    setError(null)
    try {
      const out = await postJSON('/api/restore/list', s3)
      setBackups(out)
      setKey(out[0]?.Key || '')
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }
  const restore = async (e) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      const form = new FormData()
      form.append('kit', kit)
      form.append('passphrase', passphrase)
      if (source === 's3') {
        form.append('s3', JSON.stringify(s3))
        form.append('key', key)
      } else {
        form.append('backup', file)
      }
      const resp = await fetch('/api/restore', { method: 'POST', body: form })
      const text = await resp.text()
      if (!resp.ok) {
        let msg = text
        try {
          msg = JSON.parse(text).error || text
        } catch {
          // plain text
        }
        throw new Error(msg)
      }
      setDone(JSON.parse(text))
      // The Controller starts again on what was restored: wait for it.
      for (let i = 0; i < 60; i++) {
        await new Promise((r) => setTimeout(r, 1000))
        try {
          const st = await (await fetch('/api/auth/status')).json()
          if (!st.setup_required) {
            window.location.reload()
            return
          }
        } catch {
          // restarting
        }
      }
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }
  if (done)
    return (
      <div className="stack">
        <div className="notice">
          Restored the backup of {new Date(done.created).toLocaleString()} ({done.files} files). The Controller starts again - sign in with its accounts.
        </div>
      </div>
    )
  const ready = kit && passphrase && (source === 's3' ? key : file)
  return (
    <form className="stack" onSubmit={restore}>
      <div className="tabs" role="tablist">
        <button type="button" role="tab" aria-selected={source === 's3'} className={source === 's3' ? 'tab active' : 'tab'} onClick={() => setSource('s3')}>
          From the bucket
        </button>
        <button type="button" role="tab" aria-selected={source === 'file'} className={source === 'file' ? 'tab active' : 'tab'} onClick={() => setSource('file')}>
          A backup file
        </button>
      </div>
      {source === 's3' ? (
        <div className="stack" style={{ gap: '0.5rem' }}>
          <label className="field">
            <span>S3 endpoint</span>
            <input value={s3.endpoint} onChange={set('endpoint')} placeholder="https://s3.eu-west-3.amazonaws.com" required />
          </label>
          <div className="row">
            <label className="field grow">
              <span>Bucket</span>
              <input value={s3.bucket} onChange={set('bucket')} required />
            </label>
            <label className="field grow">
              <span>Prefix</span>
              <input value={s3.prefix} onChange={set('prefix')} />
            </label>
          </div>
          <div className="row">
            <label className="field grow">
              <span>Access key</span>
              <input value={s3.access_key} onChange={set('access_key')} autoComplete="off" />
            </label>
            <label className="field grow">
              <span>Secret key</span>
              <input type="password" value={s3.secret_key} onChange={set('secret_key')} autoComplete="off" />
            </label>
          </div>
          <div className="row">
            <label className="field">
              <span>Region</span>
              <input value={s3.region} onChange={set('region')} style={{ width: '9rem' }} />
            </label>
            <label className="check" style={{ alignSelf: 'flex-end' }}>
              <input type="checkbox" checked={s3.path_style} onChange={set('path_style')} />
              <span>Path-style</span>
            </label>
            <span className="grow" />
            <button type="button" onClick={list} disabled={busy || !s3.endpoint || !s3.bucket} style={{ alignSelf: 'flex-end' }}>
              <List size={14} /> List the backups
            </button>
          </div>
          {backups && (
            <label className="field">
              <span>Backup</span>
              <select value={key} onChange={(e) => setKey(e.target.value)}>
                {backups.length === 0 && <option value="">none under this prefix</option>}
                {backups.map((b) => (
                  <option key={b.Key} value={b.Key}>
                    {b.Key} ({Math.round(b.Size / 1024)} KiB)
                  </option>
                ))}
              </select>
            </label>
          )}
        </div>
      ) : (
        <label className="field">
          <span>Backup file (.janusbackup)</span>
          <input type="file" onChange={(e) => setFile(e.target.files?.[0] || null)} />
        </label>
      )}
      <label className="field">
        <span>Backup kit</span>
        <input type="file" accept=".age,text/plain" onChange={(e) => setKit(e.target.files?.[0] || null)} />
      </label>
      <label className="field">
        <span>Its passphrase</span>
        <input
          value={passphrase}
          onChange={(e) => setPassphrase(e.target.value)}
          placeholder="XXXX-XXXX-XXXX-XXXX-XXXX-XXXX"
          className="mono"
          autoComplete="off"
          spellCheck={false}
        />
      </label>
      <ErrorBox error={error} />
      <div className="row">
        <button className="primary" type="submit" disabled={busy || !ready}>
          <ArchiveRestore size={15} /> {busy ? 'Restoring…' : 'Restore'}
        </button>
        <button type="button" className="ghost" onClick={onCancel}>
          Set up a new Controller instead
        </button>
      </div>
    </form>
  )
}
