import { ExternalLink, KeyRound, Lock, Pencil, Plus, RefreshCw, ShieldCheck, Trash2, Unlock, Users, Waypoints } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { call, postJSON } from './call.js'
import { ACME_WAYS, acmeCertStates, acmeInStep, pathCheckSummary } from './clusters.js'
import { useAct, useCan } from './me.jsx'
import { AccountForm, CertForm, JSONCard, PathCard } from './shared/ACMEConfig.jsx'
import { CLUSTER_WORDS, pretty } from './shared/acmeConfig.js'
import ProviderForm from './shared/ACMEProviderForm.jsx'
import { hunks, lineDiff } from './shared/diff.js'
import { DiffView } from './shared/Editor.jsx'
import { fmtDateTime } from './shared/units.js'
import { Badge, Card, ErrorBox, Loading, useAction, useConfirm } from './shared/ui.jsx'

// A cluster's Let's Encrypt (docs/guide/clusters.md "Let's Encrypt"), its
// tab on the cluster's page: who orders - the member the secrets come
// from -, each member's way to the CA and certificates, and the
// configuration itself, edited here as on a node's page. Saving makes it
// the cluster's (PUT /api/clusters/<id>/letsencrypt): checked on the
// orderer, given to it with its secrets - never kept by the Controller -,
// then the cluster's next version carries it without them, applied on
// every member and locked there; the others pull the secrets from the
// orderer.

const STATE_TONES = { valid: 'ok', pending: 'warn', due: 'info', expired: 'danger' }
const GUIDE = 'https://janus.sw-servers.net/docs/guide/clusters.md#lets-encrypt'

