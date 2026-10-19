import assert from 'node:assert/strict'
import test from 'node:test'
import { ServerClock, advanceEdge, area, bezier, clip, curve, ease, fitSize, gaps, nearest, niceMax, segments, snapDistance, timeStep, timeTicks } from './math.js'

test('niceMax: four round quarters', () => {
  assert.equal(niceMax(0), 1)
  assert.equal(niceMax(1), 1)
  assert.equal(niceMax(27), 32)
  assert.equal(niceMax(41), 60)
  assert.equal(niceMax(100), 100)
})

test('niceMax: quarters land on round values, the top close above', () => {
  for (const v of [0.3, 0.9, 3, 7.5, 12, 27, 41, 99, 100, 101, 999, 1234, 262144]) {
    const top = niceMax(v)
    assert.ok(top >= v, `${v} -> ${top}`)
    const q = top / 4
    const m = q / 10 ** Math.floor(Math.log10(q) + 1e-12)
    assert.ok([1, 1.5, 2, 2.5, 3, 4, 5, 6, 8].some((x) => Math.abs(m - x) < 1e-9), `${v} -> quarter ${q}`)
    assert.ok(top <= v * 1.5 + 1e-9 || v < 1, `${v} -> ${top}: too loose`)
  }
})

test('niceMax: bytes in powers of two, at least 1 KiB', () => {
  assert.equal(niceMax(0, true), 1024)
  assert.equal(niceMax(1, true), 1024)
  assert.equal(niceMax(1025, true), 2048)
  assert.equal(niceMax(70 * 1024, true), 128 * 1024)
})

test('time ticks sit on their step and keep their distance', () => {
  const step = timeStep(5 * 60 * 1000, 600)
  assert.ok((5 * 60 * 1000) / step <= 600 / 90)
  const ticks = timeTicks(1_000_000_123, 1_000_300_123, step)
  assert.ok(ticks.length >= 2)
  for (const t of ticks) assert.equal(t % step, 0)
  assert.equal(timeStep(60 * 1000, 600), 10000)
})

test('segments break on missing values and on time gaps', () => {
  const pts = [
    { t: 0, iv: 1000, v: 1 },
    { t: 1000, iv: 1000, v: 2 },
    { t: 2000, iv: 1000, v: null },
    { t: 3000, iv: 1000, v: 4 },
    { t: 4000, iv: 1000, v: 5 },
    { t: 9000, iv: 1000, v: 6 }, // a hidden tab: 5 s without a sample
    { t: 10000, iv: 1000, v: 7 },
  ]
  const runs = segments(pts, (p) => p.v)
  assert.deepEqual(
    runs.map((r) => r.map(([t]) => t)),
    [[0, 1000], [3000, 4000], [9000, 10000]],
  )
})

// Every cubic of the curve stays within its two samples' values: a
// burst never draws below zero, a full CPU never above 100 %.
test('the curve never overshoots its samples', () => {
  let seed = 7
  const rnd = () => ((seed = (seed * 1103515245 + 12345) % 2 ** 31) / 2 ** 31)
  for (let n = 0; n < 300; n++) {
    const run = []
    let t = 0
    for (let i = 0; i < 2 + Math.floor(rnd() * 30); i++) {
      t += 500 + rnd() * 3000
      run.push([t, rnd() < 0.3 ? 0 : rnd() < 0.2 ? 100 : rnd() * 100])
    }
    const d = curve(run, (x) => x / 1000, (v) => -v)
    const nums = d.match(/-?[\d.]+(e-?\d+)?/g).map(Number)
    let at = [nums[0], nums[1]]
    for (let i = 2, seg = 0; i + 5 < nums.length + 1 && seg < run.length - 1; i += 6, seg++) {
      const c1 = [nums[i], nums[i + 1]]
      const c2 = [nums[i + 2], nums[i + 3]]
      const end = [nums[i + 4], nums[i + 5]]
      const lo = Math.min(at[1], end[1]) - 1e-6
      const hi = Math.max(at[1], end[1]) + 1e-6
      for (let u = 0; u <= 1; u += 1 / 50) {
        const [x, y] = bezier(at, c1, c2, end, u)
        assert.ok(y >= lo && y <= hi, `run ${n} segment ${seg}: y ${y} outside [${lo}, ${hi}]`)
        assert.ok(x >= at[0] - 1e-6 && x <= end[0] + 1e-6, `run ${n} segment ${seg}: x goes back`)
      }
      at = end
    }
  }
})

test('a lone sample is a dot, an area closes on its baseline', () => {
  assert.match(curve([[1000, 5]], (t) => t, (v) => v), /^M1000,5h/)
  assert.match(area([[0, 1], [10, 2]], (t) => t, (v) => -v, 0), /L10,0L0,0Z$/)
  assert.equal(area([[0, 1]], (t) => t, (v) => v, 0), '')
})

