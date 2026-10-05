import { Clock, Copy, KeyRound, KeySquare, Plus, RotateCcw, Tags, Trash2, UserRound, Users, X } from 'lucide-react'
import { useCallback, useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { call, postJSON } from './call.js'
import { DOMAINS, labelString, parseLabels, ROLES, useMe } from './me.jsx'
import { MFABadge } from './MFA.jsx'
import { Badge, Card, ErrorBox, useAction, useConfirm, useToast } from './shared/ui.jsx'

// Accounts (admin): who may sign in, with which role, and the session
// policy - dashboard/backend/internal/auth.

function when(t) {
  return t ? new Date(t).toLocaleString() : 'never'
}

// ACCOUNT_ROLES: an account's role over everything - or none, then only
// its grants.
const ACCOUNT_ROLES = [...ROLES, { id: 'none', label: 'None - its grants only', about: 'reaches only the nodes its grants give it, none of the Controller own pages' }]

// grantSummary is one grant, in a few words.
function grantSummary(g) {
  const where = Object.keys(g.selector || {}).length ? labelString(g.selector) : 'every node'
  const what = g.domains?.length ? g.domains.join(', ') : 'every domain'
  return `${g.role} on ${where} - ${what}`
}

// GrantsEditor edits an account's grants: a role on the nodes whose
// labels match, narrowed to some domains.
function GrantsEditor({ user, onClose, onSaved }) {
  const [rows, setRows] = useState(() => (user.grants || []).map((g) => ({ role: g.role, labels: labelString(g.selector), domains: g.domains || [] })))
  const [error, setError] = useState(null)
  const [busy, run] = useAction()
  const set = (i, patch) => setRows(rows.map((r, j) => (j === i ? { ...r, ...patch } : r)))
  const save = async () => {
    let grants
    try {
      grants = rows.map((r) => ({ role: r.role, selector: parseLabels(r.labels), domains: r.domains }))
    } catch (err) {
      setError(err.message)
      return
    }
    setError(null)
    const out = await run(() => postJSON(`/api/users/${encodeURIComponent(user.name)}`, { grants }, 'PATCH'), `${user.name}'s grants saved`)
    if (out) onSaved()
  }
  return createPortal(
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal card wide" role="dialog" aria-modal="true" onClick={(e) => e.stopPropagation()}>
        <h2 style={{ marginBottom: '0.4rem' }}>{user.name}&apos;s grants</h2>
        <p className="muted small" style={{ marginTop: 0 }}>
          A role on the nodes whose labels match (empty: every node), narrowed to some domains (none: all of them). Reading a node&apos;s state comes with any grant on it;
          the account&apos;s strongest permission on a node wins.
        </p>
        <div className="stack" style={{ gap: '0.7rem' }}>
          {rows.length === 0 && <div className="muted">No grant.</div>}
          {rows.map((r, i) => (
            <div key={i} className="card" style={{ padding: '0.7rem' }}>
              <div className="row" style={{ alignItems: 'flex-end' }}>
                <label className="field">
                  <span>Role</span>
                  <select value={r.role} onChange={(e) => set(i, { role: e.target.value })} aria-label={`Grant ${i + 1}'s role`}>
                    {ROLES.map((x) => (
                      <option key={x.id} value={x.id}>
                        {x.label}
                      </option>
                    ))}
                  </select>
                </label>
                <label className="field grow">
                  <span>On the nodes labelled</span>
                  <input className="mono" value={r.labels} onChange={(e) => set(i, { labels: e.target.value })} placeholder="team=web, env=prod - empty: every node" aria-label={`Grant ${i + 1}'s labels`} />
                </label>
                <button className="ghost small danger" onClick={() => setRows(rows.filter((_, j) => j !== i))} title="Remove this grant">
                  <Trash2 size={14} />
                </button>
              </div>
              <div className="row" style={{ marginTop: '0.5rem', flexWrap: 'wrap', gap: '0.8rem' }}>
                <span className="muted small">Only:</span>
                {DOMAINS.map((d) => (
                  <label key={d.id} className="check small" title={d.about}>
                    <input
                      type="checkbox"
                      checked={r.domains.includes(d.id)}
                      onChange={(e) => set(i, { domains: e.target.checked ? [...r.domains, d.id] : r.domains.filter((x) => x !== d.id) })}
                    />{' '}
                    {d.label}
                  </label>
                ))}
                {!r.domains.length && <span className="muted small">(none ticked: every domain)</span>}
              </div>
            </div>
          ))}
          <div>
            <button className="small" onClick={() => setRows([...rows, { role: 'operator', labels: '', domains: [] }])}>
              <Plus size={14} /> Add a grant
            </button>
          </div>
          <ErrorBox error={error} />
          <div className="row" style={{ justifyContent: 'flex-end' }}>
            <button onClick={onClose}>Cancel</button>
            <button className="primary" onClick={save} disabled={busy}>
              Save
            </button>
          </div>
        </div>
      </div>
    </div>,
    document.body,
  )
}

export function roleTone(role) {
  return role === 'admin' ? 'accent' : role === 'operator' ? 'info' : undefined
}

// GivenPassword shows a password the Controller made for an account, once.
function GivenPassword({ given, onClose }) {
  const ref = useRef(null)
  const toast = useToast()
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(given.password)
      toast('Password copied')
    } catch {
      ref.current?.select()
      toast('Select the password and press Ctrl+C', 'warn')
    }
  }
  return (
    <div className="notice warn stack" style={{ gap: '0.5rem' }}>
      <div className="spread">
        <strong>
          {given.reset ? 'New password' : 'Password'} for {given.name} - hand it over now, it won’t be shown again
        </strong>
        <button className="ghost icon" onClick={onClose} aria-label="Close">
          <X size={16} />
        </button>
      </div>
      <div className="row">
        <input ref={ref} readOnly value={given.password} className="mono grow" onFocus={(e) => e.target.select()} />
        <button className="small" onClick={copy}>
          <Copy size={13} /> Copy
        </button>
      </div>
      <div className="small muted">{given.name} chooses their own at the first sign-in.</div>
    </div>
  )
}

