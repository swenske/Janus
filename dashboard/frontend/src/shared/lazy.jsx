import { lazy } from 'react'

const RELOADED = 'janus-chunk-reloaded'

// lazyView is React.lazy for a page's view: its module comes in its own
// chunk, fetched when the view is first shown - the page opens on what
// it needs first. A chunk that fails to load - the Controller updated
// under an open page: the chunk's hashed name is gone from it - reloads
// the page once, which fetches the new index; a second failure in a row
// (the network, a proxy) is thrown to the nearest error boundary instead
// of a loop.
export function lazyView(load) {
  return lazy(() =>
    load().then(
      (m) => {
        try {
          sessionStorage.removeItem(RELOADED)
        } catch {
          // storage blocked: fine
        }
        return m
      },
      (err) => {
        let again = false
        try {
          again = sessionStorage.getItem(RELOADED) === '1'
          if (!again) sessionStorage.setItem(RELOADED, '1')
        } catch {
          // storage blocked: reload once anyway - the flag can't be kept
        }
        if (again) throw err
        location.reload()
        return new Promise(() => {}) // the page is going away
      },
    ),
  )
}
