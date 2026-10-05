import { Copy, Download, Fingerprint, KeyRound, LogOut, MonitorSmartphone, Plus, ShieldCheck, Smartphone, Trash2 } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { call, postJSON } from './call.js'
import { Badge, ErrorBox, useAction, useToast } from './shared/ui.jsx'
import { createPasskey, passkeysSupported, signWithPasskey } from './webauthn.js'

// Second factors (dashboard/backend/mfa_handlers.go): the step that
// finishes a sign-in, setting one up - forced when the account's role
// needs one -, and the account's own list.

function when(t) {
  return t ? new Date(t).toLocaleString() : 'never'
}

// RecoveryCodes shows an account's recovery codes, this once.
export function RecoveryCodes({ name, codes, onDone }) {
  const toast = useToast()
  const text = `Janus Controller - recovery codes for ${name}\nEach works once, in place of a code from your app or a passkey.\n\n${codes.join('\n')}\n`
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text)
      toast('Recovery codes copied')
    } catch {
      toast('Select the codes and press Ctrl+C', 'warn')
    }
  }
  const download = () => {
    const url = URL.createObjectURL(new Blob([text], { type: 'text/plain' }))
    const a = document.createElement('a')
    a.href = url
    a.download = `janus-recovery-codes-${name}.txt`
    a.click()
    URL.revokeObjectURL(url)
  }
  return (
    <div className="stack">
      <div className="notice warn small">
        Keep these recovery codes with your password - in a password manager, or printed. Each one signs you in once without your app or passkey. They won&apos;t be shown again.
      </div>
      <pre className="recovery-codes mono">{codes.join('\n')}</pre>
      <div className="row">
        <button className="small" onClick={copy}>
          <Copy size={13} /> Copy
        </button>
        <button className="small" onClick={download}>
          <Download size={13} /> Download
        </button>
        <span className="grow" />
        <button className="primary" onClick={onDone}>
          I saved them
        </button>
      </div>
    </div>
  )
}

// trustLabel is how long a trusted browser skips the second factor.
function trustLabel(hours) {
  return hours % 24 === 0 ? `${hours / 24} day${hours === 24 ? '' : 's'}` : `${hours} hour${hours === 1 ? '' : 's'}`
}

// SecondFactorForm finishes a sign-in waiting for its second factor -
// offering to trust the browser, when the policy allows it -, or with
// confirming, confirms it in a sign-in a trusted browser skipped it in
// (what only a factor given now may do: adding an SSH key).
export function SecondFactorForm({ me, onDone, onSignOut, confirming = false }) {
  const here = (me.mfa?.passkeys || []).filter((p) => p.here)
  const elsewhere = (me.mfa?.passkeys || []).filter((p) => !p.here)
  const [recovery, setRecovery] = useState(!me.mfa?.totp && here.length === 0)
  const [code, setCode] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const trustHours = confirming ? 0 : me.mfa?.trust_hours || 0
  const [trust, setTrust] = useState(false)
  const attempt = async (fn) => {
    setBusy(true)
    setError(null)
    try {
      await fn()
      onDone()
    } catch (err) {
      setError(err.message)
      // The sign-in is over (too many tries): back to the password.
      if (err.status === 401) setTimeout(onDone, 1500)
    } finally {
      setBusy(false)
    }
  }
  const submit = (e) => {
    e.preventDefault()
    attempt(() => postJSON(recovery ? '/api/auth/mfa/recovery' : '/api/auth/mfa/totp', { code: code.trim(), trust: trust && trustHours > 0 }))
  }
  const passkey = () =>
    attempt(async () => {
      const options = await postJSON('/api/auth/mfa/passkey/begin')
      const answer = await signWithPasskey(options)
      await postJSON(`/api/auth/mfa/passkey/finish${trust && trustHours > 0 ? '?trust=1' : ''}`, answer)
    })
  const codeForm = me.mfa?.totp || recovery
  // Ticked before giving the factor: with the code's form, or above the
  // passkey when there's no form.
  const trustBox = trustHours > 0 && (
    <label className="check" title="Your password is still asked; you can forget this browser from your account">
      <input type="checkbox" checked={trust} onChange={(e) => setTrust(e.target.checked)} />
      <span>Trust this browser for {trustLabel(trustHours)} - no second factor here until then</span>
    </label>
  )
  return (
    <div className="stack">
      {!codeForm && trustBox}
      {here.length > 0 && passkeysSupported() && (
        <button className="primary" onClick={passkey} disabled={busy}>
          <Fingerprint size={15} /> Use a passkey
        </button>
      )}
      {codeForm && (
        <form className="stack" onSubmit={submit}>
          <label className="field">
            <span>{recovery ? 'A recovery code' : 'The code from your authenticator app'}</span>
            <input
              value={code}
              onChange={(e) => setCode(e.target.value)}
              required
              autoFocus={here.length === 0}
              autoComplete="one-time-code"
              inputMode={recovery ? 'text' : 'numeric'}
              placeholder={recovery ? 'XXXX-XXXX-XXXX' : '123456'}
              className="mono"
            />
          </label>
          {trustBox}
          <button className={here.length > 0 ? '' : 'primary'} type="submit" disabled={busy}>
            {busy ? 'Checking…' : 'Continue'}
          </button>
        </form>
      )}
      {elsewhere.length > 0 && here.length === 0 && (
        <div className="muted small">Your passkeys are for {[...new Set(elsewhere.map((p) => p.rp_id))].join(', ')}: open the Controller by that name to use them.</div>
      )}
      <ErrorBox error={error} />
      <div className="row">
        {me.mfa?.recovery_codes > 0 && (
          <button className="ghost small" onClick={() => (setRecovery(!recovery), setCode(''))}>
            {recovery ? (me.mfa?.totp ? 'Use your app instead' : '') : 'Use a recovery code'}
          </button>
        )}
        <span className="grow" />
        {onSignOut && (
          <button className="ghost small" onClick={onSignOut}>
            <LogOut size={14} /> Sign out
          </button>
        )}
      </div>
    </div>
  )
}

