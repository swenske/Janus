import { Download, ExternalLink, Route } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { call, postJSON } from './call.js'
import { Badge, Card, ErrorBox, Loading, useAction, useToast } from './shared/ui.jsx'

// Let's Encrypt (admin): the Controller's part in the nodes'
// certificates - the relay, for the nodes that reach the CA through the
// Controller (dashboard/backend/acme_relay.go,
// docs/guide/letsencrypt-controller.md). Each node chooses its way on its
// own page, Apps › Let's Encrypt.

const DOCS = 'https://janus.sw-servers.net/docs/guide/letsencrypt-controller.md'
const NODE_DOCS = 'https://janus.sw-servers.net/docs/letsencrypt.md#how-the-node-reaches-lets-encrypt'

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
          directly, through a proxy, or through this Controller
        </a>
        . The last one needs the relay below.
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
                    <td className="nowrap">{new Date(t.start).toLocaleString()}</td>
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
