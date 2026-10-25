import { CheckCircle2, Copy, ExternalLink, KeyRound, Pencil, Plus, RefreshCw, Route, ScrollText, ShieldCheck, Trash2, Upload, Waypoints } from 'lucide-react'
import { useState } from 'react'
import { postJSON } from '../api.js'
import { Badge, Card, ErrorBox, Loading, PageHeader, useAction, useConfirm, useToast } from '../../shared/ui.jsx'
import { hunks, lineDiff } from '../../shared/diff.js'
import { DiffView, Editor } from '../../shared/Editor.jsx'
import { dateTime } from '../format.js'
import { usePoll } from '../hooks.jsx'
import ProviderForm from '../../shared/ACMEProviderForm.jsx'
import { useMay } from '../may.js'

// Its page on the docs site, by the file's permalink.
const DOCS = 'https://janus.sw-servers.net/docs/letsencrypt.md'

const CAS = [
  { id: 'letsencrypt', label: "Let's Encrypt" },
  { id: 'letsencrypt-staging', label: "Let's Encrypt staging - test certificates, untrusted" },
  { id: 'custom', label: 'Another ACME CA (its directory URL)' },
]

const KEY_TYPES = ['ec256', 'ec384', 'rsa2048', 'rsa3072', 'rsa4096']
const STATE_TONES = { valid: 'ok', pending: 'warn', due: 'info', expired: 'danger' }

// canon puts a configuration in the node's own form - its fields' order,
// empty ones left out - so a change shows as just that.
const ORDER = [
  'account',
  'certificates',
  'dns_providers',
  'directory',
  'directory_ca',
  'email',
  'accept_terms',
  'eab_key_id',
  'eab_hmac_key',
  'name',
  'domains',
  'type',
  'key_type',
  'challenge',
  'dns_provider',
  'profile',
  'settings',
  'resolvers',
  'propagation_wait_seconds',
  'path',
  'mode',
  'proxy_url',
  'proxy_username',
  'proxy_password',
  'proxy_ca',
  'account_thumbprint',
]
const rank = (k) => (ORDER.includes(k) ? ORDER.indexOf(k) : ORDER.length)
function canon(v) {
  if (Array.isArray(v)) return v.map(canon)
  if (v && typeof v === 'object') {
    const out = {}
    const keys = Object.keys(v).sort((a, b) => rank(a) - rank(b) || a.localeCompare(b))
    for (const k of keys) {
      const c = canon(v[k])
      const empty =
        c === '' ||
        c === false ||
        c === 0 ||
        c == null ||
        (Array.isArray(c) && !c.length) ||
        (typeof c === 'object' && !Array.isArray(c) && !Object.keys(c).length)
      // A secret setting sent empty is kept: it still counts.
      if (!empty || k === k.toUpperCase()) out[k] = c
    }
    return out
  }
  return v
}
const pretty = (cfg) => JSON.stringify(canon(cfg ?? {}), null, 2)
const lines = (text) =>
  text
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean)

function CopyButton({ value, label }) {
  const toast = useToast()
  return (
    <button
      className="small"
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(value)
          toast(`${label} copied`)
        } catch {
          toast('Copy failed - select the text instead', 'warn')
        }
      }}
    >
      <Copy size={13} /> Copy
    </button>
  )
}

