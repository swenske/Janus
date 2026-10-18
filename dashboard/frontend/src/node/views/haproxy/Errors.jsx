import { AlertTriangle } from 'lucide-react'
import { useState } from 'react'
import { dateTime } from '../../format.js'
import { Card, Empty } from '../../../shared/ui.jsx'
import { usePoll } from '../../hooks.jsx'
import { RuntimeError } from './RuntimeError.jsx'

const DIRECTIONS = [
  { id: '', label: 'Both' },
  { id: 'request', label: 'Requests' },
  { id: 'response', label: 'Responses' },
]

// The characters one byte takes in HAProxy's escaping: "\xHH", "\r", or
// itself.
function escapedWidth(text, at) {
  if (text[at] !== '\\') return 1
  return text[at + 1] === 'x' ? 4 : 2
}

function BufferLine({ line }) {
  const { text, error_at: at } = line
  const offset = `${String(line.offset).padStart(5, '0')}${line.continued ? '+' : ' '} `
  return (
    <div className="logline">
      <span className="offset">{offset}</span>
      {at >= 0 ? (
        <>
          {text.slice(0, at)}
          <mark className="refused" title="The byte HAProxy refused">
            {text.slice(at, at + escapedWidth(text, at))}
          </mark>
          {text.slice(at + escapedWidth(text, at))}
        </>
      ) : (
        text
      )}
    </div>
  )
}

function CapturedError({ e }) {
  const peer = e.proxy_kind === 'frontend' ? (e.backend ? `to ${e.backend}${e.server ? '/' + e.server : ''}` : '') : `for ${e.frontend}${e.server ? ', server ' + e.server : ''}`
  return (
    <Card
      title={
        <span>
          {e.proxy_kind} <span className="mono">{e.proxy}</span>: invalid {e.direction}
        </span>
      }
      icon={AlertTriangle}
      actions={<span className="muted small">{Number(e.when_unix_ms) ? dateTime(Number(e.when_unix_ms)) : e.when}</span>}
    >
      <p className="muted small" style={{ marginTop: 0 }}>
        From <span className="mono">{e.source}</span> {peer} - refused at byte {e.position}. Credential headers' values are masked.
      </p>
      <div className="logview">
        {(e.lines || []).map((l) => (
          <BufferLine key={l.offset + (l.continued ? '+' : '')} line={l} />
        ))}
      </div>
      <details style={{ marginTop: '0.6rem' }}>
        <summary className="muted small">HAProxy's buffer and parser state</summary>
        <div className="logview" style={{ marginTop: '0.4rem' }}>
          {(e.details || []).map((d, i) => (
            <div key={i} className="logline">
              {d}
            </div>
          ))}
        </div>
      </details>
    </Card>
  )
}

// The last request and response HAProxy refused as invalid, per proxy -
// what it answered 400 or 502 for.
export default function Errors() {
  const [direction, setDirection] = useState('')
  const errors = usePoll(`/api/haproxy/errors${direction ? `?direction=${direction}` : ''}`)
  const list = errors.data?.errors || []
  return (
    <div className="stack">
      <div className="row">
        <div className="chips">
          {DIRECTIONS.map((d) => (
            <button key={d.id} className={`small chip ${direction === d.id ? 'active' : ''}`} onClick={() => setDirection(d.id)}>
              {d.label}
            </button>
          ))}
        </div>
        <span className="grow" />
        {errors.data && (
          <span className="muted small">
            {Number(errors.data.total)} captured since HAProxy started; the last of each proxy is kept
          </span>
        )}
      </div>
      <RuntimeError error={errors.error} what="captured errors" />
      {errors.data && !list.length && <Empty>HAProxy hasn't refused any {direction || 'request or response'} as invalid since it started.</Empty>}
      {list.map((e) => (
        <CapturedError key={`${e.proxy_kind}/${e.proxy}/${e.direction}/${e.event}`} e={e} />
      ))}
    </div>
  )
}
