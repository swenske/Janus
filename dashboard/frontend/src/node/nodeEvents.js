import { useEffect, useState } from 'react'
import { apiURL } from '../shared/base.js'

const KEEP = 1000

// useNodeEvents keeps the node's event log, live: the stream replays the
// node's last thousand events, then follows. When the node goes away - a
// revert's reboot - the stream is opened again from the last event seen,
// every 3 s until it answers.
export function useNodeEvents(enabled = true) {
  const [events, setEvents] = useState([])
  useEffect(() => {
    if (!enabled) return undefined
    let es = null
    let retry = null
    let stopped = false
    let lastId = 0
    const open = () => {
      if (stopped) return
      es = new EventSource(apiURL(`/api/stream/events${lastId ? `?since=${lastId}` : ''}`))
      es.onmessage = (m) => {
        let e
        try {
          e = JSON.parse(m.data)
        } catch {
          return
        }
        lastId = Math.max(lastId, e.id || 0)
        setEvents((all) => (all.some((x) => x.id === e.id && x.time_ns === e.time_ns) ? all : [...all, e].slice(-KEEP)))
      }
      const again = () => {
        es.close()
        if (!stopped) retry = setTimeout(open, 3000)
      }
      es.addEventListener('failure', again)
      // EventSource retries a dropped connection on its own, not a
      // refused one (the Controller answers 502 while the node is down).
      es.onerror = () => {
        if (es.readyState === EventSource.CLOSED) again()
      }
    }
    open()
    return () => {
      stopped = true
      clearTimeout(retry)
      es?.close()
    }
  }, [enabled])
  return events
}
