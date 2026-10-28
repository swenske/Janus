import {
  Boxes,
  Copy,
  Cpu,
  Download,
  Fingerprint,
  HardDrive,
  History,
  KeyRound,
  ListChecks,
  Loader2,
  MemoryStick,
  Pencil,
  Plus,
  RotateCcw,
  Server,
  Trash2,
  X,
} from 'lucide-react'
import { Fragment, useEffect, useRef, useState } from 'react'
import { call, gib, postJSON } from './call.js'
import { useCan } from './me.jsx'
import { AutoscaleFields, AutoscaleSummary } from './Autoscale.jsx'
import { autoscalePolicy, initialAutoscale, samePolicy, sizeText, useAutoscale } from './autoscale.js'
import { Badge, Card, ErrorBox, useAction, useConfirm, useToast } from './shared/ui.jsx'
import { LockNotice, ManagedBadge, PhaseBadge, PowerBadge, PowerButtons, useMachineActions } from './machines.jsx'
import Gauge from './shared/Gauge.jsx'
import { requirementError, toggleExtension } from './shared/requires.js'
import ViewToggle from './shared/ViewToggle.jsx'
import { useStoredView } from './shared/storedView.js'
import { fmtDateTime, fmtTime } from './shared/units.js'

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

// A Proxmox network may stand for several: "vmbr0.100-199", "vmbr0.*" -
// an interface then picks its VLAN, and names it ("vmbr0.150"). Mirrors
// hypervisor.ParseProxmoxAllowed.
function vlanRange(hv, entry) {
  if (hv?.kind !== 'proxmox') return null
  const m = /^([A-Za-z][A-Za-z0-9_-]{0,14})\.(?:(\*)|(\d{1,4})-(\d{1,4}))$/.exec(entry || '')
  if (!m) return null
  return m[2] ? { bridge: m[1], first: 1, last: 4094, any: true } : { bridge: m[1], first: +m[3], last: +m[4] }
}

// untaggedToo: the bridge's own, untagged network is allowed besides
// "bridge.*" - one choice then, its VLAN tag optional, as in Proxmox.
function untaggedToo(hv, r) {
  return !!r?.any && hvNetworks(hv).includes(r.bridge)
}

// networkChoices are the networks offered: every allowed one, but a
// bridge whose "bridge.*" is allowed too is that one's empty tag.
function networkChoices(hv) {
  const entries = hvNetworks(hv)
  return entries.filter((e) => !entries.some((o) => vlanRange(hv, o)?.any && vlanRange(hv, o).bridge === e))
}

// networkChoice is the choice an interface's network falls under, and
// its VLAN tag when that choice is a range.
function networkChoice(hv, network) {
  const entries = networkChoices(hv)
  if (entries.includes(network)) return { entry: network, vlan: '' }
  const m = /^(.+)\.(\d*)$/.exec(network || '')
  for (const e of entries) {
    const r = vlanRange(hv, e)
    if (!r) continue
    if (network === r.bridge && untaggedToo(hv, r)) return { entry: e, vlan: '' }
    if (m && r.bridge === m[1] && (m[2] === '' || (+m[2] >= r.first && +m[2] <= r.last))) return { entry: e, vlan: m[2] }
  }
  return null
}

// firstNetwork is an interface's network on a choice: itself; a range's
// first VLAN; for "bridge.*", the bridge untagged when that's allowed,
// else a tag still to fill in.
function firstNetwork(hv, entry = networkChoices(hv)[0] || '') {
  const r = vlanRange(hv, entry)
  if (!r) return entry
  if (r.any) return untaggedToo(hv, r) ? r.bridge : `${r.bridge}.`
  return `${r.bridge}.${r.first}`
}

