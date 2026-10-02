import { Copy, KeyRound, Plus, Trash2, X } from 'lucide-react'
import { useCallback, useEffect, useRef, useState } from 'react'
import { call, postJSON } from './call.js'
import { Badge, Card, ErrorBox, useAction, useConfirm, useToast } from './shared/ui.jsx'

// API tokens: what a program - the Terraform provider, a script - calls
// the Controller's API with (Authorization: Bearer). Shown once, when
// created; only the admin's session manages them.

const VALIDITY = [
  { days: 90, label: '90 days' },
  { days: 365, label: '1 year' },
  { days: 30, label: '30 days' },
  { days: 0, label: 'until revoked' },
]

function when(t) {
  return t ? new Date(t).toLocaleString() : '–'
}

function NewToken({ token, onClose }) {
  const ref = useRef(null)
  const toast = useToast()
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(token.token)
      toast('Token copied')
    } catch {
      ref.current?.select()
      toast('Select the token and press Ctrl+C', 'warn')
    }
  }
  return (
    <div className="notice warn stack" style={{ gap: '0.5rem' }}>
      <div className="spread">
        <strong>Token “{token.name}” - copy it now, it won’t be shown again</strong>
        <button className="ghost icon" onClick={onClose} aria-label="Close">
          <X size={16} />
        </button>
      </div>
      <div className="row">
        <input ref={ref} readOnly value={token.token} className="mono grow" onFocus={(e) => e.target.select()} />
        <button className="small" onClick={copy}>
          <Copy size={13} /> Copy
        </button>
      </div>
      <div className="small muted">
        Send it as <code>Authorization: Bearer …</code> - for the Terraform provider, in <code>JANUS_TOKEN</code> (docs/terraform.md).
      </div>
    </div>
  )
}

export default function TokensPage() {
  const [tokens, setTokens] = useState(null)
  const [error, setError] = useState(null)
  const [name, setName] = useState('')
  const [days, setDays] = useState(90)
  const [created, setCreated] = useState(null)
  const [busy, run] = useAction()
  const confirm = useConfirm()

  const load = useCallback(() => {
    call('/api/tokens').then(
      (t) => {
        setTokens(t || [])
        setError(null)
      },
      (err) => setError(err),
    )
  }, [])
  useEffect(load, [load])

  const create = async (e) => {
    e.preventDefault()
    const t = await run(() => postJSON('/api/tokens', { name: name.trim(), expires_in_days: Number(days) }))
    if (t) {
      setCreated(t)
      setName('')
      load()
    }
  }
  const revoke = async (t) => {
    const ok = await confirm({
      title: `Revoke “${t.name}”?`,
      body: <p>Whatever uses this token is refused from now on.</p>,
      action: 'Revoke',
      danger: true,
    })
    if (!ok) return
    await run(() => call(`/api/tokens/${t.id}`, { method: 'DELETE' }), `Revoked ${t.name}`)
    load()
  }

  return (
    <div className="stack">
      <div className="spread">
        <h1>API tokens</h1>
      </div>
      <p className="muted" style={{ margin: 0 }}>
        A token lets a program use this Controller&apos;s API - the Terraform provider creating and changing nodes, a script - with the same rights as you,
        except managing tokens. Only its fingerprint is kept: it&apos;s shown once.
      </p>
      {created && <NewToken token={created} onClose={() => setCreated(null)} />}
      <Card title="New token" icon={Plus}>
        <form className="row" onSubmit={create} style={{ flexWrap: 'wrap' }}>
          <label className="field grow">
            <span>Name (what uses it)</span>
            <input value={name} onChange={(e) => setName(e.target.value)} required maxLength={100} placeholder="terraform-prod" />
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
          <button className="primary" type="submit" disabled={busy} style={{ alignSelf: 'flex-end' }}>
            <KeyRound size={15} /> Create token
          </button>
        </form>
      </Card>
      <ErrorBox error={error} />
      {tokens && tokens.length === 0 && <div className="card empty">No token yet.</div>}
      {tokens && tokens.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Name</th>
                <th>Created</th>
                <th>Expires</th>
                <th>Last used</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {tokens.map((t) => (
                <tr key={t.id}>
                  <td>
                    <strong>{t.name}</strong> <span className="muted mono small">janus_{t.id}_…</span> {t.expired && <Badge tone="danger">expired</Badge>}
                  </td>
                  <td>{when(t.created_at)}</td>
                  <td>{t.expires_at ? when(t.expires_at) : 'never'}</td>
                  <td>{when(t.last_used_at)}</td>
                  <td style={{ textAlign: 'right' }}>
                    <button className="ghost small danger" onClick={() => revoke(t)} disabled={busy}>
                      <Trash2 size={14} /> Revoke
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
