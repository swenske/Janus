import { useEffect, useState } from 'react'
import { getText } from '../api.js'
import LogViewer from './LogViewer.jsx'

// BootLog shows what the node kept of a previous boot on its LOGS
// partition: url gives it whole (text/plain). Keyed by url, so each
// boot or service starts afresh.
export default function BootLog({ url, name }) {
  const [text, setText] = useState(null)
  const [error, setError] = useState(null)
  useEffect(() => {
    let live = true
    getText(url)
      .then((t) => live && setText(t))
      .catch((e) => live && setError(e.message))
    return () => {
      live = false
    }
  }, [url])
  if (error) return <div className="error-box">{error}</div>
  if (text === null) return <div className="muted">Reading the LOGS partition…</div>
  return <LogViewer text={text} name={name} emptyText="Nothing kept for this boot." />
}
