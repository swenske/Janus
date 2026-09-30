// Everything here talks to this node's own listener (same origin): the
// Janus Controller relays each call to the node with its own service
// credential - see dashboard/backend/internal/nodeproxy.

export class ApiError extends Error {
  constructor(message, status) {
    super(message)
    this.status = status
  }
}

async function request(path, opts = {}) {
  let resp
  try {
    resp = await fetch(path, opts)
  } catch (err) {
    throw new ApiError(`Controller unreachable: ${err.message}`, 0)
  }
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
