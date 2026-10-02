// call is the main page's fetch: same origin, the session cookie, JSON
// back. An error carries the Controller's message - the body as is, or
// its "error" field for the endpoints that answer {"error": ...}.
export async function call(path, opts) {
  const resp = await fetch(path, opts)
  const text = await resp.text()
  if (!resp.ok) {
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

export function postJSON(path, body, method = 'POST') {
  return call(path, { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body ?? {}) })
}

export function gib(bytes) {
  return `${(bytes / 2 ** 30).toFixed(bytes >= 100 * 2 ** 30 ? 0 : 1)} GiB`
}
