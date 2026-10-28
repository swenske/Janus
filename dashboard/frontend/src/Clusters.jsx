import { ChevronLeft, ExternalLink, FileText, History, KeyRound, Loader2, Network, Pencil, Plus, RefreshCw, ScrollText, Send, Split, Trash2, Upload, Users } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { call, postJSON } from './call.js'
import { DOMAINS, clusterNameError, domainsOf, health, memberDefaults, memberNameError, parseVars, pathError, varsText, versionChanges, when } from './clusters.js'
import { useCan } from './me.jsx'
import { DiffView, Editor } from './shared/Editor.jsx'
import { requirementError, toggleExtension } from './shared/requires.js'
import { navigate } from './shared/route.js'
import { Badge, Card, ErrorBox, Loading, Tabs, useAction, useConfirm, useToast } from './shared/ui.jsx'

// Clusters (docs/guide/clusters.md): groups of nodes the Controller keeps
// alike - one configuration in versions applied member by member, stick
// tables replicated between them, their secrets carried from member to
// member, each member locked against changes made behind the cluster's
// back (dashboard/backend: cluster*.go). Readers see them, admins change
// them. #/clusters lists them, #/clusters/<id> is one.

const DOCS = 'https://janus.sw-servers.net/docs/guide/clusters.md'
const BUSY_EVERY = 2000
const IDLE_EVERY = 15000

// usePolled loads path now and again: often while busy says so.
function usePolled(path, busyOf) {
  const [data, setData] = useState(null)
  const [error, setError] = useState(null)
  const load = useCallback(
    (background) =>
      call(path, { background, timeout: 10000 }).then(
        (d) => {
          setData(d)
          setError(null)
          return d
        },
        (err) => setError(err),
      ),
    [path],
  )
  const busy = data ? busyOf(data) : false
  useEffect(() => {
    load(false)
  }, [load])
  useEffect(() => {
    const t = setInterval(() => {
      if (!document.hidden) load(true)
    }, busy ? BUSY_EVERY : IDLE_EVERY)
    return () => clearInterval(t)
  }, [load, busy])
  return [data, error, load]
}

export default function ClustersPage({ route, nodes }) {
  const id = route.split('/')[2] || ''
  return id ? <ClusterPage id={id} nodes={nodes || []} /> : <ClusterList nodes={nodes || []} />
}

function HealthBadge({ cluster }) {
  const h = health(cluster)
  return (
    <Badge tone={h.tone} dot={!h.busy}>
      {h.busy && <Loader2 size={12} className="spin" />} {h.label}
    </Badge>
  )
}

function Guide() {
  return (
    <a className="small" href={DOCS} target="_blank" rel="noreferrer">
      Guide <ExternalLink size={11} />
    </a>
  )
}

// --- the list ---

function ClusterList({ nodes }) {
  const can = useCan()
  const [clusters, error, load] = usePolled('/api/clusters', (cs) => cs.some((c) => c.busy))
  const [creating, setCreating] = useState(false)
  if (!clusters) return error ? <ErrorBox error={error} /> : <Loading />
  const taken = new Set(clusters.flatMap((c) => c.members.map((m) => m.node_id)))
  return (
    <div className="stack">
      <div className="spread">
        <h1>Clusters</h1>
        <div className="row">
          <Guide />
          {!creating && can('admin') && (
            <button className="primary" onClick={() => setCreating(true)}>
              <Plus size={15} /> New cluster
            </button>
          )}
        </div>
      </div>
      <p className="muted">
        A cluster keeps its nodes alike: one configuration, applied member by member; stick tables replicated between them; certificates and keys carried from member to
        member, never through this Controller - and each member locked against changes made behind the cluster’s back.
      </p>
      {error && <ErrorBox error={error} />}
      {creating && (
        <CreateCluster
          nodes={nodes.filter((n) => !taken.has(n.id))}
          onCancel={() => setCreating(false)}
          onCreated={(c) => {
            setCreating(false)
            load(false)
            navigate(`/clusters/${c.id}`)
          }}
        />
      )}
      {clusters.length === 0 && !creating && <div className="card empty">No cluster yet.{can('admin') ? ' Make one from nodes that trust the fleet and run the same image.' : ''}</div>}
      <div className="node-grid">
        {clusters.map((c) => (
          <a key={c.id} className="card cluster-card" href={`#/clusters/${c.id}`}>
            <div className="spread">
              <strong className="cluster-name">
                <Network size={15} /> {c.name}
              </strong>
              <HealthBadge cluster={c} />
            </div>
            <dl className="kv small">
              <dt>Members</dt>
              <dd>{c.members.map((m) => m.name).join(', ')}</dd>
              <dt>Version</dt>
              <dd>{c.desired ? `v${c.desired}` : '-'}</dd>
              <dt>Manages</dt>
              <dd>{c.managed.join(', ')}</dd>
            </dl>
            {c.problems.length > 0 && <div className="small muted cluster-problem">{c.problems[0]}</div>}
          </a>
        ))}
      </div>
    </div>
  )
}

// MACHINE_DEFAULTS are a new cluster's machines' - what a machine made
// on its own gets.
const MACHINE_DEFAULTS = { count: 2, vcpus: 2, memory: 1024, version: '', network: '', nic: 'eth0', anti: 'required', seed: '' }

// hvNetworks are the networks a hypervisor record offers its machines.
const hvNetworks = (hv) => hv?.libvirt?.networks || hv?.proxmox?.networks || []

