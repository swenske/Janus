import { ArrowUpRight, Boxes, Check, ChevronDown, Copy, KeyRound, LogOut, Plus, RefreshCw, Rocket, Server, ShieldCheck, Trash2, X } from 'lucide-react'
import { useCallback, useEffect, useRef, useState } from 'react'
import { call } from './call.js'
import MachineConsole from './Console.jsx'
import ControllerUpdate from './ControllerUpdate.jsx'
import { FleetCard, FleetSetup, TrustBadge } from './Fleet.jsx'
import HypervisorsPage, { LockNotice, ManagedBadge, PhaseBadge, PowerBadge, PowerButtons, useMachineActions } from './Hypervisors.jsx'
import { navigate, useHashRoute } from './shared/route.js'
import { Logo, ThemeToggle } from './shared/theme.jsx'
import TokensPage from './Tokens.jsx'
import { SecurityBadge } from './SecurityBadge.jsx'
import { worstSeverity } from './severity.js'
import { Badge, Card, ErrorBox, Tabs, stateTone, useConfirm, useToast } from './shared/ui.jsx'

// dashboardd serves this SPA and its REST API on the same origin (its own
// -addr) - see dashboard/backend/main.go. Each *node's own* page lives on
// a different origin (its own allocated port, see dashboard/backend/
// internal/nodeproxy): opening it is a full navigation, not a fetch - that
// origin needs a TLS client certificate the browser negotiates per origin.
function openNode(node) {
  window.open(`https://${window.location.hostname}:${node.port}/`, '_blank', 'noopener,noreferrer')
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
function NodeCard({ node, status, onRemove, machine, vm, machineActions, fleet }) {
  const st = status
  const reachable = st?.reachable
  return (
    <section className="card node-card">
      <div className="spread" style={{ alignItems: 'flex-start' }}>
        <div style={{ minWidth: 0 }}>
          <div className="node-card-name">{node.name}</div>
          <div className="muted small mono">{node.address}</div>
        </div>
        {!st ? <Badge>checking…</Badge> : reachable ? <Badge tone="ok" dot>online</Badge> : <Badge tone="danger" dot>unreachable</Badge>}
      </div>
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
            <PowerButtons machine={machine} vm={vm} actions={machineActions} />
          </div>
        </div>
      )}
      {machine && <LockNotice m={machine} actions={machineActions} />}
      {st && !reachable && <div className="error-box small">{st.error || 'no answer'}</div>}
      {reachable && (
        <dl className="kv small">
          <dt>Host</dt>
          <dd className="mono">{st.hostname}</dd>
          <dt>Version</dt>
          <dd>
            <span className="mono">{st.version}</span>
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
            {st.haproxy_health && <Badge tone={stateTone(st.haproxy_health)}>{st.haproxy_health}</Badge>}
          </dd>
          <dt>Slot · uptime</dt>
          <dd>
            {st.active_slot ? `slot ${st.active_slot}` : 'no A/B'} · {uptime(st.boot_time_unix)}
          </dd>
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
        {machine ? (
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

// PendingList is the Tailscale-style admission queue: a node announced
// itself (see dashboard/backend/register.go) but isn't reachable until a
// human approves it here - never fully automatic.
function PendingList({ pending, onApprove, onReject, busy, machines }) {
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
            </div>
            <div className="row">
              <button className="primary small" disabled={busy} onClick={() => onApprove(p.id)}>
                <Check size={14} /> Approve
              </button>
              <button className="small danger" disabled={busy} onClick={() => onReject(p.id)}>
                <X size={14} /> Reject
              </button>
            </div>
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
    <Card title="Add a node" icon={Plus} actions={<button className="ghost icon" onClick={onClose} aria-label="Close"><X size={16} /></button>}>
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
  const toast = useToast()
  const refs = { address: useRef(null), ca: useRef(null), command: useRef(null), seed: useRef(null), nocloud: useRef(null) }
  useEffect(() => {
    call('/api/controller-info').then(setInfo).catch(() => {})
  }, [])
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
  const command = `janusctl lifecycle install -controller-address ${address} -controller-ca controller-ca.crt${netFlag} DISK BUNDLE_DIR`
  const seed = `janusctl image seed-controller -controller-address ${address} -controller-ca controller-ca.crt DISK.raw${netCfg ? '\njanusctl image seed-network -config network.json DISK.raw' : ''}`
  const nocloud = JSON.stringify({ controller_address: address, controller_ca_cert: info.ca_cert_pem, ...(netCfg ? { network: netCfg } : {}) }, null, 2)
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
            {netError && <span className="small" style={{ color: 'var(--danger)' }}>Not valid JSON: {netError}</span>}
          </div>
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
          <Logo size={44} />
          <div>
            <h1>Janus Controller</h1>
            <div className="muted">{title}</div>
          </div>
        </div>
        {children}
      </div>
      <div className="auth-theme">
        <ThemeToggle />
      </div>
    </div>
  )
}

// SetupForm is forced on the very first visit (see dashboard/backend/
// internal/auth): one admin password, no username - a single-operator tool.
function SetupForm({ onDone }) {
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
      await call('/api/auth/setup', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ password }) })
      onDone()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <AuthScreen title="First run - set the admin password">
      <form className="stack" onSubmit={submit}>
        <label className="field">
          <span>Password (at least 8 characters)</span>
          <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} required minLength={8} autoFocus />
        </label>
        <label className="field">
          <span>Confirm password</span>
          <input type="password" value={confirm} onChange={(e) => setConfirm(e.target.value)} required minLength={8} />
        </label>
        <ErrorBox error={error} />
        <button className="primary" type="submit" disabled={busy}>
          {busy ? 'Setting up…' : 'Set password and continue'}
        </button>
      </form>
    </AuthScreen>
  )
}

