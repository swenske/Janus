import { Check, Pause, Pencil, Play, Plus, Send, Trash2 } from 'lucide-react'
import { useState } from 'react'
import { newTarget, sameTarget, targetWhere, trustMode, withPaused } from './logTargets.js'
import { Badge } from './ui.jsx'

function Field({ label, children, grow, width }) {
  return (
    <label className={`field${grow ? ' grow' : ''}`} style={width ? { width } : undefined}>
      <span>{label}</span>
      {children}
    </label>
  )
}

// TargetEditor edits one target. secrets: the names of those it has
// saved (api_key, password, client_key); onTest sends one record;
// onDone folds it back into its summary.
function TargetEditor({ target, onChange, onRemove, onTest, onDone, sources, secrets = [], disabled, testing, testNote }) {
  const t = target
  const kind = t.elasticsearch ? 'elasticsearch' : 'syslog'
  const set = (patch) => onChange({ ...t, ...patch })
  const setSyslog = (patch) => set({ syslog: { ...t.syslog, ...patch } })
  const setES = (patch) => set({ elasticsearch: { ...t.elasticsearch, ...patch } })
  const setTLS = (patch) => set({ tls: { ...(t.tls || {}), ...patch } })
  const mode = trustMode(t.tls)
  const usesTLS = kind === 'elasticsearch' ? (t.elasticsearch?.url || '').startsWith('https:') : t.syslog?.transport === 'tls'
  const kept = (name) => (secrets.includes(name) ? 'kept - type to change it' : '')
  const toggleSource = (id) => set({ sources: t.sources.includes(id) ? t.sources.filter((s) => s !== id) : [...t.sources, id] })
  return (
    <fieldset className="plain stack log-target editing" disabled={disabled}>
      <div className="row" style={{ flexWrap: 'wrap', alignItems: 'flex-end' }}>
        <Field label="Name" width="11rem">
          <input className="mono" value={t.name} onChange={(e) => set({ name: e.target.value })} placeholder="nas" />
        </Field>
        <Field label="Server">
          <select
            value={kind}
            onChange={(e) =>
              e.target.value === 'elasticsearch'
                ? set({ syslog: undefined, elasticsearch: { url: 'https://', namespace: 'default' } })
                : set({ elasticsearch: undefined, syslog: { address: '', transport: 'tls', format: 'rfc5424' } })
            }
          >
            <option value="syslog">Syslog</option>
            <option value="elasticsearch">Elasticsearch</option>
          </select>
        </Field>
        <label className="check">
          <input type="checkbox" checked={!!t.disabled} onChange={(e) => set({ disabled: e.target.checked })} /> Paused
        </label>
        <span className="grow" />
        {onTest && (
          <button type="button" className="small" onClick={onTest} disabled={testing} title={testNote || 'Send one record to it now (as saved)'}>
            <Send size={14} /> Test
          </button>
        )}
        <button type="button" className="small" onClick={onRemove} aria-label={`Remove target ${t.name}`}>
          <Trash2 size={14} /> Remove
        </button>
        <button type="button" className="small" onClick={onDone}>
          <Check size={14} /> Done
        </button>
      </div>
      {kind === 'syslog' ? (
        <div className="row" style={{ flexWrap: 'wrap' }}>
          <Field label="Address (host[:port])" grow>
            <input className="mono" value={t.syslog.address} onChange={(e) => setSyslog({ address: e.target.value })} placeholder={t.syslog.transport === 'tls' ? 'logs.example.com:6514' : 'logs.example.com:514'} />
          </Field>
          <Field label="Transport">
            <select value={t.syslog.transport || 'udp'} onChange={(e) => setSyslog({ transport: e.target.value })}>
              <option value="udp">UDP</option>
              <option value="tcp">TCP</option>
              <option value="tls">TLS</option>
            </select>
          </Field>
          <Field label="Format">
            <select value={t.syslog.format || 'rfc5424'} onChange={(e) => setSyslog({ format: e.target.value, framing: '' })}>
              <option value="rfc5424">RFC 5424 (IETF)</option>
              <option value="rfc3164">RFC 3164 (BSD)</option>
            </select>
          </Field>
          {t.syslog.transport !== 'udp' && (
            <Field label="Framing">
              <select value={t.syslog.framing || ''} onChange={(e) => setSyslog({ framing: e.target.value })}>
                <option value="">default</option>
                <option value="octet-counting">octet counting</option>
                <option value="lf">a line each</option>
              </select>
            </Field>
          )}
          <Field label="Facility">
            <select value={t.syslog.facility || 'local0'} onChange={(e) => setSyslog({ facility: e.target.value })}>
              {['local0', 'local1', 'local2', 'local3', 'local4', 'local5', 'local6', 'local7', 'daemon', 'user', 'auth'].map((f) => (
                <option key={f}>{f}</option>
              ))}
            </select>
          </Field>
        </div>
      ) : (
        <>
          <div className="row" style={{ flexWrap: 'wrap' }}>
            <Field label="URL" grow>
              <input className="mono" value={t.elasticsearch.url} onChange={(e) => setES({ url: e.target.value })} placeholder="https://elastic.example.com:9200" />
            </Field>
            <Field label="Namespace" width="9rem">
              <input className="mono" value={t.elasticsearch.namespace || ''} onChange={(e) => setES({ namespace: e.target.value })} placeholder="default" />
            </Field>
            <label className="check">
              <input type="checkbox" checked={!!t.elasticsearch.gzip} onChange={(e) => setES({ gzip: e.target.checked })} /> gzip
            </label>
          </div>
          <div className="row" style={{ flexWrap: 'wrap' }}>
            <Field label="API key (encoded, or id:key)" grow>
              <input
                type="password"
                className="mono"
                value={t.elasticsearch.api_key || ''}
                onChange={(e) => setES({ api_key: e.target.value })}
                placeholder={kept('api_key')}
                autoComplete="new-password"
              />
            </Field>
            <Field label="or user" width="9rem">
              <input value={t.elasticsearch.username || ''} onChange={(e) => setES({ username: e.target.value })} autoComplete="off" />
            </Field>
            <Field label="and password" width="11rem">
              <input type="password" value={t.elasticsearch.password || ''} onChange={(e) => setES({ password: e.target.value })} placeholder={kept('password')} autoComplete="new-password" />
            </Field>
          </div>
        </>
      )}
      {usesTLS && (
        <div className="stack" style={{ gap: '0.5rem' }}>
          <div className="row" style={{ flexWrap: 'wrap' }}>
            <Field label="Trust the server by">
              <select value={mode} onChange={(e) => setTLS({ mode: e.target.value, ca_pem: '', fingerprint: '' })}>
                <option value="system">the usual CAs</option>
                <option value="ca">a CA (PEM)</option>
                <option value="fingerprint">a SHA-256 fingerprint</option>
              </select>
            </Field>
            {mode === 'fingerprint' && (
              <Field label="SHA-256 of its certificate or of a CA in its chain" grow>
                <input className="mono" value={t.tls?.fingerprint || ''} onChange={(e) => setTLS({ fingerprint: e.target.value })} placeholder="93:BA:4A:…" />
              </Field>
            )}
            <Field label="Name on its certificate (optional)" width="14rem">
              <input className="mono" value={t.tls?.server_name || ''} onChange={(e) => setTLS({ server_name: e.target.value })} />
            </Field>
          </div>
          {mode === 'ca' && (
            <Field label="CA (PEM)">
              <textarea rows={3} className="mono" value={t.tls?.ca_pem || ''} onChange={(e) => setTLS({ ca_pem: e.target.value })} placeholder="-----BEGIN CERTIFICATE-----" />
            </Field>
          )}
          <details>
            <summary className="small">Client certificate{secrets.includes('client_key') || t.tls?.client_cert_pem ? ' (set)' : ''}</summary>
            <div className="row" style={{ flexWrap: 'wrap', marginTop: '0.5rem' }}>
              <Field label="Certificate (PEM)" grow>
                <textarea rows={3} className="mono" value={t.tls?.client_cert_pem || ''} onChange={(e) => setTLS({ client_cert_pem: e.target.value })} />
              </Field>
              <Field label="Key (PEM)" grow>
                <textarea rows={3} className="mono" value={t.tls?.client_key_pem || ''} onChange={(e) => setTLS({ client_key_pem: e.target.value })} placeholder={kept('client_key')} />
              </Field>
            </div>
          </details>
        </div>
      )}
      <div className="row" style={{ flexWrap: 'wrap', gap: '0.9rem' }}>
        <span className="muted small">Sends</span>
        {sources.map((s) => (
          <label className="check" key={s.id}>
            <input type="checkbox" checked={t.sources.includes(s.id)} onChange={() => toggleSource(s.id)} /> {s.label}
          </label>
        ))}
      </div>
    </fieldset>
  )
}

