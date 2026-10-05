import { CheckCircle2, Copy, ExternalLink, KeyRound, Pencil, Plus, RefreshCw, ScrollText, ShieldCheck, Trash2, Upload, Waypoints } from 'lucide-react'
import { useState } from 'react'
import { postJSON } from '../api.js'
import { Badge, Card, ErrorBox, Loading, PageHeader, useAction, useConfirm, useToast } from '../../shared/ui.jsx'
import { hunks, lineDiff } from '../diff.js'
import { DiffView, Editor } from '../components/Editor.jsx'
import { dateTime } from '../format.js'
import { usePoll } from '../hooks.jsx'
import { useMay } from '../may.js'

const DOCS = 'https://github.com/swenske/Janus/blob/main/docs/letsencrypt.md'

const CAS = [
  { id: 'letsencrypt', label: "Let's Encrypt" },
  { id: 'letsencrypt-staging', label: "Let's Encrypt staging - test certificates, untrusted" },
  { id: 'custom', label: 'Another ACME CA (its directory URL)' },
]

// The DNS providers janus-acme has, with the settings they usually need
// (lego's names - docs/letsencrypt.md lists them all).
const PROVIDERS = {
  gandiv5: ['GANDIV5_PERSONAL_ACCESS_TOKEN'],
  ovh: ['OVH_ENDPOINT', 'OVH_APPLICATION_KEY', 'OVH_APPLICATION_SECRET', 'OVH_CONSUMER_KEY'],
  cloudflare: ['CLOUDFLARE_DNS_API_TOKEN'],
  route53: ['AWS_ACCESS_KEY_ID', 'AWS_SECRET_ACCESS_KEY', 'AWS_REGION', 'AWS_HOSTED_ZONE_ID'],
  pdns: ['PDNS_API_URL', 'PDNS_API_KEY'],
  rfc2136: ['RFC2136_NAMESERVER', 'RFC2136_TSIG_KEY', 'RFC2136_TSIG_SECRET', 'RFC2136_TSIG_ALGORITHM'],
  hetzner: ['HETZNER_API_TOKEN'],
  digitalocean: ['DO_AUTH_TOKEN'],
  scaleway: ['SCALEWAY_API_TOKEN', 'SCALEWAY_PROJECT_ID'],
  ionos: ['IONOS_API_KEY'],
  infomaniak: ['INFOMANIAK_ACCESS_TOKEN'],
  desec: ['DESEC_TOKEN'],
  httpreq: ['HTTPREQ_ENDPOINT', 'HTTPREQ_USERNAME', 'HTTPREQ_PASSWORD'],
}
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
              {st.account_uri ? (
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
      <Card title={cert ? `Edit ${cert.name}` : 'Add a certificate'} icon={ShieldCheck}>
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

function ProviderForm({ provider, names, busy, onCancel, onSave }) {
  const [name, setName] = useState(provider?.name || '')
  const [type, setType] = useState(provider?.type || 'gandiv5')
  // Saved settings come back without their values: empty keeps them.
  const [settings, setSettings] = useState(() =>
    provider ? Object.keys(provider.settings || {}).map((k) => ({ key: k, value: '', saved: true })) : PROVIDERS.gandiv5.map((k) => ({ key: k, value: '' })),
  )
  const [resolvers, setResolvers] = useState((provider?.resolvers || []).join(' '))
  const [wait, setWait] = useState(provider?.propagation_wait_seconds ? String(provider.propagation_wait_seconds) : '')
  const changeType = (t) => {
    setType(t)
    if (!provider) setSettings(PROVIDERS[t].map((k) => ({ key: k, value: '' })))
  }
  const set = (i, change) => setSettings(settings.map((s, j) => (j === i ? { ...s, ...change } : s)))
  const used = settings.filter((s) => s.key.trim() && (s.value || s.saved))
  const nameTaken = !provider && names.includes(name.trim())
  const save = () => {
    const p = { name: name.trim(), type, settings: Object.fromEntries(used.map((s) => [s.key.trim(), s.value])) }
    const r = lines(resolvers)
    if (r.length) p.resolvers = r
    if (Number(wait) > 0) p.propagation_wait_seconds = Number(wait)
    onSave(p)
  }
  return (
    <div style={{ marginTop: '1rem' }}>
      <Card title={provider ? `Edit ${provider.name}` : 'Add a DNS provider'} icon={Waypoints}>
        <div className="grid grid-2">
          <label className="field">
            <span>Name - what certificates refer to it by</span>
            <input className="mono" value={name} disabled={!!provider} onChange={(e) => setName(e.target.value)} placeholder="gandi" />
          </label>
          <label className="field">
            <span>Type</span>
            <select value={type} onChange={(e) => changeType(e.target.value)} disabled={!!provider}>
              {Object.keys(PROVIDERS).map((t) => (
                <option key={t}>{t}</option>
              ))}
            </select>
          </label>
        </div>
        <div className="field" style={{ marginTop: '0.6rem' }}>
          <span className="field-label">Settings - lego's names for {type}; see the guide for every option</span>
          {settings.map((s, i) => (
            <div className="row" key={i} style={{ marginBottom: '0.3rem' }}>
              <input
                className="mono"
                style={{ width: '45%' }}
                value={s.key}
                disabled={s.saved}
                onChange={(e) => set(i, { key: e.target.value.toUpperCase() })}
                placeholder="SETTING_NAME"
              />
              <input
                className="mono grow"
                type="password"
                autoComplete="off"
                value={s.value}
                onChange={(e) => set(i, { value: e.target.value })}
                placeholder={s.saved ? 'saved - leave empty to keep' : 'value'}
              />
              <button className="small" title="Remove" onClick={() => setSettings(settings.filter((_, j) => j !== i))}>
                <Trash2 size={13} />
              </button>
            </div>
          ))}
          <div>
            <button className="small" onClick={() => setSettings([...settings, { key: `${PROVIDERS[type][0].split('_')[0]}_`, value: '' }])}>
              <Plus size={13} /> Add a setting
            </button>
          </div>
        </div>
        <div className="grid grid-2" style={{ marginTop: '0.6rem' }}>
          <label className="field">
            <span>Resolvers for the propagation check (optional)</span>
            <input
              className="mono"
              value={resolvers}
              onChange={(e) => setResolvers(e.target.value)}
              placeholder="1.1.1.1 9.9.9.9 - public ones with split-horizon DNS"
            />
          </label>
          <label className="field">
            <span>Or wait instead of checking (seconds, optional)</span>
            <input type="number" min={0} max={3600} value={wait} onChange={(e) => setWait(e.target.value)} placeholder="check that the record is visible" />
          </label>
        </div>
        {nameTaken && <div className="notice">A provider is already named {name.trim()}.</div>}
        <div className="row" style={{ marginTop: '0.8rem' }}>
          <button className="primary" disabled={busy || !name.trim() || nameTaken || !used.length} onClick={save}>
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
