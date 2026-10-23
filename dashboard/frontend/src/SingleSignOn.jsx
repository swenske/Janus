import { CheckCircle2, Copy, ExternalLink, FlaskConical, LogIn, Pencil, Plus, Trash2, TriangleAlert } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { createPortal } from 'react-dom'
import { call, postJSON } from './call.js'
import { DOMAINS, labelString, parseLabels, ROLES } from './me.jsx'
import { Badge, Card, ErrorBox, useAction, useConfirm, useToast } from './shared/ui.jsx'

// Single sign-on (admin, on the Accounts page): the OpenID Connect
// providers accounts sign in through - dashboard/backend/internal/sso.

const DOCS = 'https://janus.sw-servers.net/docs/guide/sso.md'

const KINDS = [
  { id: 'keycloak', label: 'Keycloak' },
  { id: 'synology', label: 'Synology SSO Server' },
  { id: 'generic', label: 'Another OpenID Connect provider' },
]

// HINTS: what to set up at the provider, by kind.
const HINTS = {
  keycloak: (
    <>
      In the realm: a client with <em>Client authentication</em> on and the standard flow, PKCE method S256, the redirect and post-logout URIs below. Groups come from a{' '}
      <em>Group Membership</em> mapper named <code>groups</code>, in the ID token - full paths by default: <code>/janus-admins</code>.
    </>
  ),
  synology: (
    <>
      SSO Server › Application: an OIDC application with the redirect URI below - the Controller opened by its name, not an IP address (DSM refuses those). Groups are DSM
      groups&apos; names; with a domain, <code>DOMAIN\group</code>.
    </>
  ),
  generic: <>Register a confidential client with the redirect URI below; the groups claim names what the rules match.</>,
}

const PROVIDER_ROLES = [{ id: '', label: 'None - refused' }, ...ROLES]

function CopyLine({ label, value }) {
  const toast = useToast()
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value)
      toast(`${label} copied`)
    } catch {
      toast('Select it and press Ctrl+C', 'warn')
    }
  }
  return (
    <div className="row" style={{ alignItems: 'center' }}>
      <span className="muted small nowrap" style={{ minWidth: '7.5rem' }}>
        {label}
      </span>
      <code className="grow" style={{ overflowWrap: 'anywhere' }}>
        {value}
      </code>
      <button className="ghost small" onClick={copy} aria-label={`Copy the ${label.toLowerCase()}`}>
        <Copy size={13} />
      </button>
    </div>
  )
}

// toForm is a provider as the form edits it; toProvider back.
function toForm(p) {
  return {
    ...p,
    scopes: (p.scopes || []).join(' '),
    acr: (p.acr || []).join(' '),
    amr: (p.amr || []).join(' '),
    trust: p.tls?.ca_pem ? 'ca' : p.tls?.fingerprint ? 'fingerprint' : 'system',
    ca_pem: p.tls?.ca_pem || '',
    fingerprint: p.tls?.fingerprint || '',
    server_name: p.tls?.server_name || '',
    rules: (p.rules || []).map((r) => ({ group: r.group, role: r.role, labels: labelString(r.selector), domains: r.domains || [] })),
    secret: '',
  }
}

const words = (s) => s.split(/\s+/).filter(Boolean)

function toProvider(f) {
  const tls = f.trust === 'ca' ? { ca_pem: f.ca_pem, server_name: f.server_name } : f.trust === 'fingerprint' ? { fingerprint: f.fingerprint, server_name: f.server_name } : {}
  return {
    id: f.id,
    name: f.name.trim(),
    preset: f.preset,
    enabled: !!f.enabled,
    issuer: f.issuer.trim(),
    client_id: f.client_id.trim(),
    scopes: words(f.scopes),
    username_claim: f.username_claim.trim(),
    groups_claim: f.groups_claim.trim(),
    tls,
    mfa: f.mfa || 'janus',
    acr: f.mfa === 'provider' ? words(f.acr) : [],
    amr: f.mfa === 'provider' ? words(f.amr) : [],
    default_role: f.default_role || '',
    rules: f.rules.map((r) => {
      const selector = parseLabels(r.labels)
      return { group: r.group, role: r.role, ...(Object.keys(selector).length ? { selector } : {}), ...(r.domains.length ? { domains: r.domains } : {}) }
    }),
    sign_in_every_days: Number(f.sign_in_every_days),
    disable_pkce: !!f.disable_pkce,
    allow_missing_nonce: !!f.allow_missing_nonce,
    local_logout: !!f.local_logout,
    version: f.version || 0,
  }
}

