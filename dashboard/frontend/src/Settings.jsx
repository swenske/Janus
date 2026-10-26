import { RotateCcw, SlidersHorizontal } from 'lucide-react'
import { useCan } from './me.jsx'
import { usePreferences } from './preferences.js'
import HTTPSCertificate from './HTTPSCert.jsx'
import { Card, useAction, useConfirm } from './shared/ui.jsx'

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
      body: <p>Every node refresh, hypervisor refresh and figure choice of yours goes back to the Controller&apos;s default.</p>,
      action: 'Reset',
    })
    if (ok) run(reset, 'Back to the defaults')
  }
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
          <label className="field">
            <span>Nodes refreshed every</span>
            <select disabled={!loaded} value={prefs.nodes_refresh_seconds} onChange={(e) => set('nodes_refresh_seconds', Number(e.target.value))}>
              {prefs.choices.nodes.map((s) => (
                <option key={s} value={s}>
                  {seconds(s)}
                  {s === prefs.defaults?.nodes_refresh_seconds ? ' (default)' : ''}
                </option>
              ))}
            </select>
            <small className="muted">How often the Nodes page asks every node for its status - an API call to each.</small>
          </label>
          <label className="field">
            <span>Hypervisors refreshed every</span>
            <select disabled={!loaded} value={prefs.hypervisors_refresh_seconds} onChange={(e) => set('hypervisors_refresh_seconds', Number(e.target.value))}>
              {prefs.choices.hypervisors.map((s) => (
                <option key={s} value={s}>
                  {seconds(s)}
                  {s === prefs.defaults?.hypervisors_refresh_seconds ? ' (default)' : ''}
                </option>
              ))}
            </select>
            <small className="muted">How often each hypervisor is asked - its API, or libvirt over SSH. Every 3 s while a machine is being created or destroyed.</small>
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
          <HTTPSCertificate />
        </>
      )}
    </div>
  )
}

function seconds(s) {
  return s >= 60 ? `${s / 60} min` : `${s} s`
}
