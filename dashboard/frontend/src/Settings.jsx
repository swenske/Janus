import { Gauge, RotateCcw, SlidersHorizontal } from 'lucide-react'
import { useEffect, useState } from 'react'
import { call, postJSON } from './call.js'
import { useCan } from './me.jsx'
import { usePreferences } from './preferences.js'
import HTTPSCertificate from './HTTPSCert.jsx'
import { fmtRate, fmtTime } from './shared/units.js'
import { bytes } from './node/format.js'
import { Card, ErrorBox, useAction, useConfirm } from './shared/ui.jsx'

// The Settings tab: the account's own preferences (preferences.js - kept
// with the account on the Controller, so they follow it to any browser),
// then, for an admin, the Controller's own settings.
export default function SettingsPage() {
  const can = useCan()
  const { prefs, save, reset, loaded } = usePreferences()
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const set = (key, value) => run(() => save(key, value), 'Saved')
  const chosen = Object.keys(prefs.chosen || {}).length > 0
  const resetAll = async () => {
    const ok = await confirm({
      title: 'Back to the defaults?',
      body: <p>Every refresh, figure, time and traffic choice of yours goes back to the Controller&apos;s default.</p>,
      action: 'Reset',
    })
    if (ok) run(reset, 'Back to the defaults')
  }
  const zone = Intl.DateTimeFormat().resolvedOptions().timeZone
  // The example time: the page's opening, not a clock read at each render.
  const [opened] = useState(() => Date.now())
  return (
    <div className="stack">
      <div className="spread">
        <h1>Settings</h1>
      </div>
      <Card
        title="Your pages"
        icon={SlidersHorizontal}
        actions={
          chosen && (
            <button className="small" disabled={busy} onClick={resetAll}>
              <RotateCcw size={14} /> Reset to the defaults
            </button>
          )
        }
      >
        <p className="muted small" style={{ marginTop: 0 }}>
          Kept with your account on this Controller: they follow you to any browser, and change nothing for the other accounts.
        </p>
        <div className="settings-grid">
          <RefreshField label="Nodes refreshed every" kind="nodes" prefKey="nodes_refresh_seconds" prefs={prefs} loaded={loaded} set={set}>
            How often the Nodes page asks every node for its status - an API call to each.
          </RefreshField>
          <RefreshField label="Hypervisors refreshed every" kind="hypervisors" prefKey="hypervisors_refresh_seconds" prefs={prefs} loaded={loaded} set={set}>
            How often each hypervisor is asked - its API, or libvirt over SSH. Every 3 s while a machine is being created or destroyed.
          </RefreshField>
          <label className="field">
            <span>Times</span>
            <select disabled={!loaded} value={prefs.time_zone} onChange={(e) => set('time_zone', e.target.value)}>
              <option value="local">In this browser&apos;s zone{zone ? ` (${zone})` : ''}</option>
              <option value="utc">In UTC</option>
            </select>
            <small className="muted">
              Opened at {fmtTime(opened)}. UTC is how Elasticsearch, Prometheus and the nodes&apos; logs keep them; the node pages follow it too.
            </small>
          </label>
          <label className="field">
            <span>Traffic</span>
            <select disabled={!loaded} value={prefs.rate_unit} onChange={(e) => set('rate_unit', e.target.value)}>
              <option value="bytes">Bytes a second - KiB/s, MiB/s</option>
              <option value="bits">Bits a second - kbit/s, Mbit/s</option>
            </select>
            <small className="muted">
              1.5 MiB a second reads {fmtRate(1.5 * 2 ** 20, bytes)}. HAProxy and the kernel count bytes; a link&apos;s speed and a router&apos;s graphs, bits.
            </small>
          </label>
        </div>
        <div className="stack settings-checks">
          <label className="check">
            <input type="checkbox" disabled={!loaded} checked={prefs.node_stats} onChange={(e) => set('node_stats', e.target.checked)} />
            <span>
              <strong>Node figures</strong> - CPU, requests a second and traffic on each node of the Nodes page.
              <small className="muted">
                {' '}
                Off, the page no longer asks for them: the Controller stops sampling the nodes every 2 s for it - ten minutes later, unless another page asks.
              </small>
            </span>
          </label>
          <label className={`check ${prefs.node_stats ? '' : 'disabled'}`} title={prefs.node_stats ? '' : 'Without the figures, there is nothing to draw'}>
            <input type="checkbox" disabled={!loaded || !prefs.node_stats} checked={prefs.node_stats && prefs.node_charts} onChange={(e) => set('node_charts', e.target.checked)} />
            <span>
              <strong>Node charts</strong> - the figures&apos; last two minutes as curves; off, the figures alone. Also the particles of a node&apos;s Traffic flow.
              {!prefs.node_stats && <small className="muted"> No effect while the node figures are off.</small>}
            </span>
          </label>
        </div>
      </Card>
      {can('admin') && (
        <>
          <h2 className="settings-section">This Controller</h2>
          <RefreshFloors />
          <HTTPSCertificate />
        </>
      )}
    </div>
  )
}