function Report({ report }) {
  if (report.error) return <ErrorBox error={report.error} />
  return (
    <div className={`notice small stack ${report.warnings.length ? 'warn' : ''}`} style={{ gap: '0.35rem' }}>
      <div className="row" style={{ alignItems: 'center' }}>
        {report.warnings.length ? <TriangleAlert size={14} /> : <CheckCircle2 size={14} />}
        <span>
          Discovery and {report.keys} signing key{report.keys === 1 ? '' : 's'} fetched{report.key_algorithms.length ? ` (${report.key_algorithms.join(', ')})` : ''}; the client
          authenticates with {report.token_auth || '?'}.
        </span>
      </div>
      <div>
        Issuer <code>{report.issuer}</code> {report.issuer_matches ? <Badge tone="ok">matches</Badge> : <Badge tone="warn">differs</Badge>}
      </div>
      {report.warnings.map((w, i) => (
        <div key={i}>• {w}</div>
      ))}
    </div>
  )
}

function RuleRows({ rules, onChange }) {
  const set = (i, patch) => onChange(rules.map((r, j) => (j === i ? { ...r, ...patch } : r)))
  return (
    <div className="stack" style={{ gap: '0.6rem' }}>
      {rules.length === 0 && <div className="muted small">No rule: nobody gets in unless there&apos;s a default role.</div>}
      {rules.map((r, i) => (
        <div key={i} className="card" style={{ padding: '0.7rem' }}>
          <div className="row" style={{ alignItems: 'flex-end', flexWrap: 'wrap' }}>
            <label className="field grow">
              <span>Group</span>
              <input className="mono" value={r.group} onChange={(e) => set(i, { group: e.target.value })} required placeholder="/janus-admins" aria-label={`Rule ${i + 1}'s group`} />
            </label>
            <label className="field">
              <span>Role</span>
              <select value={r.role} onChange={(e) => set(i, { role: e.target.value })} aria-label={`Rule ${i + 1}'s role`}>
                {ROLES.map((x) => (
                  <option key={x.id} value={x.id}>
                    {x.label}
                  </option>
                ))}
              </select>
            </label>
            <label className="field grow">
              <span>On the nodes labelled (empty: everything)</span>
              <input className="mono" value={r.labels} onChange={(e) => set(i, { labels: e.target.value })} placeholder="team=web" aria-label={`Rule ${i + 1}'s labels`} />
            </label>
            <button type="button" className="ghost small danger" onClick={() => onChange(rules.filter((_, j) => j !== i))} title="Remove this rule">
              <Trash2 size={14} />
            </button>
          </div>
          <div className="row" style={{ marginTop: '0.5rem', flexWrap: 'wrap', gap: '0.8rem' }}>
            <span className="muted small">Only:</span>
            {DOMAINS.map((d) => (
              <label key={d.id} className="check small" title={d.about}>
                <input type="checkbox" checked={r.domains.includes(d.id)} onChange={(e) => set(i, { domains: e.target.checked ? [...r.domains, d.id] : r.domains.filter((x) => x !== d.id) })} />{' '}
                {d.label}
              </label>
            ))}
          </div>
        </div>
      ))}
      <div>
        <button type="button" className="small" onClick={() => onChange([...rules, { group: '', role: 'reader', labels: '', domains: [] }])}>
          <Plus size={14} /> Add a rule
        </button>
      </div>
    </div>
  )
}