test('the clock: smoothed offset, a buffer of one interval', () => {
  const c = new ServerClock()
  // The browser 7 s ahead of the Controller, samples every 5 s.
  for (let i = 0; i < 40; i++) c.update(1_000_000 + i * 5000, 1_007_000 + i * 5000 + (i % 3) * 40, 5000)
  assert.ok(Math.abs(c.now(2_000_000) - (2_000_000 - 7000)) < 100)
  assert.ok(c.buffer() >= 5250 && c.buffer() < 5500, `buffer ${c.buffer()}`)
  // A new interval: the history of gaps is dropped.
  c.update(1_200_000, 1_207_000, 1000)
  assert.equal(c.buffer(), 1250)
  c.update(1_201_000, 1_208_000, 1000)
  assert.equal(c.buffer(), 1250)
  // Bounds.
  const slow = new ServerClock()
  slow.update(0, 0, 60000)
  assert.equal(slow.buffer(), 32000)
  // A clock step is taken at once.
  c.update(1_202_000, 1_308_000, 1000)
  assert.ok(Math.abs(c.now(0) - (1_202_000 - 1_308_000)) < 1)
})

test('a pause leaves the edge one interval behind', () => {
  // Five samples a second apart, the tab hidden a minute, then again:
  // the minute is a pause, not the sampling's jitter.
  const c = new ServerClock()
  for (let i = 0; i < 5; i++) c.update(i * 1000, i * 1000, 1000)
  c.update(64_000, 64_000, 1000)
  c.update(65_000, 65_000, 1000)
  assert.equal(c.buffer(), 1250)
})

test('gaps: the times with no sample', () => {
  const pts = [0, 1000, 2000, 30000, 31000, 32000].map((t) => ({ t, iv: 1000 }))
  assert.deepEqual(gaps(pts), [[2000, 30000]])
  // A slow answer isn't one; nor is a longer interval's step.
  assert.deepEqual(gaps([0, 1000, 4000, 5000].map((t) => ({ t, iv: 1000 }))), [])
  assert.deepEqual(gaps([{ t: 0, iv: 1000 }, { t: 5000, iv: 5000 }, { t: 10000, iv: 5000 }]), [])
})

test('the edge moves forward at about the real pace, stops at the newest sample', () => {
  let edge = advanceEdge(null, 10_000, 16, 20_000)
  assert.equal(edge, 10_000)
  let desired = 10_000
  let jitter = 0
  for (let i = 0; i < 2000; i++) {
    const dt = 16 + (i % 7)
    desired += dt
    // the desired edge wobbles as samples come early or late
    jitter = ((i * 37) % 400) - 200
    const next = advanceEdge(edge, desired + jitter, dt, 1e12)
    assert.ok(next >= edge, `backwards at ${i}`)
    assert.ok(next - edge <= dt * 1.25 + 1e-9 && next - edge >= dt * 0.75 - 1e-9, `pace at ${i}: ${next - edge} for ${dt}`)
    edge = next
  }
  assert.ok(Math.abs(edge - desired) < 600)
  // No new sample: it stops there.
  assert.equal(advanceEdge(50_000, 60_000, 16, 50_005), 50_005)
  // Far behind (a tab shown again): at once.
  assert.equal(advanceEdge(10_000, 90_000, 16, 1e12), 90_000)
  // Samples back after a node's 13 s reboot, at 1 s: at once too.
  assert.equal(advanceEdge(50_000, 63_000, 16, 64_000, snapDistance(1000)), 63_000)
  // A sampling's own jitter doesn't make it jump.
  assert.ok(advanceEdge(50_000, 52_000, 16, 64_000, snapDistance(1000)) < 50_100)
  assert.equal(snapDistance(5000), 14_500)
})

test('ease reaches its target, at any frame rate', () => {
  let a = 0
  let b = 0
  for (let i = 0; i < 60; i++) a = ease(a, 100, 1000 / 60)
  for (let i = 0; i < 30; i++) b = ease(b, 100, 1000 / 30)
  assert.ok(Math.abs(a - b) < 1, `${a} vs ${b}`)
  for (let i = 0; i < 200; i++) a = ease(a, 100, 16.7)
  assert.equal(a, 100)
  assert.equal(ease(null, 5, 16), 5)
})

test('nearest', () => {
  const pts = [{ t: 0 }, { t: 10 }, { t: 20 }, { t: 30 }]
  assert.equal(nearest(pts, -5), 0)
  assert.equal(nearest(pts, 14), 1)
  assert.equal(nearest(pts, 16), 2)
  assert.equal(nearest(pts, 99), 3)
  assert.equal(nearest([], 1), -1)
})

test('the figure fits a donut hole, a long label is cut', () => {
  // The hole: 68 units; at 15 a character is 9 wide.
  assert.equal(fitSize('1M', 15, 68), 15)
  const size = fitSize('113.8 GiB', 15, 68)
  assert.ok(size < 15 && 9 * size * 0.6 <= 68, `size ${size}`)
  assert.equal(clip('be_landing_production', 13), 'be_landing_p…')
  assert.equal(clip('app', 13), 'app')
})
