import { useEffect, useState } from 'react'

// The time a node page's charts show (Overview, Metrics): one choice for
// both, remembered per browser and followed by the other node pages.
export const WINDOWS = [
  { ms: 60 * 1000, label: '1 min' },
  { ms: 5 * 60 * 1000, label: '5 min' },
  { ms: 15 * 60 * 1000, label: '15 min' },
  { ms: 0, label: 'All' },
]
const KEY = 'janus-chart-window'

function stored() {
  try {
    const s = localStorage.getItem(KEY)
    return s !== null && WINDOWS.some((w) => w.ms === Number(s)) ? Number(s) : WINDOWS[0].ms
  } catch {
    return WINDOWS[0].ms
  }
}

export function useChartWindow() {
  const [ms, setMs] = useState(stored)
  useEffect(() => {
    const on = (e) => {
      if (e.key === KEY || e.key === null) setMs(stored())
    }
    window.addEventListener('storage', on)
    return () => window.removeEventListener('storage', on)
  }, [])
  const set = (v) => {
    setMs(v)
    try {
      localStorage.setItem(KEY, String(v))
    } catch {
      // a private window: for this page only
    }
  }
  return [ms, set]
}
