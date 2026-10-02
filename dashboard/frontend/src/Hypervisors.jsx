import {
  Boxes,
  Copy,
  Cpu,
  Fingerprint,
  HardDrive,
  History,
  KeyRound,
  Loader2,
  MemoryStick,
  Pencil,
  Play,
  Plus,
  PowerOff,
  RotateCcw,
  Server,
  SquareTerminal,
  Trash2,
  X,
} from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { call, gib, postJSON } from './call.js'
import { Badge, Card, ErrorBox, Meter, stateTone, useAction, useConfirm, useToast } from './shared/ui.jsx'

// The hypervisors this Controller creates its own nodes on, and those
// machines (dashboard/backend/hypervisors.go, machines.go,
// docs/hypervisors.md).

function CopyBlock({ label, value, rows = 2 }) {
  const ref = useRef(null)
  const toast = useToast()
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value)
      toast(`${label} copied`)
    } catch {
      ref.current?.select()
      toast(`Select the ${label.toLowerCase()} and press Ctrl+C`, 'warn')
    }
  }
  return (
    <label className="field">
      <span className="spread">
        {label}
        <button type="button" className="small ghost" onClick={copy}>
          <Copy size={13} /> Copy
        </button>
      </span>
      <textarea ref={ref} readOnly rows={rows} value={value} className="mono" />
    </label>
  )
}

// --- hypervisor form (add / edit) ---

const emptyHV = { name: '', host: '', user: 'janus-ctl', pool: 'janus', networks: '', name_prefix: '', socket: '', controller_address: '' }

function toForm(hv) {
  if (!hv) return emptyHV
  const l = hv.libvirt || {}
  return {
    name: hv.name,
    host: l.host || '',
    user: l.user || '',
    pool: l.pool || '',
    networks: (l.networks || []).join(', '),
    name_prefix: l.name_prefix || '',
    socket: l.socket || '',
    controller_address: hv.controller_address || '',
  }
}

function splitList(s) {
  return s
    .split(/[\s,]+/)
    .map((x) => x.trim())
    .filter(Boolean)
}

function HypervisorForm({ hv, onSaved, onClose }) {
  const [f, setF] = useState(() => toForm(hv))
  const [error, setError] = useState(null)
  const [busy, run] = useAction()
  const set = (k) => (e) => setF({ ...f, [k]: e.target.value })
  const submit = async (e) => {
    e.preventDefault()
    setError(null)
    const body = {
      name: f.name,
      kind: 'libvirt',
      controller_address: f.controller_address.trim(),
      libvirt: {
        host: f.host.trim(),
        user: f.user.trim(),
        pool: f.pool.trim(),
        networks: splitList(f.networks),
        name_prefix: f.name_prefix.trim(),
        socket: f.socket.trim(),
      },
    }
    const saved = await run(
      async () => {
        try {
          return await postJSON(hv ? `/api/hypervisors/${hv.id}` : '/api/hypervisors', body, hv ? 'PATCH' : 'POST')
        } catch (err) {
          setError(err)
          throw err
        }
      },
      hv ? `Saved ${f.name}` : `Added ${f.name}`,
    )
    if (saved) onSaved(saved)
  }
  return (
    <Card
      title={hv ? `Edit ${hv.name}` : 'Add a hypervisor'}
      icon={Server}
      actions={
        <button className="ghost icon" onClick={onClose} aria-label="Close">
          <X size={16} />
        </button>
      }
    >
      <form className="stack" onSubmit={submit}>
        <p className="muted small" style={{ margin: 0 }}>
          A libvirt/KVM host, reached over SSH as a dedicated user in its <code>libvirt</code> group. The Controller only ever acts on the virtual machines it
          created, in the pool and on the networks listed here - <code>docs/hypervisors.md</code> sets the host up so that libvirt itself enforces it.
        </p>
        <div className="grid grid-2">
          <label className="field">
            <span>Name</span>
            <input value={f.name} onChange={set('name')} required placeholder="kvm01" />
          </label>
          <label className="field">
            <span>SSH host (host or host:port)</span>
            <input value={f.host} onChange={set('host')} required placeholder="kvm01.example.net" />
          </label>
          <label className="field">
            <span>SSH user</span>
            <input value={f.user} onChange={set('user')} required />
          </label>
          <label className="field">
            <span>Storage pool (type dir)</span>
            <input value={f.pool} onChange={set('pool')} required />
          </label>
          <label className="field">
            <span>Networks machines may use (libvirt networks, comma-separated)</span>
            <input value={f.networks} onChange={set('networks')} required placeholder="lan, dmz" />
          </label>
          <label className="field">
            <span>Virtual machine name prefix (default janus-)</span>
            <input value={f.name_prefix} onChange={set('name_prefix')} placeholder="janus-" />
          </label>
          <label className="field">
            <span>Controller address its machines register at (default: this Controller&apos;s own guess)</span>
            <input value={f.controller_address} onChange={set('controller_address')} placeholder="10.0.0.10:8443" />
          </label>
          <label className="field">
            <span>libvirt socket on the host (default /var/run/libvirt/libvirt-sock)</span>
            <input value={f.socket} onChange={set('socket')} placeholder="/var/run/libvirt/libvirt-sock" />
          </label>
        </div>
        {hv && f.host.trim() !== (hv.libvirt?.host || '') && <div className="notice warn">Another host: its host key will have to be confirmed again.</div>}
        <ErrorBox error={error} />
        <div>
          <button className="primary" type="submit" disabled={busy}>
            {busy ? 'Saving…' : hv ? 'Save' : 'Add hypervisor'}
          </button>
        </div>
      </form>
    </Card>
  )
}

