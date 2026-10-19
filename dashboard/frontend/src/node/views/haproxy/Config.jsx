import { CheckCircle2, Download, FileCode2, RotateCcw, Upload } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import { getJSON, postJSON } from '../../api.js'
import { Badge, Card, ErrorBox, Loading, useAction, useConfirm, useToast } from '../../../shared/ui.jsx'
import { DiffView, Editor } from '../../components/Editor.jsx'
import { hunks, lineDiff } from '../../diff.js'
import { useMay } from '../../may.js'
import ConfigHistory from './ConfigHistory.jsx'

export default function Config() {
  const [original, setOriginal] = useState(null)
  const [sha, setSha] = useState('')
  const [redacted, setRedacted] = useState(false)
  const [draft, setDraft] = useState('')
  const [loadError, setLoadError] = useState(null)
  const [validation, setValidation] = useState(null)
  // Bumped after an apply: the history reads its versions again.
  const [generation, setGeneration] = useState(0)
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const toast = useToast()
  const may = useMay()

  const load = async () => {
    try {
      const c = await getJSON('/api/haproxy/config')
      setOriginal(c.config)
      setDraft(c.config)
      setSha(c.sha256)
      setRedacted(!!c.redacted)
      setValidation(null)
      setLoadError(null)
    } catch (err) {
      setLoadError(err)
    }
  }
  useEffect(() => {
    load()
  }, [])

  const dirty = original != null && draft !== original
  const diff = useMemo(() => (dirty ? hunks(lineDiff(original, draft)) : []), [dirty, original, draft])
  const changes = diff.filter((d) => d.op === '+' || d.op === '-').length

  const validate = () =>
    run(async () => {
      const r = await postJSON('/api/haproxy/validate', { config: draft })
      setValidation(r)
      return r
    })

  const apply = async () => {
    const ok = await confirm({
      title: 'Apply this configuration?',
      body: (
        <>
          <p>HAProxy validates it first; if it's accepted, a new HAProxy process takes over the listening sockets without dropping connections, and the old one finishes its in-flight requests.</p>
          <DiffView diff={diff} />
        </>
      ),
      action: 'Apply',
      wide: true,
    })
    if (!ok) return
    await run(async () => {
      const r = await postJSON('/api/haproxy/config', { config: draft })
      if (!r.accepted) {
        setValidation({ valid: false, errors: [r.message] })
        throw new Error(`Rejected: ${r.message}`)
      }
      toast('Configuration applied')
      await load()
      setGeneration((g) => g + 1)
    })
  }

  const save = () => {
    const blob = new Blob([draft], { type: 'text/plain' })
    const a = document.createElement('a')
    a.href = URL.createObjectURL(blob)
    a.download = 'haproxy.cfg'
    a.click()
  }
  const importFile = (e) => {
    const f = e.target.files[0]
    if (f) f.text().then(setDraft)
    e.target.value = ''
  }

  if (loadError) return <ErrorBox error={loadError} />
  if (original == null) return <Loading />
  return (
    <div className="stack">
      <Card
        title="haproxy.cfg"
        icon={FileCode2}
        actions={
          <>
            {dirty ? <Badge tone="warn">{changes} changed lines</Badge> : <Badge tone="ok">matches the running config</Badge>}
            <span className="muted small mono" title={redacted ? 'sha256 of the configuration as shown, secrets masked' : 'sha256 of the running config'}>
              {sha.slice(0, 12)}
            </span>
          </>
        }
      >
        {redacted && (
          <div className="notice" style={{ marginBottom: '0.8rem' }}>
            Passwords and credentials are masked (<code>&lt;redacted&gt;</code>): your role may read this configuration, not apply one.
          </div>
        )}
        <Editor
          label="haproxy.cfg"
          value={draft}
          onChange={(v) => {
            setDraft(v)
            setValidation(null)
          }}
        />
        <div className="row" style={{ marginTop: '0.8rem' }}>
          <button disabled={busy} onClick={validate}>
            <CheckCircle2 size={15} /> Validate
          </button>
          {may('HAProxyService/ApplyConfig') && (
            <button className="primary" disabled={busy || !dirty} onClick={apply}>
              <Upload size={15} /> Apply…
            </button>
          )}
          <button disabled={busy || !dirty} onClick={() => setDraft(original)}>
            <RotateCcw size={15} /> Revert
          </button>
          <span className="grow" />
          <label className="button small">
            Import file
            <input type="file" hidden onChange={importFile} />
          </label>
          <button className="small" onClick={save}>
            <Download size={14} /> Download
          </button>
        </div>
        {validation && (
          <div style={{ marginTop: '0.8rem' }}>
            {validation.valid ? (
              <div className="notice">Valid: HAProxy accepts this configuration.</div>
            ) : (
              <div className="error-box">
                {/* HAProxy prints notices too - put the actual [ALERT] lines first. */}
                {[...(validation.errors || ['rejected'])].sort((a, b) => Number(b.includes('[ALERT]')) - Number(a.includes('[ALERT]'))).join('\n')}
              </div>
            )}
          </div>
        )}
      </Card>
      {dirty && (
        <Card title="Changes against the running configuration">
          <DiffView diff={diff} />
        </Card>
      )}
      <ConfigHistory
        running={original}
        generation={generation}
        onApplied={async () => {
          await load()
          setGeneration((g) => g + 1)
        }}
        onOpen={(text) => {
          setDraft(text)
          setValidation(null)
          window.scrollTo({ top: 0 })
        }}
      />
    </div>
  )
}
