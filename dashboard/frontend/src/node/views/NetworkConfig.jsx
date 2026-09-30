import { Clock, Code2, Globe, ListTree, Plus, Save, Trash2, Undo2, Waypoints } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import { getJSON, postJSON } from '../api.js'
import { hunks, lineDiff } from '../diff.js'
import { dateTime, duration } from '../format.js'
import { usePoll } from '../hooks.jsx'
import { Badge, Card, Empty, ErrorBox, Loading, PageHeader, Tabs, useAction, useConfirm, useToast } from '../../shared/ui.jsx'

// The node's own network configuration: hostname, interfaces (physical
// and 802.1Q VLANs), DNS and NTP. Applying is a trial on the node: it
// reverts by itself unless this Controller confirms it, over an address
// the new configuration keeps, before the timeout.

const MODES = [
  { id: 'ADDRESSING_MODE_DHCP', label: 'DHCP (boot lease)' },
  { id: 'ADDRESSING_MODE_STATIC', label: 'Static' },
  { id: 'ADDRESSING_MODE_NONE', label: 'Up, no address' },
  { id: 'ADDRESSING_MODE_DISABLED', label: 'Disabled' },
]
const modeLabel = (m) => (MODES.find((x) => x.id === m) || MODES[0]).label.replace(' (boot lease)', '')

const list = (s) =>
  String(s || '')
    .split(/[\s,]+/)
    .map((x) => x.trim())
    .filter(Boolean)

// toDraft/fromDraft convert between the API's NetworkConfig JSON and the
// form's editable shape (strings for the list fields).
function toDraft(cfg) {
  return {
    hostname: cfg?.hostname || '',
    interfaces: (cfg?.interfaces || []).map((i) => ({
      name: i.name || '',
      mac: i.mac || '',
      isVlan: !!i.vlan,
      vlanParent: i.vlan?.parent || '',
      vlanId: i.vlan?.id ? String(i.vlan.id) : '',
      mode: i.mode || 'ADDRESSING_MODE_DHCP',
      addresses: (i.addresses || []).join('\n'),
      gateway: i.gateway || '',
      gateway6: i.gateway6 || '',
      mtu: i.mtu ? String(i.mtu) : '',
      metric: i.route_metric ? String(i.route_metric) : '',
    })),
    dns: (cfg?.dns?.servers || []).join(', '),
    search: (cfg?.dns?.search || []).join(', '),
    ntp: (cfg?.ntp?.servers || []).join(', '),
  }
}

function fromDraft(d) {
  const cfg = {}
  if (d.hostname.trim()) cfg.hostname = d.hostname.trim()
  if (d.interfaces.length) {
    cfg.interfaces = d.interfaces.map((i) => {
      const o = { name: i.name.trim() }
      if (i.isVlan) o.vlan = { parent: i.vlanParent.trim(), id: Number(i.vlanId) || 0 }
      else if (i.mac.trim()) o.mac = i.mac.trim().toLowerCase()
      if (i.mode !== 'ADDRESSING_MODE_DHCP') o.mode = i.mode
      if (i.mode === 'ADDRESSING_MODE_STATIC') {
        const a = list(i.addresses)
        if (a.length) o.addresses = a
        if (i.gateway.trim()) o.gateway = i.gateway.trim()
        if (i.gateway6.trim()) o.gateway6 = i.gateway6.trim()
      }
      if (Number(i.mtu)) o.mtu = Number(i.mtu)
      if (Number(i.metric)) o.route_metric = Number(i.metric)
      return o
    })
  }
  const dns = { servers: list(d.dns), search: list(d.search) }
  if (dns.servers.length || dns.search.length) cfg.dns = Object.fromEntries(Object.entries(dns).filter(([, v]) => v.length))
  const ntp = list(d.ntp)
  if (ntp.length) cfg.ntp = { servers: ntp }
  return cfg
}

const pretty = (cfg) => JSON.stringify(cfg, null, 2)

function DiffView({ diff }) {
  return (
    <div className="diff">
      {diff.map((d, i) => (
        <div key={i} className={d.op === '+' ? 'add' : d.op === '-' ? 'del' : 'ctx'}>
          {d.op === '…' ? '  …' : `${d.op} ${d.text}`}
        </div>
      ))}
    </div>
  )
}

