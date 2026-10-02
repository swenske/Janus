import { SquareTerminal, X } from 'lucide-react'
import { useEffect, useMemo, useRef, useState } from 'react'
import { useSSE } from './shared/sse.js'
import { Badge, stateTone } from './shared/ui.jsx'

const MAX_CHARS = 256 * 1024

// Terminal control sequences have no meaning in a <pre>: colors, cursor
// moves and firmware screen modes are dropped, carriage returns resolved.
// eslint-disable-next-line no-control-regex
const CONTROL = /\x1b\[[0-9;?=]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[()][0-9A-Za-z]|\x1b[=>]|[\x00-\x08\x0b\x0c\x0e-\x1a\x1c-\x1f\x7f]/g

function clean(text) {
  return text.replace(CONTROL, '').replace(/\r+\n/g, '\n').replace(/\r/g, '')
}

// MachineConsole follows a machine's serial console (GET
// /api/machines/{id}/console): read-only - a Janus node has no shell -
// and only from when it's opened, the hypervisor keeps no history of it.
export default function MachineConsole({ machine, onClose }) {
  // The raw output, cleaned as a whole: an escape sequence can still be
  // cut across two messages.
  const [raw, setRaw] = useState('')
  const text = useMemo(() => clean(raw), [raw])
  const [failure, setFailure] = useState(null)
  const box = useRef(null)
  const status = useSSE(`/api/machines/${machine.id}/console`, {
    onMessage: (data) => {
      let chunk = ''
      try {
        chunk = JSON.parse(data)
      } catch {
        return
      }
      setRaw((t) => {
        const next = t + chunk
        return next.length > MAX_CHARS ? next.slice(next.length - MAX_CHARS) : next
      })
    },
    onFailure: setFailure,
  })
  useEffect(() => {
    if (box.current) box.current.scrollTop = box.current.scrollHeight
  }, [text])
  useEffect(() => {
    const onKey = (e) => e.key === 'Escape' && onClose()
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal card console-modal" role="dialog" aria-modal="true" onClick={(e) => e.stopPropagation()}>
        <div className="card-header">
          <div className="card-title">
            <SquareTerminal size={16} /> Console · {machine.spec.name}
            <Badge tone={stateTone(status)} dot>
              {status}
            </Badge>
          </div>
          <button className="ghost icon" onClick={onClose} aria-label="Close">
            <X size={16} />
          </button>
        </div>
        <pre ref={box} className="logview console-output">
          {text || <span className="muted">Waiting for output - the console shows what the machine prints from now on.</span>}
        </pre>
        {failure && <div className="error-box small">{failure}</div>}
        <p className="muted small" style={{ margin: 0 }}>
          The serial console, read-only. Private keys are hidden: a node prints its admin credential on its first boot, and the Controller never keeps it.
        </p>
      </div>
    </div>
  )
}