// NetworkPicker chooses an interface's network the way Proxmox does: a
// bridge, and a VLAN tag when the hypervisor allows a range or any tag
// on it (optional when it allows the bridge untagged too).
function NetworkPicker({ hv, value, onChange }) {
  const choice = networkChoice(hv, value) || { entry: '', vlan: '' }
  const r = vlanRange(hv, choice.entry)
  const optional = untaggedToo(hv, r)
  return (
    <div className="net-pick">
      <select value={choice.entry} onChange={(e) => onChange(firstNetwork(hv, e.target.value))} aria-label="Network">
        {!choice.entry && <option value="">-</option>}
        {networkChoices(hv).map((net) => {
          const nr = vlanRange(hv, net)
          return (
            <option key={net} value={net}>
              {nr ? `${nr.bridge} · VLAN ${nr.any ? 'tag' : `${nr.first}-${nr.last}`}` : net}
            </option>
          )
        })}
      </select>
      {r && (
        <input
          type="number"
          min={r.first}
          max={r.last}
          required={!optional}
          value={choice.vlan}
          onChange={(e) => onChange(e.target.value === '' && optional ? r.bridge : `${r.bridge}.${e.target.value}`)}
          aria-label="VLAN tag"
          placeholder={optional ? 'no tag' : 'VLAN tag'}
          title={r.any ? `VLAN tag, 1 to 4094${optional ? ' - none: untagged' : ''}` : `VLAN ${r.first} to ${r.last}`}
        />
      )}
    </div>
  )
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
  const missing = pve ? 'Fill in the name, API URL, node, token ID, pool, storages and networks first' : 'Fill in the name, SSH host and user, pool and networks first'
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
              A Proxmox VE node, through its API with a token whose rights cover one pool, two storages and the networks listed here - nothing else, not even seeing other virtual
              machines. <strong>Show host preparation</strong> sets it up, the token included.
            </>
          ) : (
            <>
              A libvirt/KVM host, reached over SSH as a dedicated user in its <code>libvirt</code> group. The Controller only ever acts on the virtual machines it created, in the
              pool and on the networks listed here - the host&apos;s preparation makes libvirt itself enforce it.
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
              <Field label="Networks machines may use (bridge, bridge.vlan, bridge.first-last or bridge.*; comma-separated)">
                <input value={f.networks} onChange={set('networks')} required placeholder="vmbr0.10, vmbr0.100-199" />
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
        {hv && !pve && f.host.trim() !== (hv.libvirt?.host || '') && <div className="notice warn">Another host: its host key will have to be confirmed again.</div>}
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
            As root on the {body.proxmox ? 'node' : 'host'}: run <code>sh {file}</code>, or paste it into <code>sudo sh</code>. Each step can also run alone, and running it again
            is harmless.
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
          The Controller&apos;s SSH key isn&apos;t in it yet: it&apos;s made when the hypervisor is added. Its card then gives the command that authorizes it - or this preparation
          again, key included.
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
            <code>{pve ? 'openssl x509 -noout -fingerprint -sha256 -in /etc/pve/local/pveproxy-ssl.pem' : 'ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub'}</code>
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

// hvBadge is a hypervisor's state: to set up, unreachable, connected,
// or checking while its first status is on its way.
function hvBadge(hv, status) {
  if (!hv.trusted) return <Badge tone="warn">to set up</Badge>
  if (status?.error)
    return (
      <Badge tone="danger" dot>
        unreachable
      </Badge>
    )
  if (status?.host)
    return (
      <Badge tone="ok" dot>
        connected
      </Badge>
    )
  return <Badge>checking…</Badge>
}

function hvWhere(hv) {
  return hv.kind === 'proxmox' ? `${hv.proxmox?.node} · ${hv.proxmox?.token_id?.split('!')[0]} · Proxmox VE` : `${hv.libvirt?.user}@${hv.libvirt?.host} · libvirt`
}

// HVError is a hypervisor's error, and what to check when it's about
// its credentials.
function HVError({ hv, error }) {
  const authError = /unable to authenticate|permission denied \(publickey|401|authentication failure|invalid token/i.test(error)
  return (
    <div className="error-box small">
      {error}
      {authError && (
        <div style={{ marginTop: '0.4rem' }}>
          {hv.kind === 'proxmox' ? "Check the API token's ID and secret (Edit)." : "Add the Controller's public key to the user's authorized_keys (Edit shows it)."}
        </div>
      )}
    </div>
  )
}

function HypervisorCard({ hv, status, onEdit, onRemove, onTrusted }) {
  const can = useCan()
  const host = status?.host
  const badge = hvBadge(hv, status)
  return (
    <section className="card node-card">
      <div className="spread" style={{ alignItems: 'flex-start' }}>
        <div style={{ minWidth: 0 }}>
          <div className="node-card-name">{hv.name}</div>
          <div className="muted small mono">{hvWhere(hv)}</div>
        </div>
        {badge}
      </div>
      {!hv.trusted && (can('admin') ? <TrustSteps hv={hv} onTrusted={onTrusted} /> : <div className="notice warn small">Not trusted yet: an admin checks its key.</div>)}
      {hv.trusted && status?.error && <HVError hv={hv} error={status.error} />}
      {host && (
        <>
          <div className="gauges hv-gauges">
            <div className="gauge-cell">
              <Gauge label="CPU busy" value={status.cpu_percent >= 0 ? status.cpu_percent : undefined} />
              <span className="muted small">CPU</span>
            </div>
            <div className="gauge-cell">
              <Gauge label="Memory used" color="var(--chart-2)" value={host.memory_total - host.memory_free} max={host.memory_total} />
              <span className="muted small">Memory</span>
            </div>
            {!host.storage.error && (
              <div className="gauge-cell">
                <Gauge label={`${host.storage.name} allocated`} color="var(--chart-4)" value={host.storage.allocated} max={host.storage.capacity} />
                <span className="muted small">{hv.kind === 'proxmox' ? 'Storage' : 'Pool'}</span>
              </div>
            )}
          </div>
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
            </dd>
            <dt>
              <MemoryStick size={13} /> Memory
            </dt>
            <dd>
              {gib(host.memory_total - host.memory_free)} of {gib(host.memory_total)} used ({pct(host.memory_total - host.memory_free, host.memory_total)})
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
                </>
              )}
            </dd>
            {host.image_storage && (
              <>
                <dt>
                  <HardDrive size={13} /> Images
                </dt>
                <dd>
                  {host.image_storage.error ? (
                    <span style={{ color: 'var(--danger)' }}>
                      {host.image_storage.name}: {host.image_storage.error}
                    </span>
                  ) : (
                    <>
                      <span className="mono">{host.image_storage.name}</span> · {gib(host.image_storage.available)} free of {gib(host.image_storage.capacity)}
                      {!host.image_storage.active && <Badge tone="danger">inactive</Badge>}
                    </>
                  )}
                </dd>
              </>
            )}
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
      {can('admin') && (
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
      )}
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
  return { network: firstNetwork(hv), name: `eth${i}`, mode: 'static', address: '', gateway: '' }
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
  const [haproxy, setHAProxy] = useState('')
  const [kernel, setKernel] = useState('')
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
  // Ticking an extension ticks what it requires, unticking one unticks
  // what requires it - said under the list.
  const [extNote, setExtNote] = useState('')
  const toggleExt = (e) => {
    const r = toggleExtension(exts, e, !exts.includes(e), catalog?.extensions)
    setExts(r.extensions)
    setExtNote(r.note)
  }
  const pickHV = (id) => {
    setHvId(id)
    const next = usable.find((h) => h.id === id)
    setNics(nics.map((n) => (networkChoice(next, n.network) ? n : { ...n, network: firstNetwork(next) })))
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
      if (haproxy) spec.haproxy = haproxy
      if (kernel) spec.kernel = kernel
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
          The Controller creates the virtual machine, boots it with its address and a one-time token, and admits the node as soon as it registers - no approval step for a machine
          it created itself.
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
                <NetworkPicker hv={hv} value={n.network} onChange={(network) => setNIC(i, { network })} />
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
                <button type="button" className="ghost icon" onClick={() => setNics(nics.filter((_, j) => j !== i))} disabled={nics.length === 1} aria-label="Remove interface">
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
              Interfaces are matched by their MAC address and renamed. Static or DHCP: a node renews its DHCP lease, and a static node doesn&apos;t wait for a DHCP server.
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
              list="janus-machine-versions"
            />
            {/* The newest pre-release, for a lab: offered, never the default. */}
            <datalist id="janus-machine-versions">{catalog?.prerelease && <option value={catalog.prerelease}>pre-release, for testing</option>}</datalist>
            {catalog?.prerelease &&
              (version.trim() === catalog.prerelease ? (
                <span className="notice warn small">
                  <strong>{catalog.prerelease} is a pre-release</strong>, for testing: not supported, and it may still change or be withdrawn. The node is offered the next release
                  as usual.
                </span>
              ) : (
                <span className="muted small">Pre-release {catalog.prerelease} available, for testing - type it to use it.</span>
              ))}
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
                  <label key={e.name} className="check" title={e.description + (e.requires?.length ? ` Requires ${e.requires.join(', ')}.` : '')}>
                    <input type="checkbox" checked={exts.includes(e.name)} onChange={() => toggleExt(e.name)} disabled={!!imageURL.trim()} /> {e.name}
                  </label>
                ))}
              </div>
            )}
            {!imageURL.trim() && requirementError(exts, extensions, catalog?.catalog_version) ? (
              <div className="notice warn small" role="alert">
                {requirementError(exts, extensions, catalog?.catalog_version)}
              </div>
            ) : (
              extNote && <span className="muted small">{extNote}</span>
            )}
          </div>
        </div>
        {catalog?.haproxy?.length > 0 && (
          <div className="grid grid-2">
            <label className="field">
              <span>HAProxy branch {haproxy && <span className="muted">· built by the image factory</span>}</span>
              <select value={haproxy} onChange={(e) => setHAProxy(e.target.value)} disabled={!!imageURL.trim()}>
                <option value="">The newest LTS branch, following it ({catalog.haproxy.find((v) => v.default)?.name} now)</option>
                {catalog.haproxy
                  .filter((v) => v.arches.includes('amd64'))
                  .map((v) => (
                    <option key={v.name} value={v.name}>
                      HAProxy {v.name} - {v.version}
                      {v.eol ? `, supported until ${v.eol.slice(0, 7)}` : ''}
                    </option>
                  ))}
              </select>
            </label>
            <label className="field">
              <span>Kernel track {kernel && <span className="muted">· built by the image factory</span>}</span>
              <select value={kernel} onChange={(e) => setKernel(e.target.value)} disabled={!!imageURL.trim()}>
                <option value="">The release's default ({catalog.kernel.find((v) => v.default)?.name} now)</option>
                {catalog.kernel
                  .filter((v) => v.arches.includes('amd64'))
                  .map((v) => (
                    <option key={v.name} value={v.name}>
                      {v.name} - {v.version}
                    </option>
                  ))}
              </select>
            </label>
          </div>
        )}
        <details>
          <summary className="small">Another image (a mirror, a development build)</summary>
          <div className="grid grid-2" style={{ marginTop: '0.5rem' }}>
            <label className="field">
              <span>qcow2 image URL (replaces version, extensions, HAProxy branch and kernel track)</span>
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

