import {
  Archive,
  ArrowUpRight,
  Boxes,
  Check,
  ChevronDown,
  Copy,
  KeyRound,
  LockKeyhole,
  LogOut,
  Plus,
  Rocket,
  ScrollText,
  Send,
  Server,
  ShieldCheck,
  Tag,
  Trash2,
  UserRound,
  Users,
  X,
} from 'lucide-react'
import { Suspense, useCallback, useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { call, postJSON, SIGNED_OUT } from './call.js'
import ControllerUpdate from './ControllerUpdate.jsx'
import { FleetCard, FleetSetup, TrustBadge } from './Fleet.jsx'
import { LockNotice, ManagedBadge, PhaseBadge, PowerBadge, PowerButtons, useMachineActions } from './machines.jsx'
import { navigate, useHashRoute } from './shared/route.js'
import BrandMark from './shared/BrandMark.jsx'
import RefreshButton from './shared/RefreshButton.jsx'
import ThemePicker from './shared/ThemePicker.jsx'
import NodeSparklines, { NodeSpark, WINDOW as SPARK_WINDOW } from './NodeSparklines.jsx'
import ViewToggle from './shared/ViewToggle.jsx'
import { useStoredView } from './shared/storedView.js'
import { useNodeMetrics } from './nodeMetrics.js'
import { labelString, MeContext, parseLabels, useCan, useMe } from './me.jsx'
import { EnrollMFA, MFAPanel, SecondFactorForm, TrustedBrowsers } from './MFA.jsx'
import { SSHKeys } from './SSHKeys.jsx'
import { Identities, LINK, SSO_DONE, SSOButtons, SSODone } from './SSO.jsx'
import { backupAlert } from './backupAlert.js'
import EnrollTokens from './Enroll.jsx'
import HTTPSCertificate from './HTTPSCert.jsx'
import { SecurityBadge } from './SecurityBadge.jsx'
import { PrereleaseBadge } from './PrereleaseBadge.jsx'
import { SupportBadge } from './SupportBadge.jsx'
import { worstSeverity } from './severity.js'
import { Badge, Card, ErrorBox, Loading, Tabs, stateTone, useConfirm, useToast } from './shared/ui.jsx'
import { lazyView } from './shared/lazy.jsx'
import { useSplash, useSplashStep } from './shared/Splash.jsx'

// The nodes page comes with the page; the other tabs, the machines'
// console, the CLI sign-in pages and the restore form in their own
// chunks, fetched when first shown (shared/lazy.jsx).
const AuditPage = lazyView(() => import('./Audit.jsx'))
const BackupsPage = lazyView(() => import('./Backups.jsx'))
const HypervisorsPage = lazyView(() => import('./Hypervisors.jsx'))
const LogForwardingPage = lazyView(() => import('./LogForwarding.jsx'))
const LetsEncryptPage = lazyView(() => import('./LetsEncrypt.jsx'))
const MachineConsole = lazyView(() => import('./Console.jsx'))
const TokensPage = lazyView(() => import('./Tokens.jsx'))
const UsersPage = lazyView(() => import('./Users.jsx'))
const CliLoginPage = lazyView(() => import('./CliLogin.jsx').then((m) => ({ default: m.CliLoginPage })))
const CliDevicePage = lazyView(() => import('./CliLogin.jsx').then((m) => ({ default: m.CliDevicePage })))
const RestoreForm = lazyView(() => import('./Restore.jsx').then((m) => ({ default: m.RestoreForm })))

// A node's page is on this origin too, under /nodes/<id>/ (dashboard/
// backend/internal/nodeproxy): it acts for the signed-in account.
function openNode(node) {
  window.open(`/nodes/${node.id}/`, '_blank', 'noopener')
}

function uptime(bootUnix) {
  if (!bootUnix) return ''
  let s = Math.max(0, Math.floor(Date.now() / 1000 - bootUnix))
  const d = Math.floor(s / 86400)
  s -= d * 86400
  const h = Math.floor(s / 3600)
  const m = Math.floor((s - h * 3600) / 60)
  return d ? `${d}d ${h}h` : h ? `${h}h ${m}m` : `${m}m`
}

// --- node cards ---

// A node the Controller created on a hypervisor shows its machine's
// state, and the hypervisor-level actions - what's left when the node
// itself doesn't answer.
// NodeLabels shows a node's labels - what grants and scoped tokens pick
// it by -, edited in place by an admin.
function NodeLabels({ node, onChanged }) {
  const can = useCan()
  const toast = useToast()
  const [editing, setEditing] = useState(null)
  const [busy, setBusy] = useState(false)
  const l = node.labels || {}
  const save = async (e) => {
    e.preventDefault()
    setBusy(true)
    try {
      await postJSON(`/api/nodes/${node.id}`, { labels: parseLabels(editing) }, 'PATCH')
      toast(`${node.name}'s labels saved`)
      setEditing(null)
      onChanged()
    } catch (err) {
      toast(err, 'danger')
    } finally {
      setBusy(false)
    }
  }
  if (editing !== null)
    return (
      <form className="row" style={{ gap: '0.3rem' }} onSubmit={save}>
        <input className="mono small grow" aria-label={`${node.name}'s labels`} value={editing} onChange={(e) => setEditing(e.target.value)} placeholder="team=web, env=prod" autoFocus />
        <button className="small primary" disabled={busy}>
          Save
        </button>
        <button type="button" className="small ghost" onClick={() => setEditing(null)}>
          Cancel
        </button>
      </form>
    )
  const keys = Object.keys(l).sort()
  if (!keys.length && !can('admin')) return null
  return (
    <div className="row" style={{ gap: '0.3rem', flexWrap: 'wrap' }}>
      {keys.map((k) => (
        <Badge key={k}>
          <span className="mono">
            {k}={l[k]}
          </span>
        </Badge>
      ))}
      {can('admin') && (
        <button className="ghost small" onClick={() => setEditing(labelString(l))} title="Its labels: grants and scoped tokens pick nodes by them">
          <Tag size={13} /> {keys.length ? 'Edit' : 'Labels'}
        </button>
      )}
    </div>
  )
}

function NodeCard({ node, status, onRemove, machine, vm, machineActions, fleet, onChanged, metrics, clock }) {
  const can = useCan()
  const st = status
  const reachable = st?.reachable
  return (
    <section className="card node-card">
      <div className="spread" style={{ alignItems: 'flex-start' }}>
        <div style={{ minWidth: 0 }}>
          <div className="node-card-name">{node.name}</div>
          <div className="muted small mono">{node.address}</div>
        </div>
        {!st ? (
          <Badge>checking…</Badge>
        ) : reachable ? (
          <Badge tone="ok" dot>
            online
          </Badge>
        ) : (
          <Badge tone="danger" dot>
            unreachable
          </Badge>
        )}
      </div>
      {reachable && <NodeSparklines points={metrics} clock={clock} />}
      {machine && (
        <div className="machine-strip small">
          <div className="row" style={{ gap: '0.4rem' }}>
            <Boxes size={14} />
            <span className="muted">
              VM <span className="mono">{machine.vm_name}</span> on {machine.hypervisor_name}
            </span>
          </div>
          <div className="row" style={{ gap: '0.3rem' }}>
            <ManagedBadge m={machine} />
            <PowerBadge power={vm?.power} />
            {machine.phase !== 'ready' && <PhaseBadge phase={machine.phase} />}
            <span className="grow" />
            <PowerButtons machine={machine} vm={vm} actions={machineActions} labels={node.labels} />
          </div>
        </div>
      )}
      <NodeLabels node={node} onChanged={onChanged} />
      {machine && <LockNotice m={machine} actions={machineActions} />}
      {st && !reachable && <div className="error-box small">{st.error || 'no answer'}</div>}
      {reachable && (
        <dl className="kv small">
          <dt>Host</dt>
          <dd className="mono">{st.hostname}</dd>
          <dt>Version</dt>
          <dd>
            <span className="mono">{st.version}</span> {st.prerelease && <PrereleaseBadge />} <SupportBadge support={st.support} />
            {st.security_update ? (
              <>
                {' '}
                <SecurityBadge severity={st.security_update}>{st.latest_release}</SecurityBadge>
              </>
            ) : (
              st.update_available && (
                <>
                  {' '}
                  <Badge tone="accent">
                    <Rocket size={11} /> {st.latest_release}
                  </Badge>
                </>
              )
            )}
          </dd>
          <dt>HAProxy</dt>
          <dd>
            <Badge tone={stateTone(st.haproxy_state)} dot>
              {st.haproxy_state || '–'}
            </Badge>{' '}
            {st.haproxy_health && <Badge tone={stateTone(st.haproxy_health)}>{st.haproxy_health}</Badge>}{' '}
            {st.haproxy_version && <span className="mono muted">{st.haproxy_version}</span>}
          </dd>
          <dt>Slot · uptime</dt>
          <dd>
            {st.active_slot ? `slot ${st.active_slot}` : 'no A/B'} · {uptime(st.boot_time_unix)}
          </dd>
          {st.kernel_version && (
            <>
              <dt>Kernel</dt>
              <dd>
                <span className="mono">{st.kernel_version}</span>
                {st.kernel_track && <span className="muted"> · {st.kernel_track}</span>}
              </dd>
            </>
          )}
          {fleet?.state === 'ready' && (
            <>
              <dt>Trust</dt>
              <dd>
                <TrustBadge fleet={fleet} nodeID={node.id} />
              </dd>
            </>
          )}
        </dl>
      )}
      <div className="row" style={{ marginTop: 'auto' }}>
        <button className="primary" onClick={() => openNode(node)}>
          Open <ArrowUpRight size={15} />
        </button>
        <span className="grow" />
        {!can('admin') ? null : machine ? (
          !machine.spec.locked && (
            <button className="ghost small danger" onClick={() => machineActions.destroy(machine)} disabled={machineActions.busy} title="Destroy its virtual machine">
              <Trash2 size={14} /> Destroy
            </button>
          )
        ) : (
          <button className="ghost small danger" onClick={() => onRemove(node)} title="Remove from this Controller">
            <Trash2 size={14} /> Remove
          </button>
        )}
      </div>
    </section>
  )
}

// NodeTable is the node list as rows of a table - many nodes at a glance:
// each one's state, its last five minutes, version, HAProxy, uptime and
// trust, and Open; its card has the rest (labels edited, a machine's
// power, kernel).
function NodeTable({ nodes, statuses, metrics, clock, machineOf, vmOf, fleet, onRemove, machineActions }) {
  const can = useCan()
  const trust = fleet?.state === 'ready'
  return (
    <div className="card node-table-card">
      <div className="table-wrap">
        <table className="node-table">
          <thead>
            <tr>
              <th>Node</th>
              <th>State</th>
              <th className="spark-col">CPU</th>
              <th className="spark-col">Req/s</th>
              <th className="spark-col">Traffic</th>
              <th>Version</th>
              <th>HAProxy</th>
              <th>Uptime</th>
              {trust && <th>Trust</th>}
              <th aria-label="Actions" />
            </tr>
          </thead>
          <tbody>
            {nodes.map((n) => {
              const st = statuses[n.id]
              const m = machineOf(n)
              const vm = vmOf(m)
              const up = st?.reachable
              return (
                <tr key={n.id}>
                  <td>
                    <div className="node-card-name">{n.name}</div>
                    <div className="muted small mono">{n.address}</div>
                    {Object.keys(n.labels || {}).length > 0 && (
                      <div className="row node-table-labels">
                        {Object.entries(n.labels).map(([k, v]) => (
                          <span key={k} className="badge mono">
                            {k}={v}
                          </span>
                        ))}
                      </div>
                    )}
                  </td>
                  <td>
                    <div className="row" style={{ gap: '0.3rem' }}>
                      {!st ? (
                        <Badge>checking…</Badge>
                      ) : up ? (
                        <Badge tone="ok" dot>
                          online
                        </Badge>
                      ) : (
                        <Badge tone="danger" dot>
                          unreachable
                        </Badge>
                      )}
                      {m && <PowerBadge power={vm?.power} />}
                    </div>
                    {st && !up && <div className="small" style={{ color: 'var(--danger)' }}>{st.error || 'no answer'}</div>}
                  </td>
                  {['cpu', 'req', 'net'].map((k) => (
                    <td key={k} className="spark-col">
                      {up && <NodeSpark kind={k} points={metrics[n.id]} clock={clock} head={false} />}
                    </td>
                  ))}
                  <td>
                    {up && (
                      <div className="row" style={{ gap: '0.3rem' }}>
                        <span className="mono small">{st.version}</span>
                        {st.prerelease && <PrereleaseBadge />}
                        {st.security_update ? (
                          <SecurityBadge severity={st.security_update}>{st.latest_release}</SecurityBadge>
                        ) : (
                          st.update_available && (
                            <Badge tone="accent">
                              <Rocket size={11} /> {st.latest_release}
                            </Badge>
                          )
                        )}
                      </div>
                    )}
                  </td>
                  <td>
                    {up && (
                      <div className="row" style={{ gap: '0.3rem' }}>
                        <Badge tone={stateTone(st.haproxy_state)} dot>
                          {st.haproxy_state || '–'}
                        </Badge>
                        {st.haproxy_version && <span className="mono muted small">{st.haproxy_version}</span>}
                      </div>
                    )}
                  </td>
                  <td className="nowrap">{up ? uptime(st.boot_time_unix) : ''}</td>
                  {trust && <td>{up && <TrustBadge fleet={fleet} nodeID={n.id} />}</td>}
                  <td className="actions">
                    <div className="row" style={{ gap: '0.3rem', flexWrap: 'nowrap', justifyContent: 'flex-end' }}>
                      <button className="primary small" onClick={() => openNode(n)}>
                        Open <ArrowUpRight size={13} />
                      </button>
                      {!can('admin') ? null : m ? (
                        !m.spec.locked && (
                          <button className="ghost icon danger" onClick={() => machineActions.destroy(m)} disabled={machineActions.busy} title="Destroy its virtual machine" aria-label={`Destroy ${n.name}'s virtual machine`}>
                            <Trash2 size={14} />
                          </button>
                        )
                      ) : (
                        <button className="ghost icon danger" onClick={() => onRemove(n)} title="Remove from this Controller" aria-label={`Remove ${n.name}`}>
                          <Trash2 size={14} />
                        </button>
                      )}
                    </div>
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>
    </div>
  )
}

// PendingList is the Tailscale-style admission queue: a node announced
// itself (see dashboard/backend/register.go) but isn't reachable until a
// human approves it here - never fully automatic.
function PendingList({ pending, onApprove, onReject, busy, machines }) {
  const can = useCan()
  if (!pending.length) return null
  // A machine this Controller created whose image is too old to present
  // its registration token: approving it links the node to the machine.
  const waiting = (name) => machines.find((m) => m.spec.name === name && !m.node_id && ['waiting-registration', 'failed'].includes(m.phase))
  return (
    <Card title={`Waiting for approval (${pending.length})`} icon={ShieldCheck} className="pending">
      <div className="stack" style={{ gap: '0.5rem' }}>
        {pending.map((p) => (
          <div key={p.id} className="spread pending-row">
            <div>
              <strong>{p.name}</strong> <span className="muted mono small">{p.address}</span>{' '}
              {waiting(p.name) && (
                <Badge tone="info">
                  <Boxes size={11} /> a machine created on {waiting(p.name).hypervisor_name}
                </Badge>
              )}
              <div className="muted small">announced {new Date(p.announced_at).toLocaleString()}</div>
              {p.ca_fingerprint && (
                <div className="muted small" title="The node's CA, as its console shows it when it announces itself: compare before approving">
                  CA <span className="mono">{p.ca_fingerprint.slice(0, 16)}…</span>
                  {p.keyless && ' · no key sent: it will trust the fleet'}
                </div>
              )}
            </div>
            {can('admin') && (
              <div className="row">
                <button className="primary small" disabled={busy} onClick={() => onApprove(p.id)}>
                  <Check size={14} /> Approve
                </button>
                <button className="small danger" disabled={busy} onClick={() => onReject(p.id)}>
                  <X size={14} /> Reject
                </button>
              </div>
            )}
          </div>
        ))}
      </div>
    </Card>
  )
}

// --- add node ---

const emptyPfx = { name: '', address: '', pfx_password: '' }
const emptyPem = { name: '', address: '', ca_cert_pem: '', bootstrap_cert_pem: '', bootstrap_key_pem: '' }

// Two ways to hand the Controller a one-time bootstrap admin credential
// for a node - see dashboard/backend/main.go's parseAddNodeRequest. .pfx
// upload is the default (the same file already imported into the browser
// to open a node's page); paste-PEM stays for scripts with PEM files in
// hand (hack/qemu-dashboard-test.sh drives that path).
function AddNodeForm({ onAdded, onClose }) {
  const [mode, setMode] = useState('pfx')
  const [pfx, setPfx] = useState(emptyPfx)
  const [file, setFile] = useState(null)
  const [pem, setPem] = useState(emptyPem)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const toast = useToast()

  async function submit(e) {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      if (mode === 'pfx') {
        if (!file) throw new Error('choose a .pfx file')
        const body = new FormData()
        body.set('name', pfx.name)
        body.set('address', pfx.address)
        body.set('pfx_password', pfx.pfx_password)
        body.set('pfx', file)
        // No Content-Type: fetch sets the multipart boundary itself.
        await call('/api/nodes', { method: 'POST', body })
      } else {
        await call('/api/nodes', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(pem) })
      }
      toast(`Added ${mode === 'pfx' ? pfx.name : pem.name}`)
      setPfx(emptyPfx)
      setFile(null)
      setPem(emptyPem)
      onAdded()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Card
      title="Add a node"
      icon={Plus}
      actions={
        <button className="ghost icon" onClick={onClose} aria-label="Close">
          <X size={16} />
        </button>
      }
    >
      <form className="stack" onSubmit={submit}>
        <div className="row">
          <label className="check">
            <input type="radio" checked={mode === 'pfx'} onChange={() => setMode('pfx')} /> Upload .pfx (recommended)
          </label>
          <label className="check">
            <input type="radio" checked={mode === 'pem'} onChange={() => setMode('pem')} /> Paste PEM (scripts/CI)
          </label>
        </div>
        {mode === 'pfx' ? (
          <>
            <div className="grid grid-2">
              <label className="field">
                <span>Name</span>
                <input value={pfx.name} onChange={(e) => setPfx({ ...pfx, name: e.target.value })} required />
              </label>
              <label className="field">
                <span>gRPC address (host:port, default port 9505)</span>
                <input value={pfx.address} onChange={(e) => setPfx({ ...pfx, address: e.target.value })} required placeholder="10.0.0.5:9505" />
              </label>
              <label className="field">
                <span>Bootstrap credential (.pfx, with the node's CA bundled)</span>
                <input type="file" accept=".pfx,.p12" onChange={(e) => setFile(e.target.files?.[0] ?? null)} required />
              </label>
              <label className="field">
                <span>.pfx password</span>
                <input type="password" value={pfx.pfx_password} onChange={(e) => setPfx({ ...pfx, pfx_password: e.target.value })} />
              </label>
            </div>
            <p className="muted small" style={{ margin: 0 }}>
              Used once, immediately, to issue this Controller its own dedicated credential - never stored. A .pfx must include the node's ca.crt (
              <code>openssl pkcs12 -export -certfile ca.crt …</code>).
            </p>
          </>
        ) : (
          <>
            <div className="grid grid-2">
              <label className="field">
                <span>Name</span>
                <input value={pem.name} onChange={(e) => setPem({ ...pem, name: e.target.value })} required />
              </label>
              <label className="field">
                <span>gRPC address (host:port)</span>
                <input value={pem.address} onChange={(e) => setPem({ ...pem, address: e.target.value })} required placeholder="10.0.0.5:9505" />
              </label>
            </div>
            <label className="field">
              <span>CA certificate (the node's ca.crt - not sensitive)</span>
              <textarea rows={4} value={pem.ca_cert_pem} onChange={(e) => setPem({ ...pem, ca_cert_pem: e.target.value })} required />
            </label>
            <div className="grid grid-2">
              <label className="field">
                <span>Bootstrap admin certificate</span>
                <textarea rows={4} value={pem.bootstrap_cert_pem} onChange={(e) => setPem({ ...pem, bootstrap_cert_pem: e.target.value })} required />
              </label>
              <label className="field">
                <span>Bootstrap admin key</span>
                <textarea rows={4} value={pem.bootstrap_key_pem} onChange={(e) => setPem({ ...pem, bootstrap_key_pem: e.target.value })} required />
              </label>
            </div>
          </>
        )}
        <ErrorBox error={error} />
        <div>
          <button className="primary" type="submit" disabled={busy}>
            {busy ? 'Adding…' : 'Add node'}
          </button>
        </div>
      </form>
    </Card>
  )
}

function CopyField({ label, value, inputRef, rows, onCopy }) {
  return (
    <label className="field">
      <span className="spread">
        {label}
        <button type="button" className="small ghost" onClick={() => onCopy(inputRef, label)}>
          <Copy size={13} /> Copy
        </button>
      </span>
      {rows ? <textarea ref={inputRef} readOnly rows={rows} value={value} /> : <input ref={inputRef} readOnly value={value} className="mono" />}
    </label>
  )
}

// ProvisionInfo hands an operator what `janusctl lifecycle install`'s
// -controller-address/-controller-ca need, so a new node self-registers
// with this Controller (GET /api/controller-info - the address is a
// best-effort suggestion, check it against the real network).
// An example network configuration for the provisioning panel - the
// NetworkConfig message's JSON form (see docs/network-configuration.md).
const NETWORK_EXAMPLE = `{
  "hostname": "lb1",
  "interfaces": [
    {"name": "eth0", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.0.2.10/24"], "gateway": "192.0.2.1"},
    {"name": "eth1", "mode": "ADDRESSING_MODE_NONE"},
    {"name": "eth1.100", "vlan": {"parent": "eth1", "id": 100}, "mode": "ADDRESSING_MODE_STATIC", "addresses": ["10.100.0.5/24"]}
  ],
  "dns": {"servers": ["192.0.2.53"]},
  "ntp": {"servers": ["ntp1.example.net", "ntp2.example.net"]}
}`

function ProvisionInfo() {
  const [info, setInfo] = useState(null)
  const [open, setOpen] = useState(false)
  const [network, setNetwork] = useState('')
  const [regToken, setRegToken] = useState('')
  const toast = useToast()
  const refs = { address: useRef(null), ca: useRef(null), root: useRef(null), command: useRef(null), seed: useRef(null), nocloud: useRef(null) }
  // Again each time it's opened: the fleet's root shows up once it's set
  // up.
  useEffect(() => {
    call('/api/controller-info')
      .then(setInfo)
      .catch(() => {})
  }, [open])
  if (!info) return null
  const address = info.address || 'YOUR-CONTROLLER-ADDRESS'
  let netCfg = null
  let netError = null
  if (network.trim()) {
    try {
      netCfg = JSON.parse(network)
    } catch (err) {
      netError = err.message
    }
  }
  const netFlag = netCfg ? ' -network-config network.json' : ''
  // With a fleet, the node checks this Controller through its root too.
  const rootFlag = info.fleet_root_pem ? ' -controller-fleet-root fleet-root.crt' : ''
  const tokenFlag = regToken.trim() ? ` -registration-token ${regToken.trim()}` : ''
  const command = `janusctl lifecycle install -controller-address ${address} -controller-ca controller-ca.crt${rootFlag}${tokenFlag}${netFlag} DISK BUNDLE_DIR`
  const seed = `janusctl image seed-controller -controller-address ${address} -controller-ca controller-ca.crt${rootFlag}${tokenFlag} DISK.raw${netCfg ? '\njanusctl image seed-network -config network.json DISK.raw' : ''}`
  const nocloud = JSON.stringify(
    {
      controller_address: address,
      controller_ca_cert: info.ca_cert_pem,
      ...(info.fleet_root_pem ? { controller_fleet_root_cert: info.fleet_root_pem } : {}),
      ...(regToken.trim() ? { registration_token: regToken.trim() } : {}),
      ...(netCfg ? { network: netCfg } : {}),
    },
    null,
    2,
  )
  const copy = async (ref, label) => {
    try {
      await navigator.clipboard.writeText(ref.current.value)
      toast(`${label} copied`)
    } catch {
      ref.current.select()
      toast(`Select the ${label.toLowerCase()} and press Ctrl+C`, 'warn')
    }
  }
  return (
    <Card
      title="Provision new nodes with this Controller"
      icon={Server}
      actions={
        <button className="small ghost" onClick={() => setOpen(!open)}>
          {open ? 'Hide' : 'Show'} <ChevronDown size={14} style={{ transform: open ? 'rotate(180deg)' : undefined }} />
        </button>
      }
    >
      <p className="muted" style={{ margin: 0 }}>
        A node installed with this Controller's address and CA announces itself on first boot and shows up above for approval.
      </p>
      {open && (
        <div className="stack" style={{ marginTop: '0.8rem' }}>
          {!info.address && <div className="notice warn">No address could be guessed - set -advertise-address on dashboardd, or fill it in yourself.</div>}
          <CopyField label="Controller address" value={address} inputRef={refs.address} onCopy={copy} />
          <CopyField label="Controller CA certificate (save as controller-ca.crt)" value={info.ca_cert_pem} inputRef={refs.ca} rows={5} onCopy={copy} />
          {info.fleet_root_pem && (
            <CopyField
              label="Fleet root (save as fleet-root.crt) - nodes of this release on check the Controller through it, and only take this fleet's trust"
              value={info.fleet_root_pem}
              inputRef={refs.root}
              rows={5}
              onCopy={copy}
            />
          )}
          <div className="field">
            <div className="spread">
              <label htmlFor="provision-network" className="field-label">
                Network configuration (optional - without one, the node takes the kernel&apos;s DHCP lease at boot, and NTP from DHCP or pool.ntp.org)
              </label>
              <button className="small ghost" type="button" onClick={() => setNetwork(NETWORK_EXAMPLE)}>
                Example
              </button>
            </div>
            <textarea
              id="provision-network"
              className="mono"
              rows={network ? 10 : 2}
              spellCheck={false}
              placeholder="save as network.json"
              value={network}
              onChange={(e) => setNetwork(e.target.value)}
            />
            {netError && (
              <span className="small" style={{ color: 'var(--danger)' }}>
                Not valid JSON: {netError}
              </span>
            )}
          </div>
          <label className="field">
            <span>Enrollment token (optional - an enrollment token below admits the nodes without approval, labelled)</span>
            <input className="mono" value={regToken} onChange={(e) => setRegToken(e.target.value)} placeholder="janus-enroll_…" />
          </label>
          <CopyField label="Install command (fill in DISK and BUNDLE_DIR)" value={command} inputRef={refs.command} rows={2} onCopy={copy} />
          <CopyField label="Or seed an already-built raw image offline" value={seed} inputRef={refs.seed} rows={2} onCopy={copy} />
          <CopyField label="Or NoCloud user-data (a cidata-labeled volume attached at first boot)" value={nocloud} inputRef={refs.nocloud} rows={6} onCopy={copy} />
        </div>
      )}
    </Card>
  )
}

// --- auth ---

function AuthScreen({ title, children }) {
  return (
    <div className="auth-screen">
      <div className="auth-box card">
        <div className="auth-brand">
          <BrandMark size={44} />
          <div>
            <h1>Janus Controller</h1>
            <div className="muted">{title}</div>
          </div>
        </div>
        {children}
      </div>
      <div className="auth-theme">
        <ThemePicker />
      </div>
    </div>
  )
}

// SetupForm is forced on the very first visit (see dashboard/backend/
// internal/auth): the first account, an admin.
function SetupForm({ onDone, onRestore }) {
  const [name, setName] = useState('admin')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [mfa, setMFA] = useState(true)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  async function submit(e) {
    e.preventDefault()
    if (password !== confirm) return setError('passwords do not match')
    setBusy(true)
    setError(null)
    try {
      await postJSON('/api/auth/setup', { name: name.trim(), password, mfa_required: mfa ? 'admins' : 'nobody' })
      onDone()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <AuthScreen title="First run - make the first admin account">
      <form className="stack" onSubmit={submit}>
        <label className="field">
          <span>Name</span>
          <input value={name} onChange={(e) => setName(e.target.value.toLowerCase())} required pattern="[a-z0-9][a-z0-9._@\-]{0,63}" autoComplete="username" />
        </label>
        <label className="field">
          <span>Password (at least 8 characters)</span>
          <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} required minLength={8} maxLength={72} autoFocus autoComplete="new-password" />
        </label>
        <label className="field">
          <span>Confirm password</span>
          <input type="password" value={confirm} onChange={(e) => setConfirm(e.target.value)} required minLength={8} maxLength={72} autoComplete="new-password" />
        </label>
        <label className="check">
          <input type="checkbox" checked={mfa} onChange={(e) => setMFA(e.target.checked)} />
          <span>Admins need a second factor - an authenticator app or a passkey (recommended; you set yours up next)</span>
        </label>
        <ErrorBox error={error} />
        <button className="primary" type="submit" disabled={busy}>
          {busy ? 'Setting up…' : 'Make the account and continue'}
        </button>
        <button type="button" className="ghost small" onClick={onRestore}>
          Restore a backup instead
        </button>
      </form>
    </AuthScreen>
  )
}

function LoginForm({ onDone, ended, sso }) {
  const [name, setName] = useState(() => {
    try {
      return localStorage.getItem('janus-last-user') || ''
    } catch {
      return ''
    }
  })
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  async function submit(e) {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await postJSON('/api/auth/login', { name: name.trim(), password })
      try {
        localStorage.setItem('janus-last-user', name.trim())
      } catch {
        // remembered for convenience only
      }
      onDone()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <AuthScreen title="Sign in">
      <form className="stack" onSubmit={submit}>
        {ended && <div className="notice small">Your session ended - sign in again.</div>}
        <label className="field">
          <span>Name</span>
          <input value={name} onChange={(e) => setName(e.target.value.toLowerCase())} required autoFocus={!name} autoComplete="username" />
        </label>
        <label className="field">
          <span>Password</span>
          <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} required autoFocus={!!name} autoComplete="current-password" />
        </label>
        <ErrorBox error={error} />
        <button className="primary" type="submit" disabled={busy}>
          {busy ? 'Signing in…' : 'Sign in'}
        </button>
      </form>
      <SSOButtons providers={sso} />
    </AuthScreen>
  )
}

// PasswordForm changes the account's own password - forced when someone
// else set it (an admin, a reset from the host).
function PasswordForm({ onDone, forced, onCancel }) {
  const [current, setCurrent] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  async function submit(e) {
    e.preventDefault()
    if (password !== confirm) return setError('passwords do not match')
    setBusy(true)
    setError(null)
    try {
      await postJSON('/api/auth/password', { current_password: current, new_password: password })
      onDone()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <form className="stack" onSubmit={submit}>
      {forced && <div className="notice small">This password was given to you: choose your own to continue.</div>}
      <label className="field">
        <span>{forced ? 'Password you were given' : 'Current password'}</span>
        <input type="password" value={current} onChange={(e) => setCurrent(e.target.value)} required autoFocus autoComplete="current-password" />
      </label>
      <label className="field">
        <span>New password (at least 8 characters)</span>
        <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} required minLength={8} maxLength={72} autoComplete="new-password" />
      </label>
      <label className="field">
        <span>Confirm the new password</span>
        <input type="password" value={confirm} onChange={(e) => setConfirm(e.target.value)} required minLength={8} maxLength={72} autoComplete="new-password" />
      </label>
      <ErrorBox error={error} />
      <div className="row" style={{ justifyContent: 'flex-end' }}>
        {onCancel && (
          <button type="button" onClick={onCancel}>
            Cancel
          </button>
        )}
        <button className="primary" type="submit" disabled={busy}>
          {busy ? 'Changing…' : 'Change password'}
        </button>
      </div>
      <div className="muted small">Your other sessions end: sign in again there.</div>
    </form>
  )
}

// signOut ends the session - and the single sign-on provider's it came
// through, when the Controller answers where (then the provider sends
// the browser back to the page).
const signOut = async () => {
  const resp = await fetch('/api/auth/logout', { method: 'POST' })
  if (resp.status === 200) {
    const out = await resp.json().catch(() => null)
    if (out?.redirect) return window.location.assign(out.redirect)
  }
  window.location.reload()
}

// AuthGate renders setup, sign-in, a forced password change, or the app -
// whatever /api/auth/status says, re-checked after each of them, and when
// a request finds the session ended.
function AuthGate({ children }) {
  const [status, setStatus] = useState(null)
  const [error, setError] = useState(null)
  const [ended, setEnded] = useState(false)
  const [restoring, setRestoring] = useState(false)
  // ssoDone: back from a single sign-on provider - finished before
  // anything else (SSO.jsx).
  const [ssoDone, setSSODone] = useState(() => window.location.hash.startsWith(SSO_DONE))
  const refreshStatus = useCallback(() => {
    call('/api/auth/status')
      .then(setStatus)
      .catch((err) => setError(err.message))
  }, [])
  useEffect(() => {
    refreshStatus()
    const onEnded = () => {
      setEnded(true)
      refreshStatus()
    }
    window.addEventListener(SIGNED_OUT, onEnded)
    return () => window.removeEventListener(SIGNED_OUT, onEnded)
  }, [refreshStatus])
  // The splash leaves at once for a form (sign-in, setup, a second
  // factor...), and comes back for the page's first load after it.
  const needs = status?.user?.needs || []
  const inside = !!status && !ssoDone && !status.setup_required && status.authenticated && !['mfa', 'password', 'mfa_enroll'].some((n) => needs.includes(n))
  const { finish, reset } = useSplash()
  const sawForm = useRef(false)
  useSplashStep('session', 'Signing in…', !!status || !!error)
  useEffect(() => {
    if (!status && !error) return
    if (!inside) {
      sawForm.current = true
      finish()
    } else if (sawForm.current) {
      sawForm.current = false
      reset()
    }
  }, [status, error, inside, finish, reset])
  if (error)
    return (
      <div className="auth-screen">
        <ErrorBox error={error} />
      </div>
    )
  if (ssoDone)
    return (
      <AuthScreen title="Single sign-on">
        <SSODone onDone={() => (setSSODone(false), setEnded(false), refreshStatus())} />
      </AuthScreen>
    )
  if (!status) return null
  if (status.setup_required)
    return restoring ? (
      <AuthScreen title="First run - restore a backup">
        <Suspense fallback={<Loading />}>
          <RestoreForm onCancel={() => setRestoring(false)} />
        </Suspense>
      </AuthScreen>
    ) : (
      <SetupForm onDone={refreshStatus} onRestore={() => setRestoring(true)} />
    )
  if (!status.authenticated) return <LoginForm onDone={() => (setEnded(false), refreshStatus())} ended={ended} sso={status.sso} />
  if (needs.includes('mfa'))
    return (
      <AuthScreen title={`Second factor for ${status.user.name}`}>
        <SecondFactorForm me={status.user} onDone={refreshStatus} onSignOut={signOut} />
      </AuthScreen>
    )
  if (needs.includes('password'))
    return (
      <AuthScreen title={`Signed in as ${status.user.name}`}>
        <PasswordForm forced onDone={refreshStatus} />
        <button className="ghost small" onClick={signOut}>
          <LogOut size={14} /> Sign out
        </button>
      </AuthScreen>
    )
  if (needs.includes('mfa_enroll'))
    return (
      <AuthScreen title={`Secure ${status.user.name}'s sign-in`}>
        <EnrollMFA me={status.user} onDone={refreshStatus} onSignOut={signOut} />
      </AuthScreen>
    )
  return <MeContext.Provider value={{ ...status.user, sso_providers: status.sso, refresh: refreshStatus }}>{children}</MeContext.Provider>
}

// signedInThrough says how the session signed in: through a single
// sign-on provider (by its name on the sign-in page), or a password.
function signedInThrough(me) {
  if (!me.provider) return 'signed in with your password'
  const p = (me.sso_providers || []).find((x) => x.id === me.provider)
  return `signed in through ${p?.name || me.provider}`
}

// AccountButton is who's signed in - opening their password change.
function AccountButton() {
  const me = useMe()
  const route = useHashRoute()
  // #/account?link=<provider>: opened by itself, on linking that
  // provider's identity (a sign-in refused for a name this account has).
  const link = route.startsWith(LINK.slice(1)) ? decodeURIComponent(route.slice(LINK.length - 1)) : ''
  const [open, setOpenState] = useState(!!link)
  const setOpen = (v) => {
    setOpenState(v)
    if (!v && route.startsWith('/account')) navigate('/')
  }
  const toast = useToast()
  return (
    <>
      <button className="ghost" onClick={() => setOpen(true)} title={`Your account - ${signedInThrough(me)}`}>
        <UserRound size={15} /> {me.name} <Badge tone={me.role === 'admin' ? 'accent' : me.role === 'operator' ? 'info' : undefined}>{me.role || 'scoped'}</Badge>
      </button>
      {open &&
        // On the page itself: the top bar's backdrop blur would hold a
        // fixed dialog inside it.
        createPortal(
          <div className="modal-backdrop" onClick={() => setOpen(false)}>
            <div className="modal card account-modal" role="dialog" aria-modal="true" onClick={(e) => e.stopPropagation()}>
              <div className="muted small" style={{ marginBottom: '0.8rem' }}>
                This session: {signedInThrough(me)}.
              </div>
              <h2 style={{ marginBottom: '0.6rem' }}>Second factors</h2>
              <MFAPanel me={me} onChanged={me.refresh} />
              {(me.mfa?.totp || me.mfa?.passkeys?.length > 0) && (
                <>
                  <h2 style={{ margin: '1.2rem 0 0.6rem' }}>Trusted browsers</h2>
                  <TrustedBrowsers me={me} />
                </>
              )}
              <h2 style={{ margin: '1.2rem 0 0.6rem' }}>SSH keys for janusctl</h2>
              <SSHKeys me={me} />
              {(me.sso || me.identities?.length > 0 || me.sso_providers?.length > 0) && (
                <>
                  <h2 style={{ margin: '1.2rem 0 0.6rem' }}>Single sign-on</h2>
                  <Identities me={me} link={link} />
                </>
              )}
              {me.has_password && (
                <>
                  <h2 style={{ margin: '1.2rem 0 0.6rem' }}>Change your password</h2>
                  <PasswordForm
                    onCancel={() => setOpen(false)}
                    onDone={() => {
                      setOpen(false)
                      toast('Password changed')
                    }}
                  />
                </>
              )}
            </div>
          </div>,
          document.body,
        )}
    </>
  )
}

// --- main ---

const STATUS_EVERY = 15000

function MainApp() {
  const route = useHashRoute()
  const can = useCan()
  const tab = ['hypervisors', 'tokens', 'accounts', 'audit', 'log-forwarding', 'letsencrypt', 'backups'].find((t) => route.startsWith(`/${t}`)) || 'nodes'
  const [nodes, setNodes] = useState(null)
  const [pending, setPending] = useState([])
  const [statuses, setStatuses] = useState({})
  const [hypervisors, setHypervisors] = useState([])
  const [machines, setMachines] = useState([])
  const [hvStatus, setHvStatus] = useState({})
  const [consoleOf, setConsoleOf] = useState(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [adding, setAdding] = useState(false)
  const [version, setVersion] = useState('')
  const [fleet, setFleet] = useState(null)
  const [backups, setBackups] = useState(null)
  const [statusLoaded, setStatusLoaded] = useState(false)
  const metrics = useNodeMetrics(tab === 'nodes' && (nodes?.length ?? 0) > 0, SPARK_WINDOW)
  const [nodesView, setNodesView] = useStoredView('janus-nodes-view')
  const isReader = can('reader')
  const confirm = useConfirm()
  const toast = useToast()

  const refresh = useCallback(async () => {
    try {
      const bg = { background: true }
      const [n, p, h, m] = await Promise.all([call('/api/nodes', bg), call('/api/pending', bg), call('/api/hypervisors', bg), call('/api/machines', bg)])
      setNodes(n ?? [])
      setPending(p ?? [])
      setHypervisors(h ?? [])
      setMachines(m ?? [])
      setError(null)
      if (isReader)
        call('/api/fleet', bg)
          .then(setFleet)
          .catch(() => {})
      return h ?? []
    } catch (err) {
      setError(err.message)
      return []
    }
  }, [isReader])
  const refreshStatus = useCallback((hvs) => {
    const done = call('/api/nodes/status', { background: true })
      .then((s) => setStatuses(s || {}))
      .catch(() => {})
      .finally(() => setStatusLoaded(true))
    for (const hv of hvs || []) {
      if (!hv.trusted) continue
      call(`/api/hypervisors/${hv.id}/status`, { background: true })
        .then((st) => setHvStatus((all) => ({ ...all, [hv.id]: st })))
        .catch(() => {})
    }
    return done
  }, [])
  const isAdmin = can('admin')
  // The header's Refresh: the releases asked from GitHub again too, past
  // the Controller's cache - a release just published shows.
  const [checkNow, setCheckNow] = useState(0)
  const reload = useCallback(async () => {
    if (isAdmin)
      call('/api/backups', { background: true })
        .then(setBackups)
        .catch(() => {})
    return refreshStatus(await refresh())
  }, [refresh, refreshStatus, isAdmin])

  // Faster while a machine is being created or destroyed: its phase moves
  // every few seconds.
  const working = machines.some((m) => !['ready', 'failed'].includes(m.phase))
  useEffect(() => {
    reload()
    const t = setInterval(reload, working ? 3000 : STATUS_EVERY)
    return () => clearInterval(t)
  }, [reload, working])
  const count = nodes?.length ?? 0
  useSplashStep('inventory', 'Loading the fleet…', nodes !== null || error !== null)
  useSplashStep('status', `Asking ${count} node${count === 1 ? '' : 's'} for their status…`, statusLoaded)
  const machineActions = useMachineActions({ onChanged: reload, onConsole: setConsoleOf })
  const machineOf = (node) => machines.find((m) => m.id === node.machine_id)
  const vmOf = (m) => m && hvStatus[m.spec.hypervisor_id]?.machines?.[m.id]
  useEffect(() => {
    call('/api/version')
      .then((v) => setVersion(v?.version || ''))
      .catch(() => {})
  }, [])

  const act = async (fn, success) => {
    setBusy(true)
    try {
      await fn()
      if (success) toast(success)
      await reload()
    } catch (err) {
      toast(err, 'danger')
    } finally {
      setBusy(false)
    }
  }
  const remove = async (node) => {
    const ok = await confirm({
      title: `Remove ${node.name}?`,
      body: <p>The Controller forgets this node and its credential. The node itself keeps running untouched; you can add it again later.</p>,
      action: 'Remove',
      danger: true,
    })
    if (ok) act(() => call(`/api/nodes/${node.id}`, { method: 'DELETE' }), `Removed ${node.name}`)
  }
  const approve = (id) => act(() => call(`/api/pending/${id}/approve`, { method: 'POST' }), 'Node approved')
  const reject = (id) => act(() => call(`/api/pending/${id}/reject`, { method: 'POST' }), 'Registration rejected')

  const values = Object.values(statuses)
  const online = values.filter((s) => s.reachable).length
  const down = values.filter((s) => !s.reachable).length
  const updates = values.filter((s) => s.update_available).length
  const security = values.filter((s) => s.security_update)

  return (
    <div className="main-page">
      <header className="topbar">
        <div className="row" style={{ gap: '0.6rem' }}>
          <BrandMark size={26} />
          <div>
            <div className="node-name">Janus Controller</div>
            <div className="muted small">
              {version && (
                <span className="mono" title="This Controller's version">
                  {version}
                </span>
              )}
              {version && ' · '}
              {nodes ? `${nodes.length} node${nodes.length === 1 ? '' : 's'}` : '…'}
            </div>
          </div>
        </div>
        <div className="row">
          {online > 0 && (
            <Badge tone="ok" dot>
              {online} online
            </Badge>
          )}
          {down > 0 && (
            <Badge tone="danger" dot>
              {down} unreachable
            </Badge>
          )}
          {security.length > 0 && (
            <SecurityBadge severity={worstSeverity(security.map((s) => s.security_update))}>
              {security.length} security update{security.length === 1 ? '' : 's'}
            </SecurityBadge>
          )}
          {updates > 0 && (
            <Badge tone="accent">
              {updates} update{updates === 1 ? '' : 's'} available
            </Badge>
          )}
          {pending.length > 0 && <Badge tone="warn">{pending.length} pending</Badge>}
          {isAdmin && backups?.alert && (
            <a
              href="#/backups"
              className="badge-link"
              title={backupAlert(backups)}
              onClick={(e) => {
                e.preventDefault()
                navigate('/backups')
              }}
            >
              <Badge tone={backups.alert === 'failed' ? 'danger' : 'warn'} dot>
                {backups.alert === 'failed' ? 'Backup failing' : 'Backup late'}
              </Badge>
            </a>
          )}
        </div>
        <span className="grow" />
        <RefreshButton
          title="Refresh"
          onRefresh={() => {
            setCheckNow((n) => n + 1)
            return reload()
          }}
        />
        <ThemePicker />
        <AccountButton />
        <button className="ghost" onClick={signOut}>
          <LogOut size={15} /> Log out
        </button>
      </header>
      <main className="content">
        <Tabs
          tabs={[
            { id: 'nodes', label: `Nodes${nodes ? ` (${nodes.length})` : ''}`, icon: Server },
            ...(can('reader') ? [{ id: 'hypervisors', label: `Hypervisors${hypervisors.length ? ` (${hypervisors.length})` : ''}`, icon: Boxes }] : []),
            { id: 'tokens', label: 'API tokens', icon: KeyRound },
            ...(can('admin')
              ? [
                  { id: 'accounts', label: 'Accounts', icon: Users },
                  { id: 'audit', label: 'Audit', icon: ScrollText },
                  { id: 'log-forwarding', label: 'Log forwarding', icon: Send },
                  { id: 'letsencrypt', label: 'Let’s Encrypt', icon: LockKeyhole },
                  { id: 'backups', label: 'Backups', icon: Archive },
                ]
              : []),
          ]}
          active={tab}
          onChange={(id) => navigate(id === 'nodes' ? '/' : `/${id}`)}
        />
        {consoleOf && (
          <Suspense fallback={null}>
            <MachineConsole machine={consoleOf} onClose={() => setConsoleOf(null)} />
          </Suspense>
        )}
        <Suspense fallback={<Loading />}>
        {route.startsWith('/cli-login') ? (
          <CliLoginPage route={route} />
        ) : route.startsWith('/cli-device') ? (
          <CliDevicePage route={route} />
        ) : tab === 'tokens' ? (
          <TokensPage />
        ) : tab === 'accounts' && can('admin') ? (
          <UsersPage />
        ) : tab === 'audit' && can('admin') ? (
          <AuditPage />
        ) : tab === 'log-forwarding' && can('admin') ? (
          <LogForwardingPage />
        ) : tab === 'letsencrypt' && can('admin') ? (
          <LetsEncryptPage />
        ) : tab === 'backups' && can('admin') ? (
          <BackupsPage />
        ) : tab === 'hypervisors' ? (
          <HypervisorsPage hypervisors={hypervisors} machines={machines} hvStatus={hvStatus} onChanged={reload} onConsole={setConsoleOf} />
        ) : (
          <div className="stack">
            {error && <ErrorBox error={error} />}
            {can('reader') && <ControllerUpdate checkNow={checkNow} />}
            {can('admin') && <FleetSetup fleet={fleet} onChanged={reload} />}
            <PendingList pending={pending} onApprove={approve} onReject={reject} busy={busy} machines={machines} />
            <div className="spread">
              <h1>Nodes</h1>
              <div className="row">
                {nodes?.length > 0 && (
                  <ViewToggle value={nodesView} onChange={setNodesView} label="Show the nodes as" />
                )}
                {!adding && can('admin') && (
                  <button className="primary" onClick={() => setAdding(true)}>
                    <Plus size={15} /> Add node
                  </button>
                )}
              </div>
            </div>
            {adding && (
              <AddNodeForm
                onAdded={() => {
                  setAdding(false)
                  reload()
                }}
                onClose={() => setAdding(false)}
              />
            )}
            {nodes && nodes.length === 0 && !adding && (
              <div className="card empty">
                {can('admin') ? (
                  <>
                    No node yet.{' '}
                    <button className="small" onClick={() => setAdding(true)}>
                      Add one
                    </button>{' '}
                    with its admin certificate, or provision new ones to self-register (below).
                  </>
                ) : (
                  'No node yet.'
                )}
              </div>
            )}
            {nodesView === 'list' && nodes?.length > 0 ? (
              <NodeTable
                nodes={nodes}
                statuses={statuses}
                metrics={metrics.byNode}
                clock={metrics.clock}
                machineOf={machineOf}
                vmOf={vmOf}
                fleet={fleet}
                onRemove={remove}
                machineActions={machineActions}
              />
            ) : (
            <div className="node-grid">
              {(nodes || []).map((n) => (
                <NodeCard
                  key={n.id}
                  node={n}
                  status={statuses[n.id]}
                  onRemove={remove}
                  machine={machineOf(n)}
                  vm={vmOf(machineOf(n))}
                  machineActions={machineActions}
                  fleet={fleet}
                  onChanged={reload}
                  metrics={metrics.byNode[n.id]}
                  clock={metrics.clock}
                />
              ))}
            </div>
            )}
            <FleetCard fleet={fleet} nodes={nodes} />
            {can('admin') && <ProvisionInfo />}
            {can('admin') && <EnrollTokens />}
            {can('admin') && <HTTPSCertificate />}
          </div>
        )}
        </Suspense>
      </main>
    </div>
  )
}

export default function App() {
  return (
    <AuthGate>
      <MainApp />
    </AuthGate>
  )
}