function NewAccount({ onCreated }) {
  const [name, setName] = useState('')
  const [role, setRole] = useState('operator')
  const [busy, run] = useAction()
  const submit = async (e) => {
    e.preventDefault()
    const u = await run(() => postJSON('/api/users', { name: name.trim(), role }))
    if (u) {
      setName('')
      onCreated(u)
    }
  }
  return (
    <Card title="New account" icon={Plus}>
      <form className="row" onSubmit={submit} style={{ flexWrap: 'wrap' }}>
        <label className="field grow">
          <span>Name</span>
          <input value={name} onChange={(e) => setName(e.target.value.toLowerCase())} required pattern="[a-z0-9][a-z0-9._@\-]{0,63}" placeholder="alice" />
        </label>
        <label className="field">
          <span>Role</span>
          <select value={role} onChange={(e) => setRole(e.target.value)}>
            {ACCOUNT_ROLES.map((r) => (
              <option key={r.id} value={r.id}>
                {r.label}
              </option>
            ))}
          </select>
        </label>
        <button className="primary" type="submit" disabled={busy} style={{ alignSelf: 'flex-end' }}>
          <UserRound size={15} /> Make the account
        </button>
      </form>
      <div className="muted small" style={{ marginTop: '0.5rem' }}>
        {ACCOUNT_ROLES.find((r) => r.id === role)?.about}. The Controller makes a password to hand over; its owner chooses their own at the first sign-in.
      </div>
    </Card>
  )
}

