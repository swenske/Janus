import { Archive, CheckCircle2, Copy, Download, KeyRound, Play, Upload } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { call, postJSON } from './call.js'
import { backupAlert } from './backupAlert.js'
import { Step } from './Fleet.jsx'
import { Badge, Card, ErrorBox, useAction, useConfirm, useToast } from './shared/ui.jsx'

// Backups (admin): everything the Controller keeps, and its nodes'
// configurations, encrypted to the backup kit and admins' keys, signed,
// to an S3 bucket on a schedule - or downloaded. dashboard/backend/
// backups.go.

function size(n) {
  if (!n) return '–'
  return n > 1 << 20 ? `${(n / (1 << 20)).toFixed(1)} MiB` : `${Math.max(1, Math.round(n / 1024))} KiB`
}

function when(t) {
  return t ? new Date(t).toLocaleString() : '–'
}

async function saveFile(path, fallback) {
  const resp = await fetch(path)
  if (!resp.ok) throw new Error((await resp.text()) || resp.statusText)
  const name = ((resp.headers.get('Content-Disposition') || '').match(/filename="([^"]+)"/) || [])[1] || fallback
  const a = document.createElement('a')
  a.href = URL.createObjectURL(await resp.blob())
  a.download = name
  a.click()
  return name
}

// KitCard makes the backup kit: the key that decrypts the backups, kept
// by you - never by the Controller.
function KitCard({ status, onChanged }) {
  const [passphrase, setPassphrase] = useState(null)
  const [saved, setSaved] = useState(false)
  const [kit, setKit] = useState('')
  const [typed, setTyped] = useState('')
  const [busy, run] = useAction()
  const toast = useToast()
  const confirm = useConfirm()
  const waiting = status.kit_waiting
  const make = async () => {
    if (status.kit === 'ready') {
      const ok = await confirm({
        title: 'Make a new backup kit?',
        body: <p>Once you confirm it, new backups open with the new kit only. Backups made before still need the old one: keep it as long as them.</p>,
        action: 'Make a new kit',
      })
      if (!ok) return
    }
    const r = await run(() => postJSON('/api/backups/kit'))
    if (r) {
      setPassphrase(r.passphrase)
      setSaved(false)
      onChanged()
    }
  }
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(passphrase)
      toast('Passphrase copied')
    } catch {
      toast('Copy failed - select the text instead', 'warn')
    }
  }
  const prove = (e) => {
    e.preventDefault()
    run(async () => {
      await postJSON('/api/backups/kit/confirm', { kit, passphrase: typed })
      setPassphrase(null)
      onChanged()
    }, 'The backup kit is ready')
  }
  if (status.kit === 'ready' && !waiting)
    return (
      <Card
        title="Backup kit"
        icon={KeyRound}
        actions={
          <button className="small" onClick={make} disabled={busy}>
            Make a new kit
          </button>
        }
      >
        <div className="row">
          <CheckCircle2 size={16} color="var(--ok)" />
          <span className="grow">
            Backups are encrypted to your backup kit{status.settings.recipients?.length ? ` and ${status.settings.recipients.length} admin key(s)` : ''}: the Controller can write
            them, not read them.
          </span>
        </div>
      </Card>
    )
  return (
    <Card title="Backup kit" icon={KeyRound}>
      <div className="stack">
        <p className="muted" style={{ margin: 0 }}>
          The backups are encrypted to a key the Controller doesn&apos;t keep: it goes in a backup kit you keep, encrypted with a passphrase - a password manager holds both. A
          restore needs them.
        </p>
        <ol className="fleet-steps">
          <Step n={1} done={waiting} active={!waiting} title="Make the backup kit">
            <div>
              <button className="primary" disabled={busy} onClick={make}>
                <KeyRound size={15} /> Make the kit
              </button>
            </div>
          </Step>
          <Step n={2} done={waiting && saved} active={waiting && !saved} title="Store the kit and its passphrase">
            <div className="stack" style={{ gap: '0.6rem' }}>
              {passphrase ? (
                <>
                  <div className="fleet-passphrase">
                    <span className="mono">{passphrase}</span>
                    <button type="button" className="small" onClick={copy}>
                      <Copy size={13} /> Copy
                    </button>
                  </div>
                  <div className="muted small">The passphrase is shown this once.</div>
                </>
              ) : (
                <div className="muted small">The passphrase was shown when the kit was made. Lost it? Make the kit again.</div>
              )}
              <div className="row">
                <button className="primary" disabled={busy} onClick={() => run(() => saveFile('/api/backups/kit', 'janus-backup-kit.age'))}>
                  <Download size={15} /> Download the backup kit
                </button>
                <button onClick={() => setSaved(true)}>I stored both</button>
                {!passphrase && (
                  <button className="ghost" disabled={busy} onClick={make}>
                    Make it again
                  </button>
                )}
              </div>
            </div>
          </Step>
          <Step n={3} active={waiting && saved} title="Prove you kept them">
            <form className="stack" style={{ gap: '0.6rem' }} onSubmit={prove}>
              <label className="field">
                <span>Backup kit</span>
                <input type="file" accept=".age,text/plain" onChange={(e) => e.target.files?.[0]?.text().then(setKit)} />
              </label>
              <label className="field">
                <span>Passphrase</span>
                <input
                  value={typed}
                  onChange={(e) => setTyped(e.target.value)}
                  placeholder="XXXX-XXXX-XXXX-XXXX-XXXX-XXXX"
                  className="mono"
                  autoComplete="off"
                  spellCheck={false}
                />
              </label>
              <div className="row">
                <button className="primary" disabled={busy || !kit || !typed}>
                  <Upload size={15} /> Confirm
                </button>
                <button type="button" className="ghost" onClick={() => setSaved(false)}>
                  Back
                </button>
              </div>
            </form>
          </Step>
        </ol>
      </div>
    </Card>
  )
}

