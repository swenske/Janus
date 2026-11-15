import { Copy, ExternalLink, KeyRound, Pencil, Plus, RefreshCw, ScrollText, ShieldCheck, Trash2, Waypoints } from 'lucide-react'
import { useState } from 'react'
import { postJSON } from '../api.js'
import { Badge, Card, ErrorBox, Loading, PageHeader, useAction, useConfirm, useToast } from '../../shared/ui.jsx'
import { hunks, lineDiff } from '../../shared/diff.js'
import { DiffView } from '../../shared/Editor.jsx'
import { AccountForm, CertForm, DOCS, JSONCard, PathCard } from '../../shared/ACMEConfig.jsx'
import { pretty } from '../../shared/acmeConfig.js'
import { dateTime } from '../format.js'
import { usePoll } from '../hooks.jsx'
import ProviderForm from '../../shared/ACMEProviderForm.jsx'
import { ClusterLockNotice } from '../components/ClusterLock.jsx'
import { useMay } from '../may.js'

const STATE_TONES = { valid: 'ok', pending: 'warn', due: 'info', expired: 'danger' }

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
  // The Controller's relay and ordering, for the way to the CA - not
  // asked of a node older than the ways (path_mode unspecified).
  const controller = usePoll('/api/haproxy/acme/controller', { every: 60000, enabled: !!status.data && status.data.path_mode !== 'unspecified' }).data
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

      <ClusterLockNotice domain="acme" />

      <PathCard
        cfg={cfg}
        st={st}
        canApply={canApply}
        canCheck={may('HAProxyService/ACMECheckPath')}
        busy={busy}
        run={run}
        apply={apply}
        controller={controller}
        onCheckPath={(path) => postJSON('/api/haproxy/acme/check-path', { path })}
      />

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

      <JSONCard
        key={pretty(cfg)}
        config={cfg}
        isDefault={saved.is_default}
        busy={busy}
        run={run}
        onCheck={(next) => postJSON('/api/haproxy/acme/check', { config: next })}
        onApply={canApply ? (next) => apply(next) : null}
      />
    </>
  )
}

