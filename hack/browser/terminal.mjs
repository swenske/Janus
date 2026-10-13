// A terminal in a page, for the screenshots of janusctl tui (shots.mjs):
// the escape sequences a 24-bit terminal gets for one frame - what
// cmd/janusctl's TestScreenshotFrame writes - read cell by cell and laid
// on a grid, in a window. Box drawing, block elements and braille are
// drawn, not taken from a font: no font of this image has them all, and
// a terminal joins its box lines from one row to the next, which text in
// a page doesn't.

const CELL_W = 9 // JetBrains Mono's advance at 15px: a cell is whole pixels
const CELL_H = 18
const COLORS = { fg: '#f3efe6', bg: '#131211' } // the terminal's own: the Controller's dark text and background

// parseFrame reads what termui.Frame.Render writes: each row addressed
// with CSI row;1H, then its cells, attributes set with SGR (bold, dim,
// reverse, 24-bit colours).
export function parseFrame(ansi) {
  const rows = []
  let row = null
  let attr = {}
  for (let i = 0; i < ansi.length; ) {
    if (ansi[i] === '\x1b' && ansi[i + 1] === '[') {
      let j = i + 2
      while (j < ansi.length && !/[A-Za-z]/.test(ansi[j])) j++
      const params = ansi.slice(i + 2, j)
      if (ansi[j] === 'H') {
        row = []
        rows[Number(params.split(';')[0]) - 1] = row
      } else if (ansi[j] === 'm') {
        attr = sgr(params, attr)
      }
      i = j + 1
      continue
    }
    const ch = String.fromCodePoint(ansi.codePointAt(i))
    i += ch.length
    if (row) row.push({ ch, ...attr })
  }
  return { cols: Math.max(...rows.map((r) => r.length)), rows }
}

function sgr(params, attr) {
  const p = params === '' ? [0] : params.split(';').map(Number)
  const out = { ...attr }
  for (let k = 0; k < p.length; k++) {
    const v = p[k]
    if (v === 0) for (const key of Object.keys(out)) delete out[key]
    else if (v === 1) out.bold = true
    else if (v === 2) out.dim = true
    else if (v === 7) out.reverse = true
    else if (v === 39) delete out.fg
    else if (v === 49) delete out.bg
    else if ((v === 38 || v === 48) && p[k + 1] === 2) {
      out[v === 38 ? 'fg' : 'bg'] = `rgb(${p[k + 2]}, ${p[k + 3]}, ${p[k + 4]})`
      k += 4
    } else throw new Error(`terminal: SGR ${v} isn't read (${params})`)
  }
  return out
}

// The box drawing characters the dashboard uses: the sides of the cell
// a line leaves by, then how it's drawn - rounded (~), heavy (!),
// dashed (-), dotted (.) - the traffic flow's links.
const BOX = {
  '─': 'lr', '│': 'ud', '┌': 'rd', '┐': 'ld', '└': 'ru', '┘': 'lu', '├': 'udr', '┤': 'udl', '┬': 'lrd', '┴': 'lru', '┼': 'lrud',
  '╭': 'rd~', '╮': 'ld~', '╰': 'ru~', '╯': 'lu~',
  '━': 'lr!', '┃': 'ud!', '╌': 'lr-', '╎': 'ud-', '┄': 'lr.', '┆': 'ud.',
}
const BLOCKS = { '█': [0, 0, 1, 1], '▀': [0, 0, 1, 0.5], '▄': [0, 0.5, 1, 0.5], '▌': [0, 0, 0.5, 1], '▐': [0.5, 0, 0.5, 1] }
const SHADES = { '░': 0.3, '▒': 0.5, '▓': 0.75 }

