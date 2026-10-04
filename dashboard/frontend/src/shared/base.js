// The node app runs under /nodes/<id>/ on the Controller's origin: its
// API is relative to that. The main page's is at the root.
const m = window.location.pathname.match(/^\/nodes\/[^/]+/)

export const BASE = m ? m[0] : ''

// apiURL is path ("/api/...") under the page's base.
export function apiURL(path) {
  return path.startsWith('/api/') ? BASE + path : path
}
