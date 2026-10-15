import { ExternalLink, Send } from 'lucide-react'
import { useState } from 'react'
import { postJSON } from '../api.js'
import { usePoll } from '../hooks.jsx'
import { useMay } from '../may.js'
import LogTargets, { TargetStatusTable } from '../../shared/LogTargets.jsx'
import { NODE_DEFAULT_SOURCES, NODE_SOURCES, cleanTarget, withSources } from '../../shared/logTargets.js'
import { Card, ErrorBox, PageHeader, useAction, useToast } from '../../shared/ui.jsx'

const DOCS = 'https://janus.sw-servers.net/docs/guide/log-forwarding.md'

// LogForwarding is where the node's logs go (SystemService.LogForwarding*):
// syslog servers, Elasticsearch - and how each target is doing. A node a
// Controller's profile picks shows it, read-only: it's changed there.
export default function LogForwarding() {
  const { data, error, reload } = usePoll('/api/log-forwarding', { every: 10000 })
  const [draft, setDraft] = useState(null)
  const [busy, run] = useAction()
  const [testing, setTesting] = useState(false)
  const toast = useToast()
  const may = useMay()
  if (error) return <ErrorBox error={error} />
  if (!data) return null

  const cfg = data.config || {}
  const managed = cfg.managed_by || ''
  const canSet = may('SystemService/LogForwardingSet') && !managed
  const secrets = Object.fromEntries((data.targets || []).map((s) => [s.name, s.secrets || []]))
  // The form's edits; null until something is changed.
  const targets = draft?.targets ?? withSources(cfg.targets, NODE_DEFAULT_SOURCES)
  const accessOnDisk = draft?.access ?? cfg.access_logs_on_disk ?? ''
  const edit = (patch) => setDraft({ targets, access: accessOnDisk, ...patch })

  const save = () =>
    run(async () => {
      await postJSON('/api/log-forwarding', { targets: targets.map(cleanTarget), labels: cfg.labels, access_logs_on_disk: accessOnDisk || undefined })
      setDraft(null)
      reload()
    }, 'Log forwarding saved')
  const test = async (t) => {
    setTesting(true)
    try {
      const r = await postJSON('/api/log-forwarding/test', cleanTarget(t))
      toast(r.ok ? `${t.name}: sent` : `${t.name}: ${r.detail}`, r.ok ? 'ok' : 'warn')
    } catch (e) {
      toast(e.message, 'warn')
    } finally {
      setTesting(false)
    }
  }

  return (
    <>
      <PageHeader
        title="Log forwarding"
        subtitle="Where this node sends its logs: syslog servers, Elasticsearch - what it keeps on disk from where each target stopped"
        actions={
          canSet && draft ? (
            <button className="primary" disabled={busy} onClick={save}>
              Save
            </button>
          ) : null
        }
      />
      {managed && (
        <div className="notice">
          Set by the Controller ({managed.split(' ').slice(0, 4).join(' ')}): change it on the Controller&apos;s Log forwarding page.
        </div>
      )}
      <Card
        title="Targets"
        icon={Send}
        actions={
          <a href={DOCS} target="_blank" rel="noreferrer">
            Log forwarding <ExternalLink size={12} />
          </a>
        }
      >
        <div className="stack">
          <LogTargets
            value={targets}
            onChange={(t) => edit({ targets: t })}
            sources={NODE_SOURCES}
            defaultSources={NODE_DEFAULT_SOURCES}
            secrets={secrets}
            onTest={may('SystemService/LogForwardingTest') ? test : null}
            testing={testing}
            testNote="Send one record to it now - as shown here"
            disabled={!canSet}
          />
          <label className="field" style={{ maxWidth: '22rem' }}>
            <span>HAProxy&apos;s access logs on the LOGS partition too</span>
            <select value={accessOnDisk || 'never'} disabled={!canSet} onChange={(e) => edit({ access: e.target.value === 'never' ? '' : e.target.value })}>
              <option value="never">never (memory only)</option>
              <option value="always">always (sent at least once)</option>
            </select>
          </label>
          <TargetStatusTable status={data.targets} />
        </div>
      </Card>
    </>
  )
}