// ProviderForm edits a provider - new, or saved already - in a dialog.
function ProviderForm({ initial, isNew, hasSecret, uris, presets, onClose, onSaved }) {
  const [f, setF] = useState(() => toForm(initial))
  const [error, setError] = useState(null)
  const [report, setReport] = useState(null)
  const [busy, run] = useAction()
  const [testing, setTesting] = useState(false)
  const set = (k) => (e) => setF({ ...f, [k]: e.target.type === 'checkbox' ? e.target.checked : e.target.value })
  const kind = (preset) => {
    const p = presets[preset]
    setF({ ...f, preset, scopes: p.scopes.join(' '), username_claim: p.username_claim, groups_claim: p.groups_claim })
  }
  const body = () => {
    const p = toProvider(f)
    return f.secret ? { ...p, client_secret: f.secret } : p
  }
  const test = async () => {
    setError(null)
    setReport(null)
    setTesting(true)
    try {
      setReport(await postJSON('/api/sso/test', body()))
    } catch (err) {
      setError(err.message)
    } finally {
      setTesting(false)
    }
  }
  const save = async (e) => {
    e.preventDefault()
    let b
    try {
      b = body()
    } catch (err) {
      return setError(err.message)
    }
    setError(null)
    const out = await run(() => postJSON(`/api/sso/providers/${encodeURIComponent(f.id)}`, b, 'PUT'), `${f.name} saved`)
    if (out) onSaved()
  }
  return createPortal(
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal card wide" role="dialog" aria-modal="true" aria-label={isNew ? 'A new provider' : f.name} onClick={(e) => e.stopPropagation()}>
        <form className="stack" onSubmit={save}>
          <h2 style={{ margin: 0 }}>{isNew ? 'A new provider' : f.name}</h2>
          <div className="row" style={{ flexWrap: 'wrap' }}>
            <label className="field">
              <span>Kind</span>
              <select value={f.preset} onChange={(e) => kind(e.target.value)}>
                {KINDS.map((k) => (
                  <option key={k.id} value={k.id}>
                    {k.label}
                  </option>
                ))}
              </select>
            </label>
            <label className="field">
              <span>ID</span>
              <input
                className="mono"
                value={f.id}
                onChange={set('id')}
                required
                pattern="[a-z0-9][a-z0-9\-]{0,31}"
                disabled={!isNew}
                placeholder={f.preset === 'generic' ? 'sso' : f.preset}
                aria-describedby="sso-id-about"
                style={{ width: '9rem' }}
              />
            </label>
            <label className="field grow">
              <span>Name (the button: Sign in with …)</span>
              <input value={f.name} onChange={set('name')} required maxLength={40} />
            </label>
          </div>
          <div id="sso-id-about" className="muted small">
            The ID is yours to choose: it names this provider on the Controller - in its URLs, the accounts it makes, the log - and is never sent to it (not its
            client ID). Lowercase letters, digits and -; set once.
          </div>
          <div className="notice small">{HINTS[f.preset]}</div>
          <CopyLine label="Redirect URI" value={uris.redirect} />
          <CopyLine label="Post-logout URI" value={uris.postLogout} />
          <div className="row" style={{ flexWrap: 'wrap' }}>
            <label className="field grow">
              <span>Issuer URL</span>
              <input
                className="mono"
                value={f.issuer}
                onChange={set('issuer')}
                required
                placeholder={f.preset === 'synology' ? 'https://nas.example.com/webman/sso' : 'https://keycloak.example.com/realms/janus'}
              />
            </label>
          </div>
          <div className="row" style={{ flexWrap: 'wrap' }}>
            <label className="field grow">
              <span>Client ID</span>
              <input className="mono" value={f.client_id} onChange={set('client_id')} required />
            </label>
            <label className="field grow">
              <span>Client secret</span>
              <input type="password" value={f.secret} onChange={set('secret')} placeholder={hasSecret ? 'kept - type to change it' : 'none: a public client'} autoComplete="new-password" />
            </label>
          </div>
          <div className="row" style={{ flexWrap: 'wrap' }}>
            <label className="field grow">
              <span>Scopes</span>
              <input className="mono" value={f.scopes} onChange={set('scopes')} required />
            </label>
            <label className="field">
              <span>Username claim</span>
              <input className="mono" value={f.username_claim} onChange={set('username_claim')} required style={{ width: '11rem' }} />
            </label>
            <label className="field">
              <span>Groups claim</span>
              <input className="mono" value={f.groups_claim} onChange={set('groups_claim')} required style={{ width: '11rem' }} />
            </label>
          </div>
          <div className="row" style={{ flexWrap: 'wrap' }}>
            <label className="field">
              <span>Its certificate is trusted by</span>
              <select value={f.trust} onChange={set('trust')}>
                <option value="system">the system&apos;s trust store</option>
                <option value="ca">a CA certificate</option>
                <option value="fingerprint">a SHA-256 fingerprint</option>
              </select>
            </label>
            {f.trust === 'fingerprint' && (
              <label className="field grow">
                <span>SHA-256 fingerprint (its certificate&apos;s, or a CA&apos;s it presents)</span>
                <input className="mono" value={f.fingerprint} onChange={set('fingerprint')} required />
              </label>
            )}
            {f.trust !== 'system' && (
              <label className="field">
                <span>Name in its certificate (optional)</span>
                <input className="mono" value={f.server_name} onChange={set('server_name')} style={{ width: '12rem' }} />
              </label>
            )}
          </div>
          {f.trust === 'ca' && (
            <label className="field">
              <span>CA certificate (PEM)</span>
              <textarea className="mono" rows={4} value={f.ca_pem} onChange={set('ca_pem')} required placeholder="-----BEGIN CERTIFICATE-----" />
            </label>
          )}
          <h3 style={{ margin: '0.4rem 0 0' }}>Rules</h3>
          <div className="muted small">
            Someone in a rule&apos;s group gets its role - over everything, or on the nodes labelled so, narrowed to the domains ticked. The highest role wins; a change applies
            to the accounts this provider made at once.
          </div>
          <RuleRows rules={f.rules} onChange={(rules) => setF({ ...f, rules })} />
          <div className="row" style={{ flexWrap: 'wrap' }}>
            <label className="field">
              <span>Anyone it signs in gets at least</span>
              <select value={f.default_role || ''} onChange={set('default_role')}>
                {PROVIDER_ROLES.map((r) => (
                  <option key={r.id} value={r.id}>
                    {r.label}
                  </option>
                ))}
              </select>
            </label>
            <label className="field">
              <span>Its accounts stop working after (days without signing in, 0: never)</span>
              <input type="number" min={0} max={365} value={f.sign_in_every_days} onChange={set('sign_in_every_days')} required style={{ width: '7rem' }} />
            </label>
          </div>
          <div className="row" style={{ flexWrap: 'wrap' }}>
            <label className="field">
              <span>The second factor</span>
              <select value={f.mfa || 'janus'} onChange={set('mfa')}>
                <option value="janus">the Controller asks for its own</option>
                <option value="provider">the provider&apos;s counts</option>
              </select>
            </label>
            {f.mfa === 'provider' && (
              <>
                <label className="field grow">
                  <span>Only with the acr (any of, optional)</span>
                  <input className="mono" value={f.acr} onChange={set('acr')} placeholder="gold" />
                </label>
                <label className="field grow">
                  <span>or the amr (any of, optional)</span>
                  <input className="mono" value={f.amr} onChange={set('amr')} placeholder="otp hwk" />
                </label>
              </>
            )}
          </div>
          {f.mfa === 'provider' && (
            <div className="muted small">
              With neither, the provider is trusted as is (a Synology says nothing about its second factor). When its ID token doesn&apos;t carry them, the Controller asks for
              its own.
            </div>
          )}
          <details>
            <summary className="small">For a provider that needs it</summary>
            <div className="stack" style={{ marginTop: '0.5rem', gap: '0.4rem' }}>
              <label className="check">
                <input type="checkbox" checked={!!f.disable_pkce} onChange={set('disable_pkce')} />
                <span>No PKCE - for a provider whose code exchange fails with it</span>
              </label>
              <label className="check">
                <input type="checkbox" checked={!!f.allow_missing_nonce} onChange={set('allow_missing_nonce')} />
                <span>Accept an ID token without the nonce - for a provider that doesn&apos;t send it back</span>
              </label>
              <label className="check">
                <input type="checkbox" checked={!!f.local_logout} onChange={set('local_logout')} />
                <span>Signing out ends the Controller&apos;s session only, never the provider&apos;s</span>
              </label>
            </div>
          </details>
          <label className="check">
            <input type="checkbox" checked={!!f.enabled} onChange={set('enabled')} />
            <span>Offer it on the sign-in page</span>
          </label>
          {report && <Report report={report} />}
          <ErrorBox error={error} />
          <div className="row" style={{ justifyContent: 'flex-end', flexWrap: 'wrap' }}>
            <button type="button" onClick={test} disabled={testing || !f.issuer}>
              <FlaskConical size={14} /> {testing ? 'Testing…' : 'Test'}
            </button>
            <span className="grow" />
            <button type="button" onClick={onClose}>
              Cancel
            </button>
            <button className="primary" type="submit" disabled={busy}>
              Save
            </button>
          </div>
        </form>
      </div>
    </div>,
    document.body,
  )
}

