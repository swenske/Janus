import { Copy, KeyRound, Plus, Trash2 } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { call, postJSON } from './call.js'
import { atLeast, ROLES } from './me.jsx'
import { Badge, ErrorBox, useAction, useConfirm, useToast } from './shared/ui.jsx'

// The account's SSH keys: what janusctl signs in with (janusctl login
// -ssh-key) - dashboard/backend/cli.go. Adding one needs a sign-in that
// gave a second factor: the key then signs janusctl in alone.

const VALIDITY = [
  { days: 0, label: 'until removed' },
  { days: 365, label: '1 year' },
  { days: 90, label: '90 days' },
  { days: 30, label: '30 days' },
]

function when(t) {
  return t ? new Date(t).toLocaleDateString() : 'never'
}

function colonHex(h) {
  return (h || '').toUpperCase().match(/.{2}/g)?.join(':') || ''
}

export function SSHKeys({ me }) {
  const [keys, setKeys] = useState(null)
  const [info, setInfo] = useState(null)
  const [error, setError] = useState(null)
  const [adding, setAdding] = useState(false)
  const [name, setName] = useState('')
  const [line, setLine] = useState('')
  const [role, setRole] = useState('')
  const [days, setDays] = useState(0)
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const toast = useToast()
  const hasFactor = me.mfa?.totp || me.mfa?.passkeys?.length > 0

  const load = useCallback(() => {
    call('/api/auth/ssh-keys').then(setKeys, setError)
  }, [])
  useEffect(() => {
    load()
    call('/api/controller-info').then(setInfo, () => {})
  }, [load])

  const add = async (e) => {
    e.preventDefault()
    const k = await run(() => postJSON('/api/auth/ssh-keys', { name: name.trim(), public_key: line.trim(), role, expires_in_days: Number(days) }), 'SSH key added')
    if (k) {
      setAdding(false)
      setName('')
      setLine('')
      load()
    }
  }
  const remove = async (k) => {
    const ok = await confirm({
      title: `Remove the key “${k.name}”?`,
      body: <p>janusctl stops signing in with it. Certificates it already got end within 12 hours.</p>,
      action: 'Remove',
      danger: true,
    })
    if (!ok) return
    await run(() => call(`/api/auth/ssh-keys/${encodeURIComponent(k.fingerprint)}`, { method: 'DELETE' }), 'SSH key removed')
    load()
  }
  const host = window.location.host
  const command = `janusctl login -controller ${host} -controller-fingerprint ${colonHex(info?.fingerprint)} -user ${me.name} -ssh-key ~/.ssh/id_ed25519.pub`
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(command)
      toast('Command copied')
    } catch {
      toast('Select the command and press Ctrl+C', 'warn')
    }
  }

  return (
    <div className="stack">
      <ErrorBox error={error} />
      {keys && keys.length === 0 && <div className="muted small">No key yet: janusctl signs in with one of your SSH keys, then reaches the nodes directly.</div>}
      {keys && keys.length > 0 && (
        <div className="factor-list">
          {keys.map((k) => (
            <div className="factor" key={k.fingerprint}>
              <KeyRound size={15} />
              <span className="grow">
                {k.name} <span className="muted small mono">{k.fingerprint.slice(0, 20)}…</span> <Badge>{k.role || me.role}</Badge>{' '}
                <span className="muted small">
                  used {when(k.last_used_at)}
                  {k.expires_at && ` · until ${when(k.expires_at)}`}
                </span>
              </span>
              <button className="ghost small danger" onClick={() => remove(k)} disabled={busy} aria-label={`Remove ${k.name}`}>
                <Trash2 size={14} />
              </button>
            </div>
          ))}
        </div>
      )}
      {!hasFactor ? (
        <div className="muted small">Adding an SSH key needs a second factor: set one up above, then sign in again.</div>
      ) : adding ? (
        <form className="stack" onSubmit={add}>
          <label className="field">
            <span>Public key (the line of your .pub file)</span>
            <textarea value={line} onChange={(e) => setLine(e.target.value)} required rows={3} className="mono" placeholder="ssh-ed25519 AAAA… you@laptop" autoFocus />
          </label>
          <div className="row" style={{ flexWrap: 'wrap' }}>
            <label className="field grow">
              <span>Name</span>
              <input value={name} onChange={(e) => setName(e.target.value)} required maxLength={60} placeholder="laptop" />
            </label>
            <label className="field">
              <span>Role</span>
              <select value={role} onChange={(e) => setRole(e.target.value)}>
                <option value="">your role ({me.role})</option>
                {ROLES.filter((r) => r.id !== me.role && atLeast(me.role, r.id)).map((r) => (
                  <option key={r.id} value={r.id}>
                    {r.label}
                  </option>
                ))}
              </select>
            </label>
            <label className="field">
              <span>Valid</span>
              <select value={days} onChange={(e) => setDays(e.target.value)}>
                {VALIDITY.map((v) => (
                  <option key={v.days} value={v.days}>
                    {v.label}
                  </option>
                ))}
              </select>
            </label>
          </div>
          <div className="muted small">
            Ed25519 - from ssh-agent or its file -, ECDSA or RSA from its file. Not a FIDO key (sk-…): it can&apos;t sign janusctl&apos;s connections.
          </div>
          <div className="row" style={{ justifyContent: 'flex-end' }}>
            <button type="button" onClick={() => setAdding(false)}>
              Cancel
            </button>
            <button className="primary" type="submit" disabled={busy}>
              Add the key
            </button>
          </div>
        </form>
      ) : (
        <div>
          <button className="small" onClick={() => setAdding(true)}>
            <Plus size={14} /> Add an SSH key
          </button>
        </div>
      )}
      {keys && keys.length > 0 && info && (
        <div className="stack" style={{ gap: '0.3rem' }}>
          <div className="muted small">Then, on your machine (a certificate for 12 hours, renewed by itself while the key is in ssh-agent):</div>
          <div className="row">
            <code className="mono small grow copy-command">{command}</code>
            <button className="ghost small" onClick={copy} aria-label="Copy the command">
              <Copy size={13} />
            </button>
          </div>
        </div>
      )}
    </div>
  )
}
