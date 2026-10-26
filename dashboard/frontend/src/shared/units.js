// The account's display choices (the Controller's Settings,
// preferences.js): its pages' times in the browser's zone or in UTC, and
// traffic in bytes or in bits a second. The page sets them from the
// account's preferences (setDisplay); the formatters below and node/
// format.js's read them - nothing else formats a time or a rate.

let zone // undefined: the browser's own
let bits = false

export function setDisplay({ time_zone: tz, rate_unit: unit } = {}) {
  zone = tz === 'utc' ? 'UTC' : undefined
  bits = unit === 'bits'
}

export function inUTC() {
  return zone === 'UTC'
}

export function rateInBits() {
  return bits
}

// A time in UTC says so ("14:02:11 UTC"); bare leaves it out - a chart's
// ticks.
function split(opts) {
  const { bare, ...rest } = opts || {}
  return [{ ...rest, timeZone: zone }, bare || !zone ? '' : ' UTC']
}

export function fmtDateTime(t, opts) {
  const [o, tag] = split(opts)
  return new Date(t).toLocaleString(undefined, o) + tag
}

export function fmtTime(t, opts) {
  const [o, tag] = split(opts)
  return new Date(t).toLocaleTimeString(undefined, o) + tag
}

export function fmtDate(t, opts) {
  const [o] = split(opts)
  return new Date(t).toLocaleDateString(undefined, o)
}

// sameDay: whether t falls on today, in the zone shown.
export function sameDay(t, now = Date.now()) {
  return fmtDate(t) === fmtDate(now)
}

// fmtRate is a rate given in bytes a second, in the unit chosen: KiB/s
// (binary, as HAProxy's counters and the kernel's), or kbit/s, Mbit/s
// (decimal, as a link's speed).
export function fmtRate(bytesPerSecond, bytesText) {
  if (bytesPerSecond == null || Number.isNaN(bytesPerSecond)) return '–'
  if (!bits) return `${bytesText(bytesPerSecond)}/s`
  const units = ['bit/s', 'kbit/s', 'Mbit/s', 'Gbit/s', 'Tbit/s']
  let v = Number(bytesPerSecond) * 8
  let i = 0
  while (Math.abs(v) >= 1000 && i < units.length - 1) {
    v /= 1000
    i++
  }
  return `${i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`
}