function ago(t) {
  if (!t || t.startsWith('0001')) return null
  const s = Math.max(0, Math.round((Date.now() - new Date(t).getTime()) / 1000))
  return s < 60 ? `${s} s ago` : s < 3600 ? `${Math.round(s / 60)} min ago` : fmtDateTime(t)
}

// --- changing a machine: its hardware ---

const blankNIC = (hv, i) => ({ network: firstNetwork(hv), name: `eth${i}`, mode: 'static', address: '', gateway: '', isNew: true })

// EditMachine changes what the hypervisor gives the machine: vCPUs,
// memory, network interfaces (added, removed, moved to another network).
// The interfaces' addresses, DNS and NTP are the node's: its own page
// changes them - only an interface added or moved gets its addresses
// here, since it needs some to be of any use.
function EditMachine({ m, hv, onClose, onSaved }) {
  const [vcpus, setVcpus] = useState(m.spec.vcpus)
  const [memory, setMemory] = useState(m.spec.memory_mib)
  const [scaling] = useAutoscale(m.id)
  const defaults = scaling?.defaults
  const [auto, setAuto] = useState(null)
  useEffect(() => {
    if (defaults && !auto) setAuto(initialAutoscale(m, defaults))
  }, [defaults]) // eslint-disable-line react-hooks/exhaustive-deps
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
    const policy = auto ? autoscalePolicy(auto, defaults) : m.spec.autoscale || null
    if (!samePolicy(policy, m.spec.autoscale)) body.autoscale = policy
    const maxima = (p) => (p ? `${p.max_vcpus}/${p.max_memory_mib}` : '')
    const removed = m.spec.nics.filter((o) => !nics.some((n) => n.mac === o.mac)).length
    const hardware =
      body.vcpus !== m.spec.vcpus ||
      body.memory_mib !== m.spec.memory_mib ||
      removed > 0 ||
      nics.some((n) => n.isNew || n.network !== n.origNetwork) ||
      maxima(policy) !== maxima(m.spec.autoscale)
    if (hardware) {
      const room = policy ? ` with room to grow to ${policy.max_vcpus} vCPU and ${policy.max_memory_mib} MiB while it runs` : ''
      const ok = await confirm({
        title: `Restart ${m.spec.name}?`,
        body: <p>Its hardware changes: the node shuts down cleanly (HAProxy stops), its virtual machine is reconfigured and started again{room}.</p>,
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
            <Pencil size={16} /> {m.spec.name}: hardware and auto-scaling
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
                    <NetworkPicker hv={hv} value={n.network} onChange={(network) => setNIC(i, { network })} />
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
                    <button type="button" className="ghost icon" onClick={() => setNics(nics.filter((_, j) => j !== i))} disabled={nics.length === 1} aria-label="Remove interface">
                      <X size={15} />
                    </button>
                  </div>
                )
              })}
            </div>
            <div className="row">
              <button type="button" className="small ghost" onClick={() => setNics([...nics, blankNIC(hv, nics.length)])} disabled={nics.length >= 8}>
                <Plus size={14} /> Interface
              </button>
            </div>
            <ul className="muted small edit-notes">
              <li>The addresses, DNS and NTP of the interfaces it has are the node&apos;s: change them on its page (System › Network).</li>
              <li>An added interface gets static addresses, or none: DHCP only works on the one the node booted with.</li>
              <li>The interface the Controller reaches the node through can&apos;t be removed or moved to another network.</li>
            </ul>
          </div>
          {auto && <AutoscaleFields form={auto} setForm={setAuto} vcpus={Number(vcpus)} memory={Number(memory)} current={!!m.spec.autoscale} />}
          <ErrorBox error={error} />
          <div className="row" style={{ justifyContent: 'flex-end' }}>
            <button type="button" onClick={onClose}>
              Cancel
            </button>
            <button className="primary" type="submit" disabled={busy || !auto}>
              {busy ? 'Changing…' : 'Change'}
            </button>
          </div>
        </form>
      </div>
    </div>
  )
}

