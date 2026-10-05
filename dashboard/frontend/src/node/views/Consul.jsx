import { ExternalLink, FileKey, Network, ScrollText, Trash2, Undo2, Upload, Waypoints } from 'lucide-react'
import { useState } from 'react'
import { Badge, Card, ErrorBox, Loading, PageHeader } from '../../shared/ui.jsx'
import ModuleConfigEditor from '../components/ModuleConfigEditor.jsx'
import { usePoll } from '../hooks.jsx'
import { useMay } from '../may.js'

const DOCS = 'https://github.com/swenske/Janus/blob/main/docs/consul.md'

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

// Consul is the consul extension: the agent's state and its cluster, the
// files its configuration names, and the configuration itself.
export default function Consul() {
  const status = usePoll('/api/network/consul', { every: 5000 })
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
          ) : !st.configured ? (
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
          <pre className="code small">{RESOLVERS}</pre>
        </Card>
      </div>

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
