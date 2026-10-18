import { Check, RefreshCw } from 'lucide-react'
import { useState } from 'react'

// RefreshButton says it did something: its icon turns while onRefresh's
// promise runs - long enough to be seen -, then a check mark, a moment.
export default function RefreshButton({ onRefresh, title = 'Refresh now' }) {
  const [state, setState] = useState('idle') // busy, done
  const click = async () => {
    if (state === 'busy') return
    setState('busy')
    const started = Date.now()
    try {
      await onRefresh()
    } catch {
      // the page says what failed; the button only that it's done
    }
    setTimeout(
      () => {
        setState('done')
        setTimeout(() => setState('idle'), 900)
      },
      Math.max(0, 600 - (Date.now() - started)),
    )
  }
  return (
    <button className={`ghost icon refresh-button ${state}`} title={state === 'busy' ? 'Refreshing…' : title} aria-label={title} aria-busy={state === 'busy'} onClick={click}>
      {state === 'done' ? <Check size={16} /> : <RefreshCw size={16} className={state === 'busy' ? 'spin' : undefined} />}
    </button>
  )
}
