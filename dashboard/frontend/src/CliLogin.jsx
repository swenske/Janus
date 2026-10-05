import { Check, Terminal, TriangleAlert, X } from 'lucide-react'
import { useState } from 'react'
import { call, postJSON } from './call.js'
import { atLeast, ROLES, useMe } from './me.jsx'
import { Card, ErrorBox, useAction } from './shared/ui.jsx'

// Signing janusctl in through this page (dashboard/backend/
// cli_browser.go): the account approves a certificate for the key
// janusctl made, by its fingerprint - from janusctl login -browser (the
// approval goes back to janusctl on 127.0.0.1) or -device (a code typed
// here).

function groups(hex) {
  return (hex || '').toUpperCase().match(/.{2}/g)?.join(':') || ''
}

function RoleSelect({ value, onChange }) {
  const me = useMe()
  return (
    <label className="field">
      <span>Its role</span>
      <select value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="">your role ({me.role})</option>
        {ROLES.filter((r) => r.id !== me.role && atLeast(me.role, r.id)).map((r) => (
          <option key={r.id} value={r.id}>
            {r.label}
          </option>
        ))}
      </select>
    </label>
  )
}

// CliLoginPage is janusctl login -browser's page.
export function CliLoginPage({ route }) {
  const me = useMe()
  const q = new URLSearchParams(route.split('?')[1] || '')
  const port = q.get('port') || ''
  const state = q.get('state') || ''
  const key = (q.get('key') || '').toLowerCase()
  const [role, setRole] = useState('')
  const [busy, run] = useAction()
  const valid = /^\d{2,5}$/.test(port) && Number(port) > 1023 && /^[0-9a-f]{64}$/.test(key) && state.length >= 16
  const back = (params) => {
    window.location.href = `http://127.0.0.1:${port}/callback?${new URLSearchParams({ state, ...params })}`
  }
  const approve = async () => {
    const g = await run(() => postJSON('/api/cli/grant', { key_fingerprint: key, role }))
    if (g?.code) back({ code: g.code })
  }
  if (!valid) return <ErrorBox error="This sign-in link isn't one janusctl made: run janusctl login again." />
  return (
    <div className="stack cli-login">
      <Card title="Sign janusctl in" icon={Terminal}>
        <div className="stack">
          <p style={{ margin: 0 }}>
            janusctl, on this machine, asks for a certificate for <strong>{me.name}</strong>: it reaches the nodes with it for 12 hours.
          </p>
          <div>
            <div className="muted small">Its key - the one janusctl printed:</div>
            <code className="mono key-fp">{groups(key)}</code>
          </div>
          <RoleSelect value={role} onChange={setRole} />
          <div className="row">
            <button className="primary" onClick={approve} disabled={busy}>
              <Check size={15} /> Approve
            </button>
            <button onClick={() => back({ error: 'denied' })} disabled={busy}>
              <X size={15} /> Deny
            </button>
          </div>
        </div>
      </Card>
    </div>
  )
}

// CliDevicePage is janusctl login -device's page: its code typed here.
export function CliDevicePage({ route }) {
  const q = new URLSearchParams(route.split('?')[1] || '')
  const [code, setCode] = useState(q.get('code') || '')
  const [request, setRequest] = useState(null)
  const [done, setDone] = useState(null)
  const [error, setError] = useState(null)
  const [role, setRole] = useState('')
  const [busy, run] = useAction()
  const lookUp = async (e) => {
    e.preventDefault()
    setError(null)
    try {
      setRequest(await call(`/api/cli/device/${encodeURIComponent(code.trim())}`))
    } catch (err) {
      setError(err.message)
    }
  }
  const decide = async (approve) => {
    const ok = await run(() => postJSON(`/api/cli/device/${encodeURIComponent(request.user_code)}/${approve ? 'approve' : 'deny'}`, approve ? { role } : {}).then(() => true))
    if (ok) setDone(approve ? 'Approved: janusctl gets its certificate.' : 'Denied: janusctl stays signed out.')
  }
  return (
    <div className="stack cli-login">
      <Card title="Sign janusctl in from another machine" icon={Terminal}>
        {done ? (
          <div className="notice">{done}</div>
        ) : !request ? (
          <form className="stack" onSubmit={lookUp}>
            <label className="field">
              <span>The code janusctl login -device shows</span>
              <input value={code} onChange={(e) => setCode(e.target.value.toUpperCase())} required autoFocus placeholder="XXXX-XXXX" className="mono" maxLength={9} />
            </label>
            <ErrorBox error={error} />
            <div>
              <button className="primary" type="submit">
                Continue
              </button>
            </div>
          </form>
        ) : (
          <div className="stack">
            <div className="notice warn small">
              <TriangleAlert size={14} /> Only approve a code you started yourself: whoever started it gets a certificate for your account.
            </div>
            <dl className="kv small">
              <dt>Code</dt>
              <dd className="mono">{request.user_code}</dd>
              <dt>Asked from</dt>
              <dd className="mono">{request.client}</dd>
              <dt>At</dt>
              <dd>{new Date(request.created_at).toLocaleString()}</dd>
              <dt>Key</dt>
              <dd className="mono key-fp">{groups(request.key_fingerprint)}</dd>
            </dl>
            <RoleSelect value={role} onChange={setRole} />
            <div className="row">
              <button className="primary" onClick={() => decide(true)} disabled={busy}>
                <Check size={15} /> Approve
              </button>
              <button onClick={() => decide(false)} disabled={busy}>
                <X size={15} /> Deny
              </button>
            </div>
          </div>
        )}
      </Card>
    </div>
  )
}
