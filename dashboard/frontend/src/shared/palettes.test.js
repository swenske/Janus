// Every palette (palettes.css), light and dark, holds the same tokens and
// WCAG AA's contrasts; theme.css (the site's too) stays the "janus" one.
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import { MODES, PALETTES } from './appearance.js'

const read = (name) => readFileSync(new URL(name, import.meta.url), 'utf8')

// blocks returns {selector: {property: value}} for a stylesheet's rules
// holding declarations (a media query's inner rule under its selector).
function blocks(css) {
  const out = {}
  const clean = css.replace(/\/\*[\s\S]*?\*\//g, '')
  for (const m of clean.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    const decls = {}
    for (const d of m[2].split(';')) {
      const i = d.indexOf(':')
      if (i > 0) decls[d.slice(0, i).trim()] = d.slice(i + 1).trim()
    }
    for (const sel of m[1].split(',')) out[sel.trim().replace(/\s+/g, ' ')] = { ...(out[sel.trim()] || {}), ...decls }
  }
  return out
}

function parse(color) {
  let m = color.match(/^#([0-9a-f]{3,8})$/i)
  if (m) {
    let h = m[1]
    if (h.length <= 4) h = [...h].map((c) => c + c).join('')
    const n = (i) => parseInt(h.slice(i, i + 2), 16)
    return { r: n(0), g: n(2), b: n(4), a: h.length === 8 ? n(6) / 255 : 1 }
  }
  m = color.match(/^rgba?\(\s*([\d.]+)\s*,\s*([\d.]+)\s*,\s*([\d.]+)\s*(?:,\s*([\d.]+)\s*)?\)$/)
  if (m) return { r: +m[1], g: +m[2], b: +m[3], a: m[4] == null ? 1 : +m[4] }
  throw new Error(`not a colour: ${color}`)
}

// over composites a (possibly translucent) colour over an opaque one.
function over(fg, bg) {
  const mix = (k) => fg[k] * fg.a + bg[k] * (1 - fg.a)
  return { r: mix('r'), g: mix('g'), b: mix('b'), a: 1 }
}

function luminance({ r, g, b }) {
  const lin = (v) => {
    const c = v / 255
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4
  }
  return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b)
}

function contrast(a, b) {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x)
  return (hi + 0.05) / (lo + 0.05)
}

const palettes = blocks(read('./palettes.css'))
const block = (palette, mode) => palettes[`:root[data-palette="${palette}"][data-theme="${mode}"]`]

test('every palette has a block per mode, for <html> and for any element', () => {
  for (const { id } of PALETTES) {
    for (const mode of MODES) {
      assert.ok(block(id, mode), `${id} ${mode}: no :root block`)
      assert.deepEqual(palettes[`[data-palette="${id}"][data-theme="${mode}"]`], block(id, mode), `${id} ${mode}: element block differs`)
    }
  }
  const known = new Set(PALETTES.map((p) => p.id))
  for (const sel of Object.keys(palettes)) {
    const id = sel.match(/data-palette="([^"]+)"/)?.[1]
    assert.ok(known.has(id), `${sel}: a palette appearance.js doesn't list`)
  }
})

test('every block defines the same tokens, and its own color-scheme', () => {
  const tokens = Object.keys(block('janus', 'light')).sort()
  for (const { id } of PALETTES) {
    for (const mode of MODES) {
      const b = block(id, mode)
      assert.deepEqual(Object.keys(b).sort(), tokens, `${id} ${mode}`)
      assert.equal(b['color-scheme'], mode, `${id} ${mode}: color-scheme`)
    }
  }
})

