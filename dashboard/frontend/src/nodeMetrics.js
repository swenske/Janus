import { useEffect, useState } from 'react'
import { call } from './call.js'
import { ServerClock } from './shared/chart/math.js'

// useNodeMetrics follows the node list's curves (/api/nodes/metrics):
// {byNode: {id: [{t, iv, cpu, req, rx, tx}]}, clock} - each request
// keeps the Controller sampling the nodes shown, so it runs only while
// the list is (on), asks only for the readings it hasn't got, and pauses
// in a hidden tab (the sampling then lapses). A background request: it
// never keeps an idle session alive.
const KEEP = 120

export function useNodeMetrics(on) {
  const [state, setState] = useState(() => ({ byNode: {}, clock: new ServerClock() }))
  useEffect(() => {
    if (!on) return undefined
    let stop = false
    let timer
    let since = 0
    let newest = 0
    const run = async () => {
      if (document.hidden) return
      let every = 5000
      try {
        const m = await call(`/api/nodes/metrics?since=${since}`, { background: true })
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
      }
      if (!stop) timer = setTimeout(run, every)
    }
    const onVisible = () => {
      if (!document.hidden) {
        clearTimeout(timer)
        run()
      }
    }
    run()
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      stop = true
      clearTimeout(timer)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [on, state.clock])
  return state
}