export default function ClusterLetsEncrypt({ cluster, reload }) {
  const offer = useAct()
  const can = useCan()
  const confirm = useConfirm()
  const [busy, run] = useAction()
  const [view, setView] = useState(null)
  const [error, setError] = useState(null)
  const [errors, setErrors] = useState(null)
  const [editing, setEditing] = useState(null) // { kind: 'account'|'cert'|'provider', index }
  const id = cluster.id
  const load = useCallback(
    (background) =>
      call(`/api/clusters/${id}/letsencrypt`, { background, timeout: 30000 }).then(
        (v) => {
          setView(v)
          setError(null)
        },
        setError,
      ),
    [id],
  )
  // Read again when the cluster moves: a new version, another orderer,
  // an operation that ended.
  useEffect(() => {
    load(false)
  }, [load, cluster.desired, cluster.secrets_from, cluster.busy])
  if (!view) return error ? <ErrorBox error={error} /> : <Loading />

  const managed = cluster.managed.includes('acme')
  const canApply = offer('admin') && !cluster.busy
  const cfg = view.config || {}
  const st = view.status || {}
  const orderer = cluster.members.find((m) => m.name === view.orderer)
  const ordererPage = orderer ? `/nodes/${encodeURIComponent(orderer.node_id)}/#/apps/letsencrypt` : ''
  const canTest = can('admin') // a test changes nothing: kept while read-only

  // put sends a configuration; a newer version than the members' waiting
  // (409): which one to start from is asked, then it goes again.
  const put = async (body) => {
    try {
      return await postJSON(`/api/clusters/${id}/letsencrypt`, body, 'PUT')
    } catch (err) {
      if (err.status !== 409 || !err.data?.unapplied) throw err
      const { unapplied, desired } = err.data
      const choice = { base: desired }
      const ok = await confirm({
        title: `Version ${unapplied} isn't applied yet`,
        body: (
          <>
            <p>
              Version {unapplied} is newer than the one the members run (v{desired}) and waits, not applied. Which one does Let’s Encrypt’s configuration start from?
            </p>
            <label className="check small">
              <input type="radio" name="acme-base" defaultChecked onChange={() => (choice.base = desired)} /> v{desired}, what the members run - v{unapplied} stays aside, not applied
            </label>
            <label className="check small">
              <input type="radio" name="acme-base" onChange={() => (choice.base = unapplied)} /> v{unapplied} - its changes applied with Let’s Encrypt’s
            </label>
          </>
        ),
        action: 'Apply',
      })
      if (!ok) return undefined
      return postJSON(`/api/clusters/${id}/letsencrypt`, { ...body, base: choice.base }, 'PUT')
    }
  }
  const renew = (names) =>
    run(
      () => postJSON(`/api/clusters/${id}/letsencrypt/renew`, { names }),
      (r) => `${view.orderer} obtains ${(r?.names || names).join(', ') || 'every certificate'} now - the others pull it at the next check (Check now).`,
    )

  // apply shows the change, then makes it the cluster's; true once the
  // operation started.
  const apply = async (next, { title = "Make this the cluster's Let's Encrypt configuration?", note, accountKey = '' } = {}) => {
    const diff = hunks(lineDiff(pretty(cfg), pretty(next)))
    const ok = await confirm({
      title,
      wide: true,
      action: 'Apply',
      body: (
        <>
          {note && <p>{note}</p>}
          {accountKey && <p>The account key goes to every member - relayed, never kept: whichever orders uses that account, and HAProxy answers HTTP-01 with its thumbprint (it reloads).</p>}
          <p>
            Checked on {view.orderer}, which orders, and given to it - a secret typed here goes to it alone, never kept by the Controller. Then the cluster’s next version
            carries the configuration without its secrets, applied one member at a time: each pulls the secrets from {view.orderer}.
            {!managed && ' From then on the cluster manages it: the same on every member, locked there - changed here only.'}
          </p>
          <DiffView diff={diff} />
        </>
      ),
    })
    if (!ok) return false
    const r = await run(async () => {
      try {
        return await put({ config: next, account_key: accountKey })
      } catch (err) {
        setErrors(err.data?.errors || null)
        throw err
      }
    }, "Let's Encrypt's configuration is being applied to the members.")
    if (!r) return false
    setErrors(null)
    reload()
    return true
  }
  const stopManaging = async () => {
    const ok = await confirm({
      title: `Stop managing Let's Encrypt in ${cluster.name}?`,
      body: (
        <p>
          Version {cluster.desired} is applied again without the lock on Let’s Encrypt: each member keeps the configuration it runs, changed on its own page from then on.{' '}
          {view.orderer} still orders, the others still pull its certificates.
        </p>
      ),
      action: 'Stop managing',
    })
    if (!ok) return
    run(async () => {
      await postJSON(`/api/clusters/${id}/apply`, { version: cluster.desired, managed: cluster.managed.filter((d) => d !== 'acme') })
      reload()
    }, `Version ${cluster.desired} is being applied without Let's Encrypt's lock.`)
  }

  const certs = cfg.certificates || []
  const providers = cfg.dns_providers || []
  const withCerts = (list) => ({ ...cfg, certificates: list })
  const withProviders = (list) => ({ ...cfg, dns_providers: list })
  const ordererCert = (name) => (view.members.find((m) => m.name === view.orderer)?.certificates || []).find((c) => c.name === name) || {}

  if (st.state === 'not_enabled') {
    return <div className="notice">The members’ image has no letsencrypt extension. Add it to the cluster’s image (Update, Change extensions…), member by member.</div>
  }
  return (
    <div className="stack cluster-acme">
      <ErrorBox error={error || (view.error ? new Error(view.error) : null)} />
      <Card
        title="In this cluster"
        icon={managed ? Lock : Users}
        actions={
          <>
            <a className="small" href={GUIDE} target="_blank" rel="noreferrer">
              Guide <ExternalLink size={11} />
            </a>
            {managed && canApply && (
              <button className="small" disabled={busy} onClick={stopManaging}>
                <Unlock size={13} /> Stop managing…
              </button>
            )}
          </>
        }
      >
        <p className="small" style={{ marginTop: 0 }}>
          {managed ? (
            <>
              <Badge tone="ok">managed</Badge> The configuration is version {cluster.desired}’s <span className="mono">letsencrypt.json</span>: the same on every member, locked
              there - whichever member orders, the others are armed alike. Its secrets are on {view.orderer}; the others pull them from it.
            </>
          ) : (
            <>
              <Badge>each member’s</Badge> {view.orderer} orders with its own configuration; the others pull it with the certificates, but keep their own way to the CA. Saving a
              change here makes it the cluster’s - the same on every member, locked there.
            </>
          )}
        </p>
        <p className="small muted">
          {view.orderer} orders: the member the secrets come from (Members tab). The others order nothing, serve its certificates and answer HTTP-01 with its account.
          {!acmeInStep(view) && ' Some members don’t serve its certificates yet: the next check has them pull them.'}
        </p>
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Member</th>
                <th>Reaches the CA</th>
                <th>Orders</th>
                <th>Certificates</th>
              </tr>
            </thead>
            <tbody>
              {view.members.map((m) => (
                <tr key={m.name}>
                  <td className="mono">{m.name}</td>
                  <td className="small">{m.error ? <span style={{ color: 'var(--danger)' }}>{m.error}</span> : m.state === 'running' ? ACME_WAYS[m.path_mode] || m.path_mode : m.state}</td>
                  <td className="small">{m.name === view.orderer ? <Badge tone="accent">orders</Badge> : m.ordered_by ? <span className="muted">follows {m.ordered_by}</span> : '-'}</td>
                  <td className="small">
                    {(m.certificates || []).length === 0
                      ? '-'
                      : m.certificates.map((c) => (
                          <span key={c.name} title={c.serial ? `serial ${c.serial}` : undefined}>
                            <Badge tone={STATE_TONES[c.state]} dot>
                              {c.name}
                            </Badge>{' '}
                          </span>
                        ))}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Card>

      {errors && <div className="error-box">{errors.join('\n')}</div>}

      <PathCard
        cfg={cfg}
        st={st}
        canApply={canApply}
        canCheck={canTest}
        onCheckPath={async (path) => pathCheckSummary(await postJSON(`/api/clusters/${id}/letsencrypt/check-path`, { path }))}
        busy={busy}
        run={run}
        apply={apply}
        controller={view.controller}
        words={CLUSTER_WORDS}
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
              {st.path_mode === 'controller_ordered' ? <span>the Controller’s - it orders the certificates</span> : <span>{view.orderer}’s own, each member keeps its key</span>}
            </dd>
            <dt>Contact</dt>
            <dd>{cfg.account?.email || <span className="muted">none</span>}</dd>
            <dt>Terms</dt>
            <dd>{cfg.account?.accept_terms ? <Badge tone="ok">accepted</Badge> : <Badge tone="warn">not accepted</Badge>}</dd>
            <dt>Thumbprint</dt>
            <dd className="mono small">{st.account_thumbprint}</dd>
          </dl>
        </Card>
        <Card title="HTTP-01 challenges" icon={ShieldCheck}>
          <p className="small" style={{ marginTop: 0 }}>
            Any member may receive a challenge: each answers with {view.orderer}’s account. The rule, in each frontend on port 80 that receives them, before any redirect:
          </p>
          <pre className="code small" style={{ whiteSpace: 'pre-wrap' }}>
            {st.http01_rule}
          </pre>
        </Card>
      </div>

      {editing?.kind === 'account' && (
        <AccountForm
          account={cfg.account || {}}
          busy={busy}
          onCancel={() => setEditing(null)}
          onSave={async (account, key) => (await apply({ ...cfg, account }, { accountKey: key })) && setEditing(null)}
        />
      )}

      <Card
        title="Certificates"
        icon={ShieldCheck}
        actions={
          <div className="row">
            {ordererPage && (
              <a className="small" href={ordererPage} target="_blank" rel="noreferrer" title={`${view.orderer}'s own page`}>
                {view.orderer} <ExternalLink size={11} />
              </a>
            )}
            {certs.length > 0 && canApply && (
              <button className="small" disabled={busy} onClick={() => renew([])} title={`${view.orderer} obtains them now; the others pull them`}>
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
                  <th>Expires</th>
                  <th>On the members</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {certs.map((c, i) => {
                  const o = ordererCert(c.name)
                  return (
                    <tr key={c.name}>
                      <td className="mono">{c.name}</td>
                      <td className="small">
                        {(c.domains || []).join(' ')}
                        <div className="muted">
                          {c.challenge || 'http-01'}
                          {c.dns_provider ? ` · ${c.dns_provider}` : ''} · {c.key_type || 'ec256'}
                        </div>
                        {o.last_error && (
                          <div style={{ color: 'var(--danger)', whiteSpace: 'pre-wrap' }} title={o.last_error}>
                            {o.last_error.split('\n')[0].slice(0, 220)}
                          </div>
                        )}
                      </td>
                      <td className="small nowrap">{o.not_after_unix ? fmtDateTime(Number(o.not_after_unix) * 1000) : '–'}</td>
                      <td className="small">
                        {acmeCertStates(view, c.name).map((s) => (
                          <span key={s.member}>
                            <Badge tone={STATE_TONES[s.state]} dot>
                              {s.member} {s.state}
                            </Badge>{' '}
                          </span>
                        ))}
                      </td>
                      <td className="nowrap">
                        {canApply && (
                          <>
                            <button className="small" disabled={busy} onClick={() => renew([c.name])} title={`Obtain it again now, on ${view.orderer}`}>
                              <RefreshCw size={13} />
                            </button>{' '}
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
                                  note: 'Its file goes from every member - refused while the cluster’s haproxy.cfg still uses it.',
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
          For the dns-01 challenge. Settings are secrets: typed here, they go to {view.orderer} alone - never into a version, never kept by the Controller - and the others
          pull them from it; they never come back.
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
        isDefault={view.is_default}
        busy={busy}
        run={run}
        onCheck={can('admin') ? (next) => postJSON(`/api/clusters/${id}/letsencrypt/check`, { config: next }) : null}
        onApply={canApply ? (next) => apply(next) : null}
      />
    </div>
  )
}
