// call is the main page's fetch: same origin, the session cookie, JSON
// back. An error carries the Controller's message - the body as is, or
// its "error" field for the endpoints that answer {"error": ...}.
//
// { background: true } marks a request the page makes by itself (a
// periodic refresh): unless the user touched the page in the last minute,
// it doesn't keep an idle session alive. A 401 outside /api/auth/ means
// the session ended: the page goes back to its sign-in screen.
export async function call(path, opts = {}) {
  const { background, ...rest } = opts
  const resp = await fetch(path, background ? { ...rest, headers: { ...(rest.headers || {}), ...backgroundHeaders() } } : rest)
  const text = await resp.text()
  if (!resp.ok) {
    if (resp.status === 401 && !path.startsWith('/api/auth/')) window.dispatchEvent(new Event(SIGNED_OUT))
    let msg = text.trim()
    try {
      const j = JSON.parse(msg)
      if (j && typeof j.error === 'string') msg = j.error
    } catch {
      // plain text
    }
    const err = new Error(msg || `${resp.status} ${resp.statusText}`)
    err.status = resp.status
    throw err
  }
  return text ? JSON.parse(text) : null
}

export const SIGNED_OUT = 'janus-signed-out'

const ACTIVE_FOR = 60 * 1000
let lastActivity = Date.now()
for (const ev of ['pointerdown', 'pointermove', 'keydown', 'wheel', 'touchstart']) {
  window.addEventListener(ev, () => (lastActivity = Date.now()), { passive: true, capture: true })
}

// backgroundHeaders are the headers of a request the page makes by itself.
export function backgroundHeaders() {
  return Date.now() - lastActivity > ACTIVE_FOR ? { 'X-Janus-Background': '1' } : {}
}

export function postJSON(path, body, method = 'POST') {
  return call(path, { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body ?? {}) })
}

export function gib(bytes) {
  return `${(bytes / 2 ** 30).toFixed(bytes >= 100 * 2 ** 30 ? 0 : 1)} GiB`
}
