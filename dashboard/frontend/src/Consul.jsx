import { CheckCircle2, ExternalLink, Network, RefreshCw, Server, Waypoints, XCircle } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { call, postJSON } from './call.js'
import { counts, dirty as isDirty, draftOf, memberTone, never, settingsOf, stateTone } from './consul.js'
import { UnsavedBar } from './shared/LogTargets.jsx'
import { Badge, Card, ErrorBox, Loading, useAction, useToast } from './shared/ui.jsx'
import { fmtDateTime } from './shared/units.js'

// Consul (admin): the Consul server the Controller gives its nodes'
// agents to - its token mints one token per node, the layer it pushes
// makes each picked node with the consul extension a client of the
// cluster, and the nodes register their own services there
// (dashboard/backend/consul.go, docs/guide/consul-controller.md). The
// datacenter, the servers and the gossip key are read from Consul.

const DOCS = 'https://janus.sw-servers.net/docs/guide/consul-controller.md'
const NODE_DOCS = 'https://janus.sw-servers.net/docs/consul.md#from-the-controller'

const SERVICES = [
  ['janus', 'janus', "janusd's API (:9505)"],
  ['exporters', 'janus-exporter, node-exporter', 'the exporters, for Prometheus'],
  ['frontends', 'haproxy-<frontend>', 'each frontend and listen section'],
  ['urls', 'haproxy-url', 'each host its maps and Host ACLs route - for a blackbox probe'],
]

function Field({ label, children, grow, className = '' }) {
  return (
    <label className={`field ${grow ? 'grow' : ''} ${className}`}>
      <span>{label}</span>
      {children}
    </label>
  )
}