function StatusPanel({ status }) {
  const t = status.time
  const trialLeft = status.trial_pending ? Math.max(0, Number(status.trial_revert_at_unix) - Date.now() / 1000) : 0
  return (
    <>
      {!status.managed && (
        <div className="notice warn" style={{ marginBottom: '1rem' }}>
          This janusd doesn't manage its host's network (it isn't running on a Janus node): what's below is observed only, and changes are refused.
        </div>
      )}
      {status.trial_pending && (
        <div className="notice warn" style={{ marginBottom: '1rem' }}>
          A configuration is on trial: the node reverts to the previous one in {duration(trialLeft)} unless it's confirmed.
        </div>
      )}
      <div className="grid grid-3" style={{ marginBottom: '1rem' }}>
        <Card title="Identity" icon={Globe}>
          <dl className="kv">
            <dt>Hostname</dt>
            <dd className="mono">{status.hostname}</dd>
            <dt>DNS</dt>
            <dd className="mono">{status.dns_servers?.join(', ') || <span className="muted">none</span>}</dd>
            {status.dns_search?.length > 0 && (
              <>
                <dt>Search</dt>
                <dd className="mono">{status.dns_search.join(', ')}</dd>
              </>
            )}
          </dl>
        </Card>
        <Card title="Clock" icon={Clock}>
          {!t ? (
            <span className="muted">…</span>
          ) : (
            <dl className="kv">
              <dt>State</dt>
              <dd>{t.synchronized ? <Badge tone="ok" dot>synchronized</Badge> : <Badge tone="warn" dot>not synchronized</Badge>}</dd>
              <dt>Servers</dt>
              <dd className="mono">
                {t.servers?.join(', ') || '-'} <span className="muted">({t.source})</span>
              </dd>
              {Number(t.last_sync_unix) > 0 && (
                <>
                  <dt>Last sync</dt>
                  <dd>
                    {dateTime(Number(t.last_sync_unix) * 1000)} <span className="muted mono">{t.last_server}</span>
                  </dd>
                  <dt>Offset</dt>
                  <dd className="mono">{(Number(t.offset_ns) / 1e6).toFixed(3)} ms</dd>
                </>
              )}
              {t.error && (
                <>
                  <dt>Last error</dt>
                  <dd className="small">{t.error}</dd>
                </>
              )}
            </dl>
          )}
        </Card>
        <Card title="Routes" icon={Waypoints}>
          {!status.routes?.length ? (
            <Empty>No routes</Empty>
          ) : (
            <div className="stack small mono">
              {status.routes
                // Default and subnet routes; not link-local, multicast or
                // single-host ones.
                .filter((r) => r.destination === 'default' || !(/^(fe80|ff)/.test(r.destination) || r.destination.endsWith('/128') || r.destination.endsWith('/32')))
                .map((r, i) => (
                  <div key={i}>
                    {r.destination}
                    {r.gateway && ` via ${r.gateway}`} <span className="muted">dev {r.interface}</span>
                    {r.metric > 0 && <span className="muted"> metric {r.metric}</span>}
                  </div>
                ))}
            </div>
          )}
        </Card>
      </div>
      <Card title="Interfaces" icon={ListTree}>
        <table className="table">
          <thead>
            <tr>
              <th>Name</th>
              <th>Kind</th>
              <th>MAC</th>
              <th>State</th>
              <th>Mode</th>
              <th>Addresses</th>
            </tr>
          </thead>
          <tbody>
            {status.interfaces
              ?.filter((i) => i.kind !== 'loopback')
              .map((i) => (
                <tr key={i.name}>
                  <td className="mono">{i.name}</td>
                  <td>{i.vlan_parent ? `VLAN ${i.vlan_id} on ${i.vlan_parent}` : i.kind}</td>
                  <td className="mono small">{i.mac}</td>
                  <td>
                    <Badge tone={!i.up ? '' : i.carrier ? 'ok' : 'warn'} dot>
                      {!i.up ? 'down' : i.carrier ? 'up' : 'no carrier'}
                    </Badge>{' '}
                    <span className="muted small">mtu {i.mtu}</span>
                  </td>
                  <td>{status.managed && (i.kind === 'physical' || i.kind === 'vlan') ? modeLabel(i.mode) : <span className="muted">-</span>}</td>
                  <td className="mono small">
                    {i.addresses?.map((a) => (
                      <div key={a}>{a}</div>
                    ))}
                    {i.dhcp && (
                      <div className="muted" title="Obtained by the kernel at boot, never renewed">
                        DHCP from {i.dhcp.server}, router {i.dhcp.router}
                        {i.dhcp.ntp_servers?.length > 0 && `, NTP ${i.dhcp.ntp_servers.join(' ')}`}
                      </div>
                    )}
                  </td>
                </tr>
              ))}
          </tbody>
        </table>
      </Card>
    </>
  )
}

function InterfaceEditor({ iface, onChange, onRemove, physical }) {
  const set = (k) => (e) => onChange({ ...iface, [k]: e.target.type === 'checkbox' ? e.target.checked : e.target.value })
  const staticMode = iface.mode === 'ADDRESSING_MODE_STATIC'
  return (
    <div className="card" style={{ padding: '0.8rem', marginBottom: '0.6rem' }}>
      <div className="grid grid-3">
        <label className="field">
          <span>Name</span>
          <input className="mono" value={iface.name} placeholder={iface.isVlan ? 'eth0.100' : 'eth0'} onChange={set('name')} />
        </label>
        {iface.isVlan ? (
          <div className="grid grid-2">
            <label className="field">
              <span>VLAN parent</span>
              <input className="mono" list="net-physical" value={iface.vlanParent} onChange={set('vlanParent')} />
            </label>
            <label className="field">
              <span>VLAN ID</span>
              <input type="number" min={1} max={4094} value={iface.vlanId} onChange={set('vlanId')} />
            </label>
          </div>
        ) : (
          <label className="field">
            <span>Match by MAC (renames it to Name)</span>
            <input className="mono" list="net-macs" value={iface.mac} placeholder="optional" onChange={set('mac')} />
          </label>
        )}
        <label className="field">
          <span>Mode</span>
          <select value={iface.mode} onChange={set('mode')}>
            {MODES.filter((m) => !(iface.isVlan && m.id === 'ADDRESSING_MODE_DHCP')).map((m) => (
              <option key={m.id} value={m.id}>
                {m.label}
              </option>
            ))}
          </select>
        </label>
      </div>
      {staticMode && (
        <div className="grid grid-3">
          <label className="field">
            <span>Addresses (CIDR, one per line)</span>
            <textarea className="mono" rows={2} value={iface.addresses} placeholder="192.0.2.10/24" onChange={set('addresses')} />
          </label>
          <label className="field">
            <span>IPv4 gateway</span>
            <input className="mono" value={iface.gateway} placeholder="none" onChange={set('gateway')} />
          </label>
          <label className="field">
            <span>IPv6 gateway</span>
            <input className="mono" value={iface.gateway6} placeholder="none" onChange={set('gateway6')} />
          </label>
        </div>
      )}
      <div className="spread">
        <div className="row">
          <label className="field" style={{ width: '8rem' }}>
            <span>MTU</span>
            <input type="number" min={68} max={65535} value={iface.mtu} placeholder="unchanged" onChange={set('mtu')} />
          </label>
          <label className="field" style={{ width: '9rem' }}>
            <span>Route metric</span>
            <input type="number" min={0} value={iface.metric} placeholder="auto" onChange={set('metric')} />
          </label>
        </div>
        <button className="small" onClick={onRemove} title="Remove this interface">
          <Trash2 size={14} /> Remove
        </button>
      </div>
      <datalist id="net-physical">
        {physical.map((p) => (
          <option key={p.name} value={p.name} />
        ))}
      </datalist>
      <datalist id="net-macs">
        {physical.map((p) => (
          <option key={p.mac} value={p.mac}>
            {p.name}
          </option>
        ))}
      </datalist>
    </div>
  )
}

function ApplyResult({ result, previousAddress }) {
  return (
    <div className={`notice ${result.confirmed ? '' : 'warn'}`} style={{ marginBottom: '1rem' }}>
      <div>{result.confirmed ? `Confirmed via ${result.confirmed_via} and saved on the node.` : result.error}</div>
      {result.confirmed && result.address !== previousAddress && <div>The node is now reached at {result.address}.</div>}
      <ul className="small" style={{ margin: '0.4rem 0 0' }}>
        {result.stages?.map((s, i) => (
          <li key={i}>{s}</li>
        ))}
      </ul>
    </div>
  )
}

function Editor({ loaded, status, onApplied, onResult }) {
  const [draft, setDraft] = useState(() => toDraft(loaded.config))
  const [mode, setMode] = useState('form')
  const [json, setJson] = useState(() => pretty(loaded.config || {}))
  const [jsonError, setJsonError] = useState(null)
  const [timeout, setTimeoutS] = useState('30')
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const toast = useToast()

  const physical = (status.interfaces || []).filter((i) => i.kind === 'physical')
  // Both sides through the same normalization, so the diff shows only
  // real changes.
  const current = pretty(fromDraft(toDraft(loaded.config || {})))
  let proposed
  try {
    proposed = mode === 'json' ? JSON.parse(json) : fromDraft(draft)
  } catch {
    proposed = null
  }
  const proposedText = proposed ? pretty(proposed) : ''
  const dirty = proposed && proposedText !== current
  const diff = useMemo(() => (dirty ? hunks(lineDiff(current, proposedText)) : []), [dirty, current, proposedText])

  const switchMode = (m) => {
    if (m === 'json') {
      setJson(pretty(fromDraft(draft)))
      setJsonError(null)
    } else {
      try {
        setDraft(toDraft(JSON.parse(json)))
        setJsonError(null)
      } catch (err) {
        setJsonError(err)
        return
      }
    }
    setMode(m)
  }
  const update = (i, v) => setDraft((d) => ({ ...d, interfaces: d.interfaces.map((x, j) => (j === i ? v : x)) }))
  const add = (isVlan) =>
    setDraft((d) => ({
      ...d,
      interfaces: [
        ...d.interfaces,
        { name: '', mac: '', isVlan, vlanParent: physical[0]?.name || '', vlanId: '', mode: isVlan ? 'ADDRESSING_MODE_STATIC' : 'ADDRESSING_MODE_STATIC', addresses: '', gateway: '', gateway6: '', mtu: '', metric: '' },
      ],
    }))
  const fromCurrent = () =>
    setDraft((d) => ({
      ...d,
      interfaces: [...physical, ...(status.interfaces || []).filter((i) => i.kind === 'vlan')].map((i) => ({
        name: i.name,
        mac: i.kind === 'physical' ? i.mac : '',
        isVlan: i.kind === 'vlan',
        vlanParent: i.vlan_parent || '',
        vlanId: i.vlan_id ? String(i.vlan_id) : '',
        mode: i.dhcp ? 'ADDRESSING_MODE_DHCP' : i.up && (i.addresses || []).some((a) => !a.startsWith('fe80')) ? 'ADDRESSING_MODE_STATIC' : i.up ? 'ADDRESSING_MODE_NONE' : 'ADDRESSING_MODE_DISABLED',
        addresses: i.dhcp ? '' : (i.addresses || []).filter((a) => !a.startsWith('fe80')).join('\n'),
        gateway: '',
        gateway6: '',
        mtu: '',
        metric: '',
      })),
    }))

  const apply = async (cfg, title) => {
    const d = hunks(lineDiff(current, pretty(cfg)))
    const ok = await confirm({
      title,
      body: (
        <>
          <p>
            The node switches to it at once, on trial. This Controller then confirms it by reaching the node over an address the new configuration keeps. If it can't within {timeout || 30} s
            - the change cut the node off - the node reverts to its current configuration by itself.
          </p>
          <DiffView diff={d} />
        </>
      ),
      action: 'Apply on trial',
      danger: true,
      wide: true,
    })
    if (!ok) return
    onResult(null)
    const r = await run(() => postJSON('/api/network/apply', { config: cfg, confirm_timeout_seconds: Number(timeout) || 0 }))
    if (!r) return
    onResult({ ...r, previousAddress: loaded.address })
    if (r.confirmed) {
      toast(r.address !== loaded.address ? `Confirmed - the node moved to ${r.address}` : `Confirmed via ${r.confirmed_via}`)
      onApplied()
    } else {
      toast(r.error || 'Not confirmed - the node reverts by itself', 'danger')
    }
  }

  return (
    <Card
      title="Configuration"
      icon={Save}
      actions={
        loaded.is_default ? (
          <Badge tone="info">defaults: kernel boot DHCP</Badge>
        ) : (
          <button className="small" disabled={busy || !status.managed} onClick={() => apply({}, 'Go back to the defaults?')}>
            <Undo2 size={14} /> Back to defaults…
          </button>
        )
      }
    >
      <Tabs
        tabs={[
          { id: 'form', label: 'Form', icon: ListTree },
          { id: 'json', label: 'JSON', icon: Code2 },
        ]}
        active={mode}
        onChange={switchMode}
      />
      <ErrorBox error={jsonError} />
      {mode === 'json' ? (
        <label className="field">
          <span>The document Install, `janusctl image seed-network` and NoCloud user-data (&quot;network&quot;) take too</span>
          <textarea className="mono" rows={18} spellCheck={false} value={json} onChange={(e) => setJson(e.target.value)} />
          {!proposed && <span className="small" style={{ color: 'var(--danger)' }}>Not valid JSON</span>}
        </label>
      ) : (
        <div className="stack">
          <div className="grid grid-3">
            <label className="field">
              <span>Hostname</span>
              <input className="mono" value={draft.hostname} placeholder="DHCP's, else janus-xxxxxx" onChange={(e) => setDraft({ ...draft, hostname: e.target.value })} />
            </label>
            <label className="field">
              <span>DNS servers (up to 3)</span>
              <input className="mono" value={draft.dns} placeholder="DHCP's" onChange={(e) => setDraft({ ...draft, dns: e.target.value })} />
            </label>
            <label className="field">
              <span>Search domains</span>
              <input className="mono" value={draft.search} placeholder="DHCP's domain" onChange={(e) => setDraft({ ...draft, search: e.target.value })} />
            </label>
          </div>
          <label className="field">
            <span>NTP servers (up to 2, host or host:port)</span>
            <input className="mono" value={draft.ntp} placeholder="DHCP's, else pool.ntp.org" onChange={(e) => setDraft({ ...draft, ntp: e.target.value })} />
          </label>
          <div className="spread">
            <strong>Interfaces</strong>
            <div className="row">
              <button className="small" onClick={fromCurrent}>
                Start from what's running
              </button>
              <button className="small" onClick={() => add(false)}>
                <Plus size={14} /> Interface
              </button>
              <button className="small" onClick={() => add(true)}>
                <Plus size={14} /> VLAN
              </button>
            </div>
          </div>
          {draft.interfaces.length === 0 ? (
            <p className="muted small" style={{ margin: 0 }}>
              None listed: the interfaces stay as the kernel's DHCP left them at boot. Once any is listed, exactly those are configured and other physical interfaces are left down (unless they carry a
              listed VLAN).
            </p>
          ) : (
            draft.interfaces.map((iface, i) => (
              <InterfaceEditor
                key={i}
                iface={iface}
                physical={physical}
                onChange={(v) => update(i, v)}
                onRemove={() => setDraft((d) => ({ ...d, interfaces: d.interfaces.filter((_, j) => j !== i) }))}
              />
            ))
          )}
          <p className="muted small" style={{ margin: 0 }}>
            DHCP is the kernel&apos;s own, done once at boot on one interface and never renewed: it&apos;s only available on the interface that got the boot lease. Use static addressing elsewhere.
          </p>
        </div>
      )}
      {dirty && (
        <div style={{ marginTop: '0.8rem' }}>
          <DiffView diff={diff} />
        </div>
      )}
      <div className="row" style={{ marginTop: '0.8rem' }}>
        <label className="field" style={{ width: '12rem' }}>
          <span>Revert unless confirmed within (s)</span>
          <input type="number" min={5} max={300} value={timeout} onChange={(e) => setTimeoutS(e.target.value)} />
        </label>
        <button className="danger solid" disabled={busy || !dirty || !status.managed} onClick={() => apply(proposed, 'Apply this network configuration?')}>
          <Save size={15} /> {busy ? 'Applying…' : 'Apply…'}
        </button>
      </div>
    </Card>
  )
}

