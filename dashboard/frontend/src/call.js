import { backgroundHeaders, SIGNED_OUT } from './shared/activity.js'

// call is the main page's fetch: same origin, the session cookie, JSON
// back. An error carries the Controller's message - the body as is, or
// its "error" field for the endpoints that answer {"error": ...} (the
// whole answer in its data: a single sign-on's code and reference).
//
// { background: true } marks a request the page makes by itself (a
// periodic refresh): unless the user touched the page in the last minute,
// it doesn't keep an idle session alive. A 401 outside /api/auth/ means
// the session ended: the page goes back to its sign-in screen.
//
// { timeout } (ms) bounds a request that may never answer (a connection
// that died while the computer slept): a poll waiting for it would stop.
//
// { withHeaders: true } answers { body, headers } - an answer that says
// more in its headers (the audit's total and next page).
export async function call(path, opts = {}) {
  const { background, timeout, withHeaders, ...rest } = opts
  if (timeout) rest.signal = AbortSignal.timeout(timeout)
  const resp = await fetch(path, background ? { ...rest, headers: { ...(rest.headers || {}), ...backgroundHeaders() } } : rest)
  const text = await resp.text()
  if (!resp.ok) {
    if (resp.status === 401 && !path.startsWith('/api/auth/')) window.dispatchEvent(new Event(SIGNED_OUT))
    let msg = text.trim()
    let data = null
    try {
      const j = JSON.parse(msg)
      if (j && typeof j.error === 'string') msg = j.error
      data = j
    } catch {
      // plain text
    }
    const err = new Error(msg || `${resp.status} ${resp.statusText}`)
    err.status = resp.status
    err.data = data
    throw err
  }
  const body = text ? JSON.parse(text) : null
  return withHeaders ? { body, headers: resp.headers } : body
}

export { backgroundHeaders, SIGNED_OUT }

export function postJSON(path, body, method = 'POST') {
  return call(path, { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body ?? {}) })
}

export function gib(bytes) {
  return `${(bytes / 2 ** 30).toFixed(bytes >= 100 * 2 ** 30 ? 0 : 1)} GiB`
}