// TOTPSetup sets an authenticator app up: its QR code, its secret as text,
// and a code from it to check.
function TOTPSetup({ onEnabled, onCancel }) {
  const [setup, setSetup] = useState(null)
  const [code, setCode] = useState('')
  const [busy, run] = useAction()
  const start = async () => {
    const s = await run(() => postJSON('/api/auth/mfa/totp/setup'))
    if (s) setSetup(s)
  }
  const enable = async (e) => {
    e.preventDefault()
    const out = await run(() => postJSON('/api/auth/mfa/totp/enable', { code: code.trim() }), 'Authenticator app set up')
    if (out) onEnabled(out.recovery_codes)
  }
  if (!setup)
    return (
      <div className="row">
        <button className="primary" onClick={start} disabled={busy}>
          <Smartphone size={15} /> Set up an authenticator app
        </button>
        {onCancel && <button onClick={onCancel}>Cancel</button>}
      </div>
    )
  return (
    <form className="stack" onSubmit={enable}>
      <div className="totp-setup">
        <img src={`data:image/svg+xml;utf8,${encodeURIComponent(setup.qr_svg)}`} alt="QR code for your authenticator app" className="totp-qr" />
        <div className="stack" style={{ gap: '0.4rem' }}>
          <div className="small">Scan it with your app - Google Authenticator, Aegis, 1Password, Bitwarden… - or enter this key:</div>
          <code className="mono totp-secret">{setup.secret.match(/.{1,4}/g).join(' ')}</code>
          <label className="field">
            <span>Then the code it shows</span>
            <input
              value={code}
              onChange={(e) => setCode(e.target.value)}
              required
              autoFocus
              autoComplete="one-time-code"
              inputMode="numeric"
              placeholder="123456"
              className="mono"
            />
          </label>
        </div>
      </div>
      <div className="row" style={{ justifyContent: 'flex-end' }}>
        {onCancel && (
          <button type="button" onClick={onCancel}>
            Cancel
          </button>
        )}
        <button className="primary" type="submit" disabled={busy}>
          Check and turn on
        </button>
      </div>
    </form>
  )
}

