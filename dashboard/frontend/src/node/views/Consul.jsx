import { ExternalLink, FileKey, Network, ScrollText, ShieldCheck, Tags, Trash2, Undo2, Upload, Waypoints } from 'lucide-react'
import { useState } from 'react'
import { Badge, Card, ErrorBox, Loading, PageHeader } from '../../shared/ui.jsx'
import DataTable from '../components/DataTable.jsx'
import ModuleConfigEditor from '../components/ModuleConfigEditor.jsx'
import { address, agentHint, checksText, counts as countKinds, keyMeta, KINDS, maintenanceReason, matches } from '../consulServices.js'
import { dateTime } from '../format.js'
import { usePoll } from '../hooks.jsx'
import { useMay } from '../may.js'

// Its page on the docs site, by the file's permalink.
const DOCS = 'https://janus.sw-servers.net/docs/consul.md'

// A starting point: checked by the extension's own consul validate.
const STARTER = `# The Consul agent's configuration, HCL or JSON - see docs/consul.md.
# Janus adds data_dir and node_id itself. The files given below are
# /run/janus/consul/files/<name>.

datacenter = "dc1"
bind_addr  = "192.0.2.10"           # this node's address on the cluster's network
retry_join = ["consul-1.example.com", "consul-2.example.com"]
encrypt    = "<consul keygen>"      # the cluster's gossip key

tls {
  defaults {
    ca_file         = "/run/janus/consul/files/consul-agent-ca.pem"
    verify_outgoing = true
  }
  internal_rpc {
    verify_server_hostname = true
  }
}
`

const RESOLVERS = `resolvers consul
    nameserver consul 127.0.0.1:8600
    accepted_payload_size 8192
    hold valid 5s

backend web
    server-template web 5 _web._tcp.service.consul resolvers consul resolve-prefer ipv4 init-addr none check`

const MEMBER_TONES = { alive: 'ok', leaving: 'warn', left: '', failed: 'danger' }
const CHECK_TONES = { passing: 'ok', warning: 'warn', critical: 'danger', maintenance: 'warn' }
const SERVICES = [
  ['janus', 'janus'],
  ['exporters', 'exporters'],
  ['frontends', 'haproxy-<frontend>'],
  ['urls', 'haproxy-url'],
]

