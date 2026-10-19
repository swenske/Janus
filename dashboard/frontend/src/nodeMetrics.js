import { useEffect, useState } from 'react'
import { call } from './call.js'
import { ServerClock } from './shared/chart/math.js'

// useNodeMetrics follows the node list's curves (/api/nodes/metrics):
// {byNode: {id: [{t, iv, cpu, req, rx, tx}]}, clock} - each request
// keeps the Controller sampling the nodes shown, so it runs only while
// the list is (on), asks only for the readings it hasn't got, and pauses
// in a hidden tab: the Controller goes on sampling ten minutes after the
// last request, so a list shown again within them has no hole. A
// background request: it never keeps an idle session alive.
const KEEP = 300

export function useNodeMetrics(on, windowMs) {
  const [state, setState] = useState(() => ({ byNode: {}, clock: new ServerClock() }))
  useEffect(() => {
    if (!on) return undefined
    let stop = false
    let timer
    let busy = false
    // The first request asks for what the curves show, not for all the
    // Controller keeps (this browser's clock is near enough for it).
    let since = Date.now() - 2 * windowMs
    let newest = 0
    const run = async () => {
      clearTimeout(timer)
      if (document.hidden) return
      busy = true
      let every = 2000
      try {
        const m = await call(`/api/nodes/metrics?since=${since}`, { background: true, timeout: 15000 })
        if (stop) return
        every = m.interval_ms || every
        // Only the readings after since: a node without a new one keeps
        // its curve.
        setState((s) => {
          const byNode = { ...s.byNode }
          for (const [id, sr] of Object.entries(m.nodes || {})) {
            const got = sr.t.map((t, i) => ({ t, iv: every, cpu: sr.cpu[i], req: sr.req[i], rx: sr.rx[i], tx: sr.tx[i] }))
            byNode[id] = [...(s.byNode[id] || []), ...got].slice(-KEEP)
          }
          return { byNode, clock: s.clock }
        })
        // The curves' clock: the newest reading's time, as it arrives.
        let top = since
        for (const sr of Object.values(m.nodes || {})) if (sr.t.length) top = Math.max(top, sr.t[sr.t.length - 1])
        if (top > newest) {
          newest = top
          state.clock.update(top, Date.now(), every)
        }
        since = top
      } catch {
        // the next round tries again
      } finally {
        busy = false
      }
      if (!stop) timer = setTimeout(run, every)
    }
    // Shown again: the readings since at once, unless a request is on
    // its way.
    const onVisible = () => {
      if (!document.hidden && !busy) run()
    }
    run()
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      stop = true
      clearTimeout(timer)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [on, windowMs, state.clock])
  return state
}