// PasskeySetup registers a passkey - for the name the Controller is
// opened by.
function PasskeySetup({ rpId, onAdded, onCancel }) {
  const [name, setName] = useState('')
  const [busy, run] = useAction()
  const add = async (e) => {
    e.preventDefault()
    const out = await run(async () => {
      const options = await postJSON('/api/auth/mfa/passkeys/begin', { name: name.trim() })
      const answer = await createPasskey(options)
      return postJSON('/api/auth/mfa/passkeys/finish', answer)
    }, 'Passkey added')
    if (out) onAdded(out.recovery_codes)
  }
  return (
    <form className="stack" onSubmit={add}>
      <label className="field">
        <span>What holds it</span>
        <input value={name} onChange={(e) => setName(e.target.value)} required maxLength={60} placeholder="YubiKey, laptop, phone…" autoFocus />
      </label>
      <div className="muted small">
        For <span className="mono">{rpId}</span>: a passkey works where the Controller is opened by this name - with a certificate the browser trusts (a self-signed one clicked
        through isn&apos;t: browsers refuse passkeys there).
      </div>
      <div className="row" style={{ justifyContent: 'flex-end' }}>
        {onCancel && (
          <button type="button" onClick={onCancel}>
            Cancel
          </button>
        )}
        <button className="primary" type="submit" disabled={busy}>
          <Fingerprint size={15} /> Add the passkey
        </button>
      </div>
    </form>
  )
}

// AddFactor offers the two kinds; passkeys only where they can work.
function AddFactor({ me, onAdded, onCancel, first }) {
  const [kind, setKind] = useState(null)
  const canPasskey = !!me.mfa?.rp_id && passkeysSupported()
  if (kind === 'totp') return <TOTPSetup onEnabled={onAdded} onCancel={() => setKind(null)} />
  if (kind === 'passkey') return <PasskeySetup rpId={me.mfa.rp_id} onAdded={onAdded} onCancel={() => setKind(null)} />
  return (
    <div className="stack">
      <div className="row" style={{ flexWrap: 'wrap' }}>
        {(first || !me.mfa?.totp) && (
          <button className={first ? 'primary' : ''} onClick={() => setKind('totp')}>
            <Smartphone size={15} /> An authenticator app
          </button>
        )}
        <button onClick={() => setKind('passkey')} disabled={!canPasskey} title={canPasskey ? '' : 'Open the Controller by its name, not an IP address'}>
          <Fingerprint size={15} /> A passkey or security key
        </button>
        {onCancel && <button onClick={onCancel}>Cancel</button>}
      </div>
      {!me.mfa?.rp_id && <div className="muted small">Passkeys need the Controller opened by its name, not an IP address.</div>}
    </div>
  )
}

// EnrollMFA is the forced setup of an account whose role needs a second
// factor.
export function EnrollMFA({ me, onDone, onSignOut }) {
  const [codes, setCodes] = useState(null)
  if (codes) return <RecoveryCodes name={me.name} codes={codes} onDone={onDone} />
  return (
    <div className="stack">
      <div className="notice small">
        {me.role === 'admin' ? 'An admin' : 'Every account'} on this Controller needs a second factor: a code from an app on your phone, or a passkey. Set one up to continue.
      </div>
      <AddFactor me={me} first onAdded={(c) => (c?.length ? setCodes(c) : onDone())} />
      <div className="row">
        <span className="grow" />
        <button className="ghost small" onClick={onSignOut}>
          <LogOut size={14} /> Sign out
        </button>
      </div>
    </div>
  )
}

