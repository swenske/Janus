import { ArrowDown, ArrowUp, ExternalLink, Layers, Plus, Send, Server, Trash2 } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { call, postJSON } from './call.js'
import LogTargets, { UnsavedBar } from './shared/LogTargets.jsx'
import { CONTROLLER_SOURCES, NODE_DEFAULT_SOURCES, NODE_SOURCES, cleanTarget, withSources } from './shared/logTargets.js'
import { Badge, Card, ErrorBox, useAction, useToast } from './shared/ui.jsx'

// Log forwarding (admin): where the Controller sends its own log and
// audit, and the fleet's profiles - a label selector and the log targets
// of the nodes it picks, applied and kept applied by the Controller.
// dashboard/backend/logforward.go, docs/guide/log-forwarding.md.

const DOCS = 'https://janus.sw-servers.net/docs/guide/log-forwarding.md'

// draftOf is the settings as the page edits them.
function draftOf(v) {
  return {
    controller: withSources(v.controller?.targets, ['controller', 'audit']),
    profiles: (v.profiles || []).map((p) => ({
      name: p.name,
      selector: p.selector || '',
      access_logs_on_disk: p.config?.access_logs_on_disk || '',
      labels: Object.entries(p.config?.labels || {})
        .map(([k, val]) => `${k}=${val}`)
        .join(','),
      targets: withSources(p.config?.targets, NODE_DEFAULT_SOURCES),
    })),
  }
}

function parseLabels(s) {
  const out = {}
  for (const part of s.split(',')) {
    const [k, ...rest] = part.split('=')
    if (k.trim()) out[k.trim()] = rest.join('=').trim()
  }
  return out
}

const nodeTone = { applied: 'ok', removed: 'warn', unsupported: 'warn', error: 'danger' }

