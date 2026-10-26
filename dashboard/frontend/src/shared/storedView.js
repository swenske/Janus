import { useState } from 'react'

// How a page shows its things - cards, or rows of a table -, remembered
// per browser under key (the node list's janus-nodes-view, the
// hypervisors' janus-hypervisors-view and machines' janus-machines-view, a node's
// backends' janus-backends-view).
function stored(key) {
  try {
    return localStorage.getItem(key) === 'list' ? 'list' : 'cards'
  } catch {
    return 'cards'
  }
}

export function useStoredView(key) {
  const [view, setView] = useState(() => stored(key))
  const set = (v) => {
    setView(v)
    try {
      localStorage.setItem(key, v)
    } catch {
      // a private window: for this page only
    }
  }
  return [view, set]
}
