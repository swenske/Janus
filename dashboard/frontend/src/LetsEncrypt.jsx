import { Download, ExternalLink, Pencil, Plus, RefreshCw, Route, ShieldCheck, Trash2, Waypoints } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { call, postJSON } from './call.js'
import ProviderForm from './shared/ACMEProviderForm.jsx'
import { Badge, Card, ErrorBox, Loading, useAction, useToast } from './shared/ui.jsx'
import { fmtDate, fmtDateTime } from './shared/units.js'

// Let's Encrypt (admin): the Controller's part in the nodes'
// certificates - the relay, for the nodes that reach the CA through the
// Controller (dashboard/backend/acme_relay.go), and the ordering, for the
// nodes whose certificates it orders (acme_order.go);
// docs/guide/letsencrypt-controller.md. Each node chooses its way on its
// own page, Apps › Let's Encrypt.

const DOCS = 'https://janus.sw-servers.net/docs/guide/letsencrypt-controller.md'
const NODE_DOCS = 'https://janus.sw-servers.net/docs/letsencrypt.md#how-the-node-reaches-lets-encrypt'
const TERMS = 'https://letsencrypt.org/repository/'
const STATE_TONES = { valid: 'ok', pending: 'warn', due: 'info', expired: 'danger' }

const when = (t) => (t && !t.startsWith('0001') ? fmtDateTime(t) : '-')

const draftOf = (c) => ({ enabled: !!c.enabled, listen: c.listen || ':8444', address: c.address || '', allowed: (c.allowed || []).join('\n') })
const configOf = (d) => ({
  enabled: d.enabled,
  listen: d.listen.trim(),
  address: d.address.trim(),
  allowed: d.allowed
    .split('\n')
    .map((l) => l.trim())
    .filter(Boolean),
})

function bytes(n) {
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MiB`
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(1)} KiB`
  return `${n} B`
}