export default function LogForwardingPage() {
  const [view, setView] = useState(null)
  const [draft, setDraft] = useState(null)
  const [error, setError] = useState(null)
  const [busy, run] = useAction()
  const [testing, setTesting] = useState(false)
  // Changes on a save or a discard: the target lists fold again.
  const [formKey, setFormKey] = useState(0)
  const toast = useToast()
  const load = useCallback(
    () =>
      call('/api/log-forwarding').then((v) => {
        setView(v)
        setDraft(draftOf(v))
        setFormKey((k) => k + 1)
      }, setError),
    [],
  )
  useEffect(() => {
    load()
  }, [load])
  if (error) return <ErrorBox error={error} />
  if (!view || !draft) return null

  const save = () =>
    run(async () => {
      const v = await postJSON(
        '/api/log-forwarding',
        {
          controller: { targets: draft.controller.map(cleanTarget) },
          profiles: draft.profiles.map((p) => ({
            name: p.name.trim(),
            selector: p.selector,
            config: { targets: p.targets.map(cleanTarget), labels: parseLabels(p.labels), access_logs_on_disk: p.access_logs_on_disk || undefined },
          })),
        },
        'PUT',
      )
      setView(v)
      setDraft(draftOf(v))
      setFormKey((k) => k + 1)
    }, 'Log forwarding saved - the nodes get their profile now')
  const discard = () => {
    setDraft(draftOf(view))
    setFormKey((k) => k + 1)
  }
  const dirty = JSON.stringify(draft) !== JSON.stringify(draftOf(view))

  const test = async (body) => {
    setTesting(true)
    try {
      const r = await postJSON('/api/log-forwarding/test', body)
      toast(r.ok ? `${body.target}: sent` : `${body.target}: ${r.detail}`, r.ok ? 'ok' : 'warn')
    } catch (e) {
      toast(e.message, 'warn')
    } finally {
      setTesting(false)
    }
  }
  const setProfile = (i, patch) => setDraft({ ...draft, profiles: draft.profiles.map((p, j) => (j === i ? { ...p, ...patch } : p)) })
  const move = (i, d) => {
    const ps = [...draft.profiles]
    ;[ps[i], ps[i + d]] = [ps[i + d], ps[i]]
    setDraft({ ...draft, profiles: ps })
  }
  const saved = (name) => view.profiles.find((p) => p.name === name)
  const nodesOf = (name) => view.nodes.filter((n) => n.profile === name && n.state === 'applied')

  return (
    <div className="stack">
      <div className="spread">
        <h1>Log forwarding</h1>
        <div className="row">
          <a href={DOCS} target="_blank" rel="noreferrer">
            Log forwarding <ExternalLink size={12} />
          </a>
          <button className="primary" disabled={busy || !dirty} onClick={save}>
            Save
          </button>
        </div>
      </div>
      <p className="muted" style={{ margin: 0 }}>
        Syslog servers (UDP, TCP, TLS) and Elasticsearch clusters. The nodes send their own logs - janusd&apos;s, HAProxy&apos;s, the extensions&apos;, the kernel&apos;s, the event
        log, the audit - each from where it stopped after an outage or a reboot. API keys, passwords and client keys are sealed with the master key, and never shown again.
      </p>

      <Card title="The Controller's own logs" icon={Server}>
        <div className="stack">
          <span className="muted small">Its log and its audit (every change made through it, every sign-in) - its audit from where it stopped, its log from memory.</span>
          <LogTargets
            key={`controller-${formKey}`}
            value={draft.controller}
            onChange={(controller) => setDraft({ ...draft, controller })}
            sources={CONTROLLER_SOURCES}
            defaultSources={['controller', 'audit']}
            saved={withSources(view.controller?.targets, ['controller', 'audit'])}
            status={view.controller_status || []}
            secrets={view.controller_secrets || {}}
            onTest={(t) => test({ target: t.name, config: cleanTarget(t) })}
            testing={testing}
          />
        </div>
      </Card>

      <Card
        title="Fleet profiles"
        icon={Layers}
        actions={
          <button
            className="small"
            onClick={() => setDraft({ ...draft, profiles: [...draft.profiles, { name: `profile-${draft.profiles.length + 1}`, selector: '', labels: '', access_logs_on_disk: '', targets: [] }] })}
          >
            <Plus size={14} /> Add a profile
          </button>
        }
      >
        <div className="stack">
          <span className="muted small">
            A node takes the first profile whose labels it has (none: every node), and the Controller keeps it applied - a change made on the node is undone, a profile removed
            is taken off. A node no profile picks keeps its own log forwarding.
          </span>
          {draft.profiles.length === 0 && <div className="muted">No profile: each node sends its logs as set on its own page.</div>}
          {draft.profiles.map((p, i) => {
            const members = nodesOf(p.name)
            return (
              <div key={i} className="stack" style={{ border: '1px solid var(--border)', borderRadius: 'var(--radius)', padding: '0.9rem' }}>
                <div className="row" style={{ flexWrap: 'wrap', alignItems: 'flex-end' }}>
                  <label className="field" style={{ width: '11rem' }}>
                    <span>Profile</span>
                    <input className="mono" value={p.name} onChange={(e) => setProfile(i, { name: e.target.value })} />
                  </label>
                  <label className="field grow">
                    <span>Nodes with the labels (key=value, comma-separated; empty: every node)</span>
                    <input className="mono" value={p.selector} onChange={(e) => setProfile(i, { selector: e.target.value })} placeholder="team=web,env=prod" />
                  </label>
                  <span className="grow" />
                  <Badge>
                    {members.length} node{members.length === 1 ? '' : 's'}
                  </Badge>
                  <button className="small" disabled={i === 0} onClick={() => move(i, -1)} aria-label="Up">
                    <ArrowUp size={14} />
                  </button>
                  <button className="small" disabled={i === draft.profiles.length - 1} onClick={() => move(i, 1)} aria-label="Down">
                    <ArrowDown size={14} />
                  </button>
                  <button className="small" onClick={() => setDraft({ ...draft, profiles: draft.profiles.filter((_, j) => j !== i) })}>
                    <Trash2 size={14} /> Remove
                  </button>
                </div>
                <div className="row" style={{ flexWrap: 'wrap' }}>
                  <label className="field grow">
                    <span>Labels added to every record (Elasticsearch&apos;s labels.*)</span>
                    <input className="mono" value={p.labels} onChange={(e) => setProfile(i, { labels: e.target.value })} placeholder="env=prod" />
                  </label>
                  <label className="field">
                    <span>HAProxy&apos;s access logs on the LOGS partition too</span>
                    <select value={p.access_logs_on_disk || 'never'} onChange={(e) => setProfile(i, { access_logs_on_disk: e.target.value === 'never' ? '' : e.target.value })}>
                      <option value="never">never (memory only)</option>
                      <option value="always">always (sent at least once)</option>
                    </select>
                  </label>
                </div>
                <LogTargets
                  key={`profile-${i}-${formKey}`}
                  value={p.targets}
                  onChange={(targets) => setProfile(i, { targets })}
                  sources={NODE_SOURCES}
                  defaultSources={NODE_DEFAULT_SOURCES}
                  saved={withSources(saved(p.name)?.config?.targets, NODE_DEFAULT_SOURCES)}
                  secrets={saved(p.name)?.secrets || {}}
                  onTest={members.length ? (t) => test({ profile: p.name, target: t.name, node: members[0].id, config: cleanTarget(t) }) : null}
                  testing={testing}
                  testNote={members.length ? `Send one record from ${members[0].name} - as shown here` : ''}
                />
              </div>
            )
          })}
        </div>
      </Card>

      {dirty && <UnsavedBar onSave={save} onDiscard={discard} busy={busy} />}

      <Card title="Nodes" icon={Send}>
        {view.nodes.length === 0 ? (
          <div className="muted">No node yet.</div>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Node</th>
                  <th>Labels</th>
                  <th>Profile</th>
                  <th>State</th>
                </tr>
              </thead>
              <tbody>
                {view.nodes.map((n) => (
                  <tr key={n.id}>
                    <td>{n.name}</td>
                    <td className="mono small">
                      {Object.entries(n.labels || {})
                        .map(([k, v]) => `${k}=${v}`)
                        .join(', ') || '-'}
                    </td>
                    <td className="mono">{n.profile || '-'}</td>
                    <td>
                      <Badge tone={nodeTone[n.state]} dot>
                        {n.state}
                      </Badge>
                      {n.error && <div className="small muted">{n.error}</div>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </div>
  )
}
