import { FileInput, History as HistoryIcon, Upload } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import { getJSON, postJSON } from '../../api.js'
import { Badge, Card, Empty, ErrorBox, Loading, useAction, useConfirm, useToast } from '../../../shared/ui.jsx'
import { DiffView } from '../../../shared/Editor.jsx'
import { hunks, lineDiff } from '../../../shared/diff.js'
import { dateTime } from '../../format.js'
import { useMay } from '../../may.js'

// haproxy.cfg's history (HAProxyService.ConfigHistory): every
// configuration the node accepted - who applied it, when, what it
// changed - each one's diff against the running configuration or the
// version before it, and going back to one (ApplyConfig of that version).
// A reader sees the versions with their secrets masked, like the running
// configuration above.

const author = (a) => (a?.name ? `${a.name}${a.via ? ` via ${a.via}` : ''}` : '-')

export default function ConfigHistory({ running, generation, onApplied, onOpen }) {
  const [history, setHistory] = useState(null)
  const [error, setError] = useState(null)
  const [selected, setSelected] = useState(null)
  const [against, setAgainst] = useState('running')
  const [texts, setTexts] = useState({})
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const toast = useToast()
  const may = useMay()
  const canApply = may('HAProxyService/ApplyConfig')

  useEffect(() => {
    let cancelled = false
    getJSON('/api/haproxy/config/history')
      .then((h) => {
        if (cancelled) return
        setHistory(h)
        setError(null)
        setTexts({})
      })
      .catch((err) => !cancelled && setError(err))
    return () => {
      cancelled = true
    }
  }, [generation])

  const versions = useMemo(() => history?.versions || [], [history])
  const index = versions.findIndex((v) => v.id === selected)
  const version = index >= 0 ? versions[index] : null
  const previous = index >= 0 ? versions[index + 1] : null

  // The texts the diff needs: the selected version's, and the one before
  // it when it's compared with that.
  useEffect(() => {
    const want = [version?.id, against === 'previous' ? previous?.id : null].filter((id) => id && texts[id] == null)
    if (!want.length) return undefined
    let cancelled = false
    Promise.all(want.map((id) => getJSON(`/api/haproxy/config/history/${id}`)))
      .then((rs) => !cancelled && setTexts((t) => ({ ...t, ...Object.fromEntries(rs.map((r) => [r.version.id, r.config])) })))
      .catch((err) => !cancelled && setError(err))
    return () => {
      cancelled = true
    }
  }, [version, previous, against, texts])

  const base = against === 'previous' ? (previous ? texts[previous.id] : '') : running
  const text = version ? texts[version.id] : null
  const diff = useMemo(() => (text != null && base != null ? hunks(lineDiff(base, text)) : null), [base, text])

  const applyVersion = async () => {
    const changes = text != null && running != null ? hunks(lineDiff(running, text)) : []
    const ok = await confirm({
      title: `Go back to version ${version.id}?`,
      body: (
        <>
          <p>HAProxy validates it first, as any configuration; accepted, it takes over without dropping connections, and it is kept as a new version of the history.</p>
          <DiffView diff={changes} />
        </>
      ),
      action: 'Apply',
      wide: true,
    })
    if (!ok) return
    await run(async () => {
      const r = await postJSON('/api/haproxy/config', { version: version.id })
      if (!r.accepted) throw new Error(`Rejected: ${r.message}`)
      toast(`Back to version ${version.id}`)
      setSelected(null)
      onApplied()
    })
  }

  if (error?.status === 501) {
    return (
      <Card title="History" icon={HistoryIcon}>
        <p className="muted small" style={{ margin: 0 }}>
          This node&apos;s version keeps no history of its configuration: update it.
        </p>
      </Card>
    )
  }
  return (
    <Card
      title="History"
      icon={HistoryIcon}
      actions={history && <span className="muted small">the newest {history.max_versions} versions are kept</span>}
    >
      <ErrorBox error={error} />
      {!history ? (
        !error && <Loading />
      ) : !versions.length ? (
        <Empty>No configuration applied since this node began keeping a history</Empty>
      ) : (
        <>
          {history.redacted && (
            <p className="muted small" style={{ marginTop: 0 }}>
              Secrets are masked in every version, and their figures are the masked texts&apos;: your role may read the configuration, not apply one.
            </p>
          )}
          <table className="table">
            <thead>
              <tr>
                <th>Version</th>
                <th>Applied</th>
                <th>By</th>
                <th>Changes</th>
                <th>SHA-256</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {versions.map((v) => (
                <tr key={v.id}>
                  <td>
                    <button className="small" aria-pressed={v.id === selected} onClick={() => setSelected(v.id === selected ? null : v.id)}>
                      #{v.id}
                    </button>
                  </td>
                  <td className="small">{v.applied_unix ? dateTime(Number(v.applied_unix) * 1000) : <span className="muted">before the history</span>}</td>
                  <td className="small" title={v.author?.roles?.join(', ')}>
                    {author(v.author)}
                  </td>
                  <td className="mono small">
                    <span style={{ color: 'var(--ok)' }}>+{v.lines_added || 0}</span> <span style={{ color: 'var(--danger)' }}>−{v.lines_removed || 0}</span>
                  </td>
                  <td className="mono small muted">{String(v.sha256).slice(0, 12)}</td>
                  <td>
                    {v.running && (
                      <Badge tone="ok" dot>
                        running
                      </Badge>
                    )}{' '}
                    {v.reverted_from > 0 && <Badge tone="info">back to #{v.reverted_from}</Badge>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {version && (
            <div className="stack" style={{ marginTop: '0.8rem' }}>
              <div className="row">
                <label className="field" style={{ width: '20rem' }}>
                  <span>Version {version.id} compared with</span>
                  <select value={against} onChange={(e) => setAgainst(e.target.value)}>
                    <option value="running">the running configuration</option>
                    <option value="previous" disabled={!previous}>
                      the version before it{previous ? ` (#${previous.id})` : ''}
                    </option>
                  </select>
                </label>
                <span className="grow" />
                <button className="small" disabled={text == null} onClick={() => onOpen(text)}>
                  <FileInput size={14} /> Open in the editor
                </button>
                {canApply && !version.running && (
                  <button className="primary small" disabled={busy || text == null} onClick={applyVersion}>
                    <Upload size={14} /> Apply this version…
                  </button>
                )}
              </div>
              {diff == null ? <Loading /> : diff.length === 0 ? <p className="muted small">The same configuration.</p> : <DiffView diff={diff} />}
            </div>
          )}
        </>
      )}
    </Card>
  )
}
