// node --test (npm test)
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { fmtDateTime, fmtRate, fmtTime, inUTC, sameDay, setDisplay } from './units.js'

const bytes = (n) => `${n} B`

test('times in UTC say so, in the browser zone they don\'t', () => {
  const t = Date.UTC(2026, 9, 26, 13, 2, 11)
  setDisplay({ time_zone: 'utc' })
  assert.ok(inUTC())
  assert.match(fmtTime(t, { hour12: false }), /^13:02:11 UTC$/)
  assert.match(fmtDateTime(t, { hour12: false }), /13:02:11 UTC$/)
  assert.match(fmtTime(t, { hour12: false, bare: true }), /^13:02:11$/)
  assert.ok(sameDay(t, Date.UTC(2026, 9, 26, 23, 59)))
  assert.ok(!sameDay(t, Date.UTC(2026, 9, 27, 0, 1)))
  setDisplay({ time_zone: 'local' })
  assert.ok(!inUTC())
  assert.doesNotMatch(fmtTime(t), /UTC/)
  setDisplay()
})

test('rates in bytes or in bits', () => {
  setDisplay({ rate_unit: 'bytes' })
  assert.equal(fmtRate(1024, bytes), '1024 B/s')
  assert.equal(fmtRate(null, bytes), '–')
  setDisplay({ rate_unit: 'bits' })
  assert.equal(fmtRate(100, bytes), '800 bit/s')
  assert.equal(fmtRate(125000, bytes), '1.0 Mbit/s')
  assert.equal(fmtRate(44_100_000, bytes), '352.8 Mbit/s')
  assert.equal(fmtRate(1.25e9, bytes), '10.0 Gbit/s')
  setDisplay()
})
