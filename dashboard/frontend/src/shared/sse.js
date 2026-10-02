import { useEffect, useRef, useState } from 'react'

// useSSE follows a Server-Sent Events stream while enabled; onMessage
// gets each data payload, onFailure a node-side error ("failure" event).
export function useSSE(url, { enabled = true, onMessage, onFailure }) {
  const [status, setStatus] = useState('connecting')
  const handlers = useRef({ onMessage, onFailure })
  handlers.current = { onMessage, onFailure }

  useEffect(() => {
    if (!enabled || !url) {
      setStatus('paused')
      return undefined
    }
    setStatus('connecting')
    const es = new EventSource(url)
    es.onopen = () => setStatus('live')
    es.onmessage = (e) => handlers.current.onMessage?.(e.data)
    es.addEventListener('failure', (e) => {
      setStatus('failed')
      handlers.current.onFailure?.(e.data)
      es.close()
    })
    // EventSource retries on its own after a dropped connection.
    es.onerror = () => setStatus(es.readyState === EventSource.CLOSED ? 'failed' : 'reconnecting')
    return () => es.close()
  }, [url, enabled])

  return status
}
