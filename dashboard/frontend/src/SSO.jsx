import { Link2, LogIn, Trash2, Unlink } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { call, postJSON } from './call.js'
import { Badge, ErrorBox, useAction, useConfirm } from './shared/ui.jsx'

// Single sign-on (dashboard/backend/sso_login.go): the page asks the
// Controller to start, goes to the provider, comes back to #/sso-done and
// finishes - signed in, or the identity linked to the account signed in.

// SSO_DONE is the route the provider's sign-in comes back to.
export const SSO_DONE = '#/sso-done'

function when(t) {
  return t ? new Date(t).toLocaleString() : 'never'
}

// returnRoute is the page's route to come back to - the janusctl sign-in
// page's included (#/cli-login?...).
function returnRoute() {
  const h = window.location.hash
  return h.startsWith('#/') && !h.startsWith(SSO_DONE) ? h : '#/'
}

// go sends the browser to the provider: a navigation, not a form (the
// page's policy allows forms to this origin only).
async function go(path, body) {
  const { redirect } = await postJSON(path, { return: returnRoute(), ...body })
  window.location.assign(redirect)
}

// SSOButtons are the sign-in page's "Sign in with ..." - one per provider.
export function SSOButtons({ providers }) {
  const [busy, setBusy] = useState(null)
  const [error, setError] = useState(null)
  if (!providers?.length) return null
  const start = async (p) => {
    setBusy(p.id)
    setError(null)
    try {
      await go(`/api/auth/sso/${encodeURIComponent(p.id)}/start`)
    } catch (err) {
      setError(err.message)
      setBusy(null)
    }
  }
  return (
    <div className="stack sso-buttons">
      <div className="or-divider" role="separator">
        <span>or</span>
      </div>
      {providers.map((p) => (
        <button key={p.id} type="button" onClick={() => start(p)} disabled={!!busy} className="sso-button">
          <LogIn size={15} /> {busy === p.id ? 'Going to ' + p.name + '…' : `Sign in with ${p.name}`}
        </button>
      ))}
      <ErrorBox error={error} />
    </div>
  )
}

// leave goes to route, a route of the page, without the sign-in's.
function leave(route, onDone) {
  window.history.replaceState(null, '', '/' + (route || '#/'))
  onDone()
}

// SSODone finishes a sign-in the provider sent the browser back from,
// then goes where the page was - or says what went wrong, with the
// reference the Controller's log has.
export function SSODone({ onDone }) {
  const [failure, setFailure] = useState(null)
  const started = useRef(false)
  useEffect(() => {
    if (started.current) return // once: a sign-in finishes once
    started.current = true
    postJSON('/api/auth/sso/finish').then(
      (out) => leave(out?.return, onDone),
      (err) => setFailure({ message: err.message, ref: err.data?.ref }),
    )
  }, [onDone])
  if (!failure) return <div className="muted">Finishing the sign-in…</div>
  return (
    <div className="stack">
      <ErrorBox error={failure.message} />
      {failure.ref && (
        <div className="muted small">
          Reference <code>{failure.ref}</code>: the Controller&apos;s log says more under it.
        </div>
      )}
      <button className="primary" onClick={() => leave('#/', onDone)}>
        Back
      </button>
    </div>
  )
}

// Identities is the account's own identities at single sign-on
// providers, in its dialog: linking one (a password account, signed in
// with its second factor), forgetting one.
export function Identities({ me }) {
  const [password, setPassword] = useState('')
  const [linking, setLinking] = useState(null)
  const [error, setError] = useState(null)
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const providers = me.sso_providers || []
  const name = (id) => providers.find((p) => p.id === id)?.name || id
  if (me.sso) {
    return (
      <div className="stack">
        <div className="muted small">
          {name(me.sso.provider)} made this account: you sign in through it, and your role is what its rules give your groups there.{' '}
          {me.sso.valid_until && <>It works until {when(me.sso.valid_until)} - each sign-in through {name(me.sso.provider)} extends that.</>}
        </div>
      </div>
    )
  }
  const forget = async (i) => {
    const ok = await confirm({ title: `Forget ${i.username} at ${name(i.provider)}?`, body: <p>Signing in through {name(i.provider)} no longer reaches this account.</p>, action: 'Forget', danger: true })
    if (ok && (await run(() => call(`/api/auth/identities/${encodeURIComponent(i.id)}`, { method: 'DELETE' }), 'Identity forgotten')) !== undefined) me.refresh()
  }
  const link = async (e) => {
    e.preventDefault()
    setError(null)
    try {
      await go(`/api/auth/sso/${encodeURIComponent(linking)}/link`, { password })
    } catch (err) {
      setError(err.message)
    }
  }
  return (
    <div className="stack">
      {me.identities?.length > 0 ? (
        <div className="factor-list">
          {me.identities.map((i) => (
            <div className="factor" key={i.id}>
              <Link2 size={15} />
              <span className="grow">
                {i.username} <span className="muted small">at {name(i.provider)} · used {when(i.last_used_at)}</span>
              </span>
              <button className="ghost small danger" onClick={() => forget(i)} disabled={busy} title="Forget it">
                <Trash2 size={14} />
              </button>
            </div>
          ))}
        </div>
      ) : (
        <div className="muted small">None: you sign in with your password{providers.length ? `, or link an identity to sign in through ${providers.map((p) => p.name).join(' or ')} too` : ''}.</div>
      )}
      {linking ? (
        <form className="stack" onSubmit={link}>
          <label className="field">
            <span>Link an identity at {name(linking)}: your password</span>
            <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} required autoFocus autoComplete="current-password" />
          </label>
          <div className="muted small">{name(linking)} asks you to sign in there; its identity then signs you in here too, as you, with your role.</div>
          <ErrorBox error={error} />
          <div className="row" style={{ justifyContent: 'flex-end' }}>
            <button type="button" onClick={() => (setLinking(null), setError(null))}>
              Cancel
            </button>
            <button className="primary" type="submit">
              Continue at {name(linking)}
            </button>
          </div>
        </form>
      ) : (
        providers.length > 0 && (
          <div className="row" style={{ flexWrap: 'wrap' }}>
            {providers.map((p) => (
              <button key={p.id} className="small" onClick={() => setLinking(p.id)}>
                <Link2 size={14} /> Link {p.name}
              </button>
            ))}
          </div>
        )
      )}
    </div>
  )
}

// SSOBadge marks an account a provider made - lapsed when it must sign in
// through it again.
export function SSOBadge({ sso }) {
  if (!sso) return null
  return sso.lapsed ? (
    <span title="It signs in through its provider again before anything works">
      <Badge tone="warn">
        <Unlink size={11} /> {sso.provider}: lapsed
      </Badge>
    </span>
  ) : (
    <span title="A single sign-on provider made it: its role comes from the provider's rules">
      <Badge tone="info">
        <Link2 size={11} /> {sso.provider}
      </Badge>
    </span>
  )
}