export default function LetsEncryptPage() {
  const [view, setView] = useState(null)
  const [draft, setDraft] = useState(null)
  const [error, setError] = useState(null)
  const [busy, run] = useAction()
  const toast = useToast()
  const load = useCallback(
    (keepDraft) =>
      call('/api/acme-relay', { background: keepDraft }).then((v) => {
        setView(v)
        setError(null)
        if (!keepDraft) setDraft(draftOf(v.config))
      }, setError),
    [],
  )
  useEffect(() => {
    load(false)
    const t = setInterval(() => load(true), 15000) // the tunnels
    return () => clearInterval(t)
  }, [load])
  if (!view || !draft) return error ? <ErrorBox error={error} /> : <Loading />
  const dirty = JSON.stringify(configOf(draft)) !== JSON.stringify(configOf(draftOf(view.config)))
  const set = (change) => setDraft({ ...draft, ...change })
  const save = () =>
    run(async () => {
      const v = await postJSON('/api/acme-relay', configOf(draft), 'PUT')
      setView(v)
      setDraft(draftOf(v.config))
      toast(v.config.enabled ? `The relay listens on ${v.listening}.` : 'The relay is off.')
    })
  const state = view.listening ? (
    <Badge tone="ok">listening on {view.listening}</Badge>
  ) : view.config.enabled ? (
    <Badge tone="danger">not listening</Badge>
  ) : (
    <Badge>off</Badge>
  )
  const identity = `data:application/x-pem-file;charset=utf-8,${encodeURIComponent(view.ca_cert_pem)}`
  return (
    <div className="stack">
      <div className="spread">
        <h1>Let’s Encrypt</h1>
        <a className="small" href={DOCS} target="_blank" rel="noreferrer">
          Guide <ExternalLink size={11} />
        </a>
      </div>
      <p className="muted">
        Each node gets its certificates itself and chooses, on its own page (Apps › Let’s Encrypt), how it reaches the CA:{' '}
        <a href={NODE_DOCS} target="_blank" rel="noreferrer">
          directly, through a proxy, through this Controller
        </a>
        , or have this Controller order its certificates. The last two need what’s set up below.
      </p>
      {error && <ErrorBox error={error} />}
      <Card title="Relay for the nodes" icon={Route} actions={state} className="acme-relay-card">
        <p className="small">
          A node that reaches this Controller but not the internet goes through it: the relay tunnels the node’s HTTPS to the CA and to its DNS provider’s API - encrypted end to end, the relay sees host names only. It knows a node by a certificate of the node’s own CA: only the nodes added to this Controller get through, and only to the destinations listed here.
        </p>
        {view.error && <div className="notice danger">{view.error}</div>}
        <fieldset className="plain relay-form" disabled={busy}>
          <label className="check">
            <input type="checkbox" checked={draft.enabled} onChange={(e) => set({ enabled: e.target.checked })} /> Relay on
          </label>
          <div className="relay-fields">
            <label className="field">
              <span>Listens on</span>
              <input className="mono" value={draft.listen} placeholder=":8444" onChange={(e) => set({ listen: e.target.value })} />
            </label>
            <label className="field">
              <span>The nodes reach it at - a name or address in the Controller’s identity</span>
              <input className="mono" value={draft.address} placeholder={view.url ? view.url.replace('https://', '') : 'host:port'} onChange={(e) => set({ address: e.target.value })} />
            </label>
          </div>
          <label className="field">
            <span>Destinations allowed besides the built-in ones - host:port, one a line: a private ACME CA, the API an httpreq or pdns provider names</span>
            <textarea className="mono small" rows={3} value={draft.allowed} placeholder="acme.internal.example:443" onChange={(e) => set({ allowed: e.target.value })} />
          </label>
        </fieldset>
        <div className="row">
          <button className="small primary" disabled={busy || !dirty} onClick={save}>
            Save
          </button>
          {dirty && (
            <button className="small ghost" disabled={busy} onClick={() => setDraft(draftOf(view.config))}>
              Discard
            </button>
          )}
        </div>
        <details className="relay-builtin">
          <summary className="small">Built-in destinations (port 443) and what can’t go through the relay</summary>
          <ul className="small">
            {Object.entries(view.built_in || {})
              .sort(([a], [b]) => (a === "Let's Encrypt" ? -1 : b === "Let's Encrypt" ? 1 : a.localeCompare(b)))
              .map(([what, hosts]) => (
                <li key={what}>
                  <strong>{what}</strong>: <span className="mono">{hosts.join(', ')}</span>
                </li>
              ))}
            {Object.entries(view.not_through_relay || {}).map(([p, why]) => (
              <li key={p}>
                <strong>{p}</strong> can’t go through the relay: {why}.
              </li>
            ))}
          </ul>
        </details>
        {view.url && (
          <div className="relay-give small">
            <div className="muted">A node’s page offers “Through this Controller” with these filled in. With janusctl:</div>
            <pre className="mono">janusctl haproxy acme path controller {view.url} -ca controller-identity.pem</pre>
            <a className="button small" href={identity} download="controller-identity.pem">
              <Download size={13} /> The Controller’s identity
            </a>
          </div>
        )}
      </Card>
      <Ordering />
      <Card title="Recent tunnels">
        {view.recent?.length ? (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>When</th>
                  <th>Node</th>
                  <th>Destination</th>
                  <th className="num">Sent</th>
                  <th className="num">Received</th>
                  <th className="num">Lasted</th>
                  <th>Result</th>
                </tr>
              </thead>
              <tbody>
                {view.recent.map((t) => (
                  <tr key={`${t.start}-${t.node_id}-${t.target}`}>
                    <td className="nowrap">{fmtDateTime(t.start)}</td>
                    <td>{t.node}</td>
                    <td className="mono">{t.target}</td>
                    <td className="num">{bytes(t.bytes_up)}</td>
                    <td className="num">{bytes(t.bytes_down)}</td>
                    <td className="num">{t.seconds.toFixed(1)} s</td>
                    <td>{t.refused ? <Badge tone="warn">refused: {t.refused}</Badge> : t.error ? <Badge tone="danger">{t.error}</Badge> : <Badge tone="ok">done</Badge>}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <p className="muted small">{view.listening ? 'No tunnel yet - a node through the relay opens one when it talks to the CA.' : 'None - the relay is off.'} {view.open ? `${view.open} open now.` : ''}</p>
        )}
      </Card>
    </div>
  )
}

// Ordering: the account the Controller orders its nodes' certificates
// with, its DNS providers, and what it did for each node.
function Ordering() {
  const [view, setView] = useState(null)
  const [error, setError] = useState(null)
  const [editing, setEditing] = useState(null) // the account, or a provider ({ index })
  const [busy, run] = useAction()
  const toast = useToast()
  const load = useCallback((background) => call('/api/acme', { background }).then(setView, setError), [])
  useEffect(() => {
    load(false)
    const t = setInterval(() => load(true), 15000)
    return () => clearInterval(t)
  }, [load])
  if (!view) return error ? <ErrorBox error={error} /> : <Loading />
  const set = view.settings
  const save = (next, done) =>
    run(async () => {
      setView(await postJSON('/api/acme', next, 'PUT'))
      toast('Saved.')
      done?.()
    })
  const providers = set.dns_providers || []
  const orderNow = () =>
    run(async () => {
      await call('/api/acme/run', { method: 'POST' })
      toast('Ordering what’s due - the list follows in a moment.')
      setTimeout(() => load(true), 5000)
    })
  const state = view.ready ? <Badge tone="ok">ready</Badge> : <Badge tone="warn">not set up</Badge>
  return (
    <>
      <Card title="Ordering for the nodes" icon={ShieldCheck} actions={state} className="acme-relay-card">
        <p className="small">
          A node whose page chose “Ordered by this Controller” reaches nothing: the Controller asks it for a key and its request for each certificate due, orders it from the CA the node’s configuration names with the account below, and installs what comes back. The keys never leave the node; the account and the DNS providers stay here, sealed with the master key.
        </p>
        {!view.client && <div className="notice danger">This Controller has no ACME client (janus-acme next to dashboardd): nothing can be ordered.</div>}
        <dl className="kv">
          <dt>Account</dt>
          <dd className="mono small">{view.thumbprint}</dd>
          <dt>Contact</dt>
          <dd>{set.email || <span className="muted">none</span>}</dd>
          <dt>Terms</dt>
          <dd>
            {set.accept_terms ? <Badge tone="ok">accepted</Badge> : <Badge tone="warn">not accepted - nothing is ordered until they are</Badge>}{' '}
            <a className="small" href={TERMS} target="_blank" rel="noreferrer">
              Let’s Encrypt’s
            </a>
          </dd>
          {set.eab_key_id && (
            <>
              <dt>External account binding</dt>
              <dd className="mono small">{set.eab_key_id}</dd>
            </>
          )}
        </dl>
        {editing === 'account' ? (
          <AccountForm settings={set} busy={busy} onCancel={() => setEditing(null)} onSave={(next) => save(next, () => setEditing(null))} />
        ) : (
          <div className="row">
            <button className="small" onClick={() => setEditing('account')}>
              <Pencil size={13} /> Edit the account
            </button>
          </div>
        )}
        <div>
          <div className="spread">
            <strong className="small">DNS providers - what a node’s dns-01 certificate names</strong>
            <button className="small" onClick={() => setEditing({ index: -1 })}>
              <Plus size={13} /> Add a DNS provider
            </button>
          </div>
          {providers.length ? (
            <ul className="small relay-providers">
              {providers.map((p, i) => (
                <li key={p.name}>
                  <span className="mono">{p.name}</span> <span className="muted">({p.type})</span>{' '}
                  <button className="small ghost" aria-label={`Edit ${p.name}`} onClick={() => setEditing({ index: i })}>
                    <Pencil size={12} />
                  </button>
                  <button className="small ghost" aria-label={`Remove ${p.name}`} onClick={() => save({ ...set, dns_providers: providers.filter((_, j) => j !== i) })}>
                    <Trash2 size={12} />
                  </button>
                </li>
              ))}
            </ul>
          ) : (
            <p className="small muted">None yet - only HTTP-01 certificates can be ordered.</p>
          )}
        </div>
      </Card>
      {editing?.index !== undefined && (
        <ProviderForm
          provider={editing.index >= 0 ? providers[editing.index] : null}
          names={providers.map((p) => p.name)}
          busy={busy}
          onCancel={() => setEditing(null)}
          onSave={(p) =>
            save({ ...set, dns_providers: editing.index >= 0 ? providers.map((x, j) => (j === editing.index ? p : x)) : [...providers, p] }, () => setEditing(null))
          }
        />
      )}
      <Card
        title="Ordered certificates"
        icon={Waypoints}
        actions={
          <button className="small" disabled={busy || !view.ready} onClick={orderNow}>
            <RefreshCw size={13} /> Order what’s due now
          </button>
        }
      >
        {view.nodes?.length ? (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Node</th>
                  <th>Certificate</th>
                  <th>State</th>
                  <th>Valid until</th>
                  <th>Last order</th>
                  <th>Next attempt</th>
                </tr>
              </thead>
              <tbody>
                {view.nodes.flatMap((n) =>
                  n.state !== 'ordered' || !n.certificates?.length
                    ? [
                        <tr key={n.id}>
                          <td>{n.name}</td>
                          <td colSpan={5}>{n.error ? <Badge tone="danger">{n.error}</Badge> : <span className="muted">no certificate configured</span>}</td>
                        </tr>,
                      ]
                    : n.certificates.map((c) => (
                        <tr key={`${n.id}/${c.name}`}>
                          <td>{n.name}</td>
                          <td>
                            <span className="mono">{c.name}</span> <span className="muted small">{c.domains.join(', ')}</span>
                          </td>
                          <td>
                            <Badge tone={STATE_TONES[c.state]}>{c.state}</Badge> {c.asked && <Badge tone="info">renewal asked</Badge>}
                          </td>
                          <td className="nowrap">{c.not_after_unix ? fmtDate(c.not_after_unix * 1000) : '-'}</td>
                          <td>{c.order.last_error ? <Badge tone="danger">{c.order.last_error}</Badge> : when(c.order.last_success)}</td>
                          <td className="nowrap">{when(c.order.next_attempt)}</td>
                        </tr>
                      )),
                )}
              </tbody>
            </table>
          </div>
        ) : (
          <p className="muted small">No node is ordered by this Controller yet - a node’s page chooses it (Apps › Let’s Encrypt, “Ordered by this Controller”).</p>
        )}
      </Card>
    </>
  )
}

function AccountForm({ settings, busy, onCancel, onSave }) {
  const [email, setEmail] = useState(settings.email || '')
  const [terms, setTerms] = useState(!!settings.accept_terms)
  const [eabID, setEabID] = useState(settings.eab_key_id || '')
  const [eabKey, setEabKey] = useState('')
  return (
    <fieldset className="plain relay-form" disabled={busy}>
      <label className="field">
        <span>Contact address (optional) - the CA writes about the account there</span>
        <input type="email" value={email} onChange={(e) => setEmail(e.target.value)} placeholder="ops@example.com" />
      </label>
      <label className="check">
        <input type="checkbox" checked={terms} onChange={(e) => setTerms(e.target.checked)} /> I accept the CA’s terms of service
      </label>
      <div className="relay-fields">
        <label className="field">
          <span>External account binding: key ID (a CA that asks for one)</span>
          <input className="mono" value={eabID} onChange={(e) => setEabID(e.target.value)} />
        </label>
        <label className="field">
          <span>Its HMAC key</span>
          <input className="mono" type="password" autoComplete="off" value={eabKey} onChange={(e) => setEabKey(e.target.value)} placeholder={settings.eab_key_id ? 'saved - leave empty to keep' : ''} />
        </label>
      </div>
      <div className="row">
        <button className="small primary" onClick={() => onSave({ ...settings, email: email.trim(), accept_terms: terms, eab_key_id: eabID.trim(), eab_hmac_key: eabKey })}>
          Save
        </button>
        <button className="small ghost" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </fieldset>
  )
}
