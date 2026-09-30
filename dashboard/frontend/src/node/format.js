export function bytes(n, digits = 1) {
  if (n == null || Number.isNaN(n)) return '–'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let i = 0
  let v = Number(n)
  while (Math.abs(v) >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${i === 0 ? Math.round(v) : v.toFixed(digits)} ${units[i]}`
}

export function rate(n) {
  if (n == null) return '–'
  return `${bytes(n)}/s`
}

export function num(n, digits = 0) {
  if (n == null || Number.isNaN(n)) return '–'
  return Number(n).toLocaleString(undefined, { maximumFractionDigits: digits, minimumFractionDigits: digits })
}

export function compact(n) {
  if (n == null || Number.isNaN(n)) return '–'
  return Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 }).format(n)
}

export function percent(n, digits = 1) {
  if (n == null || Number.isNaN(n)) return '–'
  return `${Number(n).toFixed(digits)}%`
}

export function duration(seconds) {
  if (seconds == null || Number.isNaN(seconds)) return '–'
  let s = Math.max(0, Math.floor(seconds))
  const d = Math.floor(s / 86400)
  s -= d * 86400
  const h = Math.floor(s / 3600)
  s -= h * 3600
  const m = Math.floor(s / 60)
  s -= m * 60
  if (d) return `${d}d ${h}h`
  if (h) return `${h}h ${m}m`
  if (m) return `${m}m ${s}s`
  return `${s}s`
}

export function time(ms) {
  return new Date(ms).toLocaleTimeString()
}

export function dateTime(ms) {
  return new Date(ms).toLocaleString()
}

export function fileMode(mode) {
  // Go's fs.FileMode: type bits in the high bits, permissions in the low 9.
  const types = [
    [1 << 31, 'd'],
    [1 << 27, 'l'],
    [1 << 26, 'b'],
    [1 << 21, 'c'],
    [1 << 25, 'p'],
    [1 << 24, 's'],
  ]
  let t = '-'
  for (const [bit, c] of types) {
    if ((mode >>> 0) & (bit >>> 0)) {
      t = c
      break
    }
  }
  if (t === 'b' && (mode >>> 0) & ((1 << 21) >>> 0)) t = 'c'
  const perms = 'rwxrwxrwx'
  let p = ''
  for (let i = 0; i < 9; i++) p += mode & (1 << (8 - i)) ? perms[i] : '-'
  return t + p
}

export function isDirMode(mode) {
  return ((mode >>> 0) & ((1 << 31) >>> 0)) !== 0
}
