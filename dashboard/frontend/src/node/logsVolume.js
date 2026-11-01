// The LOGS partition (/api/log-boots: Install's partition 7, raw - no
// filesystem, never mounted): its first MiB is the volume's own, the
// rest a ring of 1 MiB segments, a boot's each, the oldest written over
// once full - full is its normal state (internal/logvol). What tells an
// operator something is how far back it goes, not how full it is.
import { fmtDateTime } from '../shared/units.js'
import { bytes, duration } from './format.js'

const MiB = 1024 * 1024
const DAY = 86400

// logsVolume reads a /api/log-boots answer at now (ms); null on a node
// without LOGS. used/ring are bytes; oldest is when the oldest segment
// still held was started (ms) - logvol.Boots' Started is a boot's oldest
// segment left; span is how far back that is (seconds); full: the ring
// went round (a segment short allowed: a torn header after a power
// cut); perDay: bytes written a day, used over span, from an hour on.
export function logsVolume(resp, now = Date.now()) {
  const size = Number(resp?.size_bytes) || 0
  if (!resp?.available || !size) return null
  const boots = resp.boots || []
  const segments = Math.max(0, Math.floor(size / MiB) - 1)
  const held = boots.reduce((n, b) => n + (Number(b.mebibytes) || 0), 0)
  const ring = segments * MiB
  const used = Math.min(ring, held * MiB)
  const starts = boots.map((b) => Number(b.started_unix_ns) / 1e6).filter((t) => t > 0)
  const oldest = starts.length ? Math.min(...starts) : null
  const span = oldest == null ? null : Math.max(0, (now - oldest) / 1000)
  const full = segments >= 2 && held >= segments - 1
  const perDay = span != null && span >= 3600 && used > 0 ? (used / span) * DAY : null
  return { size, ring, used, full, boots: boots.length, oldest, span, perDay }
}

// spanText is how far back, short enough for a gauge's middle: "2d 9h",
// "5h 12m", past a month the days alone ("95d").
export function spanText(seconds) {
  if (seconds == null || Number.isNaN(seconds)) return '–'
  if (seconds >= 30 * DAY) return `${Math.round(seconds / DAY)}d`
  return duration(seconds)
}

// amount is a size to two figures or so: "3.9 MiB", "214 MiB".
function amount(n) {
  const s = bytes(n, 1)
  const m = /^(\d+\.\d) (.+)$/.exec(s)
  return m && Number(m[1]) >= 10 ? `${Math.round(Number(m[1]))} ${m[2]}` : s
}

const sinceFormat = { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }

// whole keeps a piece of a line on one line: a narrow tile breaks it
// between pieces ("9 boots · ~213 MiB/day"), not inside one.
const whole = (s) => s.replace(/ /g, '\u00a0')

// logsTexts is what the Overview's LOGS tile says of v (logsVolume):
// display in the gauge's middle, the two lines under it, the tooltip.
export function logsTexts(v, when = (t) => fmtDateTime(t, sinceFormat)) {
  const pct = v.ring ? Math.round((v.used / v.ring) * 100) : 0
  const boots = `${v.boots} ${v.boots === 1 ? 'boot' : 'boots'}`
  const rate = v.perDay != null ? `~${amount(v.perDay)}/day` : null
  // The date and the time each whole: "since Oct 29," then "09:00 PM".
  const lines = [v.oldest == null ? 'nothing kept yet' : `since ${whole(when(v.oldest)).replace(/,\u00a0/g, ', ')}`]
  const ring = `A ring of ${amount(v.ring)}`
  let title
  if (v.full) {
    lines.push([boots, rate].filter(Boolean).map(whole).join(' · '))
    title =
      `${ring}, full: the oldest boot's logs are written over first. It goes back ${spanText(v.span)}` +
      (rate ? `, ${amount(v.perDay)} written a day` : '') +
      '. To keep logs longer, send them to a log target (Log forwarding), or keep HAProxy\'s access logs off this partition (a log profile\'s "HAProxy\'s access logs on the LOGS partition too").'
  } else {
    lines.push([boots, `${pct} %`, 'none written over'].map(whole).join(' · '))
    title = `${ring}, ${pct} % used: nothing written over yet - once it is full, the oldest boot's logs go first.`
    if (v.perDay) title += ` At ~${amount(v.perDay)} a day, full in about ${spanText(((v.ring - v.used) / v.perDay) * DAY)}.`
  }
  return { display: v.oldest == null ? '–' : spanText(v.span), lines, title }
}

// cutBoot is the offset of the boot whose start the ring may have written
// over - the oldest it holds, once it went round; null otherwise.
export function cutBoot(resp) {
  const v = logsVolume(resp)
  const boots = resp?.boots || []
  if (!v?.full || boots.length === 0) return null
  return boots.reduce((o, b) => Math.min(o, Number(b.offset) || 0), 0)
}
