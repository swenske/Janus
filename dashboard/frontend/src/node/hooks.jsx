import { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react'
import { getJSON } from './api.js'
import { SIGNED_OUT } from '../shared/activity.js'
import { ServerClock } from '../shared/chart/math.js'
import { statRows } from './flow.js'

// --- refresh interval (global, remembered per browser) ---

export const INTERVALS = [
  { ms: 0, label: 'Off' },
  { ms: 1000, label: '1s' },
  { ms: 2000, label: '2s' },
  { ms: 5000, label: '5s' },
  { ms: 10000, label: '10s' },
  { ms: 30000, label: '30s' },
]

// Every second by default: the charts move at the pace of a second.
const DEFAULT_INTERVAL = 1000
const RefreshContext = createContext({ interval: DEFAULT_INTERVAL, setInterval: () => {} })

function storedInterval() {
  try {
    // Nothing stored is null, and Number(null) is 0 - "Off".
    const stored = localStorage.getItem('janus-refresh')
    const v = Number(stored)
    return stored !== null && INTERVALS.some((i) => i.ms === v) ? v : DEFAULT_INTERVAL
  } catch {
    return DEFAULT_INTERVAL
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

// Once the session has ended nothing polls again: every answer would be a
// 401 until the user signs in and reloads the page, as its banner says -
// a hidden tab sampling on would ask for 15 minutes.
let signedOut = false
if (typeof window !== 'undefined') window.addEventListener(SIGNED_OUT, () => (signedOut = true))

// pollTimeout bounds a repeated request: a few periods, never under
// `min` - one that never answers would otherwise stop the poll for good.
function pollTimeout(period, min) {
  return period > 0 ? Math.max(min, 3 * period) : undefined
}

// usePoll fetches path now, then again at the global refresh interval
// (or `every`, if given; 0 = once). Pauses while the tab is hidden. at is
// when the data arrived (Date.now()): a rate between two answers is over
// that time - not when a render got to them.
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
        const data = await getJSON(path, { timeout: pollTimeout(period, 30000) })
        if (!cancelled) setState({ data, error: null, loading: false, at: Date.now() })
      } catch (error) {
        failed = true
        if (!cancelled) setState((s) => ({ data: s.data, error, loading: false }))
      }
      // A failure (the node rebooting, say) is retried sooner: what it
      // left on screen is stale until the next answer.
      if (!cancelled && period > 0 && visible && !signedOut) timer = setTimeout(run, failed ? Math.min(period, RETRY_MS) : period)
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
// A hidden tab keeps sampling this long - a look at another tab leaves no
// hole in the curves - then stops: a tab forgotten in the background
// mustn't keep its node busy. What it missed shows as a gap, shaded.
const HIDDEN_SAMPLING_MS = 15 * 60 * 1000
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
      // A counter that went back (the node rebooted): no point, as for
      // HAProxy's - not a 0 nothing measured.
      const rate = (k) => (before && seconds > 0 && (d[k] || 0) >= (before[k] || 0) ? ((d[k] || 0) - (before[k] || 0)) / seconds : undefined)
      p.net[d.name] = {
        rxTotal: d.rx_bytes || 0,
        txTotal: d.tx_bytes || 0,
        rx: rate('rx_bytes'),
        tx: rate('tx_bytes'),
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
  const [points, setPoints] = useState([])
  const [latest, setLatest] = useState(null)
  const [error, setError] = useState(null)
  const prevRaw = useRef(null)
  const [tick, setTick] = useState(0)
  // One clock for every chart of the page: they scroll together.
  const [clock] = useState(() => new ServerClock())
  // refresh's callers, waiting for the next sample.
  const waiting = useRef([])

  useEffect(() => {
    let cancelled = false
    let timer
    let busy = false
    let hiddenSince = document.hidden ? Date.now() : null
    const run = async () => {
      clearTimeout(timer)
      busy = true
      try {
        const raw = await getJSON('/api/metrics', { timeout: pollTimeout(interval, 10000) })
        if (cancelled) return
        const point = derive(prevRaw.current, raw)
        // The interval it was taken at: the charts break their lines over a
        // longer gap, and scroll at that pace.
        point.iv = interval
        clock.update(raw.time_ms, Date.now(), interval)
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
      } finally {
        busy = false
      }
      const done = waiting.current
      waiting.current = []
      for (const resolve of done) resolve()
      if (cancelled || interval <= 0 || signedOut) return
      // Hidden too long: the poll stops, until the tab shows again.
      if (hiddenSince != null && Date.now() - hiddenSince >= HIDDEN_SAMPLING_MS) return
      timer = setTimeout(run, interval)
    }
    const onVisibility = () => {
      if (document.hidden) {
        hiddenSince = Date.now()
        return
      }
      hiddenSince = null
      // Shown again: a sample at once, unless one is on its way.
      if (!busy && interval > 0 && !signedOut) run()
    }
    run()
    document.addEventListener('visibilitychange', onVisibility)
    return () => {
      cancelled = true
      clearTimeout(timer)
      document.removeEventListener('visibilitychange', onVisibility)
    }
  }, [interval, tick, clock])

  // refresh samples now; its promise is the sample's (5 s at most).
  const refresh = useCallback(
    () =>
      new Promise((resolve) => {
        waiting.current.push(resolve)
        setTick((t) => t + 1)
        setTimeout(resolve, 5000)
      }),
    [],
  )
  const clear = useCallback(() => {
    prevRaw.current = null
    clock.reset()
    setPoints([])
  }, [clock])
  return <MetricsContext.Provider value={{ points, latest, error, refresh, clear, clock }}>{children}</MetricsContext.Provider>
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
// seeing the node back, or "Refresh now" - recheck, which asks GitHub
// again too, past the Controller's cache of its releases.
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
  const recheck = useCallback(() => {
    reloadOverview()
    return getJSON('/api/update-check?refresh=1')
      .catch(() => {})
      .finally(reloadCheck)
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

  return <NodeStatusContext.Provider value={{ overview, check, reload, recheck }}>{children}</NodeStatusContext.Provider>
}

export function useNodeStatus() {
  return useContext(NodeStatusContext)
}

// Shared with the main page.
export { navigate, useHashRoute } from '../shared/route.js'
export { useSSE } from '../shared/sse.js'

// useCountdown is the seconds left until unix (a trial's revert time),
// ticking every second; 0 without one.
export function useCountdown(unix) {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!unix) return undefined
    const t = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(t)
  }, [unix])
  return unix ? Math.max(0, Math.round(unix - now / 1000)) : 0
}

// --- HAProxy's counters, twice ---

// useStatSamples keeps the last two answers of /api/haproxy/stats (rows
// by column name, flow.js statRows) and when each came: the byte
// counters' difference is the traffic (withRates).
export function useStatSamples(data) {
  const [s, setS] = useState({ cur: null, prev: null, seconds: 0 })
  useEffect(() => {
    if (!data) return
    const now = Date.now()
    const rows = statRows(data)
    setS((p) => ({ prev: p.cur, cur: { rows, t: now }, seconds: p.cur ? (now - p.cur.t) / 1000 : 0 }))
  }, [data])
  return s
}