// The pairs held to AA: [foreground token, background tokens, minimum].
// A background named "x@y" is x composited over y.
const PAIRS = [
  ['--text', ['--bg', '--surface', '--surface-2', '--surface-3', '--card-bg@--bg'], 4.5],
  ['--muted', ['--bg', '--surface', '--surface-2', '--card-bg@--bg'], 4.5],
  ['--accent-ink', ['--bg', '--surface', '--card-bg@--bg', '--accent-soft@--surface', '--accent-soft@--bg'], 4.5],
  ...['--ok', '--warn', '--danger', '--info'].map((s) => [s, ['--bg', '--surface', '--card-bg@--bg', `${s}-soft@--surface`, `${s}-soft@--bg`], 4.5]),
  ['--accent-text', ['--accent'], 4.5],
  ['#ffffff', ['--danger-solid'], 4.5],
  ['--accent', ['--bg', '--surface'], 3],
  ...['--chart-1', '--chart-2', '--chart-3', '--chart-4', '--chart-5'].map((c) => [c, ['--surface', '--card-bg@--bg'], 3]),
  // A flow badge: its protocol's colour as text on a card.
  ...['--flow-tcp', '--flow-log', '--flow-api'].map((c) => [c, ['--surface', '--card-bg@--bg'], 4.5]),
]

test('palettes hold WCAG AA contrasts', () => {
  const failures = []
  for (const { id } of PALETTES) {
    for (const mode of MODES) {
      const b = block(id, mode)
      const colour = (name) => {
        if (name.includes('@')) {
          const [fg, bg] = name.split('@')
          return over(colour(fg), colour(bg))
        }
        return parse(name.startsWith('--') ? b[name] : name)
      }
      for (const [fg, bgs, min] of PAIRS) {
        for (const bg of bgs) {
          const ratio = contrast(over(colour(fg), colour(bg)), colour(bg))
          if (ratio < min) failures.push(`${id} ${mode}: ${fg} on ${bg} ${ratio.toFixed(2)} < ${min}`)
        }
      }
    }
  }
  assert.deepEqual(failures, [])
})

test('theme.css is the janus palette, its two dark blocks alike', () => {
  const theme = blocks(read('./theme.css'))
  const media = theme[':root:not([data-theme="light"])']
  const dark = theme[':root[data-theme="dark"]']
  assert.deepEqual(media, dark)
  const same = (base, palette, mode) => {
    for (const [k, v] of Object.entries(base)) {
      if (k.startsWith('--') && !['--font', '--mono', '--radius', '--radius-sm'].includes(k)) assert.equal(block(palette, mode)[k], v, `${mode} ${k}`)
    }
  }
  same(theme[':root'], 'janus', 'light')
  same(dark, 'janus', 'dark')
})

test('theme-boot.js sets what appearance.js would, before the page paints', () => {
  const src = read('../../public/theme-boot.js')
  const run = (stored, systemDark) => {
    const attrs = {}
    vm.runInNewContext(src, {
      localStorage: { getItem: (k) => stored[k] ?? null },
      window: { matchMedia: (q) => ({ matches: q === '(prefers-color-scheme: dark)' && systemDark }) },
      document: { documentElement: { setAttribute: (k, v) => (attrs[k] = v) } },
    })
    return attrs
  }
  assert.deepEqual(run({}, false), { 'data-theme': 'light', 'data-palette': 'janus' })
  assert.deepEqual(run({}, true), { 'data-theme': 'dark', 'data-palette': 'janus' })
  assert.deepEqual(run({ 'janus-theme': 'light', 'janus-palette': 'nord' }, true), { 'data-theme': 'light', 'data-palette': 'nord' })
  assert.deepEqual(run({ 'janus-theme': 'bogus', 'janus-palette': 'bogus' }, true), { 'data-theme': 'dark', 'data-palette': 'janus' })
  for (const { id } of PALETTES) assert.equal(run({ 'janus-palette': id }, false)['data-palette'], id)
  // A private window's localStorage throws.
  const attrs = {}
  vm.runInNewContext(src, {
    get localStorage() {
      throw new Error('denied')
    },
    window: {},
    document: { documentElement: { setAttribute: (k, v) => (attrs[k] = v) } },
  })
  assert.deepEqual(attrs, { 'data-theme': 'light', 'data-palette': 'janus' })
})
