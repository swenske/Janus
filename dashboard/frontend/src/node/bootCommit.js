// What a node's event log says about the boot confirmation of an update
// made with wait_for_health (internal/bootcommit, cmd/janusd's
// confirmBootHealth): janusd publishes bootcommit.confirmed once HAProxy
// answers healthy on the new slot, or bootcommit.reverted when it gives
// up - before the revert's reboot, so the event is in the log after it.
// haproxy.restart_failed carries HAProxy's own words meanwhile.
//
// Events are the relay's: { id, time_ns, type, payload } (nodeproxy
// streamEvents), time_ns on the node's clock.

const CONFIRMED = 'bootcommit.confirmed'
const REVERTED = 'bootcommit.reverted'
const HAPROXY_FAILED = 'haproxy.restart_failed'
const STARTED = 'janusd.started'

// The text of a haproxy.restart_failed event: HAProxy's alert without
// janusd's "haproxy refused to start: " and HAProxy's own prefix.
export function haproxyError(e) {
  let s = String(e?.payload?.error || '').trim()
  s = s.replace(/^haproxy refused to start:\s*/i, '')
  s = s.replace(/^\[ALERT\]\s*\(\d+\)\s*:\s*/, '')
  s = s.replace(/^config\s*:\s*/, '')
  return s
}

// healthCheckOutcome reads events for the confirmation of slot's boot,
// the one that started at sinceNs: 'confirmed', 'reverted' (with the
// slot it went back to), or 'pending'; error is HAProxy's last refusal
// on that boot.
export function healthCheckOutcome(events, { slot, sinceNs }) {
  let outcome = 'pending'
  let revertTo = ''
  let error = ''
  let errorNs = 0
  for (const e of events || []) {
    if (!(e?.time_ns >= sinceNs)) continue
    if (e.type === HAPROXY_FAILED && e.time_ns >= errorNs) {
      error = haproxyError(e)
      errorNs = e.time_ns
    }
    if (outcome !== 'pending' || e.payload?.slot !== slot) continue
    if (e.type === CONFIRMED) outcome = 'confirmed'
    if (e.type === REVERTED) {
      outcome = 'reverted'
      revertTo = e.payload?.revert_to || ''
    }
  }
  return { outcome, revertTo, error }
}

// undoneUpdate is the last update that didn't hold, if the node is still
// where it went back: the newest boot confirmation in the log is a
// revert, and the node runs the slot it reverted to (a later update or
// rollback moves it to the other one). With the slot that couldn't come
// up and the version it ran (that boot's janusd.started, the first event
// of each run), when, and HAProxy's last refusal on that boot. null
// otherwise.
export function undoneUpdate(events, activeSlot) {
  let last = null
  for (const e of events || []) {
    if ((e?.type === CONFIRMED || e?.type === REVERTED) && (!last || e.time_ns > last.time_ns)) last = e
  }
  if (!last || last.type !== REVERTED || !activeSlot || last.payload?.revert_to !== activeSlot) return null
  let started = null
  for (const e of events) {
    if (e?.type === STARTED && e.time_ns <= last.time_ns && (!started || e.time_ns > started.time_ns)) started = e
  }
  const fromNs = started?.time_ns || 0
  let error = ''
  let errorNs = 0
  for (const e of events) {
    if (e?.type === HAPROXY_FAILED && e.time_ns >= fromNs && e.time_ns <= last.time_ns && e.time_ns >= errorNs) {
      error = haproxyError(e)
      errorNs = e.time_ns
    }
  }
  const version = started?.payload?.slot === last.payload?.slot ? started.payload?.version || '' : ''
  return { slot: last.payload?.slot || '', version, revertTo: activeSlot, timeNs: last.time_ns, error }
}