function SettingsCard({ status, onChanged }) {
  const [form, setForm] = useState(() => ({ ...status.settings, recipients: (status.settings.recipients || []).join('\n'), secret_key: '' }))
  const [busy, run] = useAction()
  const set = (k) => (e) => setForm({ ...form, [k]: e.target.type === 'checkbox' ? e.target.checked : e.target.value })
  const save = (e) => {
    e.preventDefault()
    run(async () => {
      await postJSON(
        '/api/backups/settings',
        {
          ...form,
          interval_hours: Number(form.interval_hours),
          keep: Number(form.keep),
          recipients: form.recipients
            .split('\n')
            .map((s) => s.trim())
            .filter(Boolean),
          secret_key: form.secret_key || undefined,
        },
        'PUT',
      )
      setForm({ ...form, secret_key: '' })
      onChanged()
    }, 'Backup settings saved')
  }
  return (
    <Card title="Where and when" icon={Archive}>
      <form className="stack" onSubmit={save}>
        <div className="row" style={{ flexWrap: 'wrap' }}>
          <label className="field grow">
            <span>S3 endpoint</span>
            <input value={form.endpoint} onChange={set('endpoint')} placeholder="https://s3.eu-west-3.amazonaws.com" />
          </label>
          <label className="field">
            <span>Region</span>
            <input value={form.region} onChange={set('region')} placeholder="us-east-1" style={{ width: '9rem' }} />
          </label>
        </div>
        <div className="row" style={{ flexWrap: 'wrap' }}>
          <label className="field grow">
            <span>Bucket</span>
            <input value={form.bucket} onChange={set('bucket')} />
          </label>
          <label className="field grow">
            <span>Prefix</span>
            <input value={form.prefix} onChange={set('prefix')} placeholder="janus/" />
          </label>
        </div>
        <div className="row" style={{ flexWrap: 'wrap' }}>
          <label className="field grow">
            <span>Access key</span>
            <input value={form.access_key} onChange={set('access_key')} autoComplete="off" />
          </label>
          <label className="field grow">
            <span>Secret key</span>
            <input
              type="password"
              value={form.secret_key}
              onChange={set('secret_key')}
              placeholder={status.has_secret ? 'kept - type to change it' : ''}
              autoComplete="new-password"
            />
          </label>
        </div>
        <label className="check">
          <input type="checkbox" checked={!!form.path_style} onChange={set('path_style')} />
          <span>Path-style addresses (MinIO, Garage, Ceph and most self-hosted services)</span>
        </label>
        <div className="row" style={{ flexWrap: 'wrap' }}>
          <label className="field">
            <span>Every (hours)</span>
            <input type="number" min={1} max={168} value={form.interval_hours} onChange={set('interval_hours')} style={{ width: '7rem' }} />
          </label>
          <label className="field">
            <span>Keep the last (0: never delete)</span>
            <input type="number" min={0} max={1000} value={form.keep} onChange={set('keep')} style={{ width: '7rem' }} />
          </label>
        </div>
        <label className="field">
          <span>Admins&apos; keys that can decrypt them too (optional, one per line: age1… or ssh-ed25519 / ssh-rsa)</span>
          <textarea rows={2} className="mono" value={form.recipients} onChange={set('recipients')} />
        </label>
        <label className="check">
          <input type="checkbox" checked={!!form.enabled} onChange={set('enabled')} />
          <span>Back up on this schedule</span>
        </label>
        <div className="muted small">
          Give the Controller credentials that may only write to the bucket, and turn on its versioning or Object Lock: whoever took the Controller then can&apos;t erase its past
          backups. Without the right to delete, the bucket&apos;s own rules keep or expire them.
        </div>
        <div>
          <button className="primary" type="submit" disabled={busy}>
            Save
          </button>
        </div>
      </form>
    </Card>
  )
}

