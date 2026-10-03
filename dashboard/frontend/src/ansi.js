// The terminal colors and attributes (SGR: "ESC [ ... m") of a serial
// console, as styled segments for the web console. Every other control
// sequence - cursor moves, screen modes, titles - is dropped, carriage
// returns resolved: a <pre> has no cursor.

// eslint-disable-next-line no-control-regex
const OTHER = /\x1b\[[0-9;?=]*[ -/]*[@-ln-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[()][0-9A-Za-z]|\x1b[=>]|[\x00-\x08\x0b\x0c\x0e-\x1a\x1c-\x1f\x7f]/g
// eslint-disable-next-line no-control-regex
const SGR = /\x1b\[([0-9;]*)m/g

// The 16 basic colors, tuned for the console's dark background.
export const BASIC = [
  '#5c6370',
  '#e06c75',
  '#98c379',
  '#e5c07b',
  '#61afef',
  '#c678dd',
  '#56b6c2',
  '#d7dae0',
  '#7f848e',
  '#ff7b86',
  '#b5e890',
  '#ffd68a',
  '#82c4ff',
  '#de9cf0',
  '#7fd9e3',
  '#ffffff',
]

// color256 is xterm's 256-color palette: the basic 16, a 6x6x6 cube,
// 24 grays.
export function color256(n) {
  if (n < 16) return BASIC[n]
  if (n >= 232) {
    const v = 8 + (n - 232) * 10
    return `rgb(${v},${v},${v})`
  }
  const c = n - 16
  const level = (x) => (x === 0 ? 0 : 55 + x * 40)
  return `rgb(${level(Math.floor(c / 36))},${level(Math.floor(c / 6) % 6)},${level(c % 6)})`
}

// extended reads "5;n" or "2;r;g;b" after a 38 or 48; it returns the
// color and how many parameters it took.
function extended(params, i) {
  if (params[i] === 5 && params[i + 1] !== undefined) return [color256(params[i + 1] & 255), 2]
  if (params[i] === 2 && params[i + 3] !== undefined) return [`rgb(${params[i + 1] & 255},${params[i + 2] & 255},${params[i + 3] & 255})`, 4]
  return [null, params.length - i]
}

function apply(state, codes) {
  const params = codes === '' ? [0] : codes.split(';').map((p) => (p === '' ? 0 : Number(p)))
  const s = { ...state }
  for (let i = 0; i < params.length; i++) {
    const p = params[i]
    if (p === 0) Object.assign(s, { fg: null, bg: null, bold: false, dim: false, italic: false, underline: false, inverse: false })
    else if (p === 1) s.bold = true
    else if (p === 2) s.dim = true
    else if (p === 3) s.italic = true
    else if (p === 4) s.underline = true
    else if (p === 7) s.inverse = true
    else if (p === 22) s.bold = s.dim = false
    else if (p === 23) s.italic = false
    else if (p === 24) s.underline = false
    else if (p === 27) s.inverse = false
    else if (p >= 30 && p <= 37) s.fg = BASIC[p - 30]
    else if (p >= 90 && p <= 97) s.fg = BASIC[p - 90 + 8]
    else if (p >= 40 && p <= 47) s.bg = BASIC[p - 40]
    else if (p >= 100 && p <= 107) s.bg = BASIC[p - 100 + 8]
    else if (p === 39) s.fg = null
    else if (p === 49) s.bg = null
    else if (p === 38 || p === 48) {
      const [color, used] = extended(params, i + 1)
      if (color) s[p === 38 ? 'fg' : 'bg'] = color
      i += used
    }
  }
  return s
}

const PLAIN = { fg: null, bg: null, bold: false, dim: false, italic: false, underline: false, inverse: false }

// style is a segment's CSS, or null when it has none.
function style(s) {
  const fg = s.inverse ? s.bg || 'var(--console-bg)' : s.fg
  const bg = s.inverse ? s.fg || 'var(--console-fg)' : s.bg
  const css = {}
  if (fg) css.color = fg
  if (bg) css.backgroundColor = bg
  if (s.bold) css.fontWeight = 700
  if (s.dim) css.opacity = 0.6
  if (s.italic) css.fontStyle = 'italic'
  if (s.underline) css.textDecoration = 'underline'
  return Object.keys(css).length ? css : null
}

// ansiSegments splits a console's output into [{text, style}], adjacent
// ones with the same style merged.
export function ansiSegments(raw) {
  const text = raw.replace(/\r+\n/g, '\n').replace(/\r/g, '').replace(OTHER, '')
  const out = []
  let state = PLAIN
  let last = 0
  const push = (t) => {
    if (!t) return
    const css = style(state)
    const prev = out[out.length - 1]
    if (prev && JSON.stringify(prev.style) === JSON.stringify(css)) prev.text += t
    else out.push({ text: t, style: css })
  }
  for (const m of text.matchAll(SGR)) {
    push(text.slice(last, m.index))
    state = apply(state, m[1])
    last = m.index + m[0].length
  }
  // A sequence cut at the end (the rest comes with the next message)
  // isn't shown meanwhile.
  push(text.slice(last).replace(/\x1b\[?[0-9;]*$/, '')) // eslint-disable-line no-control-regex
  return out
}