// --- trusting a new hypervisor ---

function TrustSteps({ hv, onTrusted }) {
  const [probe, setProbe] = useState(null)
  const [error, setError] = useState(null)
  const [busy, run] = useAction()
  const readKey = () => {
    setError(null)
    return run(() =>
      postJSON(`/api/hypervisors/${hv.id}/probe`).then(setProbe, (err) => {
        setError(err)
        throw err
      }),
    )
  }
  const trust = () =>
    run(async () => {
      const saved = await postJSON(`/api/hypervisors/${hv.id}/trust`, { fingerprint: probe.fingerprint })
      onTrusted(saved)
    }, `${hv.name} trusted`)
  return (
    <div className="stack trust-steps">
      <div>
        <strong>1. Let the Controller in.</strong>{' '}
        <span className="muted small">
          On the host, add this line to <code>~{hv.libvirt?.user}/.ssh/authorized_keys</code>:
        </span>
      </div>
      <CopyBlock label="Controller public key" value={hv.authorized_key} rows={3} />
      <div>
        <strong>2. Confirm the host is the right one.</strong> <span className="muted small">The Controller never trusts a host key it was simply shown.</span>
      </div>
      {!probe ? (
        <div>
          <button className="small" onClick={readKey} disabled={busy}>
            <KeyRound size={14} /> Read the host key
          </button>
        </div>
      ) : (
        <div className="stack" style={{ gap: '0.4rem' }}>
          <div className="row">
            <Fingerprint size={16} />
            <code className="fingerprint">{probe.fingerprint}</code>
          </div>
          <div className="muted small">
            Compare it with the host&apos;s own, on the host: <code>ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub</code>
          </div>
          <div className="row">
            <button className="primary small" onClick={trust} disabled={busy}>
              It matches - trust this host
            </button>
            <button className="small ghost" onClick={() => setProbe(null)} disabled={busy}>
              Cancel
            </button>
          </div>
        </div>
      )}
      <ErrorBox error={error} />
    </div>
  )
}

// --- hypervisor card ---

function pct(used, total) {
  return total > 0 ? `${Math.round((100 * used) / total)}%` : '–'
}

