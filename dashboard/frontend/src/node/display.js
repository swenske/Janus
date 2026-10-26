import { backgroundHeaders } from '../shared/activity.js'
import { setDisplay } from '../shared/units.js'

// loadDisplay reads the account's display choices - times in the
// browser's zone or UTC, traffic in bytes or bits (the Controller's
// Settings) - from the Controller itself: /api/me/preferences, the same
// origin as this page, not the node's API. A Controller too old to have
// them (404), or no answer within 2 s: the defaults.
export async function loadDisplay() {
  const ctl = new AbortController()
  const timer = setTimeout(() => ctl.abort(), 2000)
  try {
    const r = await fetch('/api/me/preferences', { headers: backgroundHeaders(), signal: ctl.signal })
    if (r.ok) setDisplay(await r.json())
  } catch {
    // the defaults
  } finally {
    clearTimeout(timer)
  }
}
