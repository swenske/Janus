// node --test (npm test)
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { cutBoot, logsTexts as texts, logsVolume, spanText } from './logsVolume.js'

// The lines read with plain spaces; the no-break ones checked apart.
function logsTexts(v, when) {
  const t = texts(v, when)
  return { ...t, lines: t.lines.map((l) => l.replace(/\u00a0/g, ' ')), raw: t.lines }
}

const MiB = 1024 * 1024
const HOUR = 3600 * 1000
const now = Date.UTC(2026, 10, 1, 6, 0)
const boot = (offset, startedAgoMs, mebibytes) => ({ offset, number: 10 + offset, started_unix_ns: String((now - startedAgoMs) * 1e6), mebibytes })
// A 512 MiB image's partition, kluster-prod's: 512.98 MiB - the
// volume's own MiB, then 511 segments (logvol).
const size = '537903104'

test('no LOGS: nothing to show', () => {
  assert.equal(logsVolume(undefined, now), null)
  assert.equal(logsVolume({ available: false }, now), null)
  assert.equal(logsVolume({ available: true, size_bytes: '0' }, now), null)
  assert.equal(cutBoot({ available: false }), null)
})

test('a full ring: how far back, the rate, the oldest boot cut', () => {
  // 2.4 days back, nine boots, 511 segments.
  const boots = [boot(0, 5 * HOUR, 40), boot(-1, 20 * HOUR, 150), boot(-2, 30 * HOUR, 100), boot(-3, 35 * HOUR, 60), boot(-4, 40 * HOUR, 50), boot(-5, 45 * HOUR, 40), boot(-6, 50 * HOUR, 30), boot(-7, 55 * HOUR, 25), boot(-8, 57.6 * HOUR, 16)]
  const resp = { available: true, size_bytes: size, boots }
  const v = logsVolume(resp, now)
  assert.equal(v.full, true)
  assert.equal(v.ring, 511 * MiB)
  assert.equal(v.used, 511 * MiB)
  assert.equal(v.boots, 9)
  assert.equal(v.span, 57.6 * 3600)
  assert.equal(Math.round(v.perDay / MiB), 213)
  const t = logsTexts(v, () => 'Oct 29, 20:24')
  assert.equal(t.display, '2d 9h')
  assert.deepEqual(t.lines, ['since Oct 29, 20:24', '9 boots · ~213 MiB/day'])
  assert.match(t.title, /^A ring of 511 MiB, full: the oldest boot's logs are written over first\. It goes back 2d 9h, 213 MiB written a day\. To keep logs longer/)
  assert.equal(cutBoot(resp), -8)
  // A narrow tile breaks a line between its pieces, after the date's comma.
  assert.deepEqual(t.raw, ['since Oct\u00a029, 20:24', '9\u00a0boots · ~213\u00a0MiB/day'])
  // A segment short (a header torn by a power cut) is still full.
  assert.equal(logsVolume({ ...resp, boots: [...boots.slice(0, -1), boot(-8, 57.6 * HOUR, 15)] }, now).full, true)
})

test('a ring filling: none written over, when it fills', () => {
  const resp = { available: true, size_bytes: size, boots: [boot(0, 7 * 24 * HOUR, 153)] }
  const v = logsVolume(resp, now)
  assert.equal(v.full, false)
  assert.equal(v.used, 153 * MiB)
  const t = logsTexts(v, () => 'Oct 25, 06:00')
  assert.equal(t.display, '7d 0h')
  assert.deepEqual(t.lines, ['since Oct 25, 06:00', '1 boot · 30 % · none written over'])
  assert.equal(t.title, 'A ring of 511 MiB, 30 % used: nothing written over yet - once it is full, the oldest boot\'s logs go first. At ~22 MiB a day, full in about 16d 9h.')
  assert.equal(cutBoot(resp), null)
})

test('a ring just started: no rate before an hour, nothing kept yet', () => {
  const v = logsVolume({ available: true, size_bytes: size, boots: [boot(0, 10 * 60 * 1000, 1)] }, now)
  assert.equal(v.perDay, null)
  const t = logsTexts(v, () => 'x')
  assert.equal(t.display, '10m 0s')
  assert.deepEqual(t.lines, ['since x', '1 boot · 0 % · none written over'])
  assert.doesNotMatch(t.title, /a day/)
  const none = logsTexts(logsVolume({ available: true, size_bytes: size, boots: [] }, now))
  assert.equal(none.display, '–')
  assert.equal(none.lines[0], 'nothing kept yet')
})

test('spanText: past a month, the days alone', () => {
  assert.equal(spanText(null), '–')
  assert.equal(spanText(90), '1m 30s')
  assert.equal(spanText(5 * 3600 + 12 * 60), '5h 12m')
  assert.equal(spanText(29 * 86400 + 3600), '29d 1h')
  assert.equal(spanText(95.4 * 86400), '95d')
})