function HypervisorCard({ hv, status, onEdit, onRemove, onTrusted }) {
  const host = status?.host
  const authError = status?.error && /unable to authenticate|permission denied \(publickey/i.test(status.error)
  let badge = <Badge>checking…</Badge>
  if (!hv.trusted) badge = <Badge tone="warn">to set up</Badge>
  else if (status?.error)
    badge = (
      <Badge tone="danger" dot>
        unreachable
      </Badge>
    )
  else if (host)
    badge = (
      <Badge tone="ok" dot>
        connected
      </Badge>
    )
  return (
    <section className="card node-card">
      <div className="spread" style={{ alignItems: 'flex-start' }}>
        <div style={{ minWidth: 0 }}>
          <div className="node-card-name">{hv.name}</div>
          <div className="muted small mono">
            {hv.libvirt?.user}@{hv.libvirt?.host} · libvirt
          </div>
        </div>
        {badge}
      </div>
      {!hv.trusted && <TrustSteps hv={hv} onTrusted={onTrusted} />}
      {hv.trusted && status?.error && (
        <div className="error-box small">
          {status.error}
          {authError && <div style={{ marginTop: '0.4rem' }}>Add the Controller&apos;s public key to the user&apos;s authorized_keys (Edit shows it).</div>}
        </div>
      )}
      {host && (
        <>
          <dl className="kv small">
            <dt>Host</dt>
            <dd className="mono">{host.hostname}</dd>
            <dt>Software</dt>
            <dd>
              {host.hypervisor_version} · {host.library_version}
            </dd>
            <dt>
              <Cpu size={13} /> CPU
            </dt>
            <dd>
              {host.cpus} × {host.cpu_mhz} MHz{status.cpu_percent >= 0 && ` · ${status.cpu_percent.toFixed(0)}% busy`}
              {status.cpu_percent >= 0 && <Meter value={status.cpu_percent} max={100} />}
            </dd>
            <dt>
              <MemoryStick size={13} /> Memory
            </dt>
            <dd>
              {gib(host.memory_total - host.memory_free)} of {gib(host.memory_total)} used ({pct(host.memory_total - host.memory_free, host.memory_total)})
              <Meter value={host.memory_total - host.memory_free} max={host.memory_total} />
            </dd>
            <dt>
              <HardDrive size={13} /> Pool
            </dt>
            <dd>
              {host.storage.error ? (
                <span style={{ color: 'var(--danger)' }}>
                  {host.storage.name}: {host.storage.error}
                </span>
              ) : (
                <>
                  <span className="mono">{host.storage.name}</span> · {gib(host.storage.available)} free of {gib(host.storage.capacity)}
                  {!host.storage.active && <Badge tone="danger">inactive</Badge>}
                  <Meter value={host.storage.allocated} max={host.storage.capacity} />
                </>
              )}
            </dd>
            <dt>Networks</dt>
            <dd className="row" style={{ gap: '0.3rem', flexWrap: 'wrap' }}>
              {host.networks.map((n) => (
                <Badge key={n.name} tone={n.error ? 'danger' : n.active ? 'ok' : 'warn'}>
                  {n.name}
                  {n.error ? ': not found' : n.active ? '' : ': inactive'}
                </Badge>
              ))}
            </dd>
            <dt>
              <Boxes size={13} /> Machines
            </dt>
            <dd>
              {hv.machines} from this Controller
              {host.running_machines >= 0 && ` · ${host.running_machines} running on the host`}
            </dd>
          </dl>
        </>
      )}
      <div className="row" style={{ marginTop: 'auto' }}>
        <button className="small" onClick={() => onEdit(hv)}>
          <Pencil size={14} /> Edit
        </button>
        <span className="grow" />
        <button
          className="ghost small danger"
          onClick={() => onRemove(hv)}
          disabled={hv.machines > 0}
          title={hv.machines > 0 ? 'Destroy its machines first' : 'Forget this hypervisor'}
        >
          <Trash2 size={14} /> Remove
        </button>
      </div>
    </section>
  )
}

// --- creating a node ---

const MODES = [
  { id: 'static', label: 'Static' },
  { id: 'dhcp', label: 'DHCP' },
  { id: 'none', label: 'Up, no address' },
]

function newNIC(hv, i) {
  return { network: hv?.libvirt?.networks?.[0] || '', name: `eth${i}`, mode: 'static', address: '', gateway: '' }
}

function CreateMachineForm({ hypervisors, onCreated, onClose }) {
  const usable = hypervisors.filter((h) => h.trusted)
  const [hvId, setHvId] = useState(usable[0]?.id || '')
  const hv = usable.find((h) => h.id === hvId)
  const [name, setName] = useState('')
  const [vcpus, setVcpus] = useState(2)
  const [memory, setMemory] = useState(1024)
  const [version, setVersion] = useState('')
  const [exts, setExts] = useState([])
  const [nics, setNics] = useState(() => [newNIC(hv, 0)])
  const [dns, setDns] = useState('')
  const [ntp, setNtp] = useState('')
  const [imageURL, setImageURL] = useState('')
  const [imageSHA, setImageSHA] = useState('')
  const [catalog, setCatalog] = useState(null)
  const [error, setError] = useState(null)
  const [busy, run] = useAction()

  useEffect(() => {
    call('/api/catalog').then(setCatalog, () => setCatalog({}))
  }, [])

  const setNIC = (i, patch) => setNics(nics.map((n, j) => (j === i ? { ...n, ...patch } : n)))
  const toggleExt = (e) => setExts(exts.includes(e) ? exts.filter((x) => x !== e) : [...exts, e])
  const pickHV = (id) => {
    setHvId(id)
    const next = usable.find((h) => h.id === id)
    setNics(nics.map((n) => (next?.libvirt?.networks?.includes(n.network) ? n : { ...n, network: next?.libvirt?.networks?.[0] || '' })))
  }

  const submit = async (e) => {
    e.preventDefault()
    setError(null)
    const spec = {
      name: name.trim(),
      hypervisor_id: hvId,
      vcpus: Number(vcpus),
      memory_mib: Number(memory),
      nics: nics.map((n) => ({
        network: n.network,
        name: n.name.trim(),
        mode: n.mode,
        ...(n.mode === 'static' ? { addresses: splitList(n.address), ...(n.gateway.trim() ? { gateway: n.gateway.trim() } : {}) } : {}),
      })),
      ...(splitList(dns).length ? { dns: splitList(dns) } : {}),
      ...(splitList(ntp).length ? { ntp: splitList(ntp) } : {}),
    }
    if (imageURL.trim()) spec.image = { url: imageURL.trim(), sha256: imageSHA.trim() }
    else {
      if (version.trim()) spec.version = version.trim()
      if (exts.length) spec.extensions = exts
    }
    const created = await run(async () => {
      try {
        return await postJSON('/api/machines', spec)
      } catch (err) {
        setError(err)
        throw err
      }
    }, `Creating ${spec.name}`)
    if (created) onCreated(created)
  }

  const extensions = catalog?.extensions || []
  return (
    <Card
      title="Create a node"
      icon={Plus}
      actions={
        <button className="ghost icon" onClick={onClose} aria-label="Close">
          <X size={16} />
        </button>
      }
    >
      <form className="stack" onSubmit={submit}>
        <p className="muted small" style={{ margin: 0 }}>
          The Controller creates the virtual machine, boots it with its address and a one-time token, and admits the node as soon as it registers - no approval
          step for a machine it created itself.
        </p>
        <div className="grid grid-4">
          <label className="field">
            <span>Hypervisor</span>
            <select value={hvId} onChange={(e) => pickHV(e.target.value)} required>
              {usable.map((h) => (
                <option key={h.id} value={h.id}>
                  {h.name}
                </option>
              ))}
            </select>
          </label>
          <label className="field">
            <span>Name (the node&apos;s hostname)</span>
            <input value={name} onChange={(e) => setName(e.target.value)} required placeholder="lb1" />
          </label>
          <label className="field">
            <span>vCPUs</span>
            <input type="number" min={1} max={64} value={vcpus} onChange={(e) => setVcpus(e.target.value)} required />
          </label>
          <label className="field">
            <span>Memory (MiB)</span>
            <input type="number" min={512} step={256} value={memory} onChange={(e) => setMemory(e.target.value)} required />
          </label>
        </div>

        <div className="field">
          <span className="field-label">Network interfaces</span>
          <div className="stack" style={{ gap: '0.5rem' }}>
            {nics.map((n, i) => (
              <div key={i} className="nic-row">
                <select value={n.network} onChange={(e) => setNIC(i, { network: e.target.value })} aria-label="Network">
                  {(hv?.libvirt?.networks || []).map((net) => (
                    <option key={net} value={net}>
                      {net}
                    </option>
                  ))}
                </select>
                <input value={n.name} onChange={(e) => setNIC(i, { name: e.target.value })} aria-label="Interface name" placeholder={`eth${i}`} />
                <select value={n.mode} onChange={(e) => setNIC(i, { mode: e.target.value })} aria-label="Addressing">
                  {MODES.map((m) => (
                    <option key={m.id} value={m.id}>
                      {m.label}
                    </option>
                  ))}
                </select>
                <input
                  value={n.address}
                  onChange={(e) => setNIC(i, { address: e.target.value })}
                  placeholder="192.0.2.10/24"
                  aria-label="Address"
                  disabled={n.mode !== 'static'}
                  required={n.mode === 'static'}
                />
                <input
                  value={n.gateway}
                  onChange={(e) => setNIC(i, { gateway: e.target.value })}
                  placeholder="gateway (optional)"
                  aria-label="Gateway"
                  disabled={n.mode !== 'static'}
                />
                <button
                  type="button"
                  className="ghost icon"
                  onClick={() => setNics(nics.filter((_, j) => j !== i))}
                  disabled={nics.length === 1}
                  aria-label="Remove interface"
                >
                  <X size={15} />
                </button>
              </div>
            ))}
          </div>
          <div className="row">
            <button type="button" className="small ghost" onClick={() => setNics([...nics, newNIC(hv, nics.length)])} disabled={nics.length >= 8}>
              <Plus size={14} /> Interface
            </button>
            <span className="muted small">
              Interfaces are matched by their MAC address and renamed. Prefer static addresses: the kernel&apos;s DHCP lease is taken once at boot and never
              renewed.
            </span>
          </div>
        </div>
        <div className="grid grid-2">
          <label className="field">
            <span>DNS servers (optional)</span>
            <input value={dns} onChange={(e) => setDns(e.target.value)} placeholder="192.0.2.53" />
          </label>
          <label className="field">
            <span>NTP servers (optional, at most two)</span>
            <input value={ntp} onChange={(e) => setNtp(e.target.value)} placeholder="pool.ntp.org" />
          </label>
        </div>

        <div className="grid grid-2">
          <label className="field">
            <span>Janus version</span>
            <input
              value={version}
              onChange={(e) => setVersion(e.target.value)}
              placeholder={catalog?.latest ? `${catalog.latest} (the newest)` : 'the newest release'}
              disabled={!!imageURL.trim()}
            />
          </label>
          <div className="field">
            <span className="field-label">Extensions {exts.length > 0 && <span className="muted">· built by the image factory</span>}</span>
            {catalog === null ? (
              <span className="muted small">
                <Loader2 size={13} className="spin" /> loading the catalog…
              </span>
            ) : extensions.length === 0 ? (
              <span className="muted small">{catalog.catalog_error || 'none offered'}</span>
            ) : (
              <div className="ext-list">
                {extensions.map((e) => (
                  <label key={e.name} className="check" title={e.description}>
                    <input type="checkbox" checked={exts.includes(e.name)} onChange={() => toggleExt(e.name)} disabled={!!imageURL.trim()} /> {e.name}
                  </label>
                ))}
              </div>
            )}
          </div>
        </div>
        <details>
          <summary className="small">Another image (a mirror, a development build)</summary>
          <div className="grid grid-2" style={{ marginTop: '0.5rem' }}>
            <label className="field">
              <span>qcow2 image URL (replaces version and extensions)</span>
              <input value={imageURL} onChange={(e) => setImageURL(e.target.value)} placeholder="https://mirror.example.net/janus-kvm.qcow2" />
            </label>
            <label className="field">
              <span>Its SHA-256</span>
              <input value={imageSHA} onChange={(e) => setImageSHA(e.target.value)} className="mono" required={!!imageURL.trim()} />
            </label>
          </div>
        </details>
        <ErrorBox error={error} />
        <div>
          <button className="primary" type="submit" disabled={busy || !hv}>
            {busy ? 'Creating…' : 'Create node'}
          </button>
        </div>
      </form>
    </Card>
  )
}

// --- machines ---

const PHASE_LABEL = {
  pending: 'starting',
  'preparing-image': 'preparing the image',
  creating: 'creating',
  'waiting-registration': 'booting',
  ready: 'ready',
  failed: 'failed',
  destroying: 'destroying',
}

export function phaseTone(phase) {
  if (phase === 'ready') return 'ok'
  if (phase === 'failed') return 'danger'
  if (phase === 'destroying') return 'warn'
  return 'info'
}

export function PhaseBadge({ phase }) {
  const busy = !['ready', 'failed'].includes(phase)
  return (
    <Badge tone={phaseTone(phase)}>
      {busy && <Loader2 size={11} className="spin" />} {PHASE_LABEL[phase] || phase}
    </Badge>
  )
}

export function PowerBadge({ power }) {
  if (!power) return null
  return (
    <Badge tone={stateTone(power === 'off' ? 'stopped' : power)} dot>
      VM {power}
    </Badge>
  )
}

// useMachineActions: power, console, retry and destroy - shared by the
// machine cards and the node cards of machines.
export function useMachineActions({ onChanged, onConsole }) {
  const confirm = useConfirm()
  const [busy, run] = useAction()
  const power = async (m, action) => {
    const what = {
      reset: ['Reset', 'The virtual machine restarts at once, like pressing its reset button: whatever it was serving is cut off until it is back.'],
      'force-off': ['Force off', 'The virtual machine stops at once, like pulling its plug. For a clean stop, use Power on the node’s own page.'],
    }[action]
    if (what) {
      const ok = await confirm({ title: `${what[0]} ${m.spec.name}?`, body: <p>{what[1]}</p>, action: what[0], danger: true })
      if (!ok) return
    }
    await run(() => postJSON(`/api/machines/${m.id}/power`, { action }), `${m.spec.name}: ${action}`)
    onChanged()
  }
  const destroy = async (m) => {
    const ok = await confirm({
      title: `Destroy ${m.spec.name}?`,
      body: (
        <p>
          {m.node_id ? 'The node is shut down cleanly, then its' : 'Its'} virtual machine and disks are deleted from {m.hypervisor_name || 'its hypervisor'},
          and the Controller forgets the node. This can&apos;t be undone.
        </p>
      ),
      action: 'Destroy',
      danger: true,
      typeToConfirm: m.spec.name,
    })
    if (!ok) return
    await run(() => call(`/api/machines/${m.id}`, { method: 'DELETE' }), `Destroying ${m.spec.name}`)
    onChanged()
  }
  const retry = async (m) => {
    await run(() => postJSON(`/api/machines/${m.id}/retry`), `Retrying ${m.spec.name}`)
    onChanged()
  }
  return { busy, power, destroy, retry, console: onConsole }
}

export function PowerButtons({ machine, vm, actions }) {
  if (!machine.vm_uuid || machine.phase === 'destroying') return null
  const off = vm?.power === 'off'
  return (
    <>
      {off ? (
        <button className="small" onClick={() => actions.power(machine, 'start')} disabled={actions.busy} title="Start the virtual machine">
          <Play size={14} /> Start
        </button>
      ) : (
        <>
          <button
            className="small ghost"
            onClick={() => actions.power(machine, 'reset')}
            disabled={actions.busy}
            title="Reset the virtual machine (hypervisor)"
          >
            <RotateCcw size={14} />
          </button>
          <button
            className="small ghost"
            onClick={() => actions.power(machine, 'force-off')}
            disabled={actions.busy}
            title="Force the virtual machine off (hypervisor)"
          >
            <PowerOff size={14} />
          </button>
        </>
      )}
      <button className="small ghost" onClick={() => actions.console(machine)} title="Serial console">
        <SquareTerminal size={14} />
      </button>
    </>
  )
}

function MachineCard({ m, vm, vmError, actions }) {
  const [history, setHistory] = useState(false)
  const last = m.events[m.events.length - 1]
  return (
    <section className="card node-card">
      <div className="spread" style={{ alignItems: 'flex-start' }}>
        <div style={{ minWidth: 0 }}>
          <div className="node-card-name">{m.spec.name}</div>
          <div className="muted small mono">
            {m.vm_name || '…'} · {m.hypervisor_name}
          </div>
        </div>
        <div className="row" style={{ gap: '0.3rem' }}>
          <PowerBadge power={vm?.power} />
          <PhaseBadge phase={m.phase} />
        </div>
      </div>
      {m.error && <div className="error-box small">{m.error}</div>}
      {vmError && <div className="error-box small">{vmError}</div>}
      <dl className="kv small">
        <dt>Size</dt>
        <dd>
          {m.spec.vcpus} vCPU · {m.spec.memory_mib} MiB
        </dd>
        <dt>Image</dt>
        <dd className="mono">
          {m.version ? `${m.version}${m.schematic ? ` · ${m.schematic.slice(0, 8)}` : ''}` : m.spec.image ? 'custom image' : m.spec.version || 'newest release'}
        </dd>
        <dt>Network</dt>
        <dd className="mono">
          {m.spec.nics.map((n) => (
            <div key={n.mac}>
              {n.name} · {n.network} · {n.mode === 'static' ? (n.addresses || []).join(' ') : n.mode}
            </div>
          ))}
        </dd>
        <dt>Node</dt>
        <dd className="mono">{m.node_address || <span className="muted">not registered yet</span>}</dd>
      </dl>
      {last && !history && (
        <div className="muted small">
          {new Date(last.time).toLocaleTimeString()} · {last.message}
        </div>
      )}
      {history && <pre className="logview machine-events">{m.events.map((e) => `${new Date(e.time).toLocaleTimeString()}  ${e.message}`).join('\n')}</pre>}
      <div className="row" style={{ marginTop: 'auto' }}>
        <PowerButtons machine={m} vm={vm} actions={actions} />
        <button className="small ghost" onClick={() => setHistory(!history)} title="History">
          <History size={14} />
        </button>
        {m.phase === 'failed' && !m.node_id && (
          <button className="small" onClick={() => actions.retry(m)} disabled={actions.busy}>
            <RotateCcw size={14} /> Retry
          </button>
        )}
        <span className="grow" />
        {m.phase !== 'destroying' && (
          <button className="ghost small danger" onClick={() => actions.destroy(m)} disabled={actions.busy}>
            <Trash2 size={14} /> Destroy
          </button>
        )}
      </div>
    </section>
  )
}

// --- the page ---

export default function HypervisorsPage({ hypervisors, machines, hvStatus, onChanged, onConsole }) {
  const [form, setForm] = useState(null) // null, 'add', or a hypervisor to edit
  const [creating, setCreating] = useState(false)
  const confirm = useConfirm()
  const [, run] = useAction()
  const actions = useMachineActions({ onChanged, onConsole })
  const remove = async (hv) => {
    const ok = await confirm({
      title: `Remove ${hv.name}?`,
      body: <p>The Controller forgets this hypervisor and its SSH key. Nothing changes on the host: remove the key from authorized_keys there too.</p>,
      action: 'Remove',
      danger: true,
    })
    if (ok) {
      await run(() => call(`/api/hypervisors/${hv.id}`, { method: 'DELETE' }), `Removed ${hv.name}`)
      onChanged()
    }
  }
  const trusted = hypervisors.filter((h) => h.trusted)
  const vmOf = (m) => hvStatus[m.spec.hypervisor_id]?.machines?.[m.id]
  const vmErrorOf = (m) => hvStatus[m.spec.hypervisor_id]?.machine_errors?.[m.id]

  return (
    <div className="stack">
      <div className="spread">
        <h1>Hypervisors</h1>
        {!form && (
          <button className="primary" onClick={() => setForm('add')}>
            <Plus size={15} /> Add hypervisor
          </button>
        )}
      </div>
      {form && (
        <HypervisorForm
          key={form === 'add' ? 'add' : form.id}
          hv={form === 'add' ? null : form}
          onSaved={() => {
            setForm(null)
            onChanged()
          }}
          onClose={() => setForm(null)}
        />
      )}
      {hypervisors.length === 0 && !form && (
        <div className="card empty">
          No hypervisor yet.{' '}
          <button className="small" onClick={() => setForm('add')}>
            Add one
          </button>{' '}
          - a libvirt/KVM host the Controller creates its nodes on.
        </div>
      )}
      <div className="node-grid">
        {hypervisors.map((hv) => (
          <HypervisorCard key={hv.id} hv={hv} status={hvStatus[hv.id]} onEdit={setForm} onRemove={remove} onTrusted={onChanged} />
        ))}
      </div>

      <div className="spread" style={{ marginTop: '0.6rem' }}>
        <h1>Machines</h1>
        {!creating && (
          <button
            className="primary"
            onClick={() => setCreating(true)}
            disabled={trusted.length === 0}
            title={trusted.length ? '' : 'Add and trust a hypervisor first'}
          >
            <Plus size={15} /> Create node
          </button>
        )}
      </div>
      {creating && (
        <CreateMachineForm
          hypervisors={hypervisors}
          onCreated={() => {
            setCreating(false)
            onChanged()
          }}
          onClose={() => setCreating(false)}
        />
      )}
      {machines.length === 0 && !creating && <div className="card empty">No machine yet: the nodes this Controller creates appear here.</div>}
      <div className="node-grid">
        {machines.map((m) => (
          <MachineCard key={m.id} m={m} vm={vmOf(m)} vmError={vmErrorOf(m)} actions={actions} />
        ))}
      </div>
    </div>
  )
}