function MachineCard({ m, vm, vmError, actions, onEdit }) {
  const can = useCan()
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
        <dd>{sizeText(m)}</dd>
        <dt>Image</dt>
        <dd className="mono">
          {m.version ? `${m.version}${m.schematic ? ` · ${m.schematic.slice(0, 8)}` : ''}` : m.spec.image ? 'custom image' : m.spec.version || 'newest release'}
          {m.spec.haproxy && <span className="muted"> · HAProxy {m.spec.haproxy}</span>}
          {m.spec.kernel && <span className="muted"> · {m.spec.kernel} kernel</span>}
          {m.spec.update_reboot_mode === 'kexec' && <span className="muted"> · updates through kexec</span>}
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
      <AutoscaleSummary m={m} />
      {last && !history && (
        <div className="muted small">
          {fmtTime(last.time)} · {last.message}
        </div>
      )}
      {history && <pre className="logview machine-events">{m.events.map((e) => `${fmtTime(e.time)}  ${e.message}`).join('\n')}</pre>}
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
        {can('admin') && m.phase === 'ready' && m.node_id && !m.spec.locked && (
          <button className="small" onClick={() => onEdit(m)} disabled={actions.busy} title="vCPUs, memory, network interfaces, auto-scaling">
            <Pencil size={14} /> Edit
          </button>
        )}
        {can('admin') && m.phase === 'failed' && !m.node_id && (
          <button className="small" onClick={() => actions.retry(m)} disabled={actions.busy}>
            <RotateCcw size={14} /> Retry
          </button>
        )}
        <span className="grow" />
        {can('admin') && m.phase !== 'destroying' && !m.spec.locked && (
          <button className="ghost small danger" onClick={() => actions.destroy(m)} disabled={actions.busy}>
            <Trash2 size={14} /> Destroy
          </button>
        )}
      </div>
    </section>
  )
}

