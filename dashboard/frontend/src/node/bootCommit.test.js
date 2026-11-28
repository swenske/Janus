// node --test (npm test)
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { haproxyError, healthCheckOutcome, undoneUpdate } from './bootCommit.js'

const s = (sec) => sec * 1e9
const refused =
  "haproxy refused to start: [ALERT]    (93) : config : parsing [/etc/haproxy/haproxy.cfg:33] : 'resolve_retries' expects a strictly positive integer as argument (was '0')."

// v2026.11.27's lab campaign: a node on slot B (v2026.11.24) updated to
// slot A with a setting HAProxy 3.4.9 refuses - the boot on A at 1000,
// its health check giving up 61 s later, the revert's boot on B at 1070.
const before = [
  { id: 10, time_ns: s(500), type: 'janusd.started', payload: { version: 'v2026.11.24', slot: 'B' } },
  { id: 11, time_ns: s(501), type: 'bootcommit.confirmed', payload: { slot: 'B' } },
  { id: 12, time_ns: s(900), type: 'lifecycle.upgrade', payload: {} },
]
const failedBoot = [
  { id: 13, time_ns: s(1002), type: 'janusd.started', payload: { version: 'v2026.11.27', slot: 'A' } },
  { id: 14, time_ns: s(1003), type: 'haproxy.restart_failed', payload: { error: refused, retry_in_seconds: 1 } },
  { id: 15, time_ns: s(1040), type: 'haproxy.restart_failed', payload: { error: refused, retry_in_seconds: 30 } },
]
const reverted = { id: 16, time_ns: s(1063), type: 'bootcommit.reverted', payload: { slot: 'A', revert_to: 'B' } }
const revertBoot = [{ id: 17, time_ns: s(1072), type: 'janusd.started', payload: { version: 'v2026.11.24', slot: 'B' } }]

test("HAProxy's own words, without janusd's and HAProxy's prefixes", () => {
  assert.equal(
    haproxyError({ payload: { error: refused } }),
    "parsing [/etc/haproxy/haproxy.cfg:33] : 'resolve_retries' expects a strictly positive integer as argument (was '0').",
  )
  assert.equal(haproxyError({ payload: {} }), '')
  assert.equal(haproxyError(null), '')
})

test('a health check still running: pending, with what HAProxy says meanwhile', () => {
  const got = healthCheckOutcome([...before, ...failedBoot], { slot: 'A', sinceNs: s(1000) })
  assert.equal(got.outcome, 'pending')
  assert.match(got.error, /'resolve_retries' expects/)
})

test('a health check that gave up: reverted, to the slot the node went back to', () => {
  const got = healthCheckOutcome([...before, ...failedBoot, reverted, ...revertBoot], { slot: 'A', sinceNs: s(1000) })
  assert.deepEqual(got, {
    outcome: 'reverted',
    revertTo: 'B',
    error: "parsing [/etc/haproxy/haproxy.cfg:33] : 'resolve_retries' expects a strictly positive integer as argument (was '0').",
  })
})

test('a confirmed boot', () => {
  const events = [...before, { id: 13, time_ns: s(1002), type: 'janusd.started', payload: { version: 'v2026.11.27', slot: 'A' } }]
  assert.equal(healthCheckOutcome(events, { slot: 'A', sinceNs: s(1000) }).outcome, 'pending')
  events.push({ id: 14, time_ns: s(1004), type: 'bootcommit.confirmed', payload: { slot: 'A' } })
  assert.deepEqual(healthCheckOutcome(events, { slot: 'A', sinceNs: s(1000) }), { outcome: 'confirmed', revertTo: '', error: '' })
})

test("the boots before the update don't count", () => {
  // B's confirmation at 501 is the previous boot's; a confirmation of A
  // from an older update neither.
  const old = [{ id: 1, time_ns: s(100), type: 'bootcommit.confirmed', payload: { slot: 'A' } }, ...before, ...failedBoot]
  assert.equal(healthCheckOutcome(old, { slot: 'A', sinceNs: s(1000) }).outcome, 'pending')
  assert.equal(healthCheckOutcome(old, { slot: 'B', sinceNs: s(1000) }).outcome, 'pending')
  assert.equal(healthCheckOutcome([], { slot: 'A', sinceNs: s(1000) }).outcome, 'pending')
  assert.equal(healthCheckOutcome(undefined, { slot: 'A', sinceNs: s(1000) }).outcome, 'pending')
})

test('the first word on the slot decides', () => {
  const events = [...failedBoot, reverted, { id: 20, time_ns: s(2000), type: 'bootcommit.confirmed', payload: { slot: 'A' } }]
  assert.equal(healthCheckOutcome(events, { slot: 'A', sinceNs: s(1000) }).outcome, 'reverted')
})

test('an update undone, as the page finds it later', () => {
  const events = [...before, ...failedBoot, reverted, ...revertBoot]
  assert.deepEqual(undoneUpdate(events, 'B'), {
    slot: 'A',
    version: 'v2026.11.27',
    revertTo: 'B',
    timeNs: s(1063),
    error: "parsing [/etc/haproxy/haproxy.cfg:33] : 'resolve_retries' expects a strictly positive integer as argument (was '0').",
  })
  // In any order.
  assert.equal(undoneUpdate([...events].reverse(), 'B')?.version, 'v2026.11.27')
})

test('no undone update once the node has moved on', () => {
  const events = [...before, ...failedBoot, reverted, ...revertBoot]
  // Updated or rolled back since: the node runs the other slot.
  assert.equal(undoneUpdate(events, 'A'), null)
  // A later update confirmed.
  const later = [...events, { id: 30, time_ns: s(3000), type: 'janusd.started', payload: { version: 'v2026.11.28', slot: 'A' } }]
  later.push({ id: 31, time_ns: s(3001), type: 'bootcommit.confirmed', payload: { slot: 'A' } })
  assert.equal(undoneUpdate(later, 'A'), null)
  // Nothing reverted, nothing to say.
  assert.equal(undoneUpdate(before, 'B'), null)
  assert.equal(undoneUpdate([], 'B'), null)
  assert.equal(undoneUpdate(events, ''), null)
})

test("an older boot's refusals aren't the undone update's", () => {
  const oldFailure = { id: 5, time_ns: s(200), type: 'haproxy.restart_failed', payload: { error: 'haproxy refused to start: something else' } }
  const events = [oldFailure, ...before, failedBoot[0], reverted, ...revertBoot]
  const got = undoneUpdate(events, 'B')
  assert.equal(got.error, '')
  assert.equal(got.version, 'v2026.11.27')
})