export default function ConsulPage() {
  const [view, setView] = useState(null)
  const [nodes, setNodes] = useState(null)
  const [draft, setDraft] = useState(null)
  const [error, setError] = useState(null)
  const [test, setTest] = useState(null) // the last test's result
  const [testing, setTesting] = useState(false)
  const [busy, run] = useAction()
  const toast = useToast()
  const load = useCallback(
    (keepDraft) =>
      Promise.all([call('/api/consul', { background: keepDraft }), call('/api/consul/nodes', { background: keepDraft })]).then(([v, ns]) => {
        setView(v)
        setNodes(ns)
        setError(null)
        if (!keepDraft) setDraft(draftOf(v.settings))
      }, setError),
    [],
  )
  useEffect(() => {
    load(false)
    const t = setInterval(() => load(true), 15000) // the nodes, what was read of Consul
    return () => clearInterval(t)
  }, [load])
  if (!view || !draft) return error ? <ErrorBox error={error} /> : <Loading />

  const saved = view.settings
  const changed = isDirty(draft, saved)
  const set = (change) => setDraft({ ...draft, ...change })
  const setService = (k, on) => set({ services: { ...draft.services, [k]: on } })
  const save = () =>
    run(async () => {
      const v = await postJSON('/api/consul', settingsOf(draft), 'PUT')
      setView(v)
      setDraft(draftOf(v.settings))
      setTimeout(() => load(true), 4000) // the pass it started
    }, draft.enabled ? 'Saved - the nodes get their layer now' : 'Saved')
  const runTest = async () => {
    setTesting(true)
    try {
      setTest(await postJSON('/api/consul/test', settingsOf(draft)))
    } catch (e) {
      setTest({ ok: false, checks: [{ name: 'test', ok: false, detail: e.message }] })
    } finally {
      setTesting(false)
    }
  }
  const checkNow = () =>
    run(async () => {
      await call('/api/consul/run', { method: 'POST' })
      toast('Checking the nodes - the list follows in a moment.')
      setTimeout(() => load(true), 4000)
    })
  const sum = counts(nodes)
  const state = !saved.enabled ? (
    <Badge>off</Badge>
  ) : view.error ? (
    <Badge tone="danger">unreachable</Badge>
  ) : view.cluster ? (
    <Badge tone="ok">datacenter {view.cluster.datacenter}</Badge>
  ) : (
    <Badge tone="warn">not checked yet</Badge>
  )
  const tokenHint = saved.has_token ? 'set - leave empty to keep it' : 'the Controller’s token (a UUID)'

  return (
    <div className="stack">
      <div className="spread">
        <h1>Consul</h1>
        <div className="row">
          <a className="small" href={DOCS} target="_blank" rel="noreferrer">
            Guide <ExternalLink size={11} />
          </a>
          <button className="primary" disabled={busy || !changed} onClick={save}>
            Save
          </button>
        </div>
      </div>
      <p className="muted" style={{ margin: 0 }}>
        The nodes with the consul extension join your Consul cluster as clients: the Controller gives each one its own ACL token and{' '}
        <a href={NODE_DOCS} target="_blank" rel="noreferrer">
          a layer of its agent’s configuration
        </a>{' '}
        - next to the node’s own, which stays as it is -, and each node registers its services itself. The datacenter, the servers and the gossip key are read from Consul. The
        token is sealed with the master key, and never shown again.
      </p>
      {error && <ErrorBox error={error} />}

      <Card title="Consul server" icon={Server} actions={state} className="consul-card">
        <fieldset className="plain stack" disabled={busy} style={{ gap: '0.75rem' }}>
          <label className="check">
            <input type="checkbox" checked={draft.enabled} onChange={(e) => set({ enabled: e.target.checked })} /> Manage the nodes’ Consul agents
          </label>
          <div className="row" style={{ flexWrap: 'wrap', alignItems: 'flex-end' }}>
            <Field label="Its HTTP API - https:// only" grow>
              <input className="mono" value={draft.address} placeholder="https://consul.example.com" onChange={(e) => set({ address: e.target.value })} />
            </Field>
            <Field label="Token" className="consul-token">
              <input className="mono" type="password" autoComplete="new-password" value={draft.token} placeholder={tokenHint} onChange={(e) => set({ token: e.target.value })} />
            </Field>
          </div>
          <div className="row" style={{ flexWrap: 'wrap', alignItems: 'flex-end' }}>
            <Field label="Trust its certificate by">
              <select value={draft.trust} onChange={(e) => set({ trust: e.target.value })}>
                <option value="system">the usual CAs</option>
                <option value="ca">a CA (PEM)</option>
                <option value="fingerprint">a SHA-256 fingerprint</option>
              </select>
            </Field>
            {draft.trust === 'fingerprint' && (
              <Field label="SHA-256 of its certificate or of a CA in its chain" grow>
                <input className="mono" value={draft.fingerprint} placeholder="93:BA:4A:…" onChange={(e) => set({ fingerprint: e.target.value })} />
              </Field>
            )}
            <Field label="Name on its certificate (optional)">
              <input className="mono" value={draft.server_name} onChange={(e) => set({ server_name: e.target.value })} />
            </Field>
          </div>
          {draft.trust === 'ca' && (
            <Field label="CA (PEM)">
              <textarea rows={3} className="mono small" value={draft.ca_pem} placeholder="-----BEGIN CERTIFICATE-----" onChange={(e) => set({ ca_pem: e.target.value })} />
            </Field>
          )}
          <Field label="The servers’ RPC CA (PEM) - the nodes’ agents verify the servers with it and get their own certificate from them (auto_encrypt); empty: RPC without TLS">
            <textarea rows={3} className="mono small" value={draft.rpc_ca_pem} placeholder="-----BEGIN CERTIFICATE-----" onChange={(e) => set({ rpc_ca_pem: e.target.value })} />
          </Field>
          <details className="small">
            <summary>What the Controller’s token needs</summary>
            <pre className="code small" tabIndex={0} aria-label="The Controller's token's policy">{`acl      = "write"   # the nodes' tokens, the policy ${view.policy}
keyring  = "read"    # the gossip key
operator = "read"
node_prefix ""    { policy = "write" }   # deregistering a node removed
service_prefix "" { policy = "read" }
agent_prefix ""   { policy = "read" }`}</pre>
          </details>
        </fieldset>
        <div className="row">
          <button className="small" disabled={testing || !draft.address.trim()} onClick={runTest}>
            {testing ? 'Testing…' : 'Test'}
          </button>
          <span className="small muted">As shown here, saved or not - the token left empty is the saved one.</span>
        </div>
        {test && (
          <ul className="consul-checks small" aria-label="Test results">
            {test.checks.map((c) => (
              <li key={c.name}>
                {c.ok ? <CheckCircle2 size={14} className="ok-ink" aria-label="ok" /> : <XCircle size={14} className="danger-ink" aria-label="failed" />}{' '}
                <strong>{c.name}</strong>: {c.detail}
              </li>
            ))}
          </ul>
        )}
      </Card>

      <Card title="The nodes’ layer" icon={Waypoints}>
        <fieldset className="plain stack" disabled={busy} style={{ gap: '0.75rem' }}>
          <Field label="Nodes with the labels (key=value, comma-separated; empty: every node with the consul extension)">
            <input className="mono" value={draft.selector} placeholder="dc=lgs" onChange={(e) => set({ selector: e.target.value })} />
          </Field>
          <div className="consul-services">
            <strong className="small">Each node registers</strong>
            {SERVICES.map(([k, name, what]) => (
              <label className="check" key={k}>
                <input type="checkbox" checked={draft.services[k]} onChange={(e) => setService(k, e.target.checked)} /> <span className="mono">{name}</span>{' '}
                <span className="muted">- {what}</span>
              </label>
            ))}
          </div>
          <label className="check">
            <input type="checkbox" checked={draft.node_meta_from_labels} onChange={(e) => set({ node_meta_from_labels: e.target.checked })} /> Its labels as its node metadata
            in Consul <span className="muted">- a dot or a slash in a key as an underscore; janus_node_id and janus_cluster are always there</span>
          </label>
        </fieldset>
      </Card>

      {changed && <UnsavedBar onSave={save} onDiscard={() => setDraft(draftOf(saved))} busy={busy} />}

      <Card
        title="Read from Consul"
        icon={Network}
        actions={
          <button className="small" disabled={busy || !saved.enabled} onClick={checkNow}>
            <RefreshCw size={13} /> Check the nodes now
          </button>
        }
      >
        {view.error && <div className="notice danger small">{view.error}</div>}
        {view.cluster ? (
          <dl className="kv">
            <dt>Datacenter</dt>
            <dd className="mono">{view.cluster.datacenter}</dd>
            <dt>Consul</dt>
            <dd className="mono">
              {view.cluster.version} <span className="muted">({view.cluster.server})</span>
            </dd>
            <dt>Servers - the nodes join them</dt>
            <dd className="mono">{(view.cluster.servers || []).join(', ')}</dd>
            <dt>Gossip key</dt>
            <dd>{view.cluster.has_gossip_key ? <Badge tone="ok">read - the nodes encrypt with it</Badge> : <Badge tone="warn">none - the gossip isn’t encrypted</Badge>}</dd>
            <dt>Last check</dt>
            <dd>{never(view.last_pass) ? '-' : fmtDateTime(view.last_pass)}</dd>
          </dl>
        ) : (
          !view.error && <p className="muted small">{saved.enabled ? 'Not checked yet.' : 'Nothing yet - the integration is off.'}</p>
        )}
      </Card>

      <Card
        title="Nodes"
        icon={Server}
        actions={
          nodes?.length ? (
            <span className="small muted">
              {sum.applied} with the layer · {sum.alive} alive{sum.failed ? ` · ${sum.failed} failed` : ''} · {sum.urls} URLs
            </span>
          ) : null
        }
      >
        {!nodes?.length ? (
          <div className="muted">No node yet.</div>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Node</th>
                  <th>In Consul</th>
                  <th>Layer</th>
                  <th>Member</th>
                  <th className="num">Services</th>
                  <th className="num">URLs</th>
                  <th>Token</th>
                </tr>
              </thead>
              <tbody>
                {nodes.map((n) => (
                  <tr key={n.id}>
                    <td>
                      {n.name}
                      {n.labels && Object.keys(n.labels).length > 0 && (
                        <div className="mono small muted">
                          {Object.entries(n.labels)
                            .map(([k, v]) => `${k}=${v}`)
                            .join(', ')}
                        </div>
                      )}
                    </td>
                    <td className="mono">{n.node_name || '-'}</td>
                    <td>
                      <Badge tone={stateTone(n.state)} dot>
                        {n.state}
                      </Badge>
                      {n.error && <div className="small muted consul-error">{n.error}</div>}
                    </td>
                    <td>{n.member ? <Badge tone={memberTone(n.member)}>{n.member}</Badge> : <span className="muted">-</span>}</td>
                    <td className="num">{n.state === 'applied' || n.services ? n.services : '-'}</td>
                    <td className="num">{n.state === 'applied' || n.urls ? n.urls : '-'}</td>
                    <td className="mono small" title={n.token_accessor ? `accessor ${n.token_accessor}` : undefined}>
                      {n.token_accessor ? `${n.token_accessor.slice(0, 8)}…` : '-'}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </div>
  )
}