// LetsEncrypt is the letsencrypt extension: the account, the certificates
// the node obtains and renews by itself, the DNS providers for DNS-01,
// and the whole configuration as JSON.
export default function LetsEncrypt() {
  const status = usePoll('/api/haproxy/acme', { every: 5000 })
  const config = usePoll('/api/haproxy/acme/config', { every: 0 })
  const saved = config.data // { config, is_default }
  const [editing, setEditing] = useState(null) // { kind: 'account'|'cert'|'provider', index }
  const [errors, setErrors] = useState(null)
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const toast = useToast()
  const may = useMay()
  const canApply = may('HAProxyService/ACMEApplyConfig')
  const canRenew = may('HAProxyService/ACMERenew')

  const cfg = saved?.config || {}
  const st = status.data

  // apply shows the change, then saves it; true once accepted.
  const apply = async (next, { accountKey = '', title = 'Apply this configuration?', note } = {}) => {
    const diff = hunks(lineDiff(pretty(cfg), pretty(next)))
    const ok = await confirm({
      title,
      body: (
        <>
          <p>{note || 'The node saves it; certificates to obtain are obtained in the background, renewals happen by themselves.'}</p>
          {accountKey && <p>The account key is replaced: its thumbprint changes, and HAProxy is reloaded to answer HTTP-01 challenges with the new one.</p>}
          <DiffView diff={diff} />
        </>
      ),
      action: 'Apply',
      wide: true,
    })
    if (!ok) return false
    const r = await run(() => postJSON('/api/haproxy/acme/apply', { config: next, account_key: accountKey }))
    if (!r) return false
    if (!r.accepted) {
      setErrors(r.errors || ['refused'])
      toast('The node refused the configuration', 'danger')
      return false
    }
    setErrors(null)
    toast('Configuration applied')
    config.reload()
    status.reload()
    return true
  }

  const renew = (names) =>
    run(
      async () => {
        await postJSON('/api/haproxy/acme/renew', { names })
        status.reload()
      },
      names.length ? `Obtaining ${names.join(', ')}…` : 'Obtaining every certificate…',
    )

  if (st?.state === 'not_enabled') {
    return (
      <>
        <PageHeader title="Let's Encrypt" subtitle="Certificates obtained and renewed by the node itself" />
        <div className="notice">The letsencrypt extension isn't in this node's image. Add it from System › Update (Change extensions…).</div>
      </>
    )
  }
  if (config.error) return <ErrorBox error={config.error} />
  if (!saved || !st) return <Loading />

  const certs = cfg.certificates || []
  const providers = cfg.dns_providers || []
  const statusOf = (name) => (st.certificates || []).find((c) => c.name === name)
  const withCerts = (list) => ({ ...cfg, certificates: list })
  const withProviders = (list) => ({ ...cfg, dns_providers: list })

  return (
    <>
      <PageHeader
        title="Let's Encrypt"
        subtitle="Certificates obtained and renewed by the node itself, swapped into HAProxy without a reload"
        actions={
          <>
            <a className="button small" href="#/logs?service=letsencrypt">
              <ScrollText size={13} /> Log
            </a>
            <a className="button small" href={DOCS} target="_blank" rel="noreferrer">
              Guide <ExternalLink size={12} />
            </a>
          </>
        }
      />
      {status.error && <ErrorBox error={status.error} />}
      {errors && (
        <div className="error-box" style={{ marginBottom: '1rem' }}>
          {errors.join('\n')}
        </div>
      )}

      <PathCard cfg={cfg} st={st} canApply={canApply} canCheck={may('HAProxyService/ACMECheckPath')} busy={busy} run={run} apply={apply} />

      <div className="grid grid-2">
        <Card
          title="Account"
          icon={KeyRound}
          actions={
            canApply && (
              <button className="small" onClick={() => setEditing({ kind: 'account' })}>
                <Pencil size={13} /> Edit
              </button>
            )
          }
        >
          <dl className="kv">
            <dt>CA</dt>
            <dd className="mono small">{st.directory}</dd>
            <dt>Account</dt>
            <dd className="small">
              {st.ordered_by ? (
                <span>
                  <span className="mono">{st.ordered_by}</span>’s, this node’s cluster member: it orders the cluster’s certificates, this node serves what it pulls
                  and answers HTTP-01 with that account
                </span>
              ) : st.path_mode === 'controller_ordered' ? (
                <span>the Controller’s - it orders this node’s certificates; the account below is unused meanwhile</span>
              ) : st.account_uri ? (
                <span className="mono">{st.account_uri}</span>
              ) : st.account_error ? (
                <span style={{ color: 'var(--danger)' }}>{st.account_error}</span>
              ) : (
                <span className="muted">not registered yet - it is with the first certificate</span>
              )}
            </dd>
            <dt>Contact</dt>
            <dd>{cfg.account?.email || <span className="muted">none</span>}</dd>
            <dt>Terms</dt>
            <dd>
              {cfg.account?.accept_terms ? <Badge tone="ok">accepted</Badge> : <Badge tone="warn">not accepted</Badge>}{' '}
              {st.terms_url && st.terms_url.startsWith('http') && (
                <a className="small" href={st.terms_url} target="_blank" rel="noreferrer">
                  read them
                </a>
              )}
            </dd>
            <dt>Thumbprint</dt>
            <dd className="mono small">{st.account_thumbprint}</dd>
          </dl>
        </Card>
        <Card title="HTTP-01 challenges" icon={ShieldCheck} actions={<CopyButton value={st.http01_rule} label="The rule" />}>
          <p className="small" style={{ marginTop: 0 }}>
            HAProxy answers them itself: put this rule in each frontend on port 80 that receives them, before any redirect. HAProxy has the account's thumbprint
            as <span className="mono">JANUS_ACME_THUMBPRINT</span>.
          </p>
          <pre className="code small" style={{ whiteSpace: 'pre-wrap' }}>
            {st.http01_rule}
          </pre>
          <p className="small muted" style={{ marginBottom: 0 }}>
            Each certificate is <span className="mono">{st.certificates_dir}/&lt;name&gt;.pem</span> - reference it by file, or the whole directory (
            <span className="mono">crt {st.certificates_dir}/</span>). It holds a self-signed stand-in until the CA's arrives.
          </p>
        </Card>
      </div>

      {editing?.kind === 'account' && (
        <AccountForm
          account={cfg.account || {}}
          onCancel={() => setEditing(null)}
          onSave={async (account, key) => (await apply({ ...cfg, account }, { accountKey: key })) && setEditing(null)}
          busy={busy}
        />
      )}

      <div style={{ marginTop: '1rem' }}>
        <Card
          title="Certificates"
          icon={ShieldCheck}
          actions={
            <div className="row">
              {certs.length > 0 && canRenew && (
                <button className="small" disabled={busy} onClick={() => renew([])}>
                  <RefreshCw size={13} /> Renew all
                </button>
              )}
              {canApply && (
                <button className="small primary" onClick={() => setEditing({ kind: 'cert', index: -1 })}>
                  <Plus size={13} /> Add a certificate
                </button>
              )}
            </div>
          }
        >
          {!certs.length ? (
            <span className="muted small">No certificate yet.</span>
          ) : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Name</th>
                    <th>Domains</th>
                    <th>State</th>
                    <th>Expires</th>
                    <th>Renews</th>
                    <th>Issuer</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {certs.map((c, i) => {
                    const s = statusOf(c.name) || {}
                    return (
                      <tr key={c.name}>
                        <td className="mono">{c.name}</td>
                        <td className="small">
                          {(c.domains || []).join(' ')}
                          <div className="muted">
                            {c.challenge || 'http-01'}
                            {c.dns_provider ? ` · ${c.dns_provider}` : ''} · {c.key_type || 'ec256'}
                          </div>
                          {s.last_error && (
                            <div style={{ color: 'var(--danger)', whiteSpace: 'pre-wrap' }} title={s.last_error}>
                              {s.failures} failed attempt{s.failures === 1 ? '' : 's'}
                              {s.next_attempt_unix ? `, next ${dateTime(s.next_attempt_unix * 1000)}` : ''}: {s.last_error.split('\n')[0].slice(0, 220)}
                            </div>
                          )}
                        </td>
                        <td>
                          <Badge tone={STATE_TONES[s.state]} dot>
                            {s.in_progress ? 'obtaining…' : s.state || '–'}
                          </Badge>
                        </td>
                        <td className="small nowrap">{s.not_after_unix ? dateTime(s.not_after_unix * 1000) : '–'}</td>
                        <td className="small nowrap">{s.renew_at_unix ? dateTime(s.renew_at_unix * 1000) : '–'}</td>
                        <td className="small">{s.issuer || '–'}</td>
                        <td className="nowrap">
                          {canRenew && (
                            <button className="small" disabled={busy || s.in_progress} onClick={() => renew([c.name])} title="Obtain it again now">
                              <RefreshCw size={13} />
                            </button>
                          )}{' '}
                          {canApply && (
                            <>
                          <button className="small" onClick={() => setEditing({ kind: 'cert', index: i })} title="Edit">
                            <Pencil size={13} />
                          </button>{' '}
                          <button
                            className="small danger"
                            disabled={busy}
                            title="Remove"
                            onClick={() =>
                              apply(withCerts(certs.filter((x) => x.name !== c.name)), {
                                title: `Remove ${c.name}?`,
                                note: `Its file goes - refused while haproxy.cfg still uses it.`,
                              })
                            }
                          >
                            <Trash2 size={13} />
                          </button>
                            </>
                          )}
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          )}
        </Card>
      </div>

      {editing?.kind === 'cert' && (
        <CertForm
          cert={editing.index >= 0 ? certs[editing.index] : null}
          names={certs.map((c) => c.name)}
          providers={providers.map((p) => p.name)}
          busy={busy}
          onCancel={() => setEditing(null)}
          onSave={async (cert) => {
            const list = editing.index >= 0 ? certs.map((c, i) => (i === editing.index ? cert : c)) : [...certs, cert]
            if (await apply(withCerts(list))) setEditing(null)
          }}
        />
      )}

      <div style={{ marginTop: '1rem' }}>
        <Card
          title="DNS providers"
          icon={Waypoints}
          actions={
            canApply && (
              <button className="small" onClick={() => setEditing({ kind: 'provider', index: -1 })}>
                <Plus size={13} /> Add a provider
              </button>
            )
          }
        >
          <p className="small muted" style={{ marginTop: 0 }}>
            For the dns-01 challenge - wildcards, or names HAProxy doesn't receive on port 80: janus-acme creates the record through the provider's API.
            Settings are secrets: they never come back from the node.
          </p>
          {!providers.length ? (
            <span className="muted small">None.</span>
          ) : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Name</th>
                    <th>Type</th>
                    <th>Settings</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {providers.map((p, i) => (
                    <tr key={p.name}>
                      <td className="mono">{p.name}</td>
                      <td className="mono">{p.type}</td>
                      <td className="mono small">{Object.keys(p.settings || {}).join(' ')}</td>
                      <td className="nowrap">
                        {canApply && (
                          <>
                        <button className="small" onClick={() => setEditing({ kind: 'provider', index: i })} title="Edit">
                          <Pencil size={13} />
                        </button>{' '}
                        <button
                          className="small danger"
                          disabled={busy}
                          title="Remove"
                          onClick={() => apply(withProviders(providers.filter((x) => x.name !== p.name)), { title: `Remove ${p.name}?` })}
                        >
                          <Trash2 size={13} />
                        </button>
                          </>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>
      </div>

      {editing?.kind === 'provider' && (
        <ProviderForm
          provider={editing.index >= 0 ? providers[editing.index] : null}
          names={providers.map((p) => p.name)}
          busy={busy}
          onCancel={() => setEditing(null)}
          onSave={async (p) => {
            const list = editing.index >= 0 ? providers.map((x, i) => (i === editing.index ? p : x)) : [...providers, p]
            if (await apply(withProviders(list))) setEditing(null)
          }}
        />
      )}

      <JSONCard key={pretty(cfg)} config={cfg} isDefault={saved.is_default} busy={busy} run={run} onApply={canApply ? (next) => apply(next) : null} />
    </>
  )
}

function AccountForm({ account, busy, onCancel, onSave }) {
  const known = ['', 'letsencrypt', 'letsencrypt-staging'].includes(account.directory || '')
  const [ca, setCa] = useState(known ? account.directory || 'letsencrypt' : 'custom')
  const [url, setUrl] = useState(known ? '' : account.directory)
  const [email, setEmail] = useState(account.email || '')
  const [terms, setTerms] = useState(!!account.accept_terms)
  const [eabKid, setEabKid] = useState(account.eab_key_id || '')
  const [eabHmac, setEabHmac] = useState('')
  const [caCert, setCaCert] = useState(account.directory_ca || '')
  const [key, setKey] = useState('')
  const save = () =>
    onSave(
      {
        directory: ca === 'custom' ? url.trim() : ca,
        email: email.trim(),
        accept_terms: terms,
        eab_key_id: eabKid.trim(),
        eab_hmac_key: eabHmac.trim(),
        directory_ca: caCert.trim() ? caCert : '',
      },
      key,
    )
  const readKey = (e) => {
    const f = e.target.files[0]
    if (f) f.text().then(setKey)
    e.target.value = ''
  }
  return (
    <div style={{ marginTop: '1rem' }}>
      <Card title="Edit the account" icon={KeyRound}>
        <div className="grid grid-2">
          <label className="field">
            <span>CA</span>
            <select value={ca} onChange={(e) => setCa(e.target.value)}>
              {CAS.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.label}
                </option>
              ))}
            </select>
          </label>
          <label className="field">
            <span>Contact email (optional)</span>
            <input type="email" value={email} onChange={(e) => setEmail(e.target.value)} placeholder="ops@example.com" />
          </label>
        </div>
        {ca === 'custom' && (
          <div className="grid grid-2">
            <label className="field">
              <span>Directory URL</span>
              <input className="mono" value={url} onChange={(e) => setUrl(e.target.value)} placeholder="https://acme.example.com/directory" />
            </label>
            <label className="field">
              <span>Its CA certificate (PEM) - only for a private CA</span>
              <textarea className="mono" rows={3} value={caCert} onChange={(e) => setCaCert(e.target.value)} placeholder="the node's trust store otherwise" />
            </label>
            <label className="field">
              <span>External account binding: key ID</span>
              <input className="mono" value={eabKid} onChange={(e) => setEabKid(e.target.value)} placeholder="if the CA requires one" />
            </label>
            <label className="field">
              <span>External account binding: HMAC key</span>
              <input
                className="mono"
                type="password"
                value={eabHmac}
                onChange={(e) => setEabHmac(e.target.value)}
                placeholder={account.eab_key_id ? 'saved - leave empty to keep' : ''}
              />
            </label>
          </div>
        )}
        <label className="row small" style={{ marginTop: '0.6rem' }}>
          <input type="checkbox" checked={terms} onChange={(e) => setTerms(e.target.checked)} />I accept the CA's terms of service (needed to register an
          account)
        </label>
        <div className="row small" style={{ marginTop: '0.6rem', flexWrap: 'wrap' }}>
          <label className="button small">
            <Upload size={13} /> Use an existing account key…
            <input type="file" hidden onChange={readKey} />
          </label>
          {key ? (
            <Badge tone="info">a key is ready to replace the node's</Badge>
          ) : (
            <span className="muted">to keep an account - and the thumbprint a HAProxy rule may already use</span>
          )}
        </div>
        <div className="row" style={{ marginTop: '0.8rem' }}>
          <button className="primary" disabled={busy || (ca === 'custom' && !url.trim())} onClick={save}>
            <CheckCircle2 size={15} /> Save…
          </button>
          <button onClick={onCancel}>Cancel</button>
        </div>
      </Card>
    </div>
  )
}

function CertForm({ cert, names, providers, busy, onCancel, onSave }) {
  const [name, setName] = useState(cert?.name || '')
  const [domains, setDomains] = useState((cert?.domains || []).join('\n'))
  const [challenge, setChallenge] = useState(cert?.challenge || 'http-01')
  const [provider, setProvider] = useState(cert?.dns_provider || providers[0] || '')
  const [keyType, setKeyType] = useState(cert?.key_type || 'ec256')
  const [profile, setProfile] = useState(cert?.profile || '')
  const list = lines(domains)
  const wildcard = list.some((d) => d.startsWith('*.'))
  const nameTaken = !cert && names.includes(name.trim())
  const save = () => {
    const c = { name: name.trim(), domains: list, key_type: keyType, challenge }
    if (challenge === 'dns-01') c.dns_provider = provider
    if (profile.trim()) c.profile = profile.trim()
    onSave(c)
  }
  return (
    <div style={{ marginTop: '1rem' }}>
      <Card className="data-title" title={cert ? `Edit ${cert.name}` : 'Add a certificate'} icon={ShieldCheck}>
        <div className="grid grid-2">
          <label className="field">
            <span>Name - its file is /etc/haproxy/acme/&lt;name&gt;.pem</span>
            <input className="mono" value={name} disabled={!!cert} onChange={(e) => setName(e.target.value)} placeholder="example.com" />
          </label>
          <label className="field">
            <span>Domains - one per line, the first is the subject</span>
            <textarea
              className="mono"
              rows={Math.min(8, Math.max(3, list.length + 1))}
              value={domains}
              onChange={(e) => setDomains(e.target.value)}
              placeholder={'example.com\nwww.example.com'}
            />
          </label>
          <label className="field">
            <span>Challenge</span>
            <select value={challenge} onChange={(e) => setChallenge(e.target.value)}>
              <option value="http-01">http-01 - answered by HAProxy on port 80</option>
              <option value="dns-01">dns-01 - a record created through a DNS provider</option>
            </select>
          </label>
          {challenge === 'dns-01' ? (
            <label className="field">
              <span>DNS provider</span>
              <select value={provider} onChange={(e) => setProvider(e.target.value)}>
                {!providers.length && <option value="">add one below first</option>}
                {providers.map((p) => (
                  <option key={p}>{p}</option>
                ))}
              </select>
            </label>
          ) : (
            <span />
          )}
          <label className="field">
            <span>Key type</span>
            <select value={keyType} onChange={(e) => setKeyType(e.target.value)}>
              {KEY_TYPES.map((k) => (
                <option key={k}>{k}</option>
              ))}
            </select>
          </label>
          <label className="field">
            <span>Profile (optional - the CA's, e.g. Let's Encrypt's shortlived)</span>
            <input className="mono" value={profile} onChange={(e) => setProfile(e.target.value)} />
          </label>
        </div>
        {wildcard && challenge !== 'dns-01' && <div className="notice">A wildcard needs the dns-01 challenge.</div>}
        {nameTaken && <div className="notice">A certificate is already named {name.trim()}.</div>}
        <div className="row" style={{ marginTop: '0.8rem' }}>
          <button className="primary" disabled={busy || !name.trim() || !list.length || nameTaken || (challenge === 'dns-01' && !provider)} onClick={save}>
            <CheckCircle2 size={15} /> Save…
          </button>
          <button onClick={onCancel}>Cancel</button>
        </div>
      </Card>
    </div>
  )
}


// JSONCard: the whole configuration, as janusctl haproxy acme get prints
// it - secrets empty, kept when applied empty.
function JSONCard({ config, isDefault, busy, run, onApply }) {
  const text = pretty(config)
  const [draft, setDraft] = useState(text)
  const [check, setCheck] = useState(null)
  let parsed = null
  let parseError = null
  try {
    parsed = JSON.parse(draft)
  } catch (err) {
    parseError = err.message
  }
  const dirty = draft !== text
  return (
    <details style={{ marginTop: '1rem' }}>
      <summary className="muted small" style={{ cursor: 'pointer' }}>
        The configuration as JSON{isDefault ? ' - nothing saved yet' : ''}
      </summary>
      <div style={{ marginTop: '0.6rem' }}>
        <Card title="letsencrypt.json">
          <Editor
            label="letsencrypt.json"
            value={draft}
            onChange={(v) => {
              setDraft(v)
              setCheck(null)
            }}
          />
          <div className="row" style={{ marginTop: '0.8rem' }}>
            <button disabled={busy || !!parseError} onClick={() => run(async () => setCheck(await postJSON('/api/haproxy/acme/check', { config: parsed })))}>
              <CheckCircle2 size={15} /> Check
            </button>
            {onApply && (
              <button className="primary" disabled={busy || !dirty || !!parseError} onClick={() => onApply(parsed)}>
                <Upload size={15} /> Apply…
              </button>
            )}
            <button disabled={!dirty} onClick={() => setDraft(text)}>
              Revert
            </button>
          </div>
          {parseError && (
            <div className="error-box" style={{ marginTop: '0.8rem' }}>
              {parseError}
            </div>
          )}
          {check && (
            <div style={{ marginTop: '0.8rem' }}>
              {check.accepted ? <div className="notice">Valid.</div> : <div className="error-box">{(check.errors || ['refused']).join('\n')}</div>}
            </div>
          )}
        </Card>
      </div>
    </details>
  )
}

// How the node reaches the CA (docs/letsencrypt.md, "How the node reaches
// Let's Encrypt"): the modes side by side - who talks to the CA, what
// must be open -, the chosen one's settings, a test fetching the CA's
// directory that way, and what this node's certificates need open.
const PATH_DOCS = `${DOCS}#how-the-node-reaches-lets-encrypt`
const MODES = [
  {
    id: 'ACME_PATH_MODE_DIRECT',
    title: 'Directly',
    who: 'The node talks to the CA itself.',
    needs: 'The node reaches the CA over HTTPS - and, for DNS-01, its DNS provider’s API.',
  },
  {
    id: 'ACME_PATH_MODE_PROXY',
    title: 'Through a proxy',
    who: 'The node talks to the CA through an HTTP(S) proxy.',
    needs: 'The node reaches the proxy; the proxy reaches the CA and the DNS providers’ APIs.',
  },
  {
    id: 'ACME_PATH_MODE_CONTROLLER_RELAY',
    title: 'Through this Controller',
    who: 'The node talks to the CA through the Controller’s relay - encrypted end to end, the relay sees host names only.',
    needs: 'The node reaches the Controller; the Controller reaches the CA and the DNS providers’ APIs.',
  },
  {
    id: 'ACME_PATH_MODE_CONTROLLER_ORDERED',
    title: 'Ordered by this Controller',
    who: 'The Controller orders the certificates with its own account and installs them - their keys never leave the node.',
    needs: 'The node reaches nothing; the Controller reaches the CA and the DNS providers’ APIs.',
  },
]
const RELAY = 'ACME_PATH_MODE_CONTROLLER_RELAY'
const ORDERED = 'ACME_PATH_MODE_CONTROLLER_ORDERED'
const modeOf = (path) => (MODES.some((m) => m.id === path?.mode) ? path.mode : 'ACME_PATH_MODE_DIRECT')
const pathToSend = (p) => {
  const mode = modeOf(p)
  if (mode === 'ACME_PATH_MODE_DIRECT') return { mode }
  if (mode === RELAY) return { mode, proxy_url: p.proxy_url, proxy_ca: p.proxy_ca }
  if (mode === ORDERED) return { mode, account_thumbprint: p.account_thumbprint }
  return { ...p, mode, account_thumbprint: undefined }
}
// The Controller's own page, its Let's Encrypt tab (admins).
const CONTROLLER_TAB = '/#/letsencrypt'

function PathCard({ cfg, st, canApply, canCheck, busy, run, apply }) {
  const saved = cfg.path || {}
  const [draft, setDraft] = useState(null) // the path being edited
  const [check, setCheck] = useState(null) // the last test's answer
  const old = st.path_mode === 'unspecified' // a node older than the paths
  const path = draft || saved
  const mode = modeOf(path)
  const controller = usePoll('/api/haproxy/acme/controller', { every: 60000, enabled: !old }).data
  const relay = controller?.relay
  const ordering = controller?.ordering
  const relayReady = !!(relay?.enabled && relay?.listening && relay?.url)
  const orderingReady = !!(ordering?.ready && ordering?.thumbprint)
  const edit = (change) => {
    setDraft({ ...path, ...change })
    setCheck(null)
  }
  // The relay's address and the Controller's identity come with the
  // choice: nothing to type.
  // The Controller's account comes with "Ordered by this Controller".
  const choose = (id) => {
    if (id === RELAY) edit({ mode: id, proxy_url: relay?.url || '', proxy_ca: relay?.ca_cert_pem || '', proxy_username: '', proxy_password: '', account_thumbprint: '' })
    else if (id === ORDERED) edit({ mode: id, proxy_url: '', proxy_ca: '', proxy_username: '', proxy_password: '', account_thumbprint: ordering?.thumbprint || '' })
    else edit({ mode: id, account_thumbprint: '', ...([RELAY, ORDERED].includes(modeOf(saved)) ? { proxy_url: '', proxy_ca: '' } : {}) })
  }
  const moved = modeOf(saved) === RELAY && !draft && relayReady && saved.proxy_url !== relay.url
  const otherAccount = modeOf(saved) === ORDERED && !draft && orderingReady && saved.account_thumbprint !== ordering.thumbprint
  // A relay that's off or doesn't listen, ordering not set up: neither
  // can be tried nor chosen.
  const incomplete =
    (mode === 'ACME_PATH_MODE_PROXY' && !path.proxy_url) ||
    (mode === RELAY && (!path.proxy_url || !path.proxy_ca || !relayReady)) ||
    (mode === ORDERED && (!path.account_thumbprint || !orderingReady))
  const test = () =>
    run(async () => {
      setCheck(await postJSON('/api/haproxy/acme/check-path', { path: pathToSend(path) }))
    })
  const save = async () => {
    const next = { ...cfg }
    if (modeOf(draft) === 'ACME_PATH_MODE_DIRECT') delete next.path
    else next.path = { ...pathToSend(draft) }
    const how = {
      ACME_PATH_MODE_DIRECT: 'From now on the node reaches the CA directly.',
      ACME_PATH_MODE_PROXY: `From now on the node reaches the CA through the proxy ${draft.proxy_url}. A password left empty keeps the saved one.`,
      [RELAY]: `From now on the node reaches the CA through the Controller’s relay ${draft.proxy_url}.`,
      [ORDERED]: 'From now on the Controller orders this node’s certificates; HAProxy answers HTTP-01 for the Controller’s account (it reloads).',
    }
    const note = how[modeOf(draft)]
    if (await apply(next, { title: 'Change how the node reaches the CA?', note })) {
      setDraft(null)
      setCheck(null)
    }
  }
  return (
    <Card
      title="How this node reaches Let's Encrypt"
      icon={Route}
      className="acme-path"
      actions={
        <a className="small" href={PATH_DOCS} target="_blank" rel="noreferrer">
          Which one? <ExternalLink size={11} />
        </a>
      }
    >
      {old ? (
        <div className="notice">This node’s image reaches the CA directly - choosing another way needs a newer one (System › Update).</div>
      ) : (
        <>
          <div className="acme-modes" role="radiogroup" aria-label="How the node reaches the CA">
            {MODES.map((m) => (
              <label key={m.id} className={`acme-mode ${mode === m.id ? 'active' : ''}`}>
                <input type="radio" name="acme-path" value={m.id} checked={mode === m.id} disabled={!canApply} onChange={() => choose(m.id)} />
                <span className="acme-mode-title">
                  {m.title}
                  {modeOf(saved) === m.id && <Badge tone="ok">in use</Badge>}
                </span>
                <span className="small">{m.who}</span>
                <span className="small muted">{m.needs}</span>
              </label>
            ))}
          </div>
          {mode === 'ACME_PATH_MODE_PROXY' && (
            <fieldset className="plain acme-proxy" disabled={!canApply}>
              <label className="field">
                <span>Proxy URL</span>
                <input className="mono" value={path.proxy_url || ''} placeholder="http://proxy.example.net:3128" onChange={(e) => edit({ proxy_url: e.target.value.trim() })} />
              </label>
              <label className="field">
                <span>Username</span>
                <input value={path.proxy_username || ''} placeholder="none" autoComplete="off" onChange={(e) => edit({ proxy_username: e.target.value.trim() })} />
              </label>
              <label className="field">
                <span>Password</span>
                <input type="password" value={path.proxy_password || ''} placeholder={saved.proxy_username ? 'kept as saved' : 'none'} autoComplete="new-password" onChange={(e) => edit({ proxy_password: e.target.value })} />
              </label>
              <label className="field acme-proxy-ca">
                <span>The proxy’s CA (PEM) - an https:// proxy whose certificate the node’s trust store doesn’t know</span>
                <textarea className="mono small" rows={3} value={path.proxy_ca || ''} placeholder="-----BEGIN CERTIFICATE-----" onChange={(e) => edit({ proxy_ca: e.target.value })} />
              </label>
            </fieldset>
          )}
          {mode === RELAY && <RelaySettings relay={relay} path={path} moved={moved} canApply={canApply} onFollow={() => choose(RELAY)} />}
          {mode === ORDERED && <OrderingSettings ordering={ordering} path={path} otherAccount={otherAccount} canApply={canApply} onFollow={() => choose(ORDERED)} />}
          <div className="row" style={{ marginTop: '0.75rem' }}>
            {canCheck && mode !== ORDERED && (
              <button className="small" disabled={busy || incomplete} onClick={test}>
                <RefreshCw size={13} /> Test
              </button>
            )}
            {canApply && draft && (
              <>
                <button className="small primary" disabled={busy || incomplete} onClick={save}>
                  Save
                </button>
                <button className="small ghost" disabled={busy} onClick={() => (setDraft(null), setCheck(null))}>
                  Cancel
                </button>
              </>
            )}
            {check && (
              <span className="small" role="status">
                <Badge tone={check.ok ? 'ok' : 'danger'}>{check.ok ? 'reached' : 'failed'}</Badge>{' '}
                {check.ok ? `${check.directory} ${check.via}, in ${check.milliseconds} ms.` : `${check.directory || 'The CA'} ${check.via || ''}: ${check.error}`}
              </span>
            )}
          </div>
        </>
      )}
      <Needs cfg={cfg} st={st} mode={old ? 'ACME_PATH_MODE_DIRECT' : modeOf(path)} proxy={path.proxy_url} relay={relay} ordering={ordering} choosing={!!draft} />
    </Card>
  )
}

// RelaySettings: the Controller's relay as this node would use it - or
// why it can't yet.
function RelaySettings({ relay, path, moved, canApply, onFollow }) {
  if (!relay) return <Loading />
  if (!relay.enabled) {
    return (
      <div className="notice acme-relay">
        <span>
          The Controller’s relay is off. An admin turns it on on the <a href={CONTROLLER_TAB}>Controller’s Let’s Encrypt tab</a> - then this node can use it.
        </span>
      </div>
    )
  }
  if (!relay.listening) {
    return (
      <div className="notice danger acme-relay">
        <span>The Controller’s relay doesn’t listen: {relay.error || 'see the Controller’s Let’s Encrypt tab'}.</span>
      </div>
    )
  }
  return (
    <div className="acme-relay small">
      <div>
        The node goes through the relay at <span className="mono">{path.proxy_url || relay.url}</span>, checking it with the Controller’s identity; the relay knows the node by a certificate of the node’s own CA, made for each request.
      </div>
      {moved && (
        <div className="notice">
          <span>
            The relay is now at <span className="mono">{relay.url}</span>, not <span className="mono">{path.proxy_url}</span>.{' '}
            {canApply && (
              <button className="small" onClick={onFollow}>
                Use the new address
              </button>
            )}
          </span>
        </div>
      )}
    </div>
  )
}

// OrderingSettings: the Controller ordering this node's certificates -
// or why it can't yet.
function OrderingSettings({ ordering, path, otherAccount, canApply, onFollow }) {
  if (!ordering) return <Loading />
  if (!ordering.ready) {
    return (
      <div className="notice acme-relay">
        <span>
          {ordering.available ? `The Controller can’t order yet: ${ordering.why}.` : 'This Controller doesn’t order certificates.'} See the <a href={CONTROLLER_TAB}>Controller’s Let’s Encrypt tab</a>.
        </span>
      </div>
    )
  }
  return (
    <div className="acme-relay small">
      <div>
        The Controller asks this node for a key and its request for each certificate due, orders it from the CA this configuration names with its account (<span className="mono">{path.account_thumbprint || ordering.thumbprint}</span>), and installs what comes back. HAProxy answers HTTP-01 for that account; DNS-01 goes through the Controller’s DNS providers, by name.
      </div>
      {otherAccount && (
        <div className="notice">
          <span>
            This node answers HTTP-01 for another account than this Controller’s.{' '}
            {canApply && (
              <button className="small" onClick={onFollow}>
                Use this Controller’s
              </button>
            )}
          </span>
        </div>
      )}
    </div>
  )
}

// hostPort is a URL's host:port, 443 by default; '' when it isn't one.
function hostPort(u) {
  try {
    const url = new URL(u)
    return `${url.hostname}:${url.port || (url.protocol === 'http:' ? 80 : 443)}`
  } catch {
    return ''
  }
}

// Needs: what must be open for this node's certificates - the saved way,
// or the one being chosen.
function Needs({ cfg, st, mode, proxy, relay, ordering, choosing }) {
  const certs = cfg.certificates || []
  const http = certs.filter((c) => (c.challenge || 'http-01') === 'http-01').map((c) => c.name)
  const dnsCerts = certs.filter((c) => c.challenge === 'dns-01')
  const types = [...new Set(dnsCerts.map((c) => (cfg.dns_providers || []).find((p) => p.name === c.dns_provider)?.type).filter(Boolean))]
  let ca = st.directory
  try {
    ca = new URL(st.directory).host
  } catch {
    // not a URL: shown as is
  }
  const lines = []
  if (mode === ORDERED) {
    lines.push(`The Controller reaches the CA (${ca}) over HTTPS, and this node’s API - the node reaches nothing itself.`)
    if (http.length) lines.push(`The CA reaches this node on port 80 from the internet - HTTP-01, for ${http.join(', ')}: HAProxy answers on that port.`)
    const names = (ordering?.dns_providers || []).map((p) => p.name)
    for (const c of dnsCerts) {
      if (ordering && !names.includes(c.dns_provider)) lines.push(`${c.name}: the Controller has no DNS provider named ${c.dns_provider || '(none)'} - an admin adds it on its Let’s Encrypt tab.`)
    }
    if (dnsCerts.length) lines.push('DNS-01: the Controller reaches its DNS providers’ APIs, and asks DNS to see the record appear.')
    return (
      <div className="acme-needs">
        <div className="small muted">{choosing ? 'What must be open for this node’s certificates with this choice:' : 'What must be open for this node’s certificates:'}</div>
        <ul className="small">
          {lines.map((l) => (
            <li key={l}>{l}</li>
          ))}
        </ul>
      </div>
    )
  }
  if (mode === RELAY) {
    const builtIn = Object.values(relay?.built_in || {}).flat()
    const reaches = (hp) => builtIn.some((h) => `${h}:443` === hp) || (relay?.allowed || []).includes(hp)
    const caHP = hostPort(st.directory)
    lines.push(`The node reaches the Controller’s relay (${hostPort(proxy) || 'its address'}) over HTTPS; the relay reaches the CA (${ca}).`)
    if (relay && caHP && !reaches(caHP)) lines.push(`The relay doesn’t let nodes reach ${caHP} yet - an admin allows it on the Controller’s Let’s Encrypt tab.`)
    if (http.length) lines.push(`The CA reaches this node on port 80 from the internet - HTTP-01, for ${http.join(', ')}: HAProxy answers on that port.`)
    for (const t of types) {
      const never = relay?.not_through_relay?.[t]
      if (never) lines.push(`${t} can’t go through the relay: ${never}.`)
      else if (relay?.built_in?.[t]) lines.push(`The relay reaches the ${t} API - DNS-01.`)
      else lines.push(`The relay reaches the ${t} API only once an admin allows the host this node’s ${t} settings name, on the Controller’s Let’s Encrypt tab - DNS-01.`)
    }
  } else {
    const through = mode === 'ACME_PATH_MODE_PROXY' ? ` through the proxy ${proxy || '(its URL)'}` : ''
    lines.push(`The node reaches the CA (${ca}) over HTTPS${through}.`)
    if (http.length) lines.push(`The CA reaches this node on port 80 from the internet - HTTP-01, for ${http.join(', ')}: HAProxy answers on that port.`)
    const api = types.filter((t) => t !== 'rfc2136')
    if (api.length) lines.push(`The node reaches the ${api.join(', ')} API over HTTPS${through} - DNS-01.`)
    if (types.includes('rfc2136')) lines.push('The node sends rfc2136 updates straight to the zone’s name server (port 53) - a proxy can’t carry them.')
  }
  if (types.length) lines.push('The node asks DNS (its resolvers, then the zone’s servers) to find the zone and see the record appear - propagation_wait_seconds waits instead of checking.')
  return (
    <div className="acme-needs">
      <div className="small muted">{choosing ? 'What must be open for this node’s certificates with this choice:' : 'What must be open for this node’s certificates:'}</div>
      <ul className="small">
        {lines.map((l) => (
          <li key={l}>{l}</li>
        ))}
      </ul>
    </div>
  )
}
