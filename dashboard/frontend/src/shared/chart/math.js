// The charts' arithmetic, without the DOM (math.test.js): scales, ticks,
// the curve through the samples, and where the time axis stands while it
// scrolls between them.

// niceMax is the top of a value axis whose four quarter ticks fall on
// round values: 1, 1.5, 2, 2.5, 3, 4, 5, 6 or 8 times a power of ten - or, for bytes,
// a power of two of at least 1 KiB, so the quarters are powers of two too.
export function niceMax(v, binary = false) {
  if (binary) {
    let top = 1024
    while (top < v) top *= 2
    return top
  }
  if (!(v > 0)) return 1
  const quarter = v / 4
  const exp = 10 ** Math.floor(Math.log10(quarter))
  for (const m of [1, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10]) {
    if (quarter <= m * exp * (1 + 1e-9)) return 4 * m * exp
  }
  return 40 * exp
}

// The steps the time axis may use: ticks at least minGap px apart, on
// multiples of the step in absolute time - they scroll with the data.
const STEPS = [1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 10800, 21600].map((s) => s * 1000)

export function timeStep(spanMs, widthPx, minGap = 90) {
  const fit = Math.max(1, widthPx / minGap)
  return STEPS.find((s) => spanMs / s <= fit) || STEPS[STEPS.length - 1]
}

export function timeTicks(t0, t1, step) {
  const out = []
  for (let t = Math.ceil(t0 / step) * step; t <= t1; t += step) out.push(t)
  return out
}

// segments splits the points a series has values for into runs of
// [t, v]: a missing value ends one, and so does a gap longer than 2.5
// sampling intervals (a hidden tab, a node rebooting) - never a line
// drawn across a time with no data.
export function segments(points, get) {
  const out = []
  let run = null
  let prev = null
  for (const p of points) {
    const v = get(p)
    const gap = prev && p.iv > 0 && p.t - prev.t > 2.5 * Math.max(p.iv, prev.iv || 0)
    if (v == null || Number.isNaN(v) || gap) {
      run = null
      if (v == null || Number.isNaN(v)) {
        prev = p
        continue
      }
    }
    if (!run) {
      run = []
      out.push(run)
    }
    run.push([p.t, v])
    prev = p
  }
  return out
}

const sign = (v) => (v > 0 ? 1 : v < 0 ? -1 : 0)

// tangents gives each point of a run the slope a monotone cubic curve
// leaves it with (Steffen's method): never steeper than the chords on
// either side, flat at a peak or a trough - so the curve between two
// samples stays between their values, never dipping below zero beside a
// spike or rising past 100 %.
function tangents(xs, ys) {
  const n = xs.length
  const m = new Array(n).fill(0)
  if (n < 2) return m
  const s = []
  for (let i = 0; i < n - 1; i++) s.push((ys[i + 1] - ys[i]) / (xs[i + 1] - xs[i] || 1))
  for (let i = 1; i < n - 1; i++) {
    const h0 = xs[i] - xs[i - 1]
    const h1 = xs[i + 1] - xs[i]
    const p = (s[i - 1] * h1 + s[i] * h0) / (h0 + h1 || 1)
    m[i] = (sign(s[i - 1]) + sign(s[i])) * Math.min(Math.abs(s[i - 1]), Math.abs(s[i]), 0.5 * Math.abs(p)) || 0
  }
  // The ends: the parabola through the end chord and the next tangent,
  // held within the same bound.
  const end = (slope, t) => {
    const e = (3 * slope - t) / 2
    return sign(e) !== sign(slope) ? 0 : Math.abs(e) > 3 * Math.abs(slope) ? 3 * slope : e
  }
  m[0] = n > 2 ? end(s[0], m[1]) : s[0]
  m[n - 1] = n > 2 ? end(s[n - 2], m[n - 2]) : s[n - 2]
  return m
}

const f = (v) => (Math.round(v * 1000) / 1000).toString()

// curve is the SVG path through a run, x and y mapping a sample's time
// and value to the path's own coordinates.
export function curve(run, x, y) {
  const xs = run.map(([t]) => x(t))
  const ys = run.map(([, v]) => y(v))
  if (xs.length === 1) return `M${f(xs[0])},${f(ys[0])}h0.001`
  const m = tangents(xs, ys)
  let d = `M${f(xs[0])},${f(ys[0])}`
  for (let i = 0; i < xs.length - 1; i++) {
    const dx = (xs[i + 1] - xs[i]) / 3
    d += `C${f(xs[i] + dx)},${f(ys[i] + dx * m[i])},${f(xs[i + 1] - dx)},${f(ys[i + 1] - dx * m[i + 1])},${f(xs[i + 1])},${f(ys[i + 1])}`
  }
  return d
}

