// The Controller's appearance, chosen per browser: a palette
// (palettes.css) and a mode - the system's, light or dark. <html> carries
// the palette and the mode in effect (data-palette, data-theme), set
// before the page paints by public/theme-boot.js (keep its lists equal to
// these: palettes.test.js) and kept up to date here: a choice made in
// another tab - a node's page is one - or the system turning dark.
import { useSyncExternalStore } from 'react'

export const PALETTES = [
  { id: 'janus', name: 'Janus' },
  { id: 'nord', name: 'Nord' },
  { id: 'abysse', name: 'Abysse' },
  { id: 'tokyo-night', name: 'Tokyo Night' },
  { id: 'catppuccin', name: 'Catppuccin' },
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

function subscribe(fn) {
  listeners.add(fn)
  return () => listeners.delete(fn)
}

// useAppearance is {preference, palette, mode}, following every change.
export function useAppearance() {
  return useSyncExternalStore(subscribe, current, current)
}