// size is a byte count as the summary says it: 1.2 MiB.
function size(n) {
  let v = Number(n) || 0
  let i = 0
  while (v >= 1024 && i < 4) {
    v /= 1024
    i++
  }
  return `${i ? v.toFixed(1) : v} ${['B', 'KiB', 'MiB', 'GiB', 'TiB'][i]}`
}

// TargetSummary is a target folded: where it sends, what, and how it's
// doing (status: its TargetStatus, when it's saved as shown) - the bytes
// sent when it says them (a node older than bytes_sent doesn't). A
// paused target says so once: its status ("disabled") and its counts -
// none while it's stopped - aren't shown. onPause pauses or resumes it,
// in the list like Edit: the page's Save applies it.
function TargetSummary({ target, sources, status, unsaved, onEdit, onPause, onRemove, onTest, testing, testNote, disabled }) {
  const t = target
  const bytes = status && 'bytes_sent' in status ? ` (${size(status.bytes_sent)})` : ''
  const sent = status ? `${status.sent || 0} sent${bytes} · ${status.dropped || 0} dropped · ${status.queued ?? status.queued_records ?? 0} waiting` : ''
  const labels = sources.filter((s) => t.sources.includes(s.id)).map((s) => s.label)
  const showStatus = status && !unsaved && !(t.disabled && status.state === 'disabled')
  return (
    <div className="log-target">
      <div className="row" style={{ flexWrap: 'wrap', alignItems: 'center' }}>
        <strong className="mono">{t.name || '(no name)'}</strong>
        <Badge>{t.elasticsearch ? 'Elasticsearch' : 'Syslog'}</Badge>
        <span className="mono small muted">{targetWhere(t)}</span>
        {t.disabled && (
          <Badge tone="warn" dot>
            paused
          </Badge>
        )}
        {unsaved && <Badge tone="accent">not saved</Badge>}
        {showStatus && (
          <Badge tone={status.state === 'ok' ? 'ok' : status.state === 'failing' ? 'danger' : undefined} dot>
            {status.state}
          </Badge>
        )}
        {showStatus && <span className="small muted">{sent}</span>}
        <span className="grow" />
        {onTest && (
          <button type="button" className="small" onClick={onTest} disabled={testing} title={testNote || 'Send one record to it now - as shown here'}>
            <Send size={14} /> Test
          </button>
        )}
        {!disabled && (
          <>
            {t.disabled ? (
              <button type="button" className="small" onClick={onPause} aria-label={`Resume target ${t.name}`} title="Send to it again, from where it stopped - once saved">
                <Play size={14} /> Resume
              </button>
            ) : (
              <button type="button" className="small" onClick={onPause} aria-label={`Pause target ${t.name}`} title="Stop sending to it, keeping its place - once saved">
                <Pause size={14} /> Pause
              </button>
            )}
            <button type="button" className="small" onClick={onEdit}>
              <Pencil size={14} /> Edit
            </button>
            <button type="button" className="small" onClick={onRemove} aria-label={`Remove target ${t.name}`}>
              <Trash2 size={14} /> Remove
            </button>
          </>
        )}
      </div>
      <span className="small muted">Sends {labels.length ? labels.join(', ') : 'nothing'}</span>
      {status?.last_error && !unsaved && <span className="small muted">{status.last_error}</span>}
    </div>
  )
}

