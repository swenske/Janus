import { fmtDate } from './shared/units.js'

const DAY = 24 * 3600 * 1000

// fleetAlert says what's due about the fleet's issuing CA (GET
// /api/fleet), or null: level 'warn' or 'danger', the sentence, and the
// header's badge - from 60 days before its end (the card says it from
// 180, when the Controller starts asking: renew_from).
export function fleetAlert(fleet, now = Date.now()) {
  if (fleet?.state !== 'ready' || !fleet.issuing_not_after) return null
  const end = Date.parse(fleet.issuing_not_after)
  const days = Math.floor((end - now) / DAY)
  if (end <= now) {
    return {
      level: 'danger',
      badge: 'Fleet CA expired',
      text: `The fleet's issuing CA expired on ${fmtDate(end)}: the Controller reaches no node through the fleet, and janusctl gets no certificate. Renew it with the recovery kit and its passphrase.`,
    }
  }
  const renewFrom = fleet.renew_from ? Date.parse(fleet.renew_from) : end - 180 * DAY
  if (now < renewFrom) return null
  const text = `The fleet's issuing CA ends on ${fmtDate(end)} (${days} day${days === 1 ? '' : 's'}): renew it - you'll need the recovery kit and its passphrase.`
  if (days <= 30) return { level: 'danger', badge: `Fleet CA: ${days} days left`, text }
  return { level: 'warn', badge: days <= 60 ? `Fleet CA: ${days} days left` : null, text }
}

// needsKit are the nodes the Controller can't reach through the fleet
// without the recovery kit - their bundle lists no issuing CA it can
// still sign with: [{id, error}].
export function needsKit(fleet) {
  return Object.entries(fleet?.nodes || {})
    .filter(([, t]) => t.state === 'needs-kit')
    .map(([id, t]) => ({ id, error: t.error }))
}

// renewalProgress is where a renewal stands - null when none waits for
// its nodes: the bundle's version, the nodes still on an older one (by
// name), and when janusctl's certificates switch at the latest.
export function renewalProgress(fleet) {
  const r = fleet?.renewal
  if (!r || !r.version || r.switched) return null
  return { version: r.version, behind: fleet.nodes_behind || [], switchBy: r.switch_by || null }
}