function SessionPolicy() {
  const [policy, setPolicy] = useState(null)
  const [error, setError] = useState(null)
  const [busy, run] = useAction()
  useEffect(() => {
    call('/api/settings').then(setPolicy, setError)
  }, [])
  if (error) return <ErrorBox error={error} />
  if (!policy) return null
  const save = async (e) => {
    e.preventDefault()
    const p = await run(
      () =>
        postJSON(
          '/api/settings',
          { session_idle_minutes: Number(policy.session_idle_minutes), session_max_hours: Number(policy.session_max_hours), mfa_required: policy.mfa_required },
          'PUT',
        ),
      'Session policy saved',
    )
    if (p) setPolicy(p)
  }
  return (
    <Card title="Sessions" icon={Clock}>
      <form className="row" onSubmit={save} style={{ flexWrap: 'wrap' }}>
        <label className="field">
          <span>Ends after idle (minutes)</span>
          <input type="number" min={5} max={1440} value={policy.session_idle_minutes} onChange={(e) => setPolicy({ ...policy, session_idle_minutes: e.target.value })} required />
        </label>
        <label className="field">
          <span>Ends after sign-in at most (hours)</span>
          <input type="number" min={1} max={720} value={policy.session_max_hours} onChange={(e) => setPolicy({ ...policy, session_max_hours: e.target.value })} required />
        </label>
        <label className="field">
          <span>A second factor is needed by</span>
          <select value={policy.mfa_required} onChange={(e) => setPolicy({ ...policy, mfa_required: e.target.value })}>
            <option value="admins">admins</option>
            <option value="everyone">everyone</option>
            <option value="nobody">nobody</option>
          </select>
        </label>
        <button className="primary" type="submit" disabled={busy} style={{ alignSelf: 'flex-end' }}>
          Save
        </button>
      </form>
      <div className="muted small" style={{ marginTop: '0.5rem' }}>
        For every session, the live ones too. A page left open without anyone touching it doesn&apos;t count as use. An account the second-factor policy covers sets one up at its
        next sign-in.
      </div>
    </Card>
  )
}

