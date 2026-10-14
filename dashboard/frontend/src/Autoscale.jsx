import { Activity, ScrollText } from 'lucide-react'
import { useState } from 'react'
import { THRESHOLDS, useAutoscale } from './autoscale.js'
import { Badge, ErrorBox } from './shared/ui.jsx'

// A machine's auto-scaling (dashboard/backend/autoscale.go,
// docs/guide/auto-scaling.md): its policy in the Edit dialog, what it
// does on the machine's card, and its journal.

// AutoscaleFields is the Edit dialog's auto-scaling section.
export function AutoscaleFields({ form, setForm, vcpus, memory, current }) {
  const set = (patch) => setForm({ ...form, ...patch })
  const setValue = (key, v) => setForm({ ...form, values: { ...form.values, [key]: v } })
  return (
    <fieldset className="autoscale-fields">
      <legend>
        <Activity size={14} /> Auto-scaling
      </legend>
      <div className="autoscale-row">
        <label className="field">
          <span>Mode</span>
          <select value={form.mode} onChange={(e) => set({ mode: e.target.value })}>
            <option value="none">None - its size is fixed</option>
            <option value="observe">Observe - write what it would do</option>
            <option value="on">On - resize it while it runs</option>
            {current && <option value="off">Paused - back to its size, room kept</option>}
          </select>
        </label>
        {form.mode !== 'none' && (
          <>
            <label className="field">
              <span>Up to (vCPUs)</span>
              <input type="number" min={vcpus} max={64} value={form.max_vcpus} onChange={(e) => set({ max_vcpus: e.target.value })} required />
            </label>
            <label className="field">
              <span>Up to (MiB)</span>
              <input type="number" min={memory} step={128} value={form.max_memory_mib} onChange={(e) => set({ max_memory_mib: e.target.value })} required />
            </label>
          </>
        )}
      </div>
      {form.mode !== 'none' && (
        <>
          <details className="autoscale-thresholds">
            <summary>Thresholds</summary>
            <div className="threshold-grid">
              {THRESHOLDS.map((t) => (
                <label key={t.key} className="threshold">
                  <span>{t.label}</span>
                  <input type="number" min={t.unit === 'min' ? 0.5 : 1} step={t.unit === 'min' ? 0.5 : 1} value={form.values[t.key]} onChange={(e) => setValue(t.key, e.target.value)} required />
                  <span className="muted">{t.unit}</span>
                </label>
              ))}
            </div>
            <p className="muted small">
              Janus&apos;s starting points - HAProxy&apos;s documentation gives no number. Memory goes first on the way up (HAProxy reloads for a vCPU), vCPUs first on the way back; one step at a time.
            </p>
          </details>
          <label className="row small" style={{ gap: '0.45rem' }}>
            <input type="checkbox" checked={form.sysctl} onChange={(e) => set({ sysctl: e.target.checked })} />
            <span>
              Kernel parameters follow: the node&apos;s suggestions (System › Sysctl) applied on trial and confirmed - never one someone else set, put back when it&apos;s at its size again.
            </span>
          </label>
          <ul className="muted small edit-notes">
            <li>Its size above is where it starts and comes back to; the maxima are how far it grows while it runs - the node takes vCPUs and memory without a restart.</li>
            <li>Observing changes nothing: its journal says what auto-scaling would do, and the size it would have given - try it on real traffic before resizing for real.</li>
            <li>Resizing, memory is only added while the host keeps a twentieth of its own free (512 MiB at least).</li>
          </ul>
        </>
      )}
    </fieldset>
  )
}

// AutoscaleSummary is a machine card's auto-scaling: its mode, the size
// it gives (or would), what it says, and its journal.
export function AutoscaleSummary({ m }) {
  const p = m.spec.autoscale
  const st = m.autoscale_state
  const [journal, setJournal] = useState(false)
  if (!p) return null
  const tone = { observe: 'info', on: 'ok' }[p.mode] || ''
  const mode = { observe: 'observing', on: 'on', off: 'paused' }[p.mode] || p.mode
  return (
    <div className="autoscale-summary stack">
      <div className="row" style={{ gap: '0.4rem', flexWrap: 'wrap' }}>
        <Badge tone={tone}>
          <Activity size={12} /> auto-scaling {mode}
        </Badge>
        <span className="muted small">
          up to {p.max_vcpus} vCPU · {p.max_memory_mib} MiB
        </span>
        <span className="grow" />
        <button className="small ghost" onClick={() => setJournal(!journal)} title="Auto-scaling journal">
          <ScrollText size={14} /> Journal
        </button>
      </div>
      {st?.status && p.mode !== 'off' && <div className="small">{st.status}</div>}
      {(st?.warnings || []).map((w) => (
        <div key={w} className="notice warn small">
          {w}
        </div>
      ))}
      {journal && <Journal id={m.id} at={st?.at} />}
    </div>
  )
}

function Journal({ id, at }) {
  const [data, error] = useAutoscale(id, [at])
  if (error) return <ErrorBox error={error} />
  if (!data) return <div className="muted small">Loading…</div>
  if (data.journal.length === 0) return <div className="muted small">Nothing yet: no step taken - nor, observing, any it would have taken.</div>
  return (
    <pre className="logview machine-events">
      {data.journal
        .slice()
        .reverse()
        .map((e) => {
          const when = new Date(e.time).toLocaleString()
          const failed = e.error ? ` - failed: ${e.error}` : ''
          if (e.action === 'sysctl') {
            return `${when}  ${e.mode === 'on' ? 'kernel parameters' : 'would set kernel parameters'}: ${e.reason}${failed}`
          }
          const what = e.mode === 'on' ? (e.action === 'up' ? 'grew' : 'shrank') : `would ${e.action === 'up' ? 'grow' : 'shrink'}`
          return `${when}  ${what} to ${e.to.vcpus} vCPU · ${e.to.memory_mib} MiB - ${e.reason}${failed}`
        })
        .join('\n')}
    </pre>
  )
}
