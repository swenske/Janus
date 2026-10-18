import { useEffect, useState } from 'react'

// The node page's sidebar, wide enough for a screen and a mouse: a rail
// of icons that opens over the page while the pointer or the keyboard is
// in it, or kept open ("pinned") - remembered per browser, followed by
// this Controller's other node pages.
const KEY = 'janus-sidebar'

function read() {
  try {
    return localStorage.getItem(KEY) === 'pinned'
  } catch {
    return false
  }
}

export function usePinned() {
  const [pinned, setPinned] = useState(read)
  useEffect(() => {
    const on = (e) => {
      if (e.key === KEY || e.key === null) setPinned(read())
    }
    window.addEventListener('storage', on)
    return () => window.removeEventListener('storage', on)
  }, [])
  const toggle = () => {
    const next = !pinned
    setPinned(next)
    try {
      localStorage.setItem(KEY, next ? 'pinned' : 'rail')
    } catch {
      // a private window: for this page only
    }
  }
  return [pinned, toggle]
}