export default function UsersPage() {
  const me = useMe()
  const [users, setUsers] = useState(null)
  const [error, setError] = useState(null)
  const [given, setGiven] = useState(null)
  const [grantsOf, setGrantsOf] = useState(null)
  const [busy, run] = useAction()
  const confirm = useConfirm()

  const load = useCallback(() => {
    call('/api/users').then((u) => {
      setUsers(u || [])
      setError(null)
    }, setError)
  }, [])
  useEffect(load, [load])

  const patch = async (u, body, success) => {
    const out = await run(() => postJSON(`/api/users/${encodeURIComponent(u.name)}`, body, 'PATCH'), success)
    load()
    return out
  }
  const reset = async (u) => {
    const ok = await confirm({
      title: `Reset ${u.name}'s password?`,
      body: <p>The Controller makes a new one for you to hand over. Their sessions end, and they choose their own at the next sign-in. Their API tokens keep working.</p>,
      action: 'Reset',
      danger: true,
    })
    if (!ok) return
    const out = await patch(u, { reset_password: true })
    if (out?.password) setGiven({ name: u.name, password: out.password, reset: true })
  }
  const resetMFA = async (u) => {
    const ok = await confirm({
      title: `Reset ${u.name}'s second factors?`,
      body: (
        <p>
          Their authenticator app, passkeys and recovery codes are forgotten and their sessions end. They sign in with their password and set a factor up again if their role needs
          one.
        </p>
      ),
      action: 'Reset',
      danger: true,
    })
    if (ok) patch(u, { reset_mfa: true }, `${u.name}'s second factors reset`)
  }
  const toggle = async (u) => {
    if (!u.disabled) {
      const ok = await confirm({
        title: `Disable ${u.name}?`,
        body: <p>Their sessions end and their API tokens stop working until the account is enabled again.</p>,
        action: 'Disable',
        danger: true,
      })
      if (!ok) return
    }
    patch(u, { disabled: !u.disabled }, `${u.name} ${u.disabled ? 'enabled' : 'disabled'}`)
  }
  const revokeKeys = async (u) => {
    const n = u.ssh_keys.length
    const ok = await confirm({
      title: `Revoke ${u.name}'s SSH key${n === 1 ? '' : 's'}?`,
      body: (
        <>
          <p>janusctl signs in with none of them any more ({u.ssh_keys.map((k) => k.name).join(', ')}): a lost or stolen laptop.</p>
          <p className="muted">The certificates janusctl already got with them end within twelve hours; to cut them sooner, disable the account.</p>
        </>
      ),
      action: 'Revoke',
      danger: true,
    })
    if (!ok) return
    await run(() => call(`/api/users/${encodeURIComponent(u.name)}/ssh-keys`, { method: 'DELETE' }), `${u.name}'s SSH keys revoked`)
    load()
  }
  const remove = async (u) => {
    const ok = await confirm({
      title: `Delete ${u.name}?`,
      body: (
        <p>
          The account, its sessions and its {u.tokens} API token{u.tokens === 1 ? '' : 's'} are gone. What they did stays in the audit.
        </p>
      ),
      action: 'Delete',
      danger: true,
      typeToConfirm: u.name,
    })
    if (!ok) return
    await run(() => call(`/api/users/${encodeURIComponent(u.name)}`, { method: 'DELETE' }), `Deleted ${u.name}`)
    load()
  }

  return (
    <div className="stack">
      <div className="spread">
        <h1>Accounts</h1>
      </div>
      <p className="muted" style={{ margin: 0 }}>
        Who may sign in to this Controller. A role applies on the Controller and on the nodes it reaches for the account: each node logs who acted.
      </p>
      {given && <GivenPassword given={given} onClose={() => setGiven(null)} />}
      {grantsOf && (
        <GrantsEditor
          user={grantsOf}
          onClose={() => setGrantsOf(null)}
          onSaved={() => {
            setGrantsOf(null)
            load()
          }}
        />
      )}
      <NewAccount
        onCreated={(u) => {
          setGiven({ name: u.name, password: u.password })
          load()
        }}
      />
      <ErrorBox error={error} />
      {users && (
        <Card title="Accounts" icon={Users}>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Role</th>
                  <th>Grants</th>
                  <th>Last sign-in</th>
                  <th>API tokens</th>
                  <th>SSH keys</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {users.map((u) => (
                  <tr key={u.name} className={u.disabled ? 'muted' : ''}>
                    <td>
                      <strong>{u.name}</strong> {u.name === me.name && <Badge>you</Badge>} {u.disabled && <Badge tone="danger">disabled</Badge>}{' '}
                      {u.must_change_password && !u.disabled && <Badge tone="warn">password to choose</Badge>} <MFABadge on={u.mfa} />
                    </td>
                    <td>
                      <select
                        value={u.role || 'none'}
                        disabled={busy}
                        onChange={(e) => patch(u, { role: e.target.value }, `${u.name} is now ${e.target.value}`)}
                        aria-label={`${u.name}'s role`}
                      >
                        {ACCOUNT_ROLES.map((r) => (
                          <option key={r.id} value={r.id}>
                            {r.id === 'none' ? 'None' : r.label}
                          </option>
                        ))}
                      </select>
                    </td>
                    <td className="small">
                      {u.grants.map((g, i) => (
                        <div key={i} className="nowrap">
                          {grantSummary(g)}
                        </div>
                      ))}
                      <button className="ghost small" onClick={() => setGrantsOf(u)} disabled={busy} title="Roles on the nodes whose labels match">
                        <Tags size={13} /> {u.grants.length ? 'Edit' : 'Grants'}
                      </button>
                    </td>
                    <td>{when(u.last_login_at)}</td>
                    <td>
                      {u.tokens > 0 ? (
                        <span>
                          <KeyRound size={12} /> {u.tokens}
                        </span>
                      ) : (
                        <span className="muted">–</span>
                      )}
                    </td>
                    <td>
                      {u.ssh_keys?.length > 0 ? (
                        <span title={u.ssh_keys.map((k) => `${k.name} - ${k.fingerprint}`).join('\n')}>
                          <KeySquare size={12} /> {u.ssh_keys.length}{' '}
                          <button className="ghost small danger" onClick={() => revokeKeys(u)} disabled={busy} title="A lost laptop: janusctl signs in with none of them">
                            Revoke
                          </button>
                        </span>
                      ) : (
                        <span className="muted">–</span>
                      )}
                    </td>
                    <td style={{ textAlign: 'right', whiteSpace: 'nowrap' }}>
                      <button className="ghost small" onClick={() => reset(u)} disabled={busy} title="A new password to hand over">
                        <RotateCcw size={14} /> Reset password
                      </button>
                      {u.mfa && (
                        <button className="ghost small" onClick={() => resetMFA(u)} disabled={busy} title="A lost phone or key: the account sets one up again">
                          Reset 2FA
                        </button>
                      )}
                      <button className="ghost small" onClick={() => toggle(u)} disabled={busy}>
                        {u.disabled ? 'Enable' : 'Disable'}
                      </button>
                      <button className="ghost small danger" onClick={() => remove(u)} disabled={busy}>
                        <Trash2 size={14} /> Delete
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <div className="muted small" style={{ marginTop: '0.5rem' }}>
            The last enabled admin can&apos;t be demoted, disabled or deleted. Locked out of every admin account? On the Controller&apos;s host:{' '}
            <code>docker exec janus-controller /dashboardd reset-user NAME</code>.
          </div>
        </Card>
      )}
      <SessionPolicy />
    </div>
  )
}
