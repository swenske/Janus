import { useEffect, useState } from 'react'

// The signed-in account's preferences, on the Controller itself - an
// absolute path, not this node's API (apiURL): the same origin, the
// session cookie.
const PREFERENCES = '/api/me/preferences'

// nodeChartsFrom is the account's "node charts" choice in
// /api/me/preferences' answer: whether the node's page animates its
// charts and the Traffic flow's particles. Anything but an explicit
// false - the field absent, no answer - keeps them moving.
export function nodeChartsFrom(prefs) {
  return prefs?.node_charts !== false
}

// readNodeCharts asks the Controller - a request the page makes by
// itself, which never keeps the session alive (shared/activity.js); a
// Controller without the route (a 404), a failure or a timeout: true.
export async function readNodeCharts() {
  try {
    const resp = await fetch(PREFERENCES, { headers: { 'X-Janus-Background': '1' }, signal: AbortSignal.timeout(10000) })
    if (!resp.ok) return true
    return nodeChartsFrom(await resp.json())
  } catch {
    return true
  }
}

// useNodeCharts is the account's choice, read once the page opens and
// again whenever it shows - changed on the Controller's page, it's
// followed once this one is looked at again.
export function useNodeCharts() {
  const [on, setOn] = useState(true)
  useEffect(() => {
    let cancelled = false
    const read = () =>
      readNodeCharts().then((v) => {
        if (!cancelled) setOn(v)
      })
    read()
    const onShow = () => {
      if (!document.hidden) read()
    }
    document.addEventListener('visibilitychange', onShow)
    return () => {
      cancelled = true
      document.removeEventListener('visibilitychange', onShow)
    }
  }, [])
  return on
}