// area closes a run's curve down to the baseline y0.
export function area(run, x, y, y0) {
  if (run.length < 2) return ''
  return `${curve(run, x, y)}L${f(x(run[run.length - 1][0]))},${f(y0)}L${f(x(run[0][0]))},${f(y0)}Z`
}

// bezier samples a cubic between p0 and p3 (tests).
export function bezier(p0, p1, p2, p3, u) {
  const a = (1 - u) ** 3
  const b = 3 * (1 - u) ** 2 * u
  const c = 3 * (1 - u) * u ** 2
  const d = u ** 3
  return [a * p0[0] + b * p1[0] + c * p2[0] + d * p3[0], a * p0[1] + b * p1[1] + c * p2[1] + d * p3[1]]
}

function p90(values) {
  const s = [...values].sort((a, b) => a - b)
  return s[Math.min(s.length - 1, Math.floor(s.length * 0.9))]
}

// ServerClock maps this browser's clock onto the samples' (the
// Controller's: a sample's time is when it asked the node), and knows how
// far behind "now" the chart's right edge stays: about one sampling
// interval, so each new sample is already there, just off the edge, when
// the line reaches it - the line grows at the edge, never jumps.
export class ServerClock {
  constructor() {
    this.reset()
  }

  reset() {
    this.offset = null
    this.gaps = []
    this.interval = 0
    this.last = null
  }

  // update takes a sample: its time, this browser's clock when it
  // arrived, and the sampling interval then.
  update(sampleMs, localMs, intervalMs) {
    const raw = sampleMs - localMs
    // A step of the browser's clock (or the Controller's) is taken at
    // once; small differences are smoothed, a sample arriving late
    // mustn't move the axis back.
    this.offset = this.offset == null || Math.abs(raw - this.offset) > 30000 ? raw : this.offset + (raw - this.offset) * 0.1
    if (intervalMs !== this.interval) {
      // The gap since the last sample was the old interval's.
      this.gaps = []
      this.interval = intervalMs
    } else if (this.last != null && sampleMs > this.last) {
      this.gaps.push(sampleMs - this.last)
      if (this.gaps.length > 20) this.gaps.shift()
    }
    this.last = sampleMs
  }

  now(localMs) {
    return localMs + (this.offset ?? 0)
  }

  // buffer is how far behind now the right edge stays.
  buffer() {
    const gap = this.gaps.length ? Math.max(p90(this.gaps), this.interval) : this.interval
    return Math.max(1000, Math.min(32000, (gap || 1000) + 250))
  }
}

// advanceEdge moves the right edge by dt towards where it should be: at
// 0.75 to 1.25 times the real pace, never backwards; at once when it's
// far off (the first frame, a tab shown again), and never past the newest
// sample - when samples stop coming, the line stops instead of scrolling
// out of sight.
export function advanceEdge(edge, desired, dt, lastT, snapMs = 15000) {
  let next
  if (edge == null || Math.abs(desired - edge) > snapMs) next = desired
  else next = edge + Math.max(0, dt) * Math.min(1.25, Math.max(0.75, 1 + (desired - edge) / 2000))
  if (edge != null && next < edge && Math.abs(desired - edge) <= snapMs) next = edge
  return lastT != null ? Math.min(next, lastT) : next
}

// ease moves a value towards its target, the same pace at any frame rate.
export function ease(cur, target, dt) {
  if (cur == null || !Number.isFinite(cur)) return target
  const k = 1 - 0.9 ** (Math.max(0, dt) / 16.7)
  const next = cur + (target - cur) * k
  return Math.abs(next - target) < Math.abs(target) * 1e-3 ? target : next
}

// nearest is the index of the point closest in time to t (points sorted).
export function nearest(points, t) {
  if (!points.length) return -1
  let lo = 0
  let hi = points.length - 1
  while (hi - lo > 1) {
    const mid = (lo + hi) >> 1
    if (points[mid].t < t) lo = mid
    else hi = mid
  }
  return Math.abs(points[lo].t - t) <= Math.abs(points[hi].t - t) ? lo : hi
}
