import { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react'
import { getJSON } from './api.js'

// --- refresh interval (global, remembered per browser) ---

export const INTERVALS = [
  { ms: 0, label: 'Off' },
  { ms: 1000, label: '1s' },
  { ms: 2000, label: '2s' },
  { ms: 5000, label: '5s' },
  { ms: 10000, label: '10s' },
  { ms: 30000, label: '30s' },
]

const RefreshContext = createContext({ interval: 5000, setInterval: () => {} })

function storedInterval() {
  try {
    // Nothing stored is null, and Number(null) is 0 - "Off".
    const stored = localStorage.getItem('janus-refresh')
    const v = Number(stored)
    return stored !== null && INTERVALS.some((i) => i.ms === v) ? v : 5000
  } catch {
    return 5000
  }
}

export function RefreshProvider({ children }) {
  const [interval, setIntervalState] = useState(storedInterval)
  const set = useCallback((ms) => {
    setIntervalState(ms)
    try {
      localStorage.setItem('janus-refresh', String(ms))
    } catch {
      // not remembered, still applied
    }
  }, [])
  return <RefreshContext.Provider value={{ interval, setInterval: set }}>{children}</RefreshContext.Provider>
}

export function useRefresh() {
  return useContext(RefreshContext)
}

function usePageVisible() {
  const [visible, setVisible] = useState(!document.hidden)
  useEffect(() => {
    const on = () => setVisible(!document.hidden)
    document.addEventListener('visibilitychange', on)
    return () => document.removeEventListener('visibilitychange', on)
  }, [])
  return visible
}

const RETRY_MS = 5000

// usePoll fetches path now, then again at the global refresh interval
// (or `every`, if given; 0 = once). Pauses while the tab is hidden.
export function usePoll(path, { every, enabled = true } = {}) {
  const { interval } = useRefresh()
  const period = every ?? interval
  const visible = usePageVisible()
  const [state, setState] = useState({ data: null, error: null, loading: true })
  const [tick, setTick] = useState(0)
  const reload = useCallback(() => setTick((t) => t + 1), [])

  useEffect(() => {
    if (!enabled || !path) return undefined
    let cancelled = false
    let timer
    const run = async () => {
      let failed = false
      try {
        const data = await getJSON(path)
        if (!cancelled) setState({ data, error: null, loading: false })
      } catch (error) {
        failed = true
        if (!cancelled) setState((s) => ({ data: s.data, error, loading: false }))
      }
      // A failure (the node rebooting, say) is retried sooner: what it
      // left on screen is stale until the next answer.
      if (!cancelled && period > 0 && visible) timer = setTimeout(run, failed ? Math.min(period, RETRY_MS) : period)
    }
    run()
    return () => {
      cancelled = true
      clearTimeout(timer)
    }
  }, [path, period, visible, enabled, tick])

  return { ...state, reload }
}

// --- metrics history (app-wide, so charts keep their data across views) ---

const MAX_SAMPLES = 900
const MetricsContext = createContext(null)

// derive turns two consecutive raw samples into the rates the charts plot.
function derive(prev, cur) {
  const p = { t: cur.time_ms }
  const s = cur.system
  if (s) {
    p.bootTime = s.boot_time_unix
    if (prev?.system && s.cpu_total_ticks > prev.system.cpu_total_ticks) {
      const dt = s.cpu_total_ticks - prev.system.cpu_total_ticks
      const di = (s.cpu_idle_ticks || 0) - (prev.system.cpu_idle_ticks || 0)
      p.cpu = Math.max(0, Math.min(100, (1 - di / dt) * 100))
    }
  }
  const m = cur.memory
  if (m?.total_bytes) {
    p.memTotal = m.total_bytes
    p.memUsed = m.total_bytes - (m.available_bytes || 0)
    p.memCached = m.cached_bytes || 0
    p.memPct = (p.memUsed / m.total_bytes) * 100
  }
  if (cur.load) {
    p.load1 = cur.load.load1 || 0
    p.load5 = cur.load.load5 || 0
    p.load15 = cur.load.load15 || 0
  }
  const seconds = prev ? (cur.time_ms - prev.time_ms) / 1000 : 0
  if (cur.network?.devices) {
    p.net = {}
    for (const d of cur.network.devices) {
      const before = prev?.network?.devices?.find((x) => x.name === d.name)
      p.net[d.name] = {
        rxTotal: d.rx_bytes || 0,
        txTotal: d.tx_bytes || 0,
        rx: before && seconds > 0 ? Math.max(0, ((d.rx_bytes || 0) - (before.rx_bytes || 0)) / seconds) : undefined,
        tx: before && seconds > 0 ? Math.max(0, ((d.tx_bytes || 0) - (before.tx_bytes || 0)) / seconds) : undefined,
      }
    }
  }
  if (cur.services?.processes) {
    p.svc = {}
    for (const sv of cur.services.processes) {
      const before = prev?.services?.processes?.find((x) => x.id === sv.id)
      const cpu = before && seconds > 0 ? Math.max(0, (((sv.cpu_seconds || 0) - (before.cpu_seconds || 0)) / seconds) * 100) : undefined
      p.svc[sv.id] = { cpu, mem: sv.memory_bytes || 0 }
    }
  }
  const h = cur.haproxy
  if (h) {
    p.hap = {
      up: true,
      conns: h.current_connections || 0,
      maxConns: h.max_connections || 0,
      connRate: h.connection_rate || 0,
      sessRate: h.session_rate || 0,
      idle: h.idle_percent ?? 100,
      uptime: h.uptime_seconds || 0,
      version: h.version,
      cumReq: h.cumulative_requests || 0,
    }
    const hb = prev?.haproxy
    // A reload resets HAProxy's counters - never plot a negative rate.
    if (hb && seconds > 0 && (h.cumulative_requests || 0) >= (hb.cumulative_requests || 0)) {
      p.hap.reqRate = ((h.cumulative_requests || 0) - (hb.cumulative_requests || 0)) / seconds
    }
  } else if (cur.errors?.haproxy) {
    p.hap = { up: false, error: cur.errors.haproxy }
  }
  return p
}

export function MetricsProvider({ children }) {
  const { interval } = useRefresh()
  const visible = usePageVisible()
  const [points, setPoints] = useState([])
  const [latest, setLatest] = useState(null)
  const [error, setError] = useState(null)
  const prevRaw = useRef(null)
  const [tick, setTick] = useState(0)

  useEffect(() => {
    let cancelled = false
    let timer
    const run = async () => {
      try {
        const raw = await getJSON('/api/metrics')
        if (cancelled) return
        const point = derive(prevRaw.current, raw)
        prevRaw.current = raw
        setLatest(point)
        setError(null)
        setPoints((ps) => {
          const next = ps.length >= MAX_SAMPLES ? ps.slice(ps.length - MAX_SAMPLES + 1) : ps.slice()
          next.push(point)
          return next
        })
      } catch (err) {
        if (!cancelled) setError(err)
      }
      if (!cancelled && interval > 0 && visible) timer = setTimeout(run, interval)
    }
    run()
    return () => {
      cancelled = true
      clearTimeout(timer)
    }
  }, [interval, visible, tick])

  const refresh = useCallback(() => setTick((t) => t + 1), [])
  const clear = useCallback(() => {
    prevRaw.current = null
    setPoints([])
  }, [])
  return <MetricsContext.Provider value={{ points, latest, error, refresh, clear }}>{children}</MetricsContext.Provider>
}

export function useMetrics() {
  return useContext(MetricsContext)
}

// --- the node's version and update state (app-wide, one truth) ---

const NodeStatusContext = createContext(null)

// NodeStatusProvider holds what the top bar, the Update dot and several
// pages show of the node itself - /api/system/overview (hostname,
// version, CPU, boot time) and /api/update-check (latest release,
// security fixes) - fetched once for all of them. Both change on a
// reboot, so they're reloaded as soon as one shows: the metrics poll
// seeing a new boot time or the node answering again after failing
// (a reboot started from anywhere, janusctl included), WaitForNode
// seeing the node back, or "Refresh now".
export function NodeStatusProvider({ children }) {
  const overview = usePoll('/api/system/overview', { every: 60000 })
  const check = usePoll('/api/update-check', { every: 120000 })
  const { latest, error } = useMetrics()
  const reloadOverview = overview.reload
  const reloadCheck = check.reload
  const reload = useCallback(() => {
    reloadOverview()
    reloadCheck()
  }, [reloadOverview, reloadCheck])

  const boot = latest?.bootTime
  const lastBoot = useRef(boot)
  const wasDown = useRef(false)
  useEffect(() => {
    if (error) {
      wasDown.current = true
      return
    }
    const rebooted = boot && lastBoot.current && boot !== lastBoot.current
    if (boot) lastBoot.current = boot
    if (rebooted || (wasDown.current && latest)) {
      wasDown.current = false
      reload()
    }
  }, [boot, error, latest, reload])

  return <NodeStatusContext.Provider value={{ overview, check, reload }}>{children}</NodeStatusContext.Provider>
}

export function useNodeStatus() {
  return useContext(NodeStatusContext)
}

// Shared with the main page.
export { navigate, useHashRoute } from '../shared/route.js'
export { useSSE } from '../shared/sse.js'