// Consul is the consul extension: the agent's state and its cluster, the
// files its configuration names, and the configuration itself.
export default function Consul() {
  const status = usePoll('/api/network/consul', { every: 5000 })
  // The layer a Controller manages (docs/consul.md "From the
  // Controller"), its secrets left out: every reader sees it.
  const managed = usePoll('/api/network/consul/managed', { every: 15000, enabled: useMay()('NetworkService/ConsulManagedGet') })
  // Its configuration may hold the gossip key and ACL tokens: admins only.
  const canConfig = useMay()('NetworkService/ConsulGetConfig')
  const saved = usePoll('/api/network/consul/config', { every: 0, enabled: canConfig })
  const [pending, setPending] = useState({}) // name -> new content
  const [removed, setRemoved] = useState([]) // saved names to remove
  const st = status.data

  if (st?.state === 'not_enabled') {
    return (
      <>
        <PageHeader title="Consul" subtitle="The Consul agent" />
        <div className="notice">The consul extension isn't in this node's image. Add it from System › Update (Change extensions…).</div>
      </>
    )
  }

  const savedFiles = saved.data?.files || []
  const files = {}
  for (const n of savedFiles) if (!removed.includes(n)) files[n] = '' // kept as saved
  Object.assign(files, pending)
  const filesDirty = Object.keys(pending).length > 0 || removed.length > 0
  const addFiles = (e) => {
    const list = [...e.target.files]
    e.target.value = ''
    Promise.all(list.map((f) => f.text().then((t) => [f.name, t]))).then((read) => setPending((p) => ({ ...p, ...Object.fromEntries(read) })))
  }
  const applied = () => {
    setPending({})
    setRemoved([])
    saved.reload()
    status.reload()
  }

  return (
    <>
      <PageHeader
        title="Consul"
        subtitle="The Consul agent, with your configuration: service discovery for HAProxy, this node in your catalog"
        actions={
          <>
            <a className="button small" href="#/logs?service=consul">
              <ScrollText size={13} /> Log
            </a>
            <a className="button small" href={DOCS} target="_blank" rel="noreferrer">
              Guide <ExternalLink size={12} />
            </a>
          </>
        }
      />
      {status.error && <ErrorBox error={status.error} />}
      <div className="grid grid-2">
        <Card
          title="Agent"
          icon={Network}
          actions={
            st && (
              <Badge tone={st.service_state === 'running' ? 'ok' : st.service_state === 'restarting' ? 'danger' : ''} dot>
                {st.service_state || st.state}
              </Badge>
            )
          }
        >
          {!st ? (
            <Loading />
          ) : !st.configured && !st.managed ? (
            <span className="muted small">No configuration is saved: the agent waits for one. Write it below.</span>
          ) : (
            <>
              {st.service_error && st.service_state !== 'running' && <div className="error-box">Last exit: {st.service_error}</div>}
              {st.error && <div className="notice small">{st.error}</div>}
              {st.node_name && (
                <dl className="kv">
                  <dt>Node</dt>
                  <dd className="mono">{st.node_name}</dd>
                  <dt>Node ID</dt>
                  <dd className="mono small">{st.node_id}</dd>
                  <dt>Role</dt>
                  <dd>{st.server ? 'server' : 'client'}</dd>
                  <dt>Datacenter</dt>
                  <dd className="mono">{st.datacenter}</dd>
                  <dt>Consul</dt>
                  <dd className="mono">{st.version}</dd>
                  <dt>Leader</dt>
                  <dd className="mono">{st.leader || <Badge tone="danger">none</Badge>}</dd>
                </dl>
              )}
            </>
          )}
        </Card>
        <Card title="HAProxy service discovery" icon={Waypoints}>
          <p className="small" style={{ marginTop: 0 }}>
            The agent answers DNS on <span className="mono">127.0.0.1:8600</span>: a backend can take its servers from a Consul service's SRV records.
          </p>
          <pre className="code small" tabIndex={0} aria-label="HAProxy resolvers section">{RESOLVERS}</pre>
        </Card>
      </div>

      {managed.data?.managed?.managed_by && <ManagedLayer m={managed.data} />}

      {st && st.state !== 'not_enabled' && (st.configured || st.managed) && <Services st={st} />}

      {st?.members?.length > 0 && (
        <div style={{ marginTop: '1rem' }}>
          <Card title="Members" icon={Network}>
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Member</th>
                    <th>Address</th>
                    <th>Status</th>
                    <th>Role</th>
                    <th>Datacenter</th>
                  </tr>
                </thead>
                <tbody>
                  {st.members.map((m) => (
                    <tr key={m.name}>
                      <td className="mono">{m.name}</td>
                      <td className="mono">{m.address}</td>
                      <td>
                        <Badge tone={MEMBER_TONES[m.status]} dot>
                          {m.status}
                        </Badge>
                      </td>
                      <td>{m.role}</td>
                      <td className="mono">{m.datacenter}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </Card>
        </div>
      )}

      {canConfig ? (
        <>
        <div style={{ marginTop: '1rem' }}>
          <Card
            title="Files"
            icon={FileKey}
            actions={
              <label className="button small">
                <Upload size={13} /> Add files…
                <input type="file" multiple hidden onChange={addFiles} />
              </label>
            }
          >
            <p className="small muted" style={{ marginTop: 0 }}>
              What the configuration names - CA, certificates, keys: <span className="mono">{st?.files_dir || '/run/janus/consul/files'}/&lt;name&gt;</span>. Kept
              on the node, never shown again. They go with the next Apply.
            </p>
            {!savedFiles.length && !Object.keys(pending).length ? (
              <span className="muted small">None.</span>
            ) : (
              <div className="stack" style={{ gap: '0.3rem' }}>
                {savedFiles.map((n) => (
                  <div className="row" key={n}>
                    <span
                      className={`mono grow ${removed.includes(n) ? 'muted' : ''}`}
                      style={removed.includes(n) ? { textDecoration: 'line-through' } : undefined}
                    >
                      {n}
                    </span>
                    {pending[n] != null ? <Badge tone="info">replaced</Badge> : removed.includes(n) ? <Badge tone="warn">removed</Badge> : <Badge>saved</Badge>}
                    {removed.includes(n) ? (
                      <button className="small" onClick={() => setRemoved(removed.filter((x) => x !== n))} title="Keep it">
                        <Undo2 size={13} />
                      </button>
                    ) : (
                      <button className="small" onClick={() => setRemoved([...removed, n])} title="Remove it">
                        <Trash2 size={13} />
                      </button>
                    )}
                  </div>
                ))}
                {Object.keys(pending)
                  .filter((n) => !savedFiles.includes(n))
                  .map((n) => (
                    <div className="row" key={n}>
                      <span className="mono grow">{n}</span>
                      <Badge tone="info">new</Badge>
                      <button
                        className="small"
                        onClick={() => setPending(Object.fromEntries(Object.entries(pending).filter(([k]) => k !== n)))}
                        title="Don't add it"
                      >
                        <Trash2 size={13} />
                      </button>
                    </div>
                  ))}
              </div>
            )}
          </Card>
        </div>

        <div style={{ marginTop: '1rem' }}>
          <ModuleConfigEditor
            base="/api/network/consul"
            applyMethod="NetworkService/ConsulApplyConfig"
            file="consul.hcl"
            daemon="consul validate"
            starter={STARTER}
            applyNote="consul validate checks it, then the agent restarts with it."
            extraBody={{ files }}
            extraDirty={filesDirty}
            extraSummary={
              <p>
                Files: {Object.keys(files).sort().join(', ') || 'none'}
                {removed.length > 0 && ` - removed: ${removed.join(', ')}`}.
              </p>
            }
            onApplied={applied}
          />
        </div>
        </>
      ) : (
        <div className="notice" style={{ marginTop: '1rem' }}>
          The agent&apos;s configuration may hold its gossip key and ACL tokens: only an admin sees and changes it.
        </div>
      )}
    </>
  )
}

// ManagedLayer is the layer the Controller manages, read only: set from
// its Consul tab, next to the operator's configuration below.
function ManagedLayer({ m }) {
  const l = m.managed
  const meta = Object.entries(l.node_meta || {}).sort(([a], [b]) => a.localeCompare(b))
  const on = SERVICES.filter(([k]) => l.services?.[k]).map(([, name]) => name)
  const synced = Number(m.synced_unix || 0)
  return (
    <div style={{ marginTop: '1rem' }}>
      <Card
        title="Managed by the Controller"
        icon={ShieldCheck}
        actions={m.maintenance ? <Badge tone="warn">services in maintenance</Badge> : <Badge tone="ok">{m.registered || 0} services registered</Badge>}
      >
        <p className="small muted" style={{ marginTop: 0 }}>
          The Controller’s Consul tab sets this layer - read only here. It comes after the configuration below: its keys win, the lists add up. The node registers its own services
          with its token (IDs <span className="mono">janus:*</span>); a drained node’s are in maintenance.
        </p>
        {m.sync_error && <div className="notice warn small">The services: {m.sync_error}</div>}
        <dl className="kv">
          <dt>Datacenter</dt>
          <dd className="mono">{l.datacenter}</dd>
          <dt>Node name</dt>
          <dd className="mono">{l.node_name}</dd>
          <dt>Joins</dt>
          <dd className="mono">{(l.retry_join || []).join(', ')}</dd>
          <dt>Advertises</dt>
          <dd className="mono">{l.advertise_addr || <span className="muted">its main address</span>}</dd>
          <dt>Gossip key</dt>
          <dd>{l.has_encrypt ? <Badge tone="ok">set</Badge> : <Badge>none</Badge>}</dd>
          <dt>ACL token</dt>
          <dd>{l.has_agent_token ? <Badge tone="ok">its own</Badge> : <Badge tone="danger">none</Badge>}</dd>
          <dt>RPC</dt>
          <dd>{l.ca_pem ? 'TLS - the servers verified, its certificate from them (auto_encrypt)' : <span className="muted">without TLS</span>}</dd>
          <dt>Registers</dt>
          <dd className="mono">{on.join(', ') || <span className="muted">nothing</span>}</dd>
          <dt>Last sync</dt>
          <dd>{synced ? dateTime(synced * 1000) : '-'}</dd>
          <dt>By</dt>
          <dd className="mono small">{l.managed_by}</dd>
        </dl>
        {meta.length > 0 && (
          <div className="row" style={{ flexWrap: 'wrap', gap: '0.3rem', marginTop: '0.6rem' }} aria-label="Node metadata">
            <Tags size={14} className="muted" />
            {meta.map(([k, v]) => (
              <Badge key={k}>
                <span className="mono">
                  {k}={v}
                </span>
              </Badge>
            ))}
          </div>
        )}
      </Card>
    </div>
  )
}

// Services are the agent's services, an instance a row: those Janus
// registers (janus:*, when a Controller manages the agent) and those of
// the operator's configuration (services {}), by kind - a node with a
// big hosts.map has dozens of URLs -, each with its checks' state (their
// output on hover, a failing one's under it, a maintenance's reason).
function Services({ st }) {
  const [kind, setKind] = useState('all')
  const hint = agentHint(st)
  const services = st?.services || []
  const n = countKinds(services)
  const rows = services.filter((s) => matches(s, kind)).map((s) => ({ ...s, __key: s.id }))
  return (
    <div style={{ marginTop: '1rem' }}>
      <Card
        title="Services"
        icon={Waypoints}
        actions={
          services.length > 0 && (
            <div className="chips" role="group" aria-label="Kind of service">
              {[['all', 'All'], ...KINDS].map(([k, label]) => (
                <button key={k} className={`small chip ${kind === k ? 'active' : ''}`} disabled={k !== 'all' && !n[k]} aria-pressed={kind === k} onClick={() => setKind(k)}>
                  {label} ({n[k]})
                </button>
              ))}
            </div>
          )
        }
      >
        {hint ? (
          <div className="notice small">{hint}</div>
        ) : !services.length ? (
          <p className="muted small" style={{ margin: 0 }}>
            The agent runs, but nothing is registered in it.{' '}
            {st?.managed
              ? 'The node registers its services within a minute of its layer - its last sync says why not, above.'
              : 'Services come from a services block in the configuration below - or from Janus, once the Controller manages this agent.'}
          </p>
        ) : (
          <DataTable
            rows={rows}
            label="Services"
            maxHeight="60vh"
            initialSort={{ key: 'name', dir: 1 }}
            empty="None of this kind."
            columns={[
              { key: 'name', label: 'Service', render: (s) => <span className="mono nowrap">{s.name}</span> },
              { key: 'id', label: 'ID', render: (s) => <span className="mono small consul-id">{s.id}</span> },
              { key: 'address', label: 'Address', get: address, render: (s) => <span className="mono nowrap">{address(s)}</span> },
              { key: 'tags', label: 'Tags', get: (s) => (s.tags || []).join(' '), render: (s) => <span className="small muted">{(s.tags || []).join(', ') || '-'}</span> },
              { key: 'meta', label: 'URL, metrics or frontend', get: keyMeta, render: (s) => <span className="mono small nowrap">{keyMeta(s) || '-'}</span> },
              { key: 'status', label: 'Health', render: (s) => <Health s={s} /> },
              { key: 'managed', label: 'Declared by', get: (s) => (s.managed ? 'Janus' : 'configuration'), render: (s) => (s.managed ? <Badge tone="info">Janus</Badge> : <Badge>configuration</Badge>) },
            ]}
          />
        )}
      </Card>
    </div>
  )
}

// Health is a service's checks' worst state, their outputs on hover; a
// maintenance's reason or a failing check's output under it.
function Health({ s }) {
  if (!s.status) return <span className="muted small">no check</span>
  const reason = s.status === 'maintenance' ? maintenanceReason(s) : s.status !== 'passing' ? (s.checks || []).find((c) => c.status === s.status && c.output)?.output : ''
  return (
    <div title={checksText(s) || undefined}>
      <Badge tone={CHECK_TONES[s.status]} dot>
        {s.status}
      </Badge>
      {reason && <div className="small muted consul-check-output">{reason}</div>}
    </div>
  )
}
