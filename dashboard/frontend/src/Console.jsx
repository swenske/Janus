import { SquareTerminal, X } from 'lucide-react'
import { useEffect, useMemo, useRef, useState } from 'react'
import { ansiSegments } from './ansi.js'
import { useSSE } from './shared/sse.js'
import { Badge, stateTone } from './shared/ui.jsx'

const MAX_CHARS = 256 * 1024

// MachineConsole follows a machine's serial console (GET
// /api/machines/{id}/console): read-only - a Janus node has no shell -
// and only from when it's opened, the hypervisor keeps no history of it.
export default function MachineConsole({ machine, onClose }) {
  // The raw output, read as a whole: an escape sequence can still be cut
  // across two messages. Its colors are kept (ansi.js) - the node's
  // banner is drawn with them - everything else a terminal does is not.
  const [raw, setRaw] = useState('')
  const segments = useMemo(() => ansiSegments(raw), [raw])
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
  }, [segments])
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
          {segments.length ? (
            segments.map((s, i) =>
              s.style ? (
                <span key={i} style={s.style}>
                  {s.text}
                </span>
              ) : (
                s.text
              ),
            )
          ) : (
            <span className="console-waiting">Waiting for output - the console shows what the machine prints from now on.</span>
          )}
        </pre>
        {failure && <div className="error-box small">{failure}</div>}
        <p className="muted small" style={{ margin: 0 }}>
          The serial console, read-only. Private keys are hidden: a node prints its admin credential on its first boot, and the Controller never keeps it.
        </p>
      </div>
    </div>
  )
}
