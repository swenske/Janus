import { Copy, Ticket, Trash2, X } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { call, postJSON } from './call.js'
import { labelString, parseLabels } from './me.jsx'
import { Badge, Card, ErrorBox, useAction, useConfirm, useToast } from './shared/ui.jsx'

// Enrollment tokens (admins, dashboard/backend/enroll_api.go): each admits
// a batch of nodes - bare metal, a rack - without the approval step, and
// labels them. A node presents it as its registration token.

const DAYS = [1, 7, 30, 90, 365]

function when(t) {
  return t ? new Date(t).toLocaleString() : '–'
}

// Created shows a new token, once, and where it goes.
function Created({ token, onClose }) {
  const toast = useToast()
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(token.token)
      toast('Token copied')
    } catch {
      toast('Select the token and press Ctrl+C', 'warn')
    }
  }
  return (
    <div className="notice" style={{ marginBottom: '0.8rem' }}>
      <div className="spread">
        <strong>Enrollment token “{token.name}” - copy it now, it won&apos;t be shown again</strong>
        <button className="ghost icon" onClick={onClose} title="Close">
          <X size={15} />
        </button>
      </div>
      <div className="row" style={{ margin: '0.5rem 0' }}>
        <input className="mono grow" readOnly value={token.token} onFocus={(e) => e.target.select()} aria-label="The enrollment token" />
        <button className="small" onClick={copy}>
          <Copy size={13} /> Copy
        </button>
      </div>
      <div className="small">
        With the provisioning above: <code>-registration-token {token.token.slice(0, 22)}…</code> on <code>janusctl lifecycle install</code> or{' '}
        <code>image seed-controller</code>, or <code>&quot;registration_token&quot;</code> in the NoCloud user-data. Each node that presents it is admitted at once
        {Object.keys(token.labels).length > 0 && (
          <>
            {' '}
            labelled <span className="mono">{labelString(token.labels)}</span>
          </>
        )}
        {` - up to ${token.max_uses} node${token.max_uses === 1 ? '' : 's'}, until ${when(token.expires_at)}.`}
      </div>
    </div>
  )
}

export default function EnrollTokens() {
  const [tokens, setTokens] = useState(null)
  const [error, setError] = useState(null)
  const [name, setName] = useState('')
  const [uses, setUses] = useState(10)
  const [days, setDays] = useState(7)
  const [labels, setLabels] = useState('')
  const [created, setCreated] = useState(null)
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const load = useCallback(() => {
    call('/api/enroll-tokens').then(
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
    const t = await run(() => postJSON('/api/enroll-tokens', { name: name.trim(), max_uses: Number(uses), expires_in_days: Number(days), labels: parseLabels(labels) }))
    if (t) {
      setCreated(t)
      setName('')
      setLabels('')
      load()
    }
  }
  const revoke = async (t) => {
    const ok = await confirm({
      title: `Revoke “${t.name}”?`,
      body: <p>A node that presents it from now on waits for approval. The nodes it admitted stay.</p>,
      action: 'Revoke',
      danger: true,
    })
    if (!ok) return
    await run(() => call(`/api/enroll-tokens/${t.id}`, { method: 'DELETE' }), `Revoked ${t.name}`)
    load()
  }
  return (
    <Card title="Enrollment tokens" icon={Ticket}>
      <p className="muted small" style={{ marginTop: 0 }}>
        A batch of nodes - a rack, bare metal - admitted without the approval step: each that presents the token is admitted at once, with its labels (so the grants
        that pick them), until it&apos;s used up or expires.
      </p>
      {created && <Created token={created} onClose={() => setCreated(null)} />}
      <form className="row" onSubmit={create} style={{ flexWrap: 'wrap', alignItems: 'flex-end' }}>
        <label className="field grow">
          <span>Name</span>
          <input value={name} onChange={(e) => setName(e.target.value)} required maxLength={100} placeholder="rack-3" />
        </label>
        <label className="field">
          <span>Nodes</span>
          <input type="number" min={1} max={1000} value={uses} onChange={(e) => setUses(e.target.value)} style={{ width: '6rem' }} />
        </label>
        <label className="field">
          <span>Valid</span>
          <select value={days} onChange={(e) => setDays(e.target.value)}>
            {DAYS.map((d) => (
              <option key={d} value={d}>
                {d === 1 ? '1 day' : `${d} days`}
              </option>
            ))}
          </select>
        </label>
        <label className="field grow">
          <span>Labels</span>
          <input className="mono" value={labels} onChange={(e) => setLabels(e.target.value)} placeholder="team=web, env=prod" />
        </label>
        <button className="primary" type="submit" disabled={busy}>
          <Ticket size={15} /> Create
        </button>
      </form>
      <ErrorBox error={error} />
      {tokens && tokens.length > 0 && (
        <div className="table-wrap" style={{ marginTop: '0.8rem' }}>
          <table>
            <thead>
              <tr>
                <th>Name</th>
                <th>Labels</th>
                <th>Nodes</th>
                <th>Expires</th>
                <th>By</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {tokens.map((t) => (
                <tr key={t.id} className={t.usable ? '' : 'muted'}>
                  <td>
                    <strong>{t.name}</strong> <span className="muted mono small">janus-enroll_{t.id}_…</span> {!t.usable && <Badge>spent</Badge>}
                  </td>
                  <td className="mono small">{labelString(t.labels) || '–'}</td>
                  <td>
                    {t.uses} / {t.max_uses}
                  </td>
                  <td>{when(t.expires_at)}</td>
                  <td>{t.created_by}</td>
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
    </Card>
  )
}