// RefreshField picks a refresh among those the Controller's floor leaves.
function RefreshField({ label, kind, prefKey, prefs, loaded, set, children }) {
  const floor = prefs.floors?.[kind] || 0
  return (
    <label className="field">
      <span>{label}</span>
      <select disabled={!loaded} value={prefs[prefKey]} onChange={(e) => set(prefKey, Number(e.target.value))}>
        {prefs.choices[kind].map((s) => (
          <option key={s} value={s}>
            {seconds(s)}
            {s === prefs.defaults?.[prefKey] ? ' (default)' : ''}
          </option>
        ))}
      </select>
      <small className="muted">
        {children}
        {floor > 0 && ` This Controller's admins ask for ${seconds(floor)} at least.`}
      </small>
    </label>
  )
}

// RefreshFloors: the shortest refreshes any account may choose (an
// admin's, /api/settings) - so many pages open at 5 s don't keep every
// node and hypervisor busy. An account that chose faster follows them.
function RefreshFloors() {
  const { reload } = usePreferences()
  const [settings, setSettings] = useState(null)
  const [error, setError] = useState(null)
  const [busy, run] = useAction()
  useEffect(() => {
    call('/api/settings').then(setSettings, setError)
  }, [])
  const set = (key, value) =>
    run(async () => {
      setSettings(await postJSON('/api/settings', { [key]: value }, 'PUT'))
      await reload?.()
    }, 'Saved')
  const all = { nodes: [5, 10, 15, 30, 60], hypervisors: [10, 15, 30, 60, 120] }
  return (
    <Card title="Refresh floors" icon={Gauge}>
      <p className="muted small" style={{ marginTop: 0 }}>
        The shortest refresh any account may choose here: every page open asks every node, or every hypervisor, that often. An account that chose faster follows the
        floor.
      </p>
      {error && <ErrorBox error={error} />}
      <div className="settings-grid">
        {[
          ['nodes', 'nodes_refresh_floor_seconds', 'Nodes, at most every'],
          ['hypervisors', 'hypervisors_refresh_floor_seconds', 'Hypervisors, at most every'],
        ].map(([kind, key, label]) => (
          <label className="field" key={kind}>
            <span>{label}</span>
            <select disabled={!settings || busy} value={settings?.[key] || 0} onChange={(e) => set(key, Number(e.target.value))}>
              <option value={0}>{seconds(all[kind][0])} - no floor</option>
              {all[kind].slice(1).map((s) => (
                <option key={s} value={s}>
                  {seconds(s)}
                </option>
              ))}
            </select>
          </label>
        ))}
      </div>
    </Card>
  )
}

function seconds(s) {
  return s >= 60 ? `${s / 60} min` : `${s} s`
}