function LoginForm({ onDone }) {
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  async function submit(e) {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await call('/api/auth/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ password }) })
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
        <label className="field">
          <span>Password</span>
          <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} required autoFocus />
        </label>
        <ErrorBox error={error} />
        <button className="primary" type="submit" disabled={busy}>
          {busy ? 'Signing in…' : 'Sign in'}
        </button>
      </form>
    </AuthScreen>
  )
}

// AuthGate renders setup, login, or the app - whatever /api/auth/status
// says, re-checked after each setup/login rather than assumed.
function AuthGate({ children }) {
  const [status, setStatus] = useState(null)
  const [error, setError] = useState(null)
  const refreshStatus = useCallback(() => {
    call('/api/auth/status').then(setStatus).catch((err) => setError(err.message))
  }, [])
  useEffect(() => {
    refreshStatus()
  }, [refreshStatus])
  if (error) return <div className="auth-screen"><ErrorBox error={error} /></div>
  if (!status) return null
  if (status.setup_required) return <SetupForm onDone={refreshStatus} />
  if (!status.authenticated) return <LoginForm onDone={refreshStatus} />
  return children
}

// --- main ---

const STATUS_EVERY = 15000

function MainApp() {
  const route = useHashRoute()
  const tab = route.startsWith('/hypervisors') ? 'hypervisors' : route.startsWith('/tokens') ? 'tokens' : 'nodes'
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
  const confirm = useConfirm()
  const toast = useToast()

  const refresh = useCallback(async () => {
    try {
      const [n, p, h, m] = await Promise.all([call('/api/nodes'), call('/api/pending'), call('/api/hypervisors'), call('/api/machines')])
      setNodes(n ?? [])
      setPending(p ?? [])
      setHypervisors(h ?? [])
      setMachines(m ?? [])
      setError(null)
      call('/api/fleet')
        .then(setFleet)
        .catch(() => {})
      return h ?? []
    } catch (err) {
      setError(err.message)
      return []
    }
  }, [])
  const refreshStatus = useCallback((hvs) => {
    call('/api/nodes/status')
      .then((s) => setStatuses(s || {}))
      .catch(() => {})
    for (const hv of hvs || []) {
      if (!hv.trusted) continue
      call(`/api/hypervisors/${hv.id}/status`)
        .then((st) => setHvStatus((all) => ({ ...all, [hv.id]: st })))
        .catch(() => {})
    }
  }, [])
  const reload = useCallback(async () => refreshStatus(await refresh()), [refresh, refreshStatus])

  // Faster while a machine is being created or destroyed: its phase moves
  // every few seconds.
  const working = machines.some((m) => !['ready', 'failed'].includes(m.phase))
  useEffect(() => {
    reload()
    const t = setInterval(reload, working ? 3000 : STATUS_EVERY)
    return () => clearInterval(t)
  }, [reload, working])
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
  const logout = async () => {
    await fetch('/api/auth/logout', { method: 'POST' })
    window.location.reload()
  }

  const values = Object.values(statuses)
  const online = values.filter((s) => s.reachable).length
  const down = values.filter((s) => !s.reachable).length
  const updates = values.filter((s) => s.update_available).length
  const security = values.filter((s) => s.security_update)

  return (
    <div className="main-page">
      <header className="topbar">
        <div className="row" style={{ gap: '0.6rem' }}>
          <Logo size={26} />
          <div>
            <div className="node-name">Janus Controller</div>
            <div className="muted small">
              {version && <span className="mono" title="This Controller's version">{version}</span>}
              {version && ' · '}
              {nodes ? `${nodes.length} node${nodes.length === 1 ? '' : 's'}` : '…'}
            </div>
          </div>
        </div>
        <div className="row">
          {online > 0 && <Badge tone="ok" dot>{online} online</Badge>}
          {down > 0 && <Badge tone="danger" dot>{down} unreachable</Badge>}
          {security.length > 0 && (
            <SecurityBadge severity={worstSeverity(security.map((s) => s.security_update))}>
              {security.length} security update{security.length === 1 ? '' : 's'}
            </SecurityBadge>
          )}
          {updates > 0 && <Badge tone="accent">{updates} update{updates === 1 ? '' : 's'} available</Badge>}
          {pending.length > 0 && <Badge tone="warn">{pending.length} pending</Badge>}
        </div>
        <span className="grow" />
        <button className="ghost icon" title="Refresh" onClick={reload}>
          <RefreshCw size={16} />
        </button>
        <ThemeToggle />
        <button className="ghost" onClick={logout}>
          <LogOut size={15} /> Log out
        </button>
      </header>
      <main className="content">
        <Tabs
          tabs={[
            { id: 'nodes', label: `Nodes${nodes ? ` (${nodes.length})` : ''}`, icon: Server },
            { id: 'hypervisors', label: `Hypervisors${hypervisors.length ? ` (${hypervisors.length})` : ''}`, icon: Boxes },
            { id: 'tokens', label: 'API tokens', icon: KeyRound },
          ]}
          active={tab}
          onChange={(id) => navigate(id === 'nodes' ? '/' : `/${id}`)}
        />
        {consoleOf && <MachineConsole machine={consoleOf} onClose={() => setConsoleOf(null)} />}
        {tab === 'tokens' ? (
          <TokensPage />
        ) : tab === 'hypervisors' ? (
          <HypervisorsPage hypervisors={hypervisors} machines={machines} hvStatus={hvStatus} onChanged={reload} onConsole={setConsoleOf} />
        ) : (
          <div className="stack">
            {error && <ErrorBox error={error} />}
            <ControllerUpdate />
            <FleetSetup fleet={fleet} onChanged={reload} />
            <PendingList pending={pending} onApprove={approve} onReject={reject} busy={busy} machines={machines} />
            <div className="spread">
              <h1>Nodes</h1>
              {!adding && (
                <button className="primary" onClick={() => setAdding(true)}>
                  <Plus size={15} /> Add node
                </button>
              )}
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
                No node yet. <button className="small" onClick={() => setAdding(true)}>Add one</button> with its admin certificate, or provision new ones to self-register
                (below).
              </div>
            )}
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
                />
              ))}
            </div>
            <FleetCard fleet={fleet} nodes={nodes} />
            <ProvisionInfo />
          </div>
        )}
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
