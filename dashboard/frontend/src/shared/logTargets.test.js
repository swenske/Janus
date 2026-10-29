import assert from 'node:assert/strict'
import { test } from 'node:test'
import { cleanTarget, sameTarget, withPaused, withSources, NODE_DEFAULT_SOURCES } from './logTargets.js'

// A target as the API gives it back: no secret, disabled left out when
// false (Go's omitempty).
const saved = withSources([{ name: 'nas', syslog: { address: 'logs.example:6514', transport: 'tls', format: 'rfc5424' } }], NODE_DEFAULT_SOURCES)[0]

test('a paused target differs from the saved one, and goes to the API paused', () => {
  const paused = withPaused(saved, true)
  assert.equal(paused.disabled, true)
  assert.equal(saved.disabled, undefined, 'the saved target is left as it was')
  assert.equal(sameTarget(paused, saved), false)
  assert.equal(cleanTarget(paused).disabled, true)
  assert.deepEqual(cleanTarget(paused).syslog, saved.syslog)
})

test('paused then resumed before a save is the same as before', () => {
  const back = withPaused(withPaused(saved, true), false)
  assert.equal(sameTarget(back, saved), true)
  assert.equal('disabled' in back, false, 'no "disabled: false" left behind')
  assert.equal(JSON.stringify(back), JSON.stringify(saved))
})

test('a saved paused target resumed differs, and goes to the API sending', () => {
  const savedPaused = { ...saved, disabled: true }
  const resumed = withPaused(savedPaused, false)
  assert.equal(sameTarget(resumed, savedPaused), false)
  assert.equal(cleanTarget(resumed).disabled, false)
  assert.equal(sameTarget(withPaused(resumed, true), savedPaused), true)
})
