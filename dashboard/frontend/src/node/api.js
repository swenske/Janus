import { backgroundHeaders, SIGNED_OUT } from '../shared/activity.js'
import { apiURL } from '../shared/base.js'

// Everything here is this node's API on the Controller (/nodes/<id>/api/,
// same origin, the session cookie): the Controller relays each call to
// the node for the signed-in account - see dashboard/backend/internal/
// nodeproxy. A read is a background request unless the user just touched
// the page (the views refresh themselves); a 401 means the session ended.

export class ApiError extends Error {
  constructor(message, status) {
    super(message)
    this.status = status
  }
}

async function request(path, opts = {}) {
  const read = !opts.method || opts.method === 'GET'
  let resp
  try {
    resp = await fetch(apiURL(path), read ? { ...opts, headers: { ...(opts.headers || {}), ...backgroundHeaders() } } : opts)
  } catch (err) {
    throw new ApiError(`Controller unreachable: ${err.message}`, 0)
  }
  if (resp.status === 401) window.dispatchEvent(new Event(SIGNED_OUT))
  if (!resp.ok) {
    const text = (await resp.text()).trim()
    throw new ApiError(text || `${resp.status} ${resp.statusText}`, resp.status)
  }
  return resp
}

export async function getJSON(path) {
  const resp = await request(path)
  const text = await resp.text()
  return text ? JSON.parse(text) : null
}

export async function postJSON(path, body) {
  const resp = await request(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body ?? {}),
  })
  const text = await resp.text()
  return text ? JSON.parse(text) : null
}

export async function del(path) {
  await request(path, { method: 'DELETE' })
}

export async function getText(path) {
  const resp = await request(path)
  return resp.text()
}

// download fetches path (GET, or POST with a JSON body) and saves the
// response as a file, named by the server's Content-Disposition.
export async function download(path, fallbackName, body) {
  const opts = body === undefined ? {} : { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }
  const resp = await request(path, opts)
  const blob = await resp.blob()
  const disposition = resp.headers.get('Content-Disposition') || ''
  const name = (disposition.match(/filename="([^"]+)"/) || [])[1] || fallbackName
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = name
  a.click()
  setTimeout(() => URL.revokeObjectURL(url), 60000)
  return { name, size: blob.size }
}
