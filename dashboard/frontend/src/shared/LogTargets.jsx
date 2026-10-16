import { Plus, Send, Trash2 } from 'lucide-react'
import { newTarget, trustMode } from './logTargets.js'
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
// saved (api_key, password, client_key); onTest sends one record.
function TargetEditor({ target, onChange, onRemove, onTest, sources, secrets = [], disabled, testing, testNote }) {
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
    <fieldset className="plain stack log-target" disabled={disabled} style={{ borderTop: '1px solid var(--border)', paddingTop: '0.8rem' }}>
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

// LogTargets edits a list of log targets: value the targets (as the API
// gives them, sources filled in), onChange the new list. secrets maps a
// target's name to the secrets it has saved.
export default function LogTargets({ value, onChange, sources, defaultSources, secrets = {}, onTest, testing, testNote, disabled }) {
  const update = (i, t) => onChange(value.map((x, j) => (j === i ? t : x)))
  const add = () => {
    let n = value.length + 1
    while (value.some((t) => t.name === `target-${n}`)) n++
    onChange([...value, newTarget(`target-${n}`, defaultSources)])
  }
  return (
    <div className="stack">
      {value.length === 0 && <div className="muted">No target: these logs aren&apos;t sent anywhere.</div>}
      {value.map((t, i) => (
        <TargetEditor
          key={i}
          target={t}
          sources={sources}
          secrets={secrets[t.name] || []}
          onChange={(nt) => update(i, nt)}
          onRemove={() => onChange(value.filter((_, j) => j !== i))}
          onTest={onTest ? () => onTest(t) : null}
          testing={testing}
          testNote={testNote}
          disabled={disabled}
        />
      ))}
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

// TargetStatusTable is how each target is doing (logship.TargetStatus,
// or the node's LogTargetStatus).
export function TargetStatusTable({ status }) {
  if (!status?.length) return null
  const when = (v) => {
    if (!v) return '-'
    const t = typeof v === 'string' ? new Date(v) : new Date(Number(v) / 1e6)
    return t.getFullYear() < 2000 ? '-' : t.toLocaleString()
  }
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th>Target</th>
            <th>State</th>
            <th>Sent</th>
            <th>Dropped</th>
            <th>Waiting</th>
            <th>Last sent</th>
          </tr>
        </thead>
        <tbody>
          {status.map((s) => (
            <tr key={s.name}>
              <td className="mono">{s.name}</td>
              <td>
                <Badge tone={s.state === 'ok' ? 'ok' : s.state === 'failing' ? 'danger' : undefined} dot>
                  {s.state}
                </Badge>
                {s.last_error && <div className="small muted">{s.last_error}</div>}
              </td>
              <td>{s.sent || 0}</td>
              <td>{s.dropped || 0}</td>
              <td>{s.queued ?? s.queued_records ?? 0}</td>
              <td className="small">{when(s.last_sent ?? s.last_sent_unix_ns)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
