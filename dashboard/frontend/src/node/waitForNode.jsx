import { Loader2 } from 'lucide-react'
import { useEffect, useState } from 'react'
import { getJSON } from './api.js'
import { useNodeStatus } from './hooks.jsx'

// WaitForNode follows a node through a disruptive action: it waits for
// the node to go away (when it's expected to), then for it to answer
// again, and reports what came back.
// Once it's back, everything the page shows of the node itself (top bar,
// Update dot, version) is reloaded - not left as it was before.
export function WaitForNode({ expectDown = true, previousBoot, onBack, label = 'Waiting for the node…' }) {
  const { reload } = useNodeStatus()
  const [phase, setPhase] = useState(expectDown ? 'going-down' : 'coming-back')
  const [elapsed, setElapsed] = useState(0)

  useEffect(() => {
    const started = Date.now()
    let stopped = false
    let sawDown = !expectDown
    const tick = async () => {
      if (stopped) return
      setElapsed(Math.round((Date.now() - started) / 1000))
      try {
        const o = await getJSON('/api/system/overview')
        const rebooted = previousBoot && o.system?.boot_time_unix && o.system.boot_time_unix !== previousBoot
        // A janusd restart can be quicker than our polling: after 20s,
        // answering at all counts as being back.
        const longEnough = !previousBoot && Date.now() - started > 20000
        if (sawDown || rebooted || longEnough) {
          stopped = true
          setPhase('back')
          reload()
          onBack?.(o)
          return
        }
      } catch {
        sawDown = true
        setPhase('coming-back')
      }
      setTimeout(tick, 2000)
    }
    const t = setTimeout(tick, 2500)
    return () => {
      stopped = true
      clearTimeout(t)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  if (phase === 'back') return null
  return (
    <div className="notice warn row">
      <Loader2 size={16} className="spin" />
      <span>
        {label} {phase === 'going-down' ? '(going down)' : '(coming back)'} - {elapsed}s
      </span>
    </div>
  )
}