// SingleSignOn is the providers' card.
export default function SingleSignOn({ onChanged }) {
  const [data, setData] = useState(null)
  const [error, setError] = useState(null)
  const [editing, setEditing] = useState(null) // {provider, isNew, hasSecret}
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const load = useCallback(() => {
    call('/api/sso/providers').then((d) => {
      setData(d)
      setError(null)
    }, setError)
  }, [])
  useEffect(load, [load])
  if (error) return <ErrorBox error={error} />
  if (!data) return null
  const uris = { redirect: data.redirect_uri, postLogout: data.post_logout_uri }
  const add = () => {
    const preset = data.presets.keycloak
    setEditing({ provider: { ...preset, id: '', name: '', issuer: '', client_id: '' }, isNew: true, hasSecret: false })
  }
  const remove = async (p) => {
    const ok = await confirm({
      title: `Remove ${p.name}?`,
      body: (
        <>
          <p>Its sign-ins end, and the accounts it made stop working - delete them, or make one an ordinary account with dashboardd reset-user.</p>
          <p className="muted">Identities linked to accounts stay: a provider added again with the same issuer finds them.</p>
        </>
      ),
      action: 'Remove',
      danger: true,
      typeToConfirm: p.id,
    })
    if (!ok) return
    await run(() => call(`/api/sso/providers/${encodeURIComponent(p.id)}`, { method: 'DELETE' }), `${p.name} removed`)
    load()
    onChanged?.()
  }
  return (
    <Card
      title="Single sign-on"
      icon={LogIn}
      actions={
        <button className="small" onClick={add}>
          <Plus size={14} /> Add a provider
        </button>
      }
    >
      <div className="stack" style={{ gap: '0.6rem' }}>
        <div className="muted small">
          Accounts sign in through an OpenID Connect provider - Keycloak, a Synology&apos;s SSO Server, another. Its rules make an account at the first sign-in; a local
          account&apos;s owner can link their identity there instead (Your account). The password form stays, for the accounts that have one.{' '}
          <a href={DOCS} target="_blank" rel="noreferrer">
            The guide <ExternalLink size={12} />
          </a>
        </div>
        {data.providers.length === 0 ? (
          <div className="muted">No provider.</div>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Issuer</th>
                  <th>Rules</th>
                  <th>Second factor</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {data.providers.map((p) => (
                  <tr key={p.id}>
                    <td>
                      <strong>{p.name}</strong> <span className="muted small mono">{p.id}</span> {p.enabled ? <Badge tone="ok">on</Badge> : <Badge>off</Badge>}
                    </td>
                    <td className="mono small" style={{ overflowWrap: 'anywhere' }}>
                      {p.issuer}
                    </td>
                    <td className="small">
                      {p.rules.length}
                      {p.default_role ? `, everyone ${p.default_role}` : ''}
                    </td>
                    <td className="small">{p.mfa === 'provider' ? "the provider's" : "the Controller's"}</td>
                    <td style={{ textAlign: 'right', whiteSpace: 'nowrap' }}>
                      <button className="ghost small" onClick={() => setEditing({ provider: p, isNew: false, hasSecret: p.has_secret })} disabled={busy}>
                        <Pencil size={13} /> Edit
                      </button>
                      <button className="ghost small danger" onClick={() => remove(p)} disabled={busy}>
                        <Trash2 size={14} /> Remove
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
      {editing && (
        <ProviderForm
          initial={editing.provider}
          isNew={editing.isNew}
          hasSecret={editing.hasSecret}
          uris={uris}
          presets={data.presets}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null)
            load()
            onChanged?.()
          }}
        />
      )}
    </Card>
  )
}