// LogTargets edits a list of log targets: value the targets (as the API
// gives them, sources filled in), onChange the new list; saved the list
// as saved (a target that differs says so), status how each saved one is
// doing; secrets maps a target's name to the secrets it has saved. Each
// target shows folded - Edit opens it, a new one opens at once. Give it
// a key that changes on a save or a discard: what's open folds again.
export default function LogTargets({ value, onChange, sources, defaultSources, saved = [], status = [], secrets = {}, onTest, testing, testNote, disabled }) {
  const [open, setOpen] = useState(() => new Set())
  const update = (i, t) => onChange(value.map((x, j) => (j === i ? t : x)))
  const remove = (i) => {
    onChange(value.filter((_, j) => j !== i))
    setOpen(new Set([...open].filter((j) => j !== i).map((j) => (j > i ? j - 1 : j))))
  }
  const toggle = (i, on) => {
    const next = new Set(open)
    if (on) next.add(i)
    else next.delete(i)
    setOpen(next)
  }
  const add = () => {
    let n = value.length + 1
    while (value.some((t) => t.name === `target-${n}`)) n++
    onChange([...value, newTarget(`target-${n}`, defaultSources)])
    toggle(value.length, true)
  }
  return (
    <div className="stack">
      {value.length === 0 && <div className="muted">No target: these logs aren&apos;t sent anywhere.</div>}
      {value.map((t, i) => {
        const was = saved.find((s) => s.name === t.name)
        const unsaved = !was || !sameTarget(t, was)
        const st = status.find((s) => s.name === t.name)
        return open.has(i) && !disabled ? (
          <TargetEditor
            key={i}
            target={t}
            sources={sources}
            secrets={secrets[t.name] || []}
            onChange={(nt) => update(i, nt)}
            onRemove={() => remove(i)}
            onDone={() => toggle(i, false)}
            onTest={onTest ? () => onTest(t) : null}
            testing={testing}
            testNote={testNote}
          />
        ) : (
          <TargetSummary
            key={i}
            target={t}
            sources={sources}
            status={st}
            unsaved={unsaved}
            onEdit={() => toggle(i, true)}
            onPause={() => update(i, withPaused(t, !t.disabled))}
            onRemove={() => remove(i)}
            onTest={onTest ? () => onTest(t) : null}
            testing={testing}
            testNote={testNote}
            disabled={disabled}
          />
        )
      })}
      {!disabled && (
        <div>
          <button type="button" className="small" onClick={add}>
            <Plus size={14} /> Add a target
          </button>
        </div>
      )}
    </div>
  )
}

// UnsavedBar says the page holds changes not saved yet - a target added,
// changed or removed only counts once saved - with Save and Discard.
export function UnsavedBar({ onSave, onDiscard, busy }) {
  return (
    <div className="unsaved-bar" role="status">
      <span>Changes not saved yet: nothing is sent differently until you save.</span>
      <span className="grow" />
      <button type="button" onClick={onDiscard} disabled={busy}>
        Discard
      </button>
      <button type="button" className="primary" onClick={onSave} disabled={busy}>
        Save
      </button>
    </div>
  )
}