// MFAPanel is the account's own second factors.
export function MFAPanel({ me, onChanged }) {
  const [adding, setAdding] = useState(false)
  const [codes, setCodes] = useState(null)
  const [confirming, setConfirming] = useState(null) // {label, path, method}
  const [password, setPassword] = useState('')
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)
  const toast = useToast()
  const mfa = me.mfa || {}
  const confirm = async (e) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      const out = await call(confirming.path, { method: confirming.method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ password }) })
      setConfirming(null)
      setPassword('')
      if (out?.recovery_codes) setCodes(out.recovery_codes)
      else toast(confirming.done)
      onChanged()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }
  if (codes) return <RecoveryCodes name={me.name} codes={codes} onDone={() => setCodes(null)} />
  return (
    <div className="stack">
      {!mfa.totp && mfa.passkeys?.length === 0 && (
        <div className="muted small">No second factor yet{mfa.required ? ' - your role needs one' : ''}: a sign-in is your password alone.</div>
      )}
      <div className="factor-list">
        {mfa.totp && (
          <div className="factor">
            <Smartphone size={15} />
            <span className="grow">Authenticator app</span>
            <button
              className="ghost small danger"
              onClick={() => setConfirming({ label: 'Remove the authenticator app', path: '/api/auth/mfa/totp', method: 'DELETE', done: 'Authenticator app removed' })}
            >
              <Trash2 size={14} />
            </button>
          </div>
        )}
        {(mfa.passkeys || []).map((p) => (
          <div className="factor" key={p.id}>
            <Fingerprint size={15} />
            <span className="grow">
              {p.name}{' '}
              <span className="muted small">
                {p.rp_id} · used {when(p.last_used_at)}
              </span>{' '}
              {!p.here && <Badge>other name</Badge>}
            </span>
            <button
              className="ghost small danger"
              onClick={() => setConfirming({ label: `Remove the passkey “${p.name}”`, path: `/api/auth/mfa/passkeys/${p.id}`, method: 'DELETE', done: 'Passkey removed' })}
            >
              <Trash2 size={14} />
            </button>
          </div>
        ))}
        {(mfa.totp || mfa.passkeys?.length > 0) && (
          <div className="factor">
            <KeyRound size={15} />
            <span className="grow">
              Recovery codes <span className="muted small">{mfa.recovery_codes} left</span>
            </span>
            <button className="ghost small" onClick={() => setConfirming({ label: 'Make new recovery codes', path: '/api/auth/mfa/recovery-codes', method: 'POST' })}>
              New codes
            </button>
          </div>
        )}
      </div>
      {confirming ? (
        <form className="stack" onSubmit={confirm}>
          <label className="field">
            <span>{confirming.label}: your password</span>
            <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} required autoFocus autoComplete="current-password" />
          </label>
          <ErrorBox error={error} />
          <div className="row" style={{ justifyContent: 'flex-end' }}>
            <button type="button" onClick={() => (setConfirming(null), setError(null))}>
              Cancel
            </button>
            <button className="primary" type="submit" disabled={busy}>
              Confirm
            </button>
          </div>
        </form>
      ) : adding ? (
        <AddFactor
          me={me}
          onCancel={() => setAdding(false)}
          onAdded={(c) => {
            setAdding(false)
            if (c?.length) setCodes(c)
            onChanged()
          }}
        />
      ) : (
        <div>
          <button className="small" onClick={() => setAdding(true)}>
            <Plus size={14} /> Add a second factor
          </button>
        </div>
      )}
    </div>
  )
}

export function MFABadge({ on }) {
  return on ? (
    <Badge tone="ok">
      <ShieldCheck size={11} /> 2FA
    </Badge>
  ) : null
}

// TrustedBrowsers are the browsers the account trusts for its second
// factor - this one marked -, each forgotten in a click.
export function TrustedBrowsers({ me }) {
  const [list, setList] = useState(null)
  const [error, setError] = useState(null)
  const [busy, run] = useAction()
  const load = useCallback(() => {
    call('/api/auth/trusted-browsers').then(setList, setError)
  }, [])
  useEffect(load, [load])
  const forget = async (id) => {
    await run(() => call(`/api/auth/trusted-browsers${id ? `/${encodeURIComponent(id)}` : ''}`, { method: 'DELETE' }), id ? 'Browser forgotten' : 'Every browser forgotten')
    load()
  }
  const hours = me.mfa?.trust_hours || 0
  return (
    <div className="stack">
      <ErrorBox error={error} />
      {list && list.length === 0 && (
        <div className="muted small">
          {hours > 0
            ? `None: tick “Trust this browser” when giving your second factor to skip it there for ${trustLabel(hours)}.`
            : 'This Controller asks for the second factor at every sign-in.'}
        </div>
      )}
      {list && list.length > 0 && (
        <div className="factor-list">
          {list.map((b) => (
            <div className="factor" key={b.id}>
              <MonitorSmartphone size={15} />
              <span className="grow">
                {b.label} {b.current && <Badge tone="info">this browser</Badge>}{' '}
                <span className="muted small">
                  from {b.client} · used {when(b.last_used_at || b.created_at)} · until {when(b.expires_at)}
                </span>
              </span>
              <button className="ghost small danger" onClick={() => forget(b.id)} disabled={busy} aria-label={`Forget ${b.label}`} title="Forget: the second factor is asked there again">
                <Trash2 size={14} />
              </button>
            </div>
          ))}
        </div>
      )}
      {list && list.length > 1 && (
        <div>
          <button className="small" onClick={() => forget('')} disabled={busy}>
            Forget them all
          </button>
        </div>
      )}
    </div>
  )
}
