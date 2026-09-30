import { ArrowDownToLine, Download, Pause, Play, Search, Trash2, WrapText } from 'lucide-react'
import { useEffect, useMemo, useRef, useState } from 'react'
import { useSSE } from '../hooks.jsx'
import { Badge, stateTone } from '../../shared/ui.jsx'

const MAX_LINES = 5000

function lineTone(line) {
  const l = line.toLowerCase()
  if (l.includes('[alert]') || l.includes('[emerg]') || l.includes('error') || l.includes('fail') || l.includes('denied')) return 'log-danger'
  if (l.includes('[warning]') || l.includes('warn')) return 'log-warn'
  if (l.includes('[notice]')) return 'log-info'
  return ''
}

// LogViewer follows a Server-Sent Events stream of text lines. Pausing
// freezes the view (lines keep arriving and show on resume).
export default function LogViewer({ url, name = 'log', height = '65vh', emptyText = 'Waiting for output…' }) {
  const [lines, setLines] = useState([])
  const [failure, setFailure] = useState(null)
  const [paused, setPaused] = useState(false)
  const [follow, setFollow] = useState(true)
  const [wrap, setWrap] = useState(false)
  const [q, setQ] = useState('')
  const [frozen, setFrozen] = useState(null)
  const box = useRef(null)

  useEffect(() => {
    setLines([])
    setFailure(null)
  }, [url])

  const status = useSSE(url, {
    onMessage: (data) => {
      const incoming = data.split('\n')
      setLines((ls) => {
        const next = ls.concat(incoming)
        return next.length > MAX_LINES ? next.slice(next.length - MAX_LINES) : next
      })
    },
    onFailure: (msg) => setFailure(msg),
  })

  const shownSource = paused ? frozen || lines : lines
  const shown = useMemo(() => {
    if (!q) return shownSource
    const needle = q.toLowerCase()
    return shownSource.filter((l) => l.toLowerCase().includes(needle))
  }, [shownSource, q])

  useEffect(() => {
    if (follow && !paused && box.current) box.current.scrollTop = box.current.scrollHeight
  }, [shown, follow, paused])

  const togglePause = () => {
    setFrozen(paused ? null : lines)
    setPaused(!paused)
  }
  const save = () => {
    const blob = new Blob([lines.join('\n') + '\n'], { type: 'text/plain' })
    const a = document.createElement('a')
    a.href = URL.createObjectURL(blob)
    a.download = `${name}-${new Date().toISOString().replace(/[:.]/g, '-')}.log`
    a.click()
    setTimeout(() => URL.revokeObjectURL(a.href), 60000)
  }

  return (
    <div className="stack">
      <div className="row">
        <Badge tone={failure ? 'danger' : paused ? 'warn' : stateTone(status)} dot>
          {failure ? 'failed' : paused ? 'paused' : status}
        </Badge>
        <label className="search">
          <Search size={14} />
          <input placeholder="Filter lines…" value={q} onChange={(e) => setQ(e.target.value)} />
        </label>
        <span className="muted small">
          {shown.length}
          {q ? ` of ${shownSource.length}` : ''} lines
        </span>
        <span className="grow" />
        <button className="small" onClick={togglePause}>
          {paused ? <Play size={14} /> : <Pause size={14} />}
          {paused ? 'Resume' : 'Pause'}
        </button>
        <button className={`small ${follow ? 'active' : ''}`} onClick={() => setFollow(!follow)} title="Keep scrolled to the newest line">
          <ArrowDownToLine size={14} /> Follow
        </button>
        <button className={`small ${wrap ? 'active' : ''}`} onClick={() => setWrap(!wrap)}>
          <WrapText size={14} /> Wrap
        </button>
        <button className="small" onClick={() => setLines([])}>
          <Trash2 size={14} /> Clear
        </button>
        <button className="small" onClick={save} disabled={!lines.length}>
          <Download size={14} /> Download
        </button>
      </div>
      {failure && <div className="error-box">{failure}</div>}
      <div ref={box} className={`logview ${wrap ? 'wrap' : ''}`} style={{ height }}>
        {shown.length === 0 ? (
          <div className="muted">{emptyText}</div>
        ) : (
          shown.map((l, i) => (
            <div key={i} className={`logline ${lineTone(l)}`}>
              {l || ' '}
            </div>
          ))
        )}
      </div>
    </div>
  )
}