export default function NetworkConfig() {
  const status = usePoll('/api/network/status')
  const [loaded, setLoaded] = useState(null)
  const [loadError, setLoadError] = useState(null)
  const [generation, setGeneration] = useState(0)
  const [result, setResult] = useState(null)

  useEffect(() => {
    let cancelled = false
    Promise.all([getJSON('/api/network/config'), getJSON('/api/node')])
      .then(([c, node]) => !cancelled && setLoaded({ ...c, address: node.address }))
      .catch((err) => !cancelled && setLoadError(err))
    return () => {
      cancelled = true
    }
  }, [generation])

  return (
    <>
      <PageHeader title="Network" subtitle="Hostname, interfaces, VLANs, DNS and NTP" />
      <ErrorBox error={status.error || loadError} />
      {!status.data ? (
        !status.error && <Loading />
      ) : (
        <>
          <StatusPanel status={status.data} />
          <div style={{ marginTop: '1rem' }}>
            {result && <ApplyResult result={result} previousAddress={result.previousAddress} />}
            {loaded ? (
              <Editor
                key={generation}
                loaded={loaded}
                status={status.data}
                onResult={setResult}
                onApplied={() => {
                  setGeneration((g) => g + 1)
                  status.reload()
                }}
              />
            ) : (
              !loadError && <Loading />
            )}
          </div>
        </>
      )}
    </>
  )
}
