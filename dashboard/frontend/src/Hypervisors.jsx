import {
  Boxes,
  Code,
  Copy,
  Cpu,
  Download,
  Fingerprint,
  HardDrive,
  History,
  KeyRound,
  ListChecks,
  Loader2,
  Lock,
  LockOpen,
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

// useCopy copies text to the clipboard, saying so; onFail selects it
// for a Ctrl+C when the clipboard refuses.
function useCopy() {
  const toast = useToast()
  return async (text, label, onFail) => {
    try {
      await navigator.clipboard.writeText(text)
      toast(`${label} copied`)
    } catch {
      onFail?.()
      toast(`Select the ${label.toLowerCase()} and press Ctrl+C`, 'warn')
    }
  }
}

function CopyBlock({ label, value, rows = 2 }) {
  const ref = useRef(null)
  const copyText = useCopy()
  const copy = () => copyText(value, label, () => ref.current?.select())
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

const emptyHV = {
  kind: 'libvirt',
  name: '',
  controller_address: '',
  networks: '',
  name_prefix: '',
  // libvirt
  host: '',
  user: 'janus-ctl',
  pool: 'janus',
  socket: '',
  // Proxmox
  url: '',
  node: '',
  token_id: 'janus-ctl@pve!controller',
  token_secret: '',
  pve_pool: 'janus',
  storage: 'local-lvm',
  image_storage: 'janus-images',
  vmids: '',
  ca_cert: '',
}

function toForm(hv) {
  if (!hv) return emptyHV
  const l = hv.libvirt || {}
  const p = hv.proxmox || {}
  return {
    ...emptyHV,
    kind: hv.kind,
    name: hv.name,
    controller_address: hv.controller_address || '',
    networks: (l.networks || p.networks || []).join(', '),
    name_prefix: l.name_prefix || p.name_prefix || '',
    host: l.host || '',
    user: l.user || '',
    pool: l.pool || '',
    socket: l.socket || '',
    url: p.url || '',
    node: p.node || '',
    token_id: p.token_id || '',
    pve_pool: p.pool || '',
    storage: p.storage || '',
    image_storage: p.image_storage || '',
    vmids: p.vmids || '',
    ca_cert: p.ca_cert || '',
  }
}

function splitList(s) {
  return s
    .split(/[\s,]+/)
    .map((x) => x.trim())
    .filter(Boolean)
}

// hvNetworks: the networks a hypervisor's machines may use, any kind.
export function hvNetworks(hv) {
  return hv?.libvirt?.networks || hv?.proxmox?.networks || []
}

function hvBody(f) {
  const common = { name: f.name, kind: f.kind, controller_address: f.controller_address.trim() }
  if (f.kind === 'proxmox') {
    return {
      ...common,
      ...(f.token_secret.trim() ? { token_secret: f.token_secret.trim() } : {}),
      proxmox: {
        url: f.url.trim(),
        node: f.node.trim(),
        token_id: f.token_id.trim(),
        pool: f.pve_pool.trim(),
        storage: f.storage.trim(),
        image_storage: f.image_storage.trim(),
        networks: splitList(f.networks),
        name_prefix: f.name_prefix.trim(),
        vmids: f.vmids.trim(),
        ca_cert: f.ca_cert.trim(),
      },
    }
  }
  return {
    ...common,
    libvirt: {
      host: f.host.trim(),
      user: f.user.trim(),
      pool: f.pool.trim(),
      networks: splitList(f.networks),
      name_prefix: f.name_prefix.trim(),
      socket: f.socket.trim(),
    },
  }
}

// What a host's preparation names, per kind: changed, the host needs
// preparing again. And what the preparation needs.
const PREPARED = { libvirt: ['user', 'pool', 'networks', 'name_prefix'], proxmox: ['token_id', 'pve_pool', 'storage', 'image_storage', 'networks', 'node'] }
const NEEDED = { libvirt: ['name', 'host', 'user', 'pool'], proxmox: ['name', 'url', 'node', 'token_id', 'pve_pool', 'storage', 'image_storage'] }

function Field({ label, children }) {
  return (
    <label className="field">
      <span>{label}</span>
      {children}
    </label>
  )
}

function HypervisorForm({ hv, onSaved, onClose }) {
  const [f, setF] = useState(() => toForm(hv))
  const [error, setError] = useState(null)
  const [prep, setPrep] = useState(false)
  const [busy, run] = useAction()
  const set = (k) => (e) => setF({ ...f, [k]: e.target.value })
  const pve = f.kind === 'proxmox'
  const ready = NEEDED[f.kind].every((k) => f[k].trim()) && splitList(f.networks).length > 0
  const saved0 = toForm(hv)
  const reprepare = hv && PREPARED[f.kind].some((k) => (k === 'networks' ? splitList(f[k]).join() !== splitList(saved0[k]).join() : f[k].trim() !== saved0[k]))
  const submit = async (e) => {
    e.preventDefault()
    setError(null)
    const body = hvBody(f)
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
  const missing = pve
    ? 'Fill in the name, API URL, node, token ID, pool, storages and networks first'
    : 'Fill in the name, SSH host and user, pool and networks first'
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
        {!hv && (
          <Field label="Kind">
            <select
              value={f.kind}
              onChange={(e) => {
                setPrep(false)
                setF({ ...f, kind: e.target.value })
              }}
            >
              <option value="libvirt">libvirt / KVM - over SSH</option>
              <option value="proxmox">Proxmox VE - its API, with a token</option>
            </select>
          </Field>
        )}
        <p className="muted small" style={{ margin: 0 }}>
          {pve ? (
            <>
              A Proxmox VE node, through its API with a token whose rights cover one pool, two storages and the networks listed here - nothing else, not even
              seeing other virtual machines. <strong>Show host preparation</strong> sets it up, the token included.
            </>
          ) : (
            <>
              A libvirt/KVM host, reached over SSH as a dedicated user in its <code>libvirt</code> group. The Controller only ever acts on the virtual machines
              it created, in the pool and on the networks listed here - the host&apos;s preparation makes libvirt itself enforce it.
            </>
          )}
        </p>
        <div className="grid grid-2">
          <Field label="Name">
            <input value={f.name} onChange={set('name')} required placeholder={pve ? 'pve01' : 'kvm01'} />
          </Field>
          {pve ? (
            <>
              <Field label="API URL">
                <input value={f.url} onChange={set('url')} required placeholder="https://pve01.example.net:8006" />
              </Field>
              <Field label="Node machines are created on">
                <input value={f.node} onChange={set('node')} required placeholder="pve01" />
              </Field>
              <Field label="API token (user@realm!name)">
                <input value={f.token_id} onChange={set('token_id')} required />
              </Field>
              <Field label={hv?.has_token_secret ? 'Token secret (kept - give one to replace it)' : 'Token secret (step 4 of the preparation shows it)'}>
                <input
                  type="password"
                  autoComplete="off"
                  value={f.token_secret}
                  onChange={set('token_secret')}
                  required={!hv}
                  placeholder={hv?.has_token_secret ? '••••••••' : 'xxxxxxxx-xxxx-…'}
                />
              </Field>
              <Field label="Pool">
                <input value={f.pve_pool} onChange={set('pve_pool')} required />
              </Field>
              <Field label="Storage for the disks">
                <input value={f.storage} onChange={set('storage')} required />
              </Field>
              <Field label="Storage for the images (a directory, import and iso)">
                <input value={f.image_storage} onChange={set('image_storage')} required />
              </Field>
              <Field label="Networks machines may use (bridge, or bridge.vlan; comma-separated)">
                <input value={f.networks} onChange={set('networks')} required placeholder="vmbr0.10, vmbr0.20" />
              </Field>
              <Field label="Virtual machine name prefix (default janus-)">
                <input value={f.name_prefix} onChange={set('name_prefix')} placeholder="janus-" />
              </Field>
              <Field label="VM IDs (first-last; default: the cluster's next free one)">
                <input value={f.vmids} onChange={set('vmids')} placeholder="9000-9099" />
              </Field>
              <Field label="Controller address its machines register at (default: this Controller's own guess)">
                <input value={f.controller_address} onChange={set('controller_address')} placeholder="10.0.0.10:8443" />
              </Field>
            </>
          ) : (
            <>
              <Field label="SSH host (host or host:port)">
                <input value={f.host} onChange={set('host')} required placeholder="kvm01.example.net" />
              </Field>
              <Field label="SSH user">
                <input value={f.user} onChange={set('user')} required />
              </Field>
              <Field label="Storage pool (type dir)">
                <input value={f.pool} onChange={set('pool')} required />
              </Field>
              <Field label="Networks machines may use (libvirt networks, comma-separated)">
                <input value={f.networks} onChange={set('networks')} required placeholder="lan, dmz" />
              </Field>
              <Field label="Virtual machine name prefix (default janus-)">
                <input value={f.name_prefix} onChange={set('name_prefix')} placeholder="janus-" />
              </Field>
              <Field label="Controller address its machines register at (default: this Controller's own guess)">
                <input value={f.controller_address} onChange={set('controller_address')} placeholder="10.0.0.10:8443" />
              </Field>
              <Field label="libvirt socket on the host (default /var/run/libvirt/libvirt-sock)">
                <input value={f.socket} onChange={set('socket')} placeholder="/var/run/libvirt/libvirt-sock" />
              </Field>
            </>
          )}
        </div>
        {pve && (
          <Field label="CA certificate of the API (optional, PEM) - a certificate renewed now and then stays trusted; without it, you confirm the certificate's fingerprint once added">
            <textarea rows={3} className="mono" value={f.ca_cert} onChange={set('ca_cert')} placeholder="-----BEGIN CERTIFICATE-----" />
          </Field>
        )}
        {hv && !pve && f.host.trim() !== (hv.libvirt?.host || '') && (
          <div className="notice warn">Another host: its host key will have to be confirmed again.</div>
        )}
        {hv && pve && f.url.trim() !== (hv.proxmox?.url || '') && !f.ca_cert.trim() && (
          <div className="notice warn">Another address: its certificate will have to be confirmed again.</div>
        )}
        {reprepare && (
          <div className="notice warn">
            {pve
              ? "The node's pool, storages, roles and rights name the token's user, pool, storages and networks: prepare it again with the new ones."
              : "The host's sshd, nftables and polkit files name the account, pool, networks and prefix: prepare the host again with the new ones."}
          </div>
        )}
        <ErrorBox error={error} />
        <div className="row">
          <button className="primary" type="submit" disabled={busy}>
            {busy ? 'Saving…' : hv ? 'Save' : 'Add hypervisor'}
          </button>
          <button
            type="button"
            onClick={() => setPrep(!prep)}
            disabled={!ready && !prep}
            aria-expanded={prep}
            title={ready ? 'What to run on the host, with these settings' : missing}
          >
            <ListChecks size={15} /> {prep ? 'Hide host preparation' : 'Show host preparation'}
          </button>
        </div>
      </form>
      {prep && ready && <HostPrep body={{ ...hvBody(f), ...(hv ? { id: hv.id } : {}) }} />}
    </Card>
  )
}

// --- preparing the host ---

// HostPrep shows what to run on the host, as root, for these settings -
// the Controller writes it (POST /api/hypervisors/preparation), again on
// every change to them.
function HostPrep({ body }) {
  const [prep, setPrep] = useState(null)
  const [error, setError] = useState(null)
  const copy = useCopy()
  const key = JSON.stringify(body)
  useEffect(() => {
    let live = true
    const t = setTimeout(() => {
      postJSON('/api/hypervisors/preparation', JSON.parse(key)).then(
        (p) => {
          if (!live) return
          setPrep(p)
          setError(null)
        },
        (err) => {
          if (live) setError(err)
        },
      )
    }, 250)
    return () => {
      live = false
      clearTimeout(t)
    }
  }, [key])
  const file = `janus-${(body.name || 'hypervisor').replace(/[^A-Za-z0-9_.-]+/g, '-')}-host.sh`
  const download = () => {
    const url = URL.createObjectURL(new Blob([prep.script], { type: 'text/x-shellscript' }))
    const a = document.createElement('a')
    a.href = url
    a.download = file
    a.click()
    setTimeout(() => URL.revokeObjectURL(url), 1000)
  }
  if (error) return <ErrorBox error={error} />
  if (!prep) return <div className="muted small">Writing the host preparation…</div>
  return (
    <div className="stack host-prep">
      <div className="spread" style={{ alignItems: 'flex-start', gap: '0.75rem' }}>
        <div>
          <strong>Preparing {body.proxmox ? `node ${body.proxmox.node}` : body.libvirt.host}</strong>
          <div className="muted small">
            As root on the {body.proxmox ? 'node' : 'host'}: run <code>sh {file}</code>, or paste it into <code>sudo sh</code>. Each step can also run alone,
            and running it again is harmless.
          </div>
        </div>
        <div className="row" style={{ flexShrink: 0 }}>
          <button type="button" className="small" onClick={() => copy(prep.script, 'Host preparation')}>
            <Copy size={14} /> Copy all
          </button>
          <button type="button" className="small" onClick={download} title={`Save it as ${file}`}>
            <Download size={14} /> Download
          </button>
        </div>
      </div>
      {body.proxmox && (
        <div className="notice small">
          Step 4 shows the API token&apos;s secret, once: paste it in the form as the token secret. The Controller keeps it and never shows it again.
        </div>
      )}
      {!prep.has_key && !body.proxmox && (
        <div className="notice small">
          The Controller&apos;s SSH key isn&apos;t in it yet: it&apos;s made when the hypervisor is added. Its card then gives the command that authorizes it -
          or this preparation again, key included.
        </div>
      )}
      <ol className="prep-steps">
        {prep.steps.map((s, i) => (
          <li key={i}>
            <div className="spread">
              <strong>{s.title}</strong>
              <button type="button" className="small ghost" onClick={() => copy(s.script, `Step ${i + 1}`)}>
                <Copy size={13} /> Copy
              </button>
            </div>
            <p className="muted small">{s.about}</p>
            <pre className="logview prep-script">{s.script}</pre>
          </li>
        ))}
      </ol>
    </div>
  )
}

// --- trusting a new hypervisor ---

function PrepModal({ hv, onClose }) {
  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal card wide host-prep-modal" role="dialog" aria-modal="true" onClick={(e) => e.stopPropagation()}>
        <div className="card-header">
          <div className="card-title">
            <ListChecks size={16} /> {hv.name}: preparing the host
          </div>
          <button className="ghost icon" onClick={onClose} aria-label="Close">
            <X size={16} />
          </button>
        </div>
        <HostPrep body={{ ...hvBody(toForm(hv)), id: hv.id }} />
      </div>
    </div>
  )
}

function TrustSteps({ hv, onTrusted }) {
  const [probe, setProbe] = useState(null)
  const [prep, setPrep] = useState(false)
  const pve = hv.kind === 'proxmox'
  const user = hv.libvirt?.user
  const keyFile = `~${user}/.ssh/authorized_keys`
  const authorize = `grep -qxF '${hv.authorized_key}' ${keyFile} || echo '${hv.authorized_key}' >> ${keyFile}`
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
  const prepButton = (
    <div className="row">
      <button type="button" className="small ghost" onClick={() => setPrep(!prep)} aria-expanded={prep}>
        <ListChecks size={14} /> Host not prepared yet? Show host preparation
      </button>
    </div>
  )
  return (
    <div className="stack trust-steps">
      {prep && <PrepModal hv={hv} onClose={() => setPrep(false)} />}
      {pve ? (
        prepButton
      ) : (
        <>
          <div>
            <strong>1. Let the Controller in.</strong>{' '}
            <span className="muted small">
              On the host, as root - it adds the Controller&apos;s key to <code>{keyFile}</code>:
            </span>
          </div>
          <CopyBlock label="Command" value={authorize} rows={4} />
          {prepButton}
        </>
      )}
      <div>
        <strong>{pve ? 'Confirm the API is the right one.' : '2. Confirm the host is the right one.'}</strong>{' '}
        <span className="muted small">The Controller never trusts a {pve ? 'certificate' : 'host key'} it was simply shown.</span>
      </div>
      {!probe ? (
        <div>
          <button className="small" onClick={readKey} disabled={busy}>
            <KeyRound size={14} /> {pve ? 'Read the certificate' : 'Read the host key'}
          </button>
        </div>
      ) : (
        <div className="stack" style={{ gap: '0.4rem' }}>
          <div className="row">
            <Fingerprint size={16} />
            <code className="fingerprint">{probe.fingerprint}</code>
          </div>
          {probe.subject && (
            <div className="muted small mono" style={{ overflowWrap: 'anywhere' }}>
              {probe.subject}
              <br />
              issued by {probe.issuer}
            </div>
          )}
          <div className="muted small">
            Compare it with the {pve ? "node's own, on the node" : "host's own, on the host"}:{' '}
            <code>
              {pve ? 'openssl x509 -noout -fingerprint -sha256 -in /etc/pve/local/pveproxy-ssl.pem' : 'ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub'}
            </code>
            {pve && " (pve-ssl.pem without a certificate of your own) - the preparation's last step prints it."}
          </div>
          <div className="row">
            <button className="primary small" onClick={trust} disabled={busy}>
              It matches - trust this {pve ? 'certificate' : 'host'}
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
  const authError = status?.error && /unable to authenticate|permission denied \(publickey|401|authentication failure|invalid token/i.test(status.error)
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
            {hv.kind === 'proxmox'
              ? `${hv.proxmox?.node} · ${hv.proxmox?.token_id?.split('!')[0]} · Proxmox VE`
              : `${hv.libvirt?.user}@${hv.libvirt?.host} · libvirt`}
          </div>
        </div>
        {badge}
      </div>
      {!hv.trusted && <TrustSteps hv={hv} onTrusted={onTrusted} />}
      {hv.trusted && status?.error && (
        <div className="error-box small">
          {status.error}
          {authError && (
            <div style={{ marginTop: '0.4rem' }}>
              {hv.kind === 'proxmox'
                ? "Check the API token's ID and secret (Edit)."
                : "Add the Controller's public key to the user's authorized_keys (Edit shows it)."}
            </div>
          )}
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
              <HardDrive size={13} /> {hv.kind === 'proxmox' ? 'Storage' : 'Pool'}
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
              {host.running_machines >= 0 && ` · ${host.running_machines} running that it may see`}
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
  return { network: hvNetworks(hv)[0] || '', name: `eth${i}`, mode: 'static', address: '', gateway: '' }
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
    setNics(nics.map((n) => (hvNetworks(next).includes(n.network) ? n : { ...n, network: hvNetworks(next)[0] || '' })))
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
                  {hvNetworks(hv).map((net) => (
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
  updating: 'updating',
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
  const release = async (m) => {
    const by = m.spec.managed_by || 'code'
    const ok = await confirm({
      title: `Release ${m.spec.name} from ${by}?`,
      body: (
        <p>
          Its pages may change it again - but {by} still manages it: its next run undoes what was changed here
          {by === 'terraform' ? ', and locks it again while its configuration says lock_ui = true' : ''}.
        </p>
      ),
      action: 'Release',
      danger: true,
    })
    if (!ok) return
    await run(() => postJSON(`/api/machines/${m.id}`, { locked: false }, 'PATCH'), `${m.spec.name} released`)
    onChanged()
  }
  return { busy, power, destroy, retry, release, console: onConsole }
}

// ManagedBadge: what manages the machine as code, and its lock.
export function ManagedBadge({ m }) {
  if (!m?.spec?.managed_by) return null
  return (
    <Badge tone={m.spec.locked ? 'info' : 'warn'}>
      {m.spec.locked ? <Lock size={11} /> : <Code size={11} />} {m.spec.managed_by}
    </Badge>
  )
}

function ago(t) {
  if (!t || t.startsWith('0001')) return null
  const s = Math.max(0, Math.round((Date.now() - new Date(t).getTime()) / 1000))
  return s < 60 ? `${s} s ago` : s < 3600 ? `${Math.round(s / 60)} min ago` : new Date(t).toLocaleString()
}

// --- changing a machine: its hardware ---

const blankNIC = (networks, i) => ({ network: networks[0] || '', name: `eth${i}`, mode: 'static', address: '', gateway: '', isNew: true })

// EditMachine changes what the hypervisor gives the machine: vCPUs,
// memory, network interfaces (added, removed, moved to another network).
// The interfaces' addresses, DNS and NTP are the node's: its own page
// changes them - only an interface added or moved gets its addresses
// here, since it needs some to be of any use.
function EditMachine({ m, hv, onClose, onSaved }) {
  const networks = hvNetworks(hv)
  const [vcpus, setVcpus] = useState(m.spec.vcpus)
  const [memory, setMemory] = useState(m.spec.memory_mib)
  const [nics, setNics] = useState(() =>
    m.spec.nics.map((n) => ({ ...n, origNetwork: n.network, address: (n.addresses || []).join(', '), gateway: n.gateway || '', isNew: false })),
  )
  const [error, setError] = useState(null)
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const setNIC = (i, patch) => setNics(nics.map((n, j) => (j === i ? { ...n, ...patch } : n)))
  const addresses = (n) =>
    n.address
      .split(/[\s,]+/)
      .map((x) => x.trim())
      .filter(Boolean)

  const submit = async (e) => {
    e.preventDefault()
    setError(null)
    const body = {
      vcpus: Number(vcpus),
      memory_mib: Number(memory),
      nics: nics.map((n) => {
        const edited = n.isNew || n.network !== n.origNetwork
        const out = { network: n.network, name: n.name.trim(), mode: n.mode }
        if (!n.isNew) out.mac = n.mac
        if (edited && n.mode === 'static') {
          out.addresses = addresses(n)
          if (n.gateway.trim()) out.gateway = n.gateway.trim()
        } else if (!edited) {
          out.addresses = n.addresses
          if (n.gateway) out.gateway = n.gateway
        }
        return out
      }),
    }
    const removed = m.spec.nics.filter((o) => !nics.some((n) => n.mac === o.mac)).length
    const hardware =
      body.vcpus !== m.spec.vcpus || body.memory_mib !== m.spec.memory_mib || removed > 0 || nics.some((n) => n.isNew || n.network !== n.origNetwork)
    if (hardware) {
      const ok = await confirm({
        title: `Restart ${m.spec.name}?`,
        body: <p>Its hardware changes: the node shuts down cleanly (HAProxy stops), its virtual machine is reconfigured and started again.</p>,
        action: 'Change and restart',
        danger: true,
      })
      if (!ok) return
    }
    const saved = await run(async () => {
      try {
        return await postJSON(`/api/machines/${m.id}`, body, 'PATCH')
      } catch (err) {
        setError(err)
        throw err
      }
    }, `Changing ${m.spec.name}`)
    if (saved) onSaved()
  }

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal card wide edit-machine" role="dialog" aria-modal="true" onClick={(e) => e.stopPropagation()}>
        <div className="card-header">
          <div className="card-title">
            <Pencil size={16} /> {m.spec.name}: hardware
          </div>
          <button className="ghost icon" onClick={onClose} aria-label="Close">
            <X size={16} />
          </button>
        </div>
        <form className="stack" onSubmit={submit}>
          <div className="grid grid-2">
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
              {nics.map((n, i) => {
                const editable = n.isNew || n.network !== n.origNetwork
                return (
                  <div key={n.mac || `new-${i}`} className="nic-row">
                    <select value={n.network} onChange={(e) => setNIC(i, { network: e.target.value })} aria-label="Network">
                      {networks.map((net) => (
                        <option key={net} value={net}>
                          {net}
                        </option>
                      ))}
                    </select>
                    <input value={n.name} onChange={(e) => setNIC(i, { name: e.target.value })} disabled={!n.isNew} aria-label="Interface name" />
                    {editable ? (
                      <>
                        <select value={n.mode} onChange={(e) => setNIC(i, { mode: e.target.value })} aria-label="Addressing">
                          <option value="static">Static</option>
                          <option value="none">Up, no address</option>
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
                      </>
                    ) : (
                      <span className="muted small mono nic-current">
                        {n.mode === 'static' ? (n.addresses || []).join(' ') : n.mode} {n.gateway ? `via ${n.gateway}` : ''}
                      </span>
                    )}
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
                )
              })}
            </div>
            <div className="row">
              <button type="button" className="small ghost" onClick={() => setNics([...nics, blankNIC(networks, nics.length)])} disabled={nics.length >= 8}>
                <Plus size={14} /> Interface
              </button>
            </div>
            <ul className="muted small edit-notes">
              <li>The addresses, DNS and NTP of the interfaces it has are the node&apos;s: change them on its page (System › Network).</li>
              <li>An added interface gets static addresses, or none: DHCP only works on the one the node booted with.</li>
              <li>The interface the Controller reaches the node through can&apos;t be removed or moved to another network.</li>
            </ul>
          </div>
          <ErrorBox error={error} />
          <div className="row" style={{ justifyContent: 'flex-end' }}>
            <button type="button" onClick={onClose}>
              Cancel
            </button>
            <button className="primary" type="submit" disabled={busy}>
              {busy ? 'Changing…' : 'Change'}
            </button>
          </div>
        </form>
      </div>
    </div>
  )
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

// LockNotice says a machine is managed as code, and whether its pages may
// change it.
export function LockNotice({ m, actions }) {
  if (!m.spec.managed_by) return null
  const by = m.spec.managed_by
  return m.spec.locked ? (
    <div className="notice small lock-notice">
      <Lock size={14} />
      <span className="grow">
        Managed by <strong>{by}</strong> and locked: change it there. A change made on this Controller would be undone by its next run.
      </span>
      <button className="small ghost" onClick={() => actions.release(m)} disabled={actions.busy} title="Let its pages change it again">
        <LockOpen size={14} /> Release
      </button>
    </div>
  ) : (
    <div className="notice warn small">
      Managed by <strong>{by}</strong>, released: a change made here is undone by its next run.
    </div>
  )
}

function MachineCard({ m, vm, vmError, actions, onEdit }) {
  const [history, setHistory] = useState(false)
  const last = m.events[m.events.length - 1]
  const synced = ago(m.synced_at)
  return (
    <section className="card node-card">
      <div className="spread" style={{ alignItems: 'flex-start' }}>
        <div style={{ minWidth: 0 }}>
          <div className="node-card-name">{m.spec.name}</div>
          <div className="muted small mono">
            {m.vm_name || '…'} · {m.hypervisor_name}
          </div>
        </div>
        <div className="row" style={{ gap: '0.3rem', flexWrap: 'wrap', justifyContent: 'flex-end' }}>
          <ManagedBadge m={m} />
          <PowerBadge power={vm?.power} />
          <PhaseBadge phase={m.phase} />
        </div>
      </div>
      <LockNotice m={m} actions={actions} />
      {m.error && <div className="error-box small">{m.error}</div>}
      {m.warning && !m.node_id && (
        <div className="notice warn small">
          {m.warning} <span className="muted">Its console shows the rest.</span>
        </div>
      )}
      {vmError && <div className="error-box small">{vmError}</div>}
      {m.node_hostname && m.node_hostname !== m.spec.name && (
        <div className="notice warn small">The node calls itself {m.node_hostname}: its hostname was changed on its page.</div>
      )}
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
      {m.node_id && (synced || m.sync_error) && (
        <div className={`small ${m.sync_error ? '' : 'muted'}`} style={m.sync_error ? { color: 'var(--warn)' } : undefined}>
          {m.sync_error ? `Couldn't read it from its node: ${m.sync_error}` : `Read from its node and hypervisor ${synced}`}
        </div>
      )}
      <div className="row" style={{ marginTop: 'auto' }}>
        <PowerButtons machine={m} vm={vm} actions={actions} />
        <button className="small ghost" onClick={() => setHistory(!history)} title="History">
          <History size={14} />
        </button>
        {m.phase === 'ready' && m.node_id && !m.spec.locked && (
          <button className="small" onClick={() => onEdit(m)} disabled={actions.busy} title="vCPUs, memory, network interfaces">
            <Pencil size={14} /> Edit
          </button>
        )}
        {m.phase === 'failed' && !m.node_id && (
          <button className="small" onClick={() => actions.retry(m)} disabled={actions.busy}>
            <RotateCcw size={14} /> Retry
          </button>
        )}
        <span className="grow" />
        {m.phase !== 'destroying' && !m.spec.locked && (
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
  const [editing, setEditing] = useState(null) // a machine
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
  const editingHV = editing && hypervisors.find((h) => h.id === editing.spec.hypervisor_id)
  const vmOf = (m) => hvStatus[m.spec.hypervisor_id]?.machines?.[m.id]
  const vmErrorOf = (m) => hvStatus[m.spec.hypervisor_id]?.machine_errors?.[m.id]

  return (
    <div className="stack">
      {editing && (
        <EditMachine
          m={editing}
          hv={editingHV}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null)
            onChanged()
          }}
        />
      )}
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
          - a libvirt/KVM host or a Proxmox VE node the Controller creates its nodes on.
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
          <MachineCard key={m.id} m={m} vm={vmOf(m)} vmError={vmErrorOf(m)} actions={actions} onEdit={setEditing} />
        ))}
      </div>
    </div>
  )
}
