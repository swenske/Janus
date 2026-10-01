import { CheckCircle2, Download, RotateCcw, Trash2, Upload } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import { getJSON, postJSON } from '../api.js'
import { Badge, Card, ErrorBox, Loading, useAction, useConfirm, useToast } from '../../shared/ui.jsx'
import { hunks, lineDiff } from '../diff.js'
import { DiffView, Editor } from './Editor.jsx'

// ModuleConfigEditor edits an optional module daemon's configuration
// (keepalived.conf, bird.conf): `${base}/config` loads it, `${base}/check`
// has the daemon check it, `${base}/apply` saves and reloads it. starter
// fills the editor while nothing is saved.
export default function ModuleConfigEditor({ base, file, daemon, starter, applyNote, onApplied }) {
  const [saved, setSaved] = useState(null) // { config, is_default }
  const [draft, setDraft] = useState('')
  const [loadError, setLoadError] = useState(null)
  const [check, setCheck] = useState(null)
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const toast = useToast()

  const load = async () => {
    try {
      const r = await getJSON(`${base}/config`)
      setSaved(r)
      setDraft(r.is_default ? starter : r.config)
      setCheck(null)
      setLoadError(null)
    } catch (err) {
      setLoadError(err)
    }
  }
  useEffect(() => {
    load()
  }, [base]) // eslint-disable-line react-hooks/exhaustive-deps

  const original = saved ? saved.config : ''
  const dirty = saved != null && draft !== original
  const diff = useMemo(() => (dirty ? hunks(lineDiff(original, draft)) : []), [dirty, original, draft])

  const doCheck = () =>
    run(async () => {
      setCheck(await postJSON(`${base}/check`, { config: draft }))
    })

  const apply = async (config) => {
    const ok = await confirm({
      title: config ? `Apply this ${file}?` : `Remove ${file}?`,
      body: config ? (
        <>
          <p>{applyNote}</p>
          <DiffView diff={hunks(lineDiff(original, config))} />
        </>
      ) : (
        <p>{daemon} stops, and gives up what it held.</p>
      ),
      action: config ? 'Apply' : 'Remove',
      danger: !config,
      wide: !!config,
    })
    if (!ok) return
    const r = await run(() => postJSON(`${base}/apply`, { config }))
    if (!r) return
    if (!r.accepted) {
      setCheck(r)
      return
    }
    toast(config ? `${file} applied` : `${file} removed`)
    await load()
    onApplied?.()
  }

  const download = () => {
    const a = document.createElement('a')
    a.href = URL.createObjectURL(new Blob([draft], { type: 'text/plain' }))
    a.download = file
    a.click()
  }
  const importFile = (e) => {
    const f = e.target.files[0]
    if (f) f.text().then(setDraft)
    e.target.value = ''
  }

  if (loadError) return <ErrorBox error={loadError} />
  if (!saved) return <Loading />
  return (
    <div className="stack">
      <Card
        title={file}
        actions={
          saved.is_default ? (
            <Badge>a starting point - nothing saved yet</Badge>
          ) : dirty ? (
            <Badge tone="warn">{diff.filter((d) => d.op === '+' || d.op === '-').length} changed lines</Badge>
          ) : (
            <Badge tone="ok">matches the saved {file}</Badge>
          )
        }
      >
        <Editor
          value={draft}
          onChange={(v) => {
            setDraft(v)
            setCheck(null)
          }}
        />
        <div className="row" style={{ marginTop: '0.8rem', flexWrap: 'wrap' }}>
          <button disabled={busy} onClick={doCheck}>
            <CheckCircle2 size={15} /> Check
          </button>
          <button className="primary" disabled={busy || (!dirty && !saved.is_default) || !draft.trim()} onClick={() => apply(draft)}>
            <Upload size={15} /> Apply…
          </button>
          <button disabled={busy || !dirty} onClick={() => setDraft(original || starter)}>
            <RotateCcw size={15} /> Revert
          </button>
          <span className="grow" />
          {!saved.is_default && (
            <button className="danger small" disabled={busy} onClick={() => apply('')}>
              <Trash2 size={14} /> Remove…
            </button>
          )}
          <label className="button small">
            Import file
            <input type="file" hidden onChange={importFile} />
          </label>
          <button className="small" onClick={download}>
            <Download size={14} /> Download
          </button>
        </div>
        {check && (
          <div style={{ marginTop: '0.8rem' }}>
            {check.accepted ? <div className="notice">Valid: {daemon} accepts this configuration.</div> : <div className="error-box">{(check.errors || ['refused']).join('\n')}</div>}
          </div>
        )}
      </Card>
      {dirty && !saved.is_default && (
        <Card title={`Changes against the saved ${file}`}>
          <DiffView diff={diff} />
        </Card>
      )}
    </div>
  )
}