export default function BackupsPage() {
  const [status, setStatus] = useState(null)
  const [error, setError] = useState(null)
  const [busy, run] = useAction()
  const toast = useToast()
  const load = useCallback(() => call('/api/backups').then(setStatus, setError), [])
  useEffect(() => {
    load()
  }, [load])
  if (error) return <ErrorBox error={error} />
  if (!status) return null
  const ready = status.kit === 'ready'
  const copyKey = async () => {
    try {
      await navigator.clipboard.writeText(status.signing_key)
      toast('Signing key copied')
    } catch {
      toast('Copy failed - select the text instead', 'warn')
    }
  }
  return (
    <div className="stack">
      <div className="spread">
        <h1>Backups</h1>
        {ready && (
          <div className="row">
            <button
              disabled={busy}
              onClick={() =>
                run(
                  () => saveFile('/api/backups/download', 'janus.janusbackup'),
                  (name) => `Saved ${name}`,
                )
              }
            >
              <Download size={15} /> Download a backup
            </button>
            <button className="primary" disabled={busy || !status.settings.endpoint} onClick={() => run(() => postJSON('/api/backups/run').then(load), 'Backed up')}>
              <Play size={15} /> Back up now
            </button>
          </div>
        )}
      </div>
      <p className="muted" style={{ margin: 0 }}>
        Everything the Controller keeps - accounts, nodes, the fleet, hypervisors, machines, the audit, its master key - and each node&apos;s configuration (HAProxy, maps, files,
        network, firewall, VRRP, BGP, Consul, Let&apos;s Encrypt, exporters), never a node&apos;s private keys. Encrypted to your backup kit, signed by the Controller. A new
        Controller restores one from its first page.
      </p>
      {status.alert && <div className="notice warn">{backupAlert(status)}</div>}
      <KitCard status={status} onChanged={load} />
      {ready && <SettingsCard status={status} onChanged={load} />}
      {ready && (
        <Card title="Recent" icon={Archive} actions={status.next && status.settings.enabled ? <span className="muted small">next {when(status.next)}</span> : null}>
          {status.history.length === 0 ? (
            <div className="muted">No backup yet.</div>
          ) : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>When</th>
                    <th>Backup</th>
                    <th>Size</th>
                    <th>Result</th>
                  </tr>
                </thead>
                <tbody>
                  {status.history.map((r, i) => (
                    <tr key={i}>
                      <td style={{ whiteSpace: 'nowrap' }}>{when(r.time)}</td>
                      <td className="mono small">{r.key || '–'}</td>
                      <td>{size(r.size)}</td>
                      <td>
                        {r.error ? <Badge tone="danger">failed</Badge> : <Badge tone="ok">done</Badge>} <span className="small muted">{r.error || r.note}</span>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {status.signing_key && (
            <div className="muted small" style={{ marginTop: '0.6rem' }}>
              Restoring with an admin&apos;s key instead of the kit (<code>dashboardd restore -identity … -signing-key …</code>) checks the backups with this key - keep it with
              yours: <code className="mono">{status.signing_key}</code>{' '}
              <button className="ghost small" onClick={copyKey} aria-label="Copy the signing key">
                <Copy size={12} />
              </button>
            </div>
          )}
        </Card>
      )}
    </div>
  )
}
