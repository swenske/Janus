import { createContext, useContext } from 'react'

// The Read-only switch's state (#527, docs/controller-ui.md "Read-only
// mode") - the components are in shared/readOnly.jsx.
//
// It's the browser tab's (sessionStorage), so the main page and the node
// pages it opens agree - a node page opens in a new tab, which gets it
// through its address (carryEditing). A new tab or a new sign-in starts
// from the account's preference (pages_open).

const KEY = 'janus-read-only'

// load is the tab's state - { editing, at: last activity in edit mode } -
// or null when it chose nothing yet (or the browser keeps nothing).
function load() {
  try {
    const v = JSON.parse(sessionStorage.getItem(KEY) || 'null')
    return v && typeof v.editing === 'boolean' ? v : null
  } catch {
    return null
  }
}

// memory is the state when sessionStorage can't keep it (a private
// window, blocked site data): this page alone.
let memory = null

// store keeps the tab's state.
export function store(v) {
  memory = v
  try {
    sessionStorage.setItem(KEY, JSON.stringify(v))
  } catch {
    // memory only
  }
}

// current is the tab's state, null if it chose nothing.
export function current() {
  return load() || memory
}

// forgetReadOnly drops the tab's choice - at a sign-in or a sign-out: the
// next pages start from the account's preference.
export function forgetReadOnly() {
  memory = null
  try {
    sessionStorage.removeItem(KEY)
  } catch {
    // nothing kept
  }
}

// The address carries the opener's edit mode to a node page in a new tab
// (?edit=1): read once, then taken off the address.
const PARAM = 'edit'

// carryEditing is url with the tab's edit mode, for a page it opens in
// a new tab - a tab opened with noopener gets no copy of sessionStorage.
export function carryEditing(url) {
  const s = current()
  if (!s?.editing) return url
  const u = new URL(url, window.location.href)
  u.searchParams.set(PARAM, '1')
  return u.pathname + u.search + u.hash
}

function takeCarried() {
  try {
    const u = new URL(window.location.href)
    if (u.searchParams.get(PARAM) !== '1') return
    u.searchParams.delete(PARAM)
    window.history.replaceState(window.history.state, '', u.pathname + u.search + u.hash)
    if (!load()) store({ editing: true, at: Date.now() })
  } catch {
    // an address the page can't rewrite: nothing carried
  }
}
takeCarried()

export const EditingContext = createContext({ editing: true, after: 0, setEditing: () => {} })

// useEditing is { editing, after, setEditing }: whether the tab is in
// edit mode, the delay back to read-only (minutes, 0 never), and the
// switch. A page is read-only when its account may change something
// there (else there's nothing to hide) and the tab isn't editing - each
// app says what "may change something" is (node/may.js, me.jsx).
export function useEditing() {
  return useContext(EditingContext)
}