// glyph is a drawn character as SVG in currentColor, or null for text.
function glyph(ch) {
  const W = CELL_W
  const H = CELL_H
  const cx = W / 2
  const cy = H / 2
  const svg = (body) => `<svg viewBox="0 0 ${W} ${H}" width="${W}" height="${H}" aria-hidden="true">${body}</svg>`
  const cp = ch.codePointAt(0)
  if (BOX[ch]) {
    const sides = BOX[ch]
    const look = { '!': 'stroke-width="2.5"', '-': 'stroke-width="1" stroke-dasharray="3 2"', '.': 'stroke-width="1" stroke-dasharray="1 2"' }[sides.at(-1)]
    if (look) {
      const d = sides.startsWith('l') ? `M0 ${cy}H${W}` : `M${cx} 0V${H}`
      return svg(`<path d="${d}" stroke="currentColor" ${look}/>`)
    }
    if (sides.endsWith('~')) {
      const r = 4
      const [h, v] = sides
      const hx = h === 'r' ? W : 0
      const vy = v === 'd' ? H : 0
      const sx = h === 'r' ? cx + r : cx - r
      const sy = v === 'd' ? cy + r : cy - r
      return svg(`<path d="M${hx} ${cy}H${sx}Q${cx} ${cy} ${cx} ${sy}V${vy}" fill="none" stroke="currentColor" stroke-width="1"/>`)
    }
    const end = { l: [0, cy], r: [W, cy], u: [cx, 0], d: [cx, H] }
    return svg([...sides].map((s) => `<path d="M${cx} ${cy}L${end[s][0]} ${end[s][1]}" stroke="currentColor" stroke-width="1" stroke-linecap="square"/>`).join(''))
  }
  if (BLOCKS[ch]) {
    const [x, y, w, h] = BLOCKS[ch]
    return svg(`<rect x="${x * W}" y="${y * H}" width="${w * W}" height="${h * H}" fill="currentColor"/>`)
  }
  if (cp >= 0x2581 && cp <= 0x2587) {
    const h = ((cp - 0x2580) / 8) * H
    return svg(`<rect x="0" y="${H - h}" width="${W}" height="${h}" fill="currentColor"/>`)
  }
  if (SHADES[ch]) return svg(`<rect width="${W}" height="${H}" fill="currentColor" opacity="${SHADES[ch]}"/>`)
  if (cp >= 0x2800 && cp <= 0x28ff) {
    // Dots 1-3 and 7 down the left column, 4-6 and 8 down the right.
    const bits = cp - 0x2800
    const at = [[0, 0], [0, 1], [0, 2], [1, 0], [1, 1], [1, 2], [0, 3], [1, 3]]
    const dots = at.filter((_, b) => bits & (1 << b)).map(([c, r]) => `<circle cx="${W / 4 + (c * W) / 2}" cy="${H / 8 + (r * H) / 4}" r="1.75" fill="currentColor"/>`)
    return dots.length ? svg(dots.join('')) : ''
  }
  return null
}

const esc = (s) => s.replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c])

// terminalPage is the page: a window titled `title`, the frame in it.
export function terminalPage(ansi, { title }) {
  const { cols, rows } = parseFrame(ansi)
  const body = rows
    .map((row) =>
      row
        .map((c) => {
          let fg = c.fg || COLORS.fg
          let bg = c.bg || null
          if (c.reverse) [fg, bg] = [bg || COLORS.bg, fg]
          const style = [`color:${fg}`, bg && `background:${bg}`, c.bold && 'font-weight:700', c.dim && 'opacity:.6'].filter(Boolean).join(';')
          const g = glyph(c.ch)
          return `<span style="${style}">${g ?? (c.ch === ' ' ? '' : esc(c.ch))}</span>`
        })
        .join(''),
    )
    .map((r) => `<div class="row">${r}</div>`)
    .join('')
  return `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>${esc(title)}</title><style>
:root { --bar: #e8e4da; --bar-line: #dcd6ca; --bar-text: #6b665c; --dot: #c9c2b4; }
@media (prefers-color-scheme: dark) { :root { --bar: #23211e; --bar-line: #33302b; --bar-text: #a39d91; --dot: #4a463f; } }
* { box-sizing: border-box; }
html, body { margin: 0; background: ${COLORS.bg}; }
.window { width: ${cols * CELL_W + 20}px; }
.bar { height: 38px; display: flex; align-items: center; gap: 8px; padding: 0 14px; background: var(--bar); border-bottom: 1px solid var(--bar-line); font: 500 13px Inter, sans-serif; color: var(--bar-text); }
.bar i { width: 12px; height: 12px; border-radius: 50%; background: var(--dot); }
.bar span { flex: 1; text-align: center; margin-right: 52px; }
.screen { padding: 12px 10px; background: ${COLORS.bg}; font: 15px/${CELL_H}px 'JetBrains Mono', monospace; font-variant-ligatures: none; }
.row { display: grid; grid-template-columns: repeat(${cols}, ${CELL_W}px); height: ${CELL_H}px; }
.row span { display: block; height: ${CELL_H}px; white-space: pre; overflow: visible; }
.row svg { display: block; }
</style></head><body><div class="window"><div class="bar"><i></i><i></i><i></i><span>${esc(title)}</span></div><div class="screen">${body}</div></div></body></html>`
}