// --- as lists: the same data as the cards, a row each ---

// HypervisorTable lists the hypervisors as rows: what a card shows, its
// set-up steps and errors on a row of their own below.
function HypervisorTable({ hypervisors, hvStatus, onEdit, onRemove, onTrusted }) {
  const can = useCan()
  const columns = can('admin') ? 8 : 7
  return (
    <div className="card node-table-card">
      <div className="table-wrap">
        <table className="node-table">
          <thead>
            <tr>
              <th>Hypervisor</th>
              <th>State</th>
              <th>Host</th>
              <th>CPU</th>
              <th>Memory</th>
              <th>Storage</th>
              <th>Machines</th>
              {can('admin') && <th aria-label="Actions" />}
            </tr>
          </thead>
          <tbody>
            {hypervisors.map((hv) => {
              const status = hvStatus[hv.id]
              const host = status?.host
              return (
                <Fragment key={hv.id}>
                  <tr>
                    <td>
                      <div className="node-card-name">{hv.name}</div>
                      <div className="muted small mono">{hvWhere(hv)}</div>
                    </td>
                    <td>{hvBadge(hv, status)}</td>
                    <td className="small">
                      {host ? (
                        <>
                          <div className="mono">{host.hostname}</div>
                          <div className="muted">
                            {host.hypervisor_version} · {host.library_version}
                          </div>
                        </>
                      ) : (
                        <span className="muted">–</span>
                      )}
                    </td>
                    <td className="small">
                      {host ? (
                        <>
                          {host.cpus} × {host.cpu_mhz} MHz
                          {status.cpu_percent >= 0 && <div className="muted">{status.cpu_percent.toFixed(0)}% busy</div>}
                        </>
                      ) : (
                        <span className="muted">–</span>
                      )}
                    </td>
                    <td className="small">
                      {host ? (
                        <>
                          {gib(host.memory_total - host.memory_free)} of {gib(host.memory_total)}
                          <div className="muted">{pct(host.memory_total - host.memory_free, host.memory_total)} used</div>
                        </>
                      ) : (
                        <span className="muted">–</span>
                      )}
                    </td>
                    <td className="small">
                      {host ? (
                        <>
                          <StorageText s={host.storage} />
                          {host.image_storage && (
                            <div>
                              <span className="muted">images </span>
                              <StorageText s={host.image_storage} />
                            </div>
                          )}
                        </>
                      ) : (
                        <span className="muted">–</span>
                      )}
                    </td>
                    <td className="small">
                      {hv.machines}
                      {host?.running_machines >= 0 && <div className="muted">{host.running_machines} running</div>}
                    </td>
                    {can('admin') && (
                      <td className="actions">
                        <div className="row" style={{ gap: '0.3rem', flexWrap: 'nowrap' }}>
                          <button className="small" onClick={() => onEdit(hv)}>
                            <Pencil size={14} /> Edit
                          </button>
                          <button
                            className="ghost icon danger"
                            onClick={() => onRemove(hv)}
                            disabled={hv.machines > 0}
                            title={hv.machines > 0 ? 'Destroy its machines first' : 'Forget this hypervisor'}
                            aria-label={`Remove ${hv.name}`}
                          >
                            <Trash2 size={14} />
                          </button>
                        </div>
                      </td>
                    )}
                  </tr>
                  {(!hv.trusted || status?.error) && (
                    <tr className="table-detail">
                      <td colSpan={columns}>
                        {!hv.trusted &&
                          (can('admin') ? <TrustSteps hv={hv} onTrusted={onTrusted} /> : <div className="notice warn small">Not trusted yet: an admin checks its key.</div>)}
                        {hv.trusted && status?.error && <HVError hv={hv} error={status.error} />}
                      </td>
                    </tr>
                  )}
                </Fragment>
              )
            })}
          </tbody>
        </table>
      </div>
    </div>
  )
}