function CreateCluster({ nodes, onCancel, onCreated }) {
  const [from, setFrom] = useState('nodes') // or 'machines'
  const [hypervisors, setHypervisors] = useState(null)
  const [catalog, setCatalog] = useState(null)
  const [mach, setMach] = useState(MACHINE_DEFAULTS)
  const [hvs, setHvs] = useState([]) // the records picked, in order
  const [exts, setExts] = useState([])
  const [name, setName] = useState('')
  const [picked, setPicked] = useState({}) // node id -> {name, address}
  const [source, setSource] = useState('')
  const [error, setError] = useState(null)
  const [busy, run] = useAction()
  const toggle = (n) => {
    const next = { ...picked }
    if (next[n.id]) {
      delete next[n.id]
      if (source === n.id) setSource('')
    } else {
      next[n.id] = memberDefaults(n)
      if (!source) setSource(n.id)
    }
    setPicked(next)
  }
  const set = (id, change) => setPicked({ ...picked, [id]: { ...picked[id], ...change } })
  const ids = Object.keys(picked)
  useEffect(() => {
    call('/api/hypervisors').then(setHypervisors, () => setHypervisors([]))
    call('/api/catalog').then(setCatalog, () => setCatalog({}))
  }, [])
  const usable = (hypervisors || []).filter((h) => h.trusted)
  const toggleHv = (id) => setHvs(hvs.includes(id) ? hvs.filter((x) => x !== id) : [...hvs, id])
  // Ticking an extension ticks what it requires, unticking one unticks
  // what requires it - said under the list.
  const [extNote, setExtNote] = useState('')
  const toggleExt = (x) => {
    const r = toggleExtension(exts, x, !exts.includes(x), catalog?.extensions)
    setExts(r.extensions)
    setExtNote(r.note)
  }
  const extProblem = requirementError(exts, catalog?.extensions, catalog?.catalog_version)
  // The networks every hypervisor picked offers.
  const networks = hvs.length ? hvNetworks(usable.find((h) => h.id === hvs[0])).filter((n) => hvs.every((id) => hvNetworks(usable.find((h) => h.id === id)).includes(n))) : []
  const problem =
    clusterNameError(name) ||
    (from === 'nodes'
      ? (ids.length === 0 ? 'its nodes' : '') ||
        ids.map((id) => memberNameError(picked[id].name) || (picked[id].address ? '' : 'each member’s address')).find(Boolean) ||
        (source ? '' : 'the source')
      : (hvs.length === 0 ? 'a hypervisor' : '') || (mach.network.trim() ? '' : 'the network') || (Number(mach.count) >= 1 && Number(mach.count) <= 16 ? '' : 'how many machines, 1 to 16'))
  const submit = (e) => {
    e.preventDefault()
    setError(null)
    const body =
      from === 'nodes'
        ? { name, source, members: ids.map((id) => ({ node_id: id, name: picked[id].name, address: picked[id].address.trim() })) }
        : {
            name,
            machines: {
              count: Number(mach.count),
              anti_affinity: mach.anti,
              hypervisors: hvs,
              ...(mach.seed ? { seed_from: mach.seed } : {}),
              spec: {
                vcpus: Number(mach.vcpus),
                memory_mib: Number(mach.memory),
                ...(mach.version.trim() ? { version: mach.version.trim() } : {}),
                ...(exts.length ? { extensions: exts } : {}),
                nics: [{ network: mach.network.trim(), name: mach.nic.trim() || 'eth0', mode: 'dhcp' }],
              },
            },
          }
    run(async () => {
      try {
        onCreated(await postJSON('/api/clusters', body))
      } catch (err) {
        setError(err)
      }
    })
  }
  return (
    <Card title="New cluster" icon={Plus}>
      <form className="stack" onSubmit={submit}>
        <label className="field">
          <span>Name - lowercase letters, digits, dashes</span>
          <input autoFocus value={name} onChange={(e) => setName(e.target.value.trim())} placeholder="edge" />
        </label>
        <fieldset className="plain">
          <div className="row" style={{ flexWrap: 'wrap' }}>
            <label className="check small">
              <input type="radio" name="cluster-from" checked={from === 'nodes'} onChange={() => setFrom('nodes')} /> Nodes already there
            </label>
            <label className="check small">
              <input type="radio" name="cluster-from" checked={from === 'machines'} disabled={usable.length === 0} onChange={() => setFrom('machines')} /> New machines
              {hypervisors && usable.length === 0 && <span className="muted"> - no trusted hypervisor</span>}
            </label>
          </div>
        </fieldset>
        {from === 'machines' ? (
          <div className="stack">
            <div className="row" style={{ flexWrap: 'wrap', alignItems: 'flex-end' }}>
              <label className="field">
                <span>Machines</span>
                <input type="number" min="1" max="16" value={mach.count} onChange={(e) => setMach({ ...mach, count: e.target.value })} />
              </label>
              <label className="field">
                <span>Anti-affinity</span>
                <select value={mach.anti} onChange={(e) => setMach({ ...mach, anti: e.target.value })}>
                  <option value="required">Required - a host of its own each</option>
                  <option value="preferred">Preferred - as few together as can be</option>
                  <option value="off">Off</option>
                </select>
              </label>
              <label className="field">
                <span>vCPUs</span>
                <input type="number" min="1" max="64" value={mach.vcpus} onChange={(e) => setMach({ ...mach, vcpus: e.target.value })} />
              </label>
              <label className="field">
                <span>Memory, MiB</span>
                <input type="number" min="512" step="256" value={mach.memory} onChange={(e) => setMach({ ...mach, memory: e.target.value })} />
              </label>
              <label className="field">
                <span>Release - empty: the latest</span>
                <input className="mono" value={mach.version} placeholder="v2026.10.24" onChange={(e) => setMach({ ...mach, version: e.target.value })} />
              </label>
            </div>
            <div>
              <div className="small muted">Hypervisors - two records reaching one host count as one</div>
              <div className="row" style={{ flexWrap: 'wrap' }}>
                {usable.map((h) => (
                  <label key={h.id} className="check small">
                    <input type="checkbox" checked={hvs.includes(h.id)} onChange={() => toggleHv(h.id)} /> {h.name}
                    {h.host_name && <span className="muted"> ({h.host_name})</span>}
                  </label>
                ))}
              </div>
            </div>
            <div className="row" style={{ flexWrap: 'wrap', alignItems: 'flex-end' }}>
              <label className="field">
                <span>Network - DHCP</span>
                <input className="mono" list="cluster-networks" value={mach.network} onChange={(e) => setMach({ ...mach, network: e.target.value })} />
                <datalist id="cluster-networks">
                  {networks.map((n) => (
                    <option key={n} value={n} />
                  ))}
                </datalist>
              </label>
              <label className="field">
                <span>Interface name</span>
                <input className="mono" value={mach.nic} onChange={(e) => setMach({ ...mach, nic: e.target.value })} />
              </label>
            </div>
            {catalog?.extensions?.length > 0 && (
              <div>
                <div className="small muted">Extensions</div>
                <div className="row" style={{ flexWrap: 'wrap' }}>
                  {catalog.extensions.map((x) => (
                    <label key={x.name} className="check small" title={x.requires?.length ? `Requires ${x.requires.join(', ')}` : undefined}>
                      <input type="checkbox" checked={exts.includes(x.name)} onChange={() => toggleExt(x.name)} /> {x.name}
                    </label>
                  ))}
                </div>
                {extProblem ? (
                  <div className="notice warn small" role="alert">
                    {extProblem}
                  </div>
                ) : (
                  extNote && <div className="small muted">{extNote}</div>
                )}
              </div>
            )}
            {nodes.length > 0 && (
              <label className="field">
                <span>Start from</span>
                <select value={mach.seed} onChange={(e) => setMach({ ...mach, seed: e.target.value })}>
                  <option value="">A new node’s own configuration</option>
                  {nodes.map((n) => (
                    <option key={n.id} value={n.id}>
                      {n.name} - its configuration, files and certificates
                    </option>
                  ))}
                </select>
              </label>
            )}
            <p className="small muted">
              The Controller makes the machines - named after the cluster, {name || 'edge'}-1, {name || 'edge'}-2… - each on a host of its own unless anti-affinity says
              otherwise, waits until they register and trust the fleet, then makes the cluster from them: the first one’s configuration becomes its version 1. Refused before
              anything is made when the hosts don’t allow it.
              {mach.seed && (
                <>
                  {' '}
                  Starting from a node, the first machine takes that node’s state first - its HAProxy configuration and history, files, certificates, Let’s Encrypt and
                  settings, not its identity nor its network - pulled from node to node, never through the Controller; the node itself stays as it is. Its extensions must
                  be the machines’ for what it runs.
                </>
              )}
            </p>
          </div>
        ) : nodes.length === 0 ? (
          <div className="notice warn">Every node is in a cluster already, or there is none.</div>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Member</th>
                  <th>Node</th>
                  <th>Member name</th>
                  <th>Address the others reach it on</th>
                  <th>Source</th>
                </tr>
              </thead>
              <tbody>
                {nodes.map((n) => {
                  const m = picked[n.id]
                  return (
                    <tr key={n.id}>
                      <td>
                        <input type="checkbox" aria-label={`${n.name} a member`} checked={!!m} onChange={() => toggle(n)} />
                      </td>
                      <td>
                        {n.name} <span className="muted mono small">{n.address}</span>
                      </td>
                      <td>{m && <input className="mono" aria-label={`${n.name}'s member name`} value={m.name} onChange={(e) => set(n.id, { name: e.target.value.trim() })} />}</td>
                      <td>
                        {m && <input className="mono" aria-label={`${n.name}'s address`} value={m.address} placeholder="192.0.2.11" onChange={(e) => set(n.id, { address: e.target.value })} />}
                      </td>
                      <td>{m && <input type="radio" name="source" aria-label={`${n.name} the source`} checked={source === n.id} onChange={() => setSource(n.id)} />}</td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
        {from === 'nodes' && (
          <p className="small muted">
            The source’s configuration becomes the cluster’s first version - its haproxy.cfg and files -, and its certificates and keys every member’s. The members trust this
            Controller’s fleet, run the same image, and reach each other on TCP 9505 and 10057. The cluster then manages HAProxy’s configuration and files; VRRP, BGP and the
            firewall come with a version carrying their template.
          </p>
        )}
        <ErrorBox error={error} />
        <div className="row">
          <button className="primary" type="submit" disabled={busy || !!problem} title={problem ? `Missing: ${problem}` : undefined}>
            <Network size={15} /> Create
          </button>
          <button type="button" onClick={onCancel}>
            Cancel
          </button>
          {problem && <span className="small muted">Still needed: {problem}</span>}
        </div>
      </form>
    </Card>
  )
}

// --- one cluster ---

function ClusterPage({ id, nodes }) {
  const can = useCan()
  const confirm = useConfirm()
  const [busyAction, run] = useAction()
  const [cluster, error, load] = usePolled(`/api/clusters/${id}`, (c) => c.busy)
  const [tab, setTab] = useState('members')
  if (!cluster) {
    return (
      <div className="stack">
        <BackLink />
        {error ? <ErrorBox error={error} /> : <Loading />}
      </div>
    )
  }
  const act = (path, success, method = 'POST') => run(() => postJSON(`/api/clusters/${id}${path}`, {}, method).then(() => load(false)), success)
  const remove = async () => {
    const vms = cluster.members.filter((m) => m.machine_id)
    const choice = { destroy: false }
    if (
      !(await confirm({
        title: `Delete cluster ${cluster.name}?`,
        body: (
          <>
            <p>Every member leaves it: its peers section names it alone again, its locks go, its member certificate and the cluster’s CA are deleted. The members keep the configuration they run.</p>
            {vms.length > 0 && (
              <label className="check">
                <input type="checkbox" onChange={(e) => (choice.destroy = e.target.checked)} /> Delete the members’ virtual machines too ({vms.map((m) => m.name).join(', ')}) - destroyed once
                they left, their disks with them
                {vms.length < cluster.members.length ? '; the other members aren’t this Controller’s machines and stay' : ''}
              </label>
            )}
          </>
        ),
        action: 'Delete',
        danger: true,
        typeToConfirm: cluster.name,
      }))
    )
      return
    run(
      async () => {
        await call(`/api/clusters/${id}${choice.destroy ? '?machines=destroy' : ''}`, { method: 'DELETE' })
        navigate('/clusters')
      },
      choice.destroy ? `Cluster ${cluster.name} is being deleted, its members’ virtual machines destroyed.` : `Cluster ${cluster.name} is being deleted.`,
    )
  }
  return (
    <div className="stack">
      <BackLink />
      <div className="spread">
        <div className="row">
          <h1>{cluster.name}</h1>
          <HealthBadge cluster={cluster} />
        </div>
        <div className="row">
          <Guide />
          {can('admin') && (
            <>
              <button className="small" disabled={busyAction || cluster.busy} onClick={() => run(() => postJSON(`/api/clusters/${id}/check`, {}).then(() => load(false)), 'Every member looked at.')}>
                <RefreshCw size={14} /> Check now
              </button>
              <button className="small danger" disabled={busyAction || cluster.busy} onClick={remove}>
                <Trash2 size={14} /> Delete
              </button>
            </>
          )}
        </div>
      </div>
      <dl className="kv small">
        <dt>Members</dt>
        <dd>{cluster.members.length}</dd>
        <dt>Version</dt>
        <dd>{cluster.desired ? `v${cluster.desired}` : '-'}</dd>
        <dt>Manages</dt>
        <dd>{cluster.managed.join(', ')}</dd>
        <dt>Secrets from</dt>
        <dd>{cluster.secrets_from}</dd>
        {cluster.placement && (
          <>
            <dt>Placement</dt>
            <dd>
              anti-affinity {cluster.placement.anti_affinity}, {cluster.placement.hypervisors.length} hypervisor{cluster.placement.hypervisors.length === 1 ? '' : 's'}
            </dd>
          </>
        )}
        <dt>Peers port</dt>
        <dd className="mono">{cluster.peers_port}</dd>
        <dt>CA</dt>
        <dd className="mono">{cluster.ca_fingerprint ? `SHA-256 ${cluster.ca_fingerprint.slice(0, 16)}…` : '-'}</dd>
      </dl>
      {cluster.busy && (
        <div className="notice">
          <Loader2 size={14} className="spin" /> {cluster.phase}…
        </div>
      )}
      {error && <ErrorBox error={error} />}
      {cluster.problems.length > 0 && (
        <div className="notice warn">
          <ul className="plain-list">
            {cluster.problems.map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
        </div>
      )}
      <Tabs
        tabs={[
          { id: 'members', label: 'Members', icon: Users },
          { id: 'configuration', label: 'Configuration', icon: FileText },
          { id: 'certificates', label: 'Certificates', icon: KeyRound },
          { id: 'journal', label: 'Journal', icon: ScrollText },
        ]}
        active={tab}
        onChange={setTab}
      />
      {tab === 'members' && <Members cluster={cluster} nodes={nodes} act={act} busy={busyAction} reload={() => load(false)} />}
      {tab === 'configuration' && <Configuration cluster={cluster} reload={() => load(false)} />}
      {tab === 'certificates' && <Certificates cluster={cluster} act={act} busy={busyAction} reload={() => load(false)} />}
      {tab === 'journal' && <Journal cluster={cluster} />}
    </div>
  )
}

function BackLink() {
  return (
    <a className="small" href="#/clusters">
      <ChevronLeft size={13} /> Clusters
    </a>
  )
}

function Members({ cluster, nodes, act, busy, reload }) {
  const can = useCan()
  const confirm = useConfirm()
  const toast = useToast()
  const [, run] = useAction()
  const [adding, setAdding] = useState(false)
  const [vars, setVars] = useState(null) // member name being edited
  const [recycling, setRecycling] = useState(null) // member name being recycled
  const disabled = busy || cluster.busy
  const removeMember = async (m) => {
    let force = false
    if (
      !(await confirm({
        title: `Remove ${m.name} from ${cluster.name}?`,
        body: (
          <>
            <p>It is drained first - its virtual IPs and routes move to the other members -, then leaves the cluster: the others drop it as a peer, its locks go.</p>
            <label className="check">
              <input type="checkbox" onChange={(e) => (force = e.target.checked)} /> Even if it doesn’t answer or drain (a node gone for good)
            </label>
          </>
        ),
        action: 'Remove',
        danger: true,
      }))
    )
      return
    run(async () => {
      await call(`/api/clusters/${cluster.id}/members/${m.name}${force ? '?force=true' : ''}`, { method: 'DELETE' })
      reload()
    }, `${m.name} is being removed.`)
  }
  const drain = async (m) => {
    if (m.state.drained) return act(`/members/${m.name}/undrain`, `${m.name} is going back in service.`)
    if (
      await confirm({
        title: `Drain ${m.name}?`,
        body: <p>It stays a member, HAProxy keeps serving, but keepalived and BIRD see it unhealthy: its virtual IPs move to a peer, its routes are withdrawn - until you undrain it.</p>,
        action: 'Drain',
        danger: true,
      })
    )
      act(`/members/${m.name}/drain`, `${m.name} is draining.`)
  }
  return (
    <div className="stack">
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>Member</th>
              <th>Node</th>
              <th>Address</th>
              <th>Version</th>
              <th>Peers</th>
              <th>Stick tables</th>
              <th>Certificate</th>
              <th>State</th>
              {can('admin') && <th />}
            </tr>
          </thead>
          <tbody>
            {cluster.members.map((m) => {
              const s = m.state
              const checked = s.checked && !String(s.checked).startsWith('0001')
              return (
                <tr key={m.name}>
                  <td>
                    <strong>{m.name}</strong> {m.name === cluster.secrets_from && <Badge tone="info">secrets</Badge>}
                  </td>
                  <td>
                    {m.node_found ? m.node_name : <Badge tone="danger">gone</Badge>}
                    {m.host_name && <div className="small muted">on {m.host_name}</div>}
                  </td>
                  <td className="mono small">{m.address}</td>
                  <td>{s.applied ? <Badge tone={s.applied === cluster.desired ? 'ok' : 'warn'}>v{s.applied}</Badge> : '-'}</td>
                  <td>{checked ? <Badge tone={s.peers_up < s.peers_total ? 'danger' : 'ok'}>{`${s.peers_up}/${s.peers_total}`}</Badge> : '-'}</td>
                  <td>{checked && s.peers_total > 0 ? <Badge tone={s.tables_synced ? 'ok' : 'warn'}>{s.tables_synced ? 'in sync' : 'catching up'}</Badge> : '-'}</td>
                  <td className="small">{when(s.cert_not_after)}</td>
                  <td>
                    <div className="row" style={{ flexWrap: 'wrap', gap: '0.3rem' }}>
                      {s.drained && <Badge tone="warn">drained</Badge>}
                      {s.drift?.length > 0 && <Badge tone="danger">drifted: {s.drift.join(', ')}</Badge>}
                      {s.released?.length > 0 && <Badge tone="warn">released: {s.released.join(', ')}</Badge>}
                      {s.error && <Badge tone="danger">error</Badge>}
                      {!s.drained && !s.drift?.length && !s.released?.length && !s.error && checked && <Badge tone="ok">in sync</Badge>}
                    </div>
                    {s.error && <div className="small muted">{s.error}</div>}
                  </td>
                  {can('admin') && (
                    <td>
                      <div className="row">
                        <button className="small" disabled={disabled} onClick={() => drain(m)}>
                          {s.drained ? 'Undrain' : 'Drain'}
                        </button>
                        <button className="small" disabled={disabled} onClick={() => setVars(vars === m.name ? null : m.name)} title="The variables its templates use">
                          <Pencil size={13} /> Variables
                        </button>
                        <button className="small" disabled={disabled} onClick={() => setRecycling(recycling === m.name ? null : m.name)} title="Replace it by a new member, the traffic moving once">
                          <RefreshCw size={13} /> Recycle
                        </button>
                        <button className="small ghost icon" disabled={disabled || cluster.members.length === 1} aria-label={`Remove ${m.name}`} title="Remove from the cluster" onClick={() => removeMember(m)}>
                          <Trash2 size={14} />
                        </button>
                      </div>
                    </td>
                  )}
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>
      {vars && <Variables cluster={cluster} member={cluster.members.find((m) => m.name === vars)} onDone={() => setVars(null)} reload={reload} />}
      {recycling && <Recycle cluster={cluster} member={cluster.members.find((m) => m.name === recycling)} nodes={nodes} onDone={() => setRecycling(null)} reload={reload} />}
      {can('admin') &&
        (adding ? (
          <AddMember cluster={cluster} nodes={nodes} onDone={() => setAdding(false)} reload={reload} />
        ) : (
          <div className="row">
            <button disabled={disabled} onClick={() => setAdding(true)}>
              <Plus size={15} /> Add a member
            </button>
            <SecretsFrom cluster={cluster} disabled={disabled} reload={reload} toast={toast} />
          </div>
        ))}
      {cluster.placement && <Scaling cluster={cluster} disabled={disabled} reload={reload} />}
      <Balancer cluster={cluster} disabled={disabled} reload={reload} />
    </div>
  )
}

// Balancer: the hook telling a balancer in front which members take the
// traffic - before one is drained or removed, once one is in service -,
// or a RouterOS router whose ECMP routes the Controller keeps itself - or
// only reads, the members announcing them over BGP (read only)
// (docs/guide/clusters.md "A balancer in front", "RouterOS").
function Balancer({ cluster, disabled, reload }) {
  const can = useCan()
  const toast = useToast()
  const confirm = useConfirm()
  const [busy, run] = useAction()
  const b = cluster.balancer
  const st = cluster.balancer_state || {}
  const ros = b?.kind === 'routeros'
  const [editing, setEditing] = useState(false)
  const [form, setForm] = useState({
    kind: b?.kind || 'webhook',
    url: b?.url || '',
    delay: b?.drain_delay_seconds || 0,
    secret: '',
    ca: b?.ca_cert || '',
    fingerprint: b?.fingerprint || '',
    user: b?.user || '',
    prefixes: (b?.prefixes || []).join(', '),
    distance: b?.distance || 1,
    table: b?.routing_table || 'main',
    check: !!b?.check_gateway,
    readOnly: !!b?.read_only,
  })
  const [error, setError] = useState(null)
  const formROS = form.kind === 'routeros'
  const set = (k) => (e) => setForm({ ...form, [k]: e.target.value })
  const send = (fn, done) =>
    run(async () => {
      setError(null)
      try {
        await fn()
        reload()
        setEditing(false)
        toast(done)
      } catch (err) {
        setError(err)
      }
    })
  const save = () =>
    send(
      () => {
        const body = { kind: form.kind, url: form.url.trim(), drain_delay_seconds: Number(form.delay) || 0, ca_cert: form.ca.trim(), fingerprint: form.fingerprint.trim() }
        if (formROS) {
          Object.assign(body, {
            user: form.user.trim(),
            prefixes: form.prefixes.split(/[\s,]+/).filter(Boolean),
            distance: form.readOnly ? 0 : Number(form.distance) || 0,
            routing_table: form.table.trim() === 'main' ? '' : form.table.trim(),
            check_gateway: !form.readOnly && form.check,
            read_only: form.readOnly,
          })
        }
        if (form.secret !== '') body.secret = form.secret
        return postJSON(`/api/clusters/${cluster.id}/balancer`, body, 'PUT')
      },
      formROS
        ? form.readOnly
          ? 'RouterOS settings saved: its routes are read, none written.'
          : 'RouterOS settings saved, and its routes made to match the members.'
        : 'Balancer hook saved, and called with the members.',
    )
  const sync = () => send(() => postJSON(`/api/clusters/${cluster.id}/balancer/sync`), ros ? 'The router’s routes match the members.' : 'The balancer was told the members.')
  const remove = async () => {
    const body = b?.read_only ? (
      <p>The router&apos;s routes are no longer read: the members&apos; pages show their own BGP state alone.</p>
    ) : ros ? (
      <p>
        The router&apos;s routes are no longer kept. Those it has stay - remove them on the router (comment <span className="mono">janus-cluster:{cluster.id}</span>).
      </p>
    ) : (
      <p>The balancer in front is no longer told which members take the traffic.</p>
    )
    if (await confirm({ title: ros ? 'Stop keeping the router’s routes?' : 'Remove the balancer hook?', body, action: 'Remove', danger: true })) {
      send(() => postJSON(`/api/clusters/${cluster.id}/balancer`, undefined, 'DELETE'), ros ? 'The router is no longer kept.' : 'Balancer hook removed.')
    }
  }
  const trust = b?.fingerprint ? 'its certificate pinned' : b?.ca_cert ? 'its own CA trusted' : 'the system’s CAs trusted'
  if (!b && !can('admin')) return null
  return (
    <Card title="Balancer in front" icon={Split}>
      <div className="stack">
        {b ? (
          <dl className="kv small">
            <dt>{ros ? 'Router' : 'Hook'}</dt>
            <dd className="mono">{b.url}</dd>
            {ros ? (
              <>
                <dt>Routes</dt>
                <dd>
                  {b.read_only ? (
                    <>
                      to <span className="mono">{(b.prefixes || []).join(', ')}</span>, read - never written: the members&apos; BGP brings them (table{' '}
                      {b.routing_table || 'main'})
                    </>
                  ) : (
                    <>
                      to <span className="mono">{(b.prefixes || []).join(', ')}</span>, one per member in service (ECMP) - distance {b.distance || 1}, table{' '}
                      {b.routing_table || 'main'}
                      {b.check_gateway ? ', each member pinged by the router' : ''}
                    </>
                  )}
                </dd>
                <RouterSays cluster={cluster} />
                <dt>Signs in</dt>
                <dd>
                  as <span className="mono">{b.user}</span>
                  {b.has_secret ? '' : ', no password'}, {trust};{' '}
                  {b.read_only
                    ? 'a drained member withdraws its BGP route itself'
                    : `the drain ${b.drain_delay_seconds ? `${b.drain_delay_seconds} s after its route goes` : 'once its route is gone'}`}
                </dd>
              </>
            ) : (
              <>
                <dt>Calls</dt>
                <dd>
                  {b.has_secret ? 'signed' : 'not signed'}, {trust}, the drain {b.drain_delay_seconds ? `${b.drain_delay_seconds} s after it is told` : 'once it answers'}
                </dd>
              </>
            )}
            <dt>Last told</dt>
            <dd>
              {when(st.sent_at)}
              {st.error && (
                <>
                  {' '}
                  <Badge tone="danger">not told</Badge> <span className="small">{st.error}</span>
                </>
              )}
            </dd>
          </dl>
        ) : (
          <p className="small muted">
            A cloud load balancer, a router&apos;s ECMP routes - something in front that sends to the members: a hook tells it which ones take the traffic, before one is drained
            or removed and once one is in service. Or a RouterOS router, its routes kept by the Controller itself.
          </p>
        )}
        {can('admin') &&
          (editing ? (
            <>
              <div className="row" style={{ flexWrap: 'wrap', alignItems: 'flex-end' }}>
                <label className="field">
                  <span>Kind</span>
                  <select value={form.kind} onChange={set('kind')}>
                    <option value="webhook">Webhook</option>
                    <option value="routeros">RouterOS - ECMP routes</option>
                  </select>
                </label>
                <label className="field" style={{ flex: '1 1 20rem' }}>
                  <span>{formROS ? 'Router' : 'Hook URL'}</span>
                  <input
                    type="url"
                    className="mono"
                    value={form.url}
                    placeholder={formROS ? 'https://router.example.net' : 'https://lb-api.example.net/janus'}
                    onChange={set('url')}
                  />
                </label>
                <label className="field">
                  <span>Drain delay (s)</span>
                  <input type="number" min="0" max="3600" value={form.delay} onChange={set('delay')} />
                </label>
              </div>
              {formROS && (
                <div className="row" style={{ flexWrap: 'wrap', alignItems: 'flex-end' }}>
                  <label className="field">
                    <span>User</span>
                    <input value={form.user} autoComplete="off" placeholder="janus-hook" onChange={set('user')} />
                  </label>
                  <label className="field" style={{ flex: '1 1 14rem' }}>
                    <span>Service prefixes</span>
                    <input className="mono" value={form.prefixes} placeholder="203.0.113.10/32" onChange={set('prefixes')} />
                  </label>
                  {!form.readOnly && (
                    <label className="field">
                      <span>Distance</span>
                      <input type="number" min="1" max="255" value={form.distance} onChange={set('distance')} />
                    </label>
                  )}
                  <label className="field">
                    <span>Routing table</span>
                    <input value={form.table} onChange={set('table')} />
                  </label>
                  {!form.readOnly && (
                    <label className="row small">
                      <input type="checkbox" checked={form.check} onChange={(e) => setForm({ ...form, check: e.target.checked })} />
                      The router pings each member (check-gateway=ping)
                    </label>
                  )}
                  <label className="row small">
                    <input type="checkbox" checked={form.readOnly} onChange={(e) => setForm({ ...form, readOnly: e.target.checked })} />
                    Read only - the members announce the prefixes over BGP: read the routes, write none
                  </label>
                </div>
              )}
              <div className="row" style={{ flexWrap: 'wrap', alignItems: 'flex-end' }}>
                <label className="field">
                  <span>{formROS ? 'Password' : 'Secret'}</span>
                  <input type="password" autoComplete="new-password" value={form.secret} placeholder={b?.has_secret && b?.kind === form.kind ? 'unchanged' : 'none'} onChange={set('secret')} />
                </label>
                <label className="field" style={{ flex: '1 1 24rem' }}>
                  <span>Its certificate&apos;s SHA-256 (optional - pins it)</span>
                  <input className="mono" value={form.fingerprint} onChange={set('fingerprint')} />
                </label>
              </div>
              <label className="field">
                <span>Its CA (PEM, optional - else the system&apos;s)</span>
                <textarea className="mono small" rows={3} value={form.ca} onChange={set('ca')} />
              </label>
              <div className="row">
                <button className="primary" disabled={disabled || busy || !form.url.trim()} onClick={save}>
                  Save
                </button>
                <button className="ghost" disabled={busy} onClick={() => setEditing(false)}>
                  Cancel
                </button>
              </div>
            </>
          ) : (
            <div className="row">
              <button disabled={disabled || busy} onClick={() => setEditing(true)}>
                <Pencil size={15} /> {b ? 'Change' : 'Set a hook'}
              </button>
              {b && (
                <>
                  <button className="ghost" disabled={disabled || busy} onClick={sync}>
                    <Send size={15} /> Tell it now
                  </button>
                  <button className="ghost danger" disabled={disabled || busy} onClick={remove}>
                    <Trash2 size={15} /> Remove
                  </button>
                </>
              )}
            </div>
          ))}
        <ErrorBox error={error} />
      </div>
    </Card>
  )
}

// RouterSays: each member's role on the router for each service prefix,
// as the cluster's RouterOS hook reads it - multipath, master, backup,
// absent; the same reading as the members' pages.
function RouterSays({ cluster }) {
  const [view] = usePolled(`/api/clusters/${cluster.id}/router`, () => false)
  if (!view?.available) return null
  const tone = (role) => (role === 'multipath' || role === 'master' ? 'ok' : role === 'absent' ? 'danger' : '')
  return (
    <>
      <dt>Router says</dt>
      <dd>
        {view.error ? (
          <span className="small">can&apos;t be read: {view.error}</span>
        ) : (
          <div className="stack" style={{ gap: '0.25rem' }}>
            {cluster.members.map((m) => (
              <div key={m.name} className="row" style={{ flexWrap: 'wrap', gap: '0.4rem' }}>
                <span className="mono">{m.name}</span>
                {(view.members?.[m.name] || []).map((r) => (
                  <span key={r.prefix} title={`${r.prefix}: ${r.active} of ${r.total} route(s) to it active`}>
                    <Badge tone={tone(r.role)}>
                      {r.role} <span className="mono">{r.prefix}</span>
                    </Badge>
                  </span>
                ))}
              </div>
            ))}
            <span className="small muted">read {when(view.read_at)}</span>
          </div>
        )}
      </dd>
    </>
  )
}

// Scaling: a cluster made on hypervisors gains and loses members - set
// by hand, or by its auto-scaling (docs/guide/clusters.md "Scaling").
function Scaling({ cluster, disabled, reload }) {
  const can = useCan()
  const toast = useToast()
  const [busy, run] = useAction()
  const as = cluster.autoscale
  const [members, setMembers] = useState(cluster.members.length)
  const [policy, setPolicy] = useState({ mode: as?.mode || 'off', min: as?.min_members || 2, max: as?.max_members || Math.max(4, cluster.members.length) })
  const [error, setError] = useState(null)
  const load = cluster.load
  const pct = (v) => (v == null || v < 0 ? '-' : `${Math.round(v)} %`)
  const scale = () =>
    run(async () => {
      setError(null)
      try {
        await postJSON(`/api/clusters/${cluster.id}/scale`, { members: Number(members) })
        reload()
        toast(`Cluster ${cluster.name} is being brought to ${members} members.`)
      } catch (err) {
        setError(err)
      }
    })
  const save = () =>
    run(async () => {
      setError(null)
      try {
        await postJSON(`/api/clusters/${cluster.id}/autoscale`, { ...(as || {}), mode: policy.mode, min_members: Number(policy.min), max_members: Number(policy.max), paused_by: '' }, 'PUT')
        reload()
        toast(`Auto-scaling ${policy.mode}.`)
      } catch (err) {
        setError(err)
      }
    })
  return (
    <Card title="Scaling" icon={Users}>
      <div className="stack">
        {as && as.mode !== 'off' && (
          <dl className="kv small">
            <dt>Auto-scaling</dt>
            <dd>
              {as.mode}, {as.min_members} to {as.max_members} members{as.paused_by && <Badge tone="warn">paused by {as.paused_by}</Badge>}
            </dd>
            {cluster.scale_status && (
              <>
                <dt>Now</dt>
                <dd>{cluster.scale_status}</dd>
              </>
            )}
            {load && (
              <>
                <dt>Load</dt>
                <dd>
                  {load.members} member{load.members === 1 ? '' : 's'} read: HAProxy idle {pct(load.idle_percent)}, CPUs {pct(load.busy_percent)} busy (steal {pct(load.steal_percent)}),
                  connections at {pct(load.conn_percent)} of maxconn at most
                </dd>
              </>
            )}
          </dl>
        )}
        {can('admin') && (
          <>
            <div className="row" style={{ flexWrap: 'wrap', alignItems: 'flex-end' }}>
              <label className="field">
                <span>Members</span>
                <input type="number" min="1" max="16" value={members} onChange={(e) => setMembers(e.target.value)} />
              </label>
              <button disabled={disabled || busy || Number(members) === cluster.members.length} onClick={scale}>
                <Users size={15} /> Set
              </button>
              <span className="small muted">A member more is a new machine like the newest, placed; a member less the newest, drained and destroyed.</span>
            </div>
            <div className="row" style={{ flexWrap: 'wrap', alignItems: 'flex-end' }}>
              <label className="field">
                <span>Auto-scaling</span>
                <select value={policy.mode} onChange={(e) => setPolicy({ ...policy, mode: e.target.value })}>
                  <option value="off">Off</option>
                  <option value="observe">Observe - journal what it would do</option>
                  <option value="on">On</option>
                </select>
              </label>
              <label className="field">
                <span>From</span>
                <input type="number" min="2" max="16" value={policy.min} onChange={(e) => setPolicy({ ...policy, min: e.target.value })} />
              </label>
              <label className="field">
                <span>To</span>
                <input type="number" min="2" max="16" value={policy.max} onChange={(e) => setPolicy({ ...policy, max: e.target.value })} />
              </label>
              <button disabled={disabled || busy} onClick={save}>
                Save
              </button>
            </div>
            <p className="small muted">
              A member more when, over 5 minutes, HAProxy idles below 30 %, the CPUs are busier than 75 % or the connections above 70 % of maxconn; one less when one fewer would
              stay quiet on every count for 30 minutes. Not for a VRRP cluster: one member holds each virtual IP. Thresholds: through the API.
            </p>
          </>
        )}
        <ErrorBox error={error} />
      </div>
    </Card>
  )
}

function SecretsFrom({ cluster, disabled, reload, toast }) {
  const [, run] = useAction()
  return (
    <label className="row small secrets-from">
      <span className="muted">Secrets come from</span>
      <select
        value={cluster.secrets_from}
        disabled={disabled}
        onChange={(e) =>
          run(async () => {
            await postJSON(`/api/clusters/${cluster.id}`, { secrets_from: e.target.value }, 'PATCH')
            reload()
            toast(`The members now pull from ${e.target.value}.`)
          })
        }
      >
        {cluster.members.map((m) => (
          <option key={m.name}>{m.name}</option>
        ))}
      </select>
    </label>
  )
}

function Variables({ cluster, member, onDone, reload }) {
  const [text, setText] = useState(varsText(member.vars))
  const [error, setError] = useState(null)
  const [busy, run] = useAction()
  const save = () => {
    let vars
    try {
      vars = parseVars(text)
    } catch (err) {
      setError(err)
      return
    }
    run(async () => {
      await postJSON(`/api/clusters/${cluster.id}`, { vars: { [member.name]: vars } }, 'PATCH')
      reload()
      onDone()
    }, `${member.name}’s variables saved: the next apply renders its templates with them.`)
  }
  return (
    <Card title={`${member.name}’s variables`} icon={Pencil} className="data-title">
      <p className="small muted">
        What its templates use as <span className="mono">{'{{ .Member.Vars.key }}'}</span> - a VRRP priority, a BGP router ID. One <span className="mono">key=value</span> a line.
      </p>
      <textarea className="mono" rows={5} aria-label={`${member.name}’s variables`} value={text} onChange={(e) => setText(e.target.value)} placeholder="priority=150" />
      <ErrorBox error={error} />
      <div className="row">
        <button className="small primary" disabled={busy} onClick={save}>
          Save
        </button>
        <button className="small" onClick={onDone}>
          Cancel
        </button>
      </div>
    </Card>
  )
}

function AddMember({ cluster, nodes, onDone, reload }) {
  const [clusters] = usePolled('/api/clusters', () => false)
  const taken = new Set((clusters || []).flatMap((c) => c.members.map((m) => m.node_id)))
  const free = nodes.filter((n) => !taken.has(n.id))
  const [nodeID, setNodeID] = useState('')
  const [member, setMember] = useState({ name: '', address: '' })
  const [vars, setVars] = useState('')
  const [error, setError] = useState(null)
  const [busy, run] = useAction()
  const pick = (id) => {
    setNodeID(id)
    setMember(memberDefaults(nodes.find((n) => n.id === id)))
  }
  const problem = (nodeID ? '' : 'a node') || memberNameError(member.name) || (member.address ? '' : 'its address')
  const submit = (e) => {
    e.preventDefault()
    setError(null)
    let parsed
    try {
      parsed = parseVars(vars)
    } catch (err) {
      setError(err)
      return
    }
    run(async () => {
      try {
        await postJSON(`/api/clusters/${cluster.id}/members`, { node_id: nodeID, name: member.name, address: member.address.trim(), vars: parsed })
        reload()
        onDone()
      } catch (err) {
        setError(err)
      }
    })
  }
  return (
    <Card title="Add a member" icon={Plus}>
      <form className="row" style={{ flexWrap: 'wrap', alignItems: 'flex-end' }} onSubmit={submit}>
        <label className="field">
          <span>Node</span>
          <select value={nodeID} onChange={(e) => pick(e.target.value)}>
            <option value="">-</option>
            {free.map((n) => (
              <option key={n.id} value={n.id}>
                {n.name}
              </option>
            ))}
          </select>
        </label>
        <label className="field">
          <span>Member name</span>
          <input className="mono" value={member.name} onChange={(e) => setMember({ ...member, name: e.target.value.trim() })} />
        </label>
        <label className="field">
          <span>Address the others reach it on</span>
          <input className="mono" value={member.address} placeholder="192.0.2.13" onChange={(e) => setMember({ ...member, address: e.target.value })} />
        </label>
        <label className="field grow">
          <span>Variables its templates use - key=value, one a line</span>
          <textarea className="mono" rows={2} value={vars} placeholder="priority=50" onChange={(e) => setVars(e.target.value)} />
        </label>
        <button className="primary" type="submit" disabled={busy || !!problem}>
          Add
        </button>
        <button type="button" onClick={onDone}>
          Cancel
        </button>
      </form>
      <p className="small muted">
        It is checked - the cluster’s version must render for it, its variables given -, given its member certificate, told to the others; it pulls the secrets, then
        gets the cluster’s version.
      </p>
      <ErrorBox error={error} />
    </Card>
  )
}

// Recycle replaces a member (docs/guide/clusters.md "Recycling"): by a
// new machine like it when it is one of the Controller's, else by a node
// the admin picks - the newcomer joins drained, learns the stick tables,
// and comes into service as the member is drained and leaves.
function Recycle({ cluster, member, nodes, onDone, reload }) {
  const [clusters] = usePolled('/api/clusters', () => false)
  const taken = new Set((clusters || []).flatMap((c) => c.members.map((m) => m.node_id)))
  const free = nodes.filter((n) => !taken.has(n.id))
  const [how, setHow] = useState(member.machine_id ? 'machine' : 'node')
  const [version, setVersion] = useState('')
  const [nodeID, setNodeID] = useState('')
  const [repl, setRepl] = useState({ name: '', address: '' })
  const [newImage, setNewImage] = useState(false)
  const [error, setError] = useState(null)
  const [busy, run] = useAction()
  const pick = (id) => {
    setNodeID(id)
    setRepl(memberDefaults(nodes.find((n) => n.id === id)))
  }
  const problem = how === 'node' ? (nodeID ? '' : 'a node') || memberNameError(repl.name) || (repl.address ? '' : 'its address') : ''
  const submit = (e) => {
    e.preventDefault()
    setError(null)
    const body = { members: [member.name], new_image: newImage }
    if (how === 'machine') {
      if (version.trim()) body.version = version.trim()
    } else {
      body.with = { [member.name]: { node_id: nodeID, name: repl.name, address: repl.address.trim() } }
    }
    run(async () => {
      try {
        await postJSON(`/api/clusters/${cluster.id}/recycle`, body)
        reload()
        onDone()
      } catch (err) {
        setError(err)
      }
    })
  }
  return (
    <Card title={`Recycle ${member.name}`} icon={RefreshCw} className="data-title">
      <form className="stack" onSubmit={submit}>
        <fieldset className="plain">
          <div className="row" style={{ flexWrap: 'wrap' }}>
            <label className="check small">
              <input type="radio" name="recycle-how" checked={how === 'machine'} disabled={!member.machine_id} onChange={() => setHow('machine')} /> A new machine like it
              {!member.machine_id && <span className="muted"> - not one of this Controller’s machines</span>}
            </label>
            <label className="check small">
              <input type="radio" name="recycle-how" checked={how === 'node'} onChange={() => setHow('node')} /> A node of this Controller’s
            </label>
          </div>
        </fieldset>
        {how === 'machine' ? (
          <label className="field">
            <span>Its release - empty: the same as {member.name}’s</span>
            <input className="mono" value={version} placeholder="v2026.10.24" onChange={(e) => setVersion(e.target.value)} />
          </label>
        ) : (
          <div className="row" style={{ flexWrap: 'wrap', alignItems: 'flex-end' }}>
            <label className="field">
              <span>Node</span>
              <select value={nodeID} onChange={(e) => pick(e.target.value)}>
                <option value="">-</option>
                {free.map((n) => (
                  <option key={n.id} value={n.id}>
                    {n.name}
                  </option>
                ))}
              </select>
            </label>
            <label className="field">
              <span>Member name</span>
              <input className="mono" value={repl.name} onChange={(e) => setRepl({ ...repl, name: e.target.value.trim() })} />
            </label>
            <label className="field">
              <span>Address the others reach it on</span>
              <input className="mono" value={repl.address} placeholder="192.0.2.14" onChange={(e) => setRepl({ ...repl, address: e.target.value })} />
            </label>
          </div>
        )}
        <label className="check small">
          <input type="checkbox" checked={newImage} onChange={(e) => setNewImage(e.target.checked)} /> It may run another image (other extensions, HAProxy branch or kernel)
        </label>
        <p className="small muted">
          The newcomer joins drained, gets the secrets and the cluster’s version, learns the stick tables, then comes into service as {member.name} is drained - the
          traffic moves once - and {member.name} leaves{member.machine_id ? ', its machine destroyed' : ''}. Its variables are {member.name}’s. A failure before {member.name}’s
          drain takes the newcomer out again.
        </p>
        <ErrorBox error={error} />
        <div className="row">
          <button className="primary" type="submit" disabled={busy || !!problem}>
            <RefreshCw size={15} /> Recycle {member.name}
          </button>
          <button type="button" onClick={onDone}>
            Cancel
          </button>
        </div>
      </form>
    </Card>
  )
}

// --- the configuration ---

function Configuration({ cluster, reload }) {
  const can = useCan()
  const confirm = useConfirm()
  const toast = useToast()
  const [busy, run] = useAction()
  const [versions, , loadVersions] = usePolled(`/api/clusters/${cluster.id}/versions`, () => false)
  const [selected, setSelected] = useState(0)
  const [version, setVersion] = useState(null) // the selected version, with its files
  const [desired, setDesired] = useState(null) // the cluster's, for diffs
  const [file, setFile] = useState('haproxy.cfg')
  const [draft, setDraft] = useState(null) // {files, note} while editing
  const [newPath, setNewPath] = useState('')
  const [error, setError] = useState(null)
  const shown = selected || cluster.desired || versions?.[0]?.version || 0
  useEffect(() => {
    if (!shown) return
    call(`/api/clusters/${cluster.id}/versions/${shown}`).then((v) => {
      setVersion(v)
      setFile((f) => (v.content[f] !== undefined ? f : v.files[0] || 'haproxy.cfg'))
    }, setError)
  }, [cluster.id, shown])
  useEffect(() => {
    if (!cluster.desired) return
    call(`/api/clusters/${cluster.id}/versions/${cluster.desired}`).then(setDesired, setError)
  }, [cluster.id, cluster.desired])
  if (!versions) return <Loading />
  const files = draft ? draft.files : version?.content || {}
  const paths = Object.keys(files).sort()
  const edit = () => setDraft({ files: { ...(version?.content || {}) }, note: '' })
  const save = () =>
    run(async () => {
      try {
        const base = version?.content || {}
        const changed = Object.fromEntries(Object.entries(draft.files).filter(([p, c]) => base[p] !== c))
        const removed = Object.keys(base).filter((p) => draft.files[p] === undefined)
        const v = await postJSON(`/api/clusters/${cluster.id}/versions`, { base: version ? version.version : -1, files: changed, delete: removed, note: draft.note })
        setDraft(null)
        setSelected(v.version)
        loadVersions(false)
        setError(null)
        toast(`Version ${v.version} made - apply it to put it on the members.`)
      } catch (err) {
        setError(err)
      }
    })
  const apply = async () => {
    const changes = versionChanges(desired?.content || {}, version.content)
    let managed = [...new Set([...cluster.managed, ...domainsOf(version.files)])].filter((d) => DOMAINS.some((x) => x.id === d))
    const ok = await confirm({
      title: `Apply version ${version.version} to ${cluster.name}?`,
      wide: true,
      action: `Apply v${version.version}`,
      body: (
        <>
          <p>
            Checked on {cluster.members[0]?.name} first, then applied one member at a time: the files, HAProxy, the files it no longer has, the firewall (on trial, confirmed), VRRP, BGP.
            The first failure stops it.
          </p>
          <fieldset className="plain">
            <legend className="small muted">The cluster manages - locked on its members</legend>
            <div className="row" style={{ flexWrap: 'wrap' }}>
              {DOMAINS.map((d) => (
                <label key={d.id} className="check small">
                  <input
                    type="checkbox"
                    defaultChecked={managed.includes(d.id)}
                    onChange={(e) => (managed = e.target.checked ? [...managed, d.id] : managed.filter((x) => x !== d.id))}
                  />{' '}
                  {d.label}
                </label>
              ))}
            </div>
          </fieldset>
          {changes.length === 0 ? (
            <p className="small muted">Its files are version {cluster.desired}’s.</p>
          ) : (
            changes.map((c) => (
              <div key={c.path} className="stack" style={{ gap: '0.3rem' }}>
                <div className="small">
                  <span className="mono">{c.path}</span> <Badge tone={c.kind === 'removed' ? 'danger' : c.kind === 'added' ? 'ok' : 'info'}>{c.kind}</Badge>
                </div>
                <DiffView diff={c.diff} />
              </div>
            ))
          )}
        </>
      ),
    })
    if (!ok) return
    run(async () => {
      await postJSON(`/api/clusters/${cluster.id}/apply`, { version: version.version, managed: DOMAINS.map((d) => d.id).filter((d) => managed.includes(d)) })
      reload()
    }, `Version ${version.version} is being applied.`)
  }
  return (
    <div className="cluster-config">
      <Card title="Versions" icon={History} className="cluster-versions">
        {versions.length === 0 && <p className="small muted">None yet.</p>}
        <ul className="plain-list version-list">
          {versions.map((v) => (
            <li key={v.version}>
              <button className={v.version === shown ? 'ghost active' : 'ghost'} disabled={!!draft} onClick={() => setSelected(v.version)}>
                <span className="mono">v{v.version}</span> {v.version === cluster.desired && <Badge tone="ok">desired</Badge>}
                <span className="small muted version-meta">
                  {v.author} · {when(v.created)}
                </span>
                {v.note && <span className="small version-note">{v.note}</span>}
              </button>
            </li>
          ))}
        </ul>
      </Card>
      <Card
        title={draft ? `New version, from v${version?.version || '-'}` : version ? `Version ${version.version}` : 'Configuration'}
        icon={FileText}
        className="data-title"
        actions={
          can('admin') &&
          (draft ? (
            <>
              <button className="small primary" disabled={busy} onClick={save}>
                Save as a new version
              </button>
              <button className="small" onClick={() => setDraft(null)}>
                Cancel
              </button>
            </>
          ) : (
            <>
              <button className="small" disabled={busy} onClick={edit}>
                <Pencil size={13} /> Edit
              </button>
              {version && (
                <button className="small primary" disabled={busy || cluster.busy} onClick={apply}>
                  Apply v{version.version}…
                </button>
              )}
            </>
          ))
        }
      >
        <ErrorBox error={error} />
        <div className="row" style={{ flexWrap: 'wrap', gap: '0.3rem' }}>
          {paths.map((p) => (
            <button key={p} className={p === file ? 'small active' : 'small ghost'} onClick={() => setFile(p)}>
              <span className="mono">{p}</span>
            </button>
          ))}
        </div>
        {draft && (
          <div className="row" style={{ flexWrap: 'wrap', alignItems: 'flex-end' }}>
            <label className="field grow">
              <span>Add a file - haproxy.cfg, files/NAME (no key), keepalived.conf, bird.conf, firewall.nft</span>
              <input className="mono" value={newPath} onChange={(e) => setNewPath(e.target.value.trim())} placeholder="keepalived.conf" />
            </label>
            <button
              className="small"
              disabled={!newPath || !!pathError(newPath) || draft.files[newPath] !== undefined}
              title={newPath && pathError(newPath) ? pathError(newPath) : undefined}
              onClick={() => {
                setDraft({ ...draft, files: { ...draft.files, [newPath]: '' } })
                setFile(newPath)
                setNewPath('')
              }}
            >
              <Plus size={13} /> Add
            </button>
            {files[file] !== undefined && file !== 'haproxy.cfg' && (
              <button
                className="small danger"
                onClick={() => {
                  const next = { ...draft.files }
                  delete next[file]
                  setDraft({ ...draft, files: next })
                  setFile(Object.keys(next).sort()[0] || '')
                }}
              >
                <Trash2 size={13} /> Remove {file}
              </button>
            )}
          </div>
        )}
        {files[file] === undefined ? (
          <p className="small muted">No file.</p>
        ) : draft ? (
          <Editor label={file} value={files[file]} onChange={(v) => setDraft({ ...draft, files: { ...draft.files, [file]: v } })} />
        ) : (
          <pre className="logview config-view" tabIndex={0} aria-label={file}>
            {files[file]}
          </pre>
        )}
        {draft && (
          <>
            {['keepalived.conf', 'bird.conf', 'firewall.nft'].includes(file) && (
              <p className="small muted">
                A template, rendered for each member: <span className="mono">{'{{ .Member.Name }}'}</span>, <span className="mono">{'{{ .Member.Address }}'}</span>,{' '}
                <span className="mono">{'{{ .Member.Vars.priority }}'}</span>, <span className="mono">{'{{ range .Peers }}{{ .Address }}{{ end }}'}</span>.
              </p>
            )}
            <label className="field">
              <span>Note</span>
              <input value={draft.note} onChange={(e) => setDraft({ ...draft, note: e.target.value })} placeholder="what this version changes" />
            </label>
          </>
        )}
      </Card>
    </div>
  )
}

// --- certificates ---

function Certificates({ cluster, act, busy, reload }) {
  const can = useCan()
  const [name, setName] = useState('')
  const [pem, setPem] = useState('')
  const [crtList, setCrtList] = useState('')
  const [, run] = useAction()
  const upload = (e) => {
    e.preventDefault()
    run(async () => {
      await postJSON(`/api/clusters/${cluster.id}/certificates`, { name, pem, crt_list: crtList.trim() })
      setName('')
      setPem('')
      reload()
    }, `${name} is being uploaded to ${cluster.secrets_from}, then pulled by the others.`)
  }
  return (
    <div className="stack">
      <p className="muted">
        Certificates, their keys, the Let’s Encrypt certificates and the TLS ticket key ring go from {cluster.secrets_from} to the other members, over their own mutual TLS -
        never through this Controller. The ring rotates every day{cluster.tickets_rotated ? ` (last ${when(cluster.tickets_rotated)})` : ''}, and is
        renewed - every key replaced - when a member leaves: it can neither read nor forge the tickets of the members that stay.
      </p>
      {can('admin') && (
        <>
          <Card title="Certificate for every member" icon={Upload}>
            <form className="stack" onSubmit={upload}>
              <div className="row" style={{ flexWrap: 'wrap' }}>
                <label className="field">
                  <span>Name</span>
                  <input className="mono" value={name} onChange={(e) => setName(e.target.value.trim())} placeholder="www" />
                </label>
                <label className="field grow">
                  <span>crt-list to bind it into (optional)</span>
                  <input className="mono" value={crtList} onChange={(e) => setCrtList(e.target.value)} placeholder="/etc/haproxy/files/certs.list" />
                </label>
              </div>
              <label className="field">
                <span>The certificate, its chain and its key (PEM)</span>
                <textarea className="mono small" rows={6} value={pem} onChange={(e) => setPem(e.target.value)} />
              </label>
              <label className="field">
                <span>Or a file</span>
                <input type="file" accept=".pem,.crt,.key" onChange={(e) => e.target.files?.[0]?.text().then(setPem)} />
              </label>
              <p className="small muted">Relayed to {cluster.secrets_from} as a node’s page relays one - never kept here -, then pulled by the others.</p>
              <div className="row">
                <button className="primary" type="submit" disabled={busy || cluster.busy || !name || !pem.includes('PRIVATE KEY')}>
                  <Upload size={15} /> Upload
                </button>
              </div>
            </form>
          </Card>
          <div className="row">
            <button disabled={busy || cluster.busy} onClick={() => act('/pull', 'The members are pulling the secrets again.')}>
              <RefreshCw size={15} /> Pull everything again from {cluster.secrets_from}
            </button>
          </div>
        </>
      )}
    </div>
  )
}

// --- journal ---

function Journal({ cluster }) {
  const [entries, error] = usePolled(`/api/clusters/${cluster.id}/journal?n=200`, () => false)
  if (!entries) return error ? <ErrorBox error={error} /> : <Loading />
  if (entries.length === 0) return <div className="card empty">Nothing yet.</div>
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th>When</th>
            <th>Who</th>
            <th>What</th>
            <th>Member</th>
            <th>Version</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {entries.map((e, i) => (
            <tr key={i}>
              <td className="small">{when(e.time)}</td>
              <td>{e.actor}</td>
              <td>
                <Badge tone={e.error ? 'danger' : e.action === 'drift' ? 'warn' : ''}>{e.action}</Badge>
              </td>
              <td>{e.member || ''}</td>
              <td className="mono">{e.version ? `v${e.version}` : ''}</td>
              <td>
                {e.message}
                {e.error && <div className="small error-text">{e.error}</div>}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
