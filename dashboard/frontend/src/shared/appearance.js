// The Controller's appearance, chosen per browser: a palette
// (palettes.css) and a mode - the system's, light or dark. <html> carries
// the palette and the mode in effect (data-palette, data-theme), set
// before the page paints by public/theme-boot.js (keep its lists equal to
// these: palettes.test.js) and kept up to date here: a choice made in
// another tab - a node's page is one - or the system turning dark.
import { useSyncExternalStore } from 'react'

// Janus's own two first, then the well-known ones by name.
export const PALETTES = [
  { id: 'janus', name: 'Janus' },
  { id: 'abysse', name: 'Abysse' },
  { id: 'ayu', name: 'Ayu' },
  { id: 'catppuccin', name: 'Catppuccin' },
  { id: 'dracula', name: 'Dracula' },
  { id: 'everforest', name: 'Everforest' },
  { id: 'flexoki', name: 'Flexoki' },
  { id: 'github', name: 'GitHub' },
  { id: 'gruvbox', name: 'Gruvbox' },
  { id: 'kanagawa', name: 'Kanagawa' },
  { id: 'night-owl', name: 'Night Owl' },
  { id: 'nightfox', name: 'Nightfox' },
  { id: 'nord', name: 'Nord' },
  { id: 'one', name: 'One' },
  { id: 'rose-pine', name: 'Rosé Pine' },
  { id: 'solarized', name: 'Solarized' },
  { id: 'tokyo-night', name: 'Tokyo Night' },
  { id: 'tomorrow', name: 'Tomorrow' },
]
export const MODES = ['light', 'dark']
export const PREFERENCES = ['system', 'light', 'dark']

// The mode's key is the one the site and its docs use too.
const MODE_KEY = 'janus-theme'
const PALETTE_KEY = 'janus-palette'
const DARK = '(prefers-color-scheme: dark)'

function stored(key) {
  try {
    return localStorage.getItem(key)
  } catch {
    return null // a private window
  }
}

function read() {
  const mode = stored(MODE_KEY)
  const palette = stored(PALETTE_KEY)
  return {
    preference: PREFERENCES.includes(mode) ? mode : 'system',
    palette: PALETTES.some((p) => p.id === palette) ? palette : 'janus',
  }
}

function systemDark() {
  return typeof window !== 'undefined' && !!window.matchMedia?.(DARK).matches
}

let state = null

function current() {
  const next = read()
  const mode = next.preference === 'system' ? (systemDark() ? 'dark' : 'light') : next.preference
  if (!state || state.preference !== next.preference || state.palette !== next.palette || state.mode !== mode) state = { ...next, mode }
  return state
}

function apply() {
  const { mode, palette } = current()
  const root = document.documentElement
  root.setAttribute('data-theme', mode)
  root.setAttribute('data-palette', palette)
  // The browser's own chrome (a phone's address bar) in the page's colour.
  const meta = document.querySelector('meta[name="theme-color"]')
  const bg = getComputedStyle(root).getPropertyValue('--bg').trim()
  if (meta && bg) meta.setAttribute('content', bg)
}

const listeners = new Set()

function changed() {
  apply()
  for (const fn of listeners) fn()
}

if (typeof document !== 'undefined') {
  apply()
  window.addEventListener('storage', (e) => {
    if (e.key === MODE_KEY || e.key === PALETTE_KEY || e.key === null) changed()
  })
  window.matchMedia?.(DARK).addEventListener?.('change', changed)
}

function save(key, value) {
  try {
    localStorage.setItem(key, value)
  } catch {
    // a private window: the choice holds for this page only
  }
}

export function setPreference(preference) {
  if (!PREFERENCES.includes(preference)) return
  save(MODE_KEY, preference)
  changed()
}

export function setPalette(palette) {
  if (!PALETTES.some((p) => p.id === palette)) return
  save(PALETTE_KEY, palette)
  changed()
}

// cyclePalette moves to the next palette (step 1) or the previous (-1),
// and says which.
export function cyclePalette(step) {
  const i = PALETTES.findIndex((p) => p.id === current().palette)
  const next = PALETTES[(i + step + PALETTES.length) % PALETTES.length]
  setPalette(next.id)
  return next
}

// The keyboard's T (shift: back) through the palettes - a key alone, so
// it can be turned off (WCAG 2.1.4), per browser.
const SHORTCUT_KEY = 'janus-palette-shortcut'

export function shortcutOn() {
  return stored(SHORTCUT_KEY) !== 'off'
}

export function setShortcut(on) {
  save(SHORTCUT_KEY, on ? 'on' : 'off')
}

function subscribe(fn) {
  listeners.add(fn)
  return () => listeners.delete(fn)
}

// useAppearance is {preference, palette, mode}, following every change.
export function useAppearance() {
  return useSyncExternalStore(subscribe, current, current)
}