// StorageText is a storage pool's name and room, or its error.
function StorageText({ s }) {
  if (s.error)
    return (
      <span style={{ color: 'var(--danger)' }} className="wrap-anywhere">
        {s.name}: {s.error}
      </span>
    )
  return (
    <>
      <span className="mono">{s.name}</span> · {gib(s.available)} free of {gib(s.capacity)}
      {!s.active && <Badge tone="danger">inactive</Badge>}
    </>
  )
}

// MachineTable lists the machines as rows: what a card shows, its
// notices and errors - and its history when asked - on a row below.
function MachineTable({ machines, vmOf, vmErrorOf, actions, onEdit }) {
  return (
    <div className="card node-table-card">
      <div className="table-wrap">
        <table className="node-table">
          <thead>
            <tr>
              <th>Machine</th>
              <th>State</th>
              <th>Size</th>
              <th>Image</th>
              <th>Network</th>
              <th>Node</th>
              <th>Last event</th>
              <th aria-label="Actions" />
            </tr>
          </thead>
          <tbody>
            {machines.map((m) => (
              <MachineRow key={m.id} m={m} vm={vmOf(m)} vmError={vmErrorOf(m)} actions={actions} onEdit={onEdit} />
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}

function MachineRow({ m, vm, vmError, actions, onEdit }) {
  const can = useCan()
  const [history, setHistory] = useState(false)
  const last = m.events[m.events.length - 1]
  const synced = ago(m.synced_at)
  const notices =
    m.error || vmError || (m.warning && !m.node_id) || (m.node_hostname && m.node_hostname !== m.spec.name) || m.spec.managed_by || history || m.spec.autoscale
  return (
    <>
      <tr>
        <td>
          <div className="node-card-name">{m.spec.name}</div>
          <div className="muted small mono">
            {m.vm_name || '…'} · {m.hypervisor_name}
          </div>
        </td>
        <td>
          <div className="row" style={{ gap: '0.3rem', flexWrap: 'wrap' }}>
            <ManagedBadge m={m} />
            <PowerBadge power={vm?.power} />
            <PhaseBadge phase={m.phase} />
          </div>
        </td>
        <td className="small">{sizeText(m)}</td>
        <td className="small mono">
          {m.version ? `${m.version}${m.schematic ? ` · ${m.schematic.slice(0, 8)}` : ''}` : m.spec.image ? 'custom image' : m.spec.version || 'newest release'}
          {(m.spec.haproxy || m.spec.kernel) && (
            <div className="muted">
              {[m.spec.haproxy && `HAProxy ${m.spec.haproxy}`, m.spec.kernel && `${m.spec.kernel} kernel`].filter(Boolean).join(' · ')}
            </div>
          )}
        </td>
        <td className="small mono">
          {m.spec.nics.map((n) => (
            <div key={n.mac}>
              {n.name} · {n.network} · {n.mode === 'static' ? (n.addresses || []).join(' ') : n.mode}
            </div>
          ))}
        </td>
        <td className="small">
          <span className="mono">{m.node_address || <span className="muted">not registered yet</span>}</span>
          {m.node_id && (synced || m.sync_error) && (
            <div className={m.sync_error ? 'wrap-anywhere' : 'muted'} style={m.sync_error ? { color: 'var(--warn)' } : undefined} title={m.sync_error || ''}>
              {m.sync_error ? "couldn't read it from its node" : `read ${synced}`}
            </div>
          )}
        </td>
        <td className="small machine-last-event" title={last ? `${fmtDateTime(last.time)} · ${last.message}` : ''}>
          {last ? (
            <>
              <span className="muted">{fmtTime(last.time)}</span> {last.message}
            </>
          ) : (
            <span className="muted">–</span>
          )}
        </td>
        <td className="actions">
          <div className="row" style={{ gap: '0.3rem', flexWrap: 'nowrap' }}>
            <PowerButtons machine={m} vm={vm} actions={actions} />
            <button className="small ghost" onClick={() => setHistory(!history)} title="History" aria-pressed={history}>
              <History size={14} />
            </button>
            {can('admin') && m.phase === 'ready' && m.node_id && !m.spec.locked && (
              <button className="small" onClick={() => onEdit(m)} disabled={actions.busy} title="vCPUs, memory, network interfaces, auto-scaling">
                <Pencil size={14} /> Edit
              </button>
            )}
            {can('admin') && m.phase === 'failed' && !m.node_id && (
              <button className="small" onClick={() => actions.retry(m)} disabled={actions.busy}>
                <RotateCcw size={14} /> Retry
              </button>
            )}
            {can('admin') && m.phase !== 'destroying' && !m.spec.locked && (
              <button className="ghost icon danger" onClick={() => actions.destroy(m)} disabled={actions.busy} title="Destroy" aria-label={`Destroy ${m.spec.name}`}>
                <Trash2 size={14} />
              </button>
            )}
          </div>
        </td>
      </tr>
      {notices && (
        <tr className="table-detail">
          <td colSpan={8}>
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
            <AutoscaleSummary m={m} />
            {history && <pre className="logview machine-events">{m.events.map((e) => `${fmtTime(e.time)}  ${e.message}`).join('\n')}</pre>}
          </td>
        </tr>
      )}
    </>
  )
}

// --- the page ---

export default function HypervisorsPage({ hypervisors, machines, hvStatus, onChanged, onConsole }) {
  const can = useCan()
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
  // Cards or a list, each list its own choice, remembered per browser.
  const [hvView, setHvView] = useStoredView('janus-hypervisors-view')
  const [machinesView, setMachinesView] = useStoredView('janus-machines-view')

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
        <div className="row">
          {hypervisors.length > 0 && <ViewToggle value={hvView} onChange={setHvView} label="Show the hypervisors as" />}
          {!form && can('admin') && (
            <button className="primary" onClick={() => setForm('add')}>
              <Plus size={15} /> Add hypervisor
            </button>
          )}
        </div>
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
          {can('admin') ? (
            <>
              No hypervisor yet.{' '}
              <button className="small" onClick={() => setForm('add')}>
                Add one
              </button>{' '}
              - a libvirt/KVM host or a Proxmox VE node the Controller creates its nodes on.
            </>
          ) : (
            'No hypervisor yet: the libvirt/KVM hosts and Proxmox VE nodes the Controller creates its nodes on appear here.'
          )}
        </div>
      )}
      {hvView === 'list' && hypervisors.length > 0 ? (
        <HypervisorTable hypervisors={hypervisors} hvStatus={hvStatus} onEdit={setForm} onRemove={remove} onTrusted={onChanged} />
      ) : (
        <div className="node-grid">
          {hypervisors.map((hv) => (
            <HypervisorCard key={hv.id} hv={hv} status={hvStatus[hv.id]} onEdit={setForm} onRemove={remove} onTrusted={onChanged} />
          ))}
        </div>
      )}

      <div className="spread" style={{ marginTop: '0.6rem' }}>
        <h1>Machines</h1>
        <div className="row">
          {machines.length > 0 && <ViewToggle value={machinesView} onChange={setMachinesView} label="Show the machines as" />}
          {!creating && can('admin') && (
            <button className="primary" onClick={() => setCreating(true)} disabled={trusted.length === 0} title={trusted.length ? '' : 'Add and trust a hypervisor first'}>
              <Plus size={15} /> Create node
            </button>
          )}
        </div>
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
      {machinesView === 'list' && machines.length > 0 ? (
        <MachineTable machines={machines} vmOf={vmOf} vmErrorOf={vmErrorOf} actions={actions} onEdit={setEditing} />
      ) : (
        <div className="node-grid">
          {machines.map((m) => (
            <MachineCard key={m.id} m={m} vm={vmOf(m)} vmError={vmErrorOf(m)} actions={actions} onEdit={setEditing} />
          ))}
        </div>
      )}
    </div>
  )
}
