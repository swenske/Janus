import { useState } from 'react'

// How the node list shows its nodes - cards, or rows of a table -,
// remembered per browser.
const KEY = 'janus-nodes-view'

function stored() {
  try {
    return localStorage.getItem(KEY) === 'list' ? 'list' : 'cards'
  } catch {
    return 'cards'
  }
}

export function useNodesView() {
  const [view, setView] = useState(stored)
  const set = (v) => {
    setView(v)
    try {
      localStorage.setItem(KEY, v)
    } catch {
      // a private window: for this page only
    }
  }
  return [view, set]
}
