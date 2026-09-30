// The site's API (site/backend): same origin.

export class ApiError extends Error {}

async function request(path, opts = {}) {
  let resp
  try {
    resp = await fetch(path, opts)
  } catch (err) {
    throw new ApiError(`The site is unreachable: ${err.message}`)
  }
  const text = await resp.text()
  const body = text ? JSON.parse(text) : null
  if (!resp.ok) throw new ApiError(body?.error || `${resp.status} ${resp.statusText}`)
  return body
}

export const getJSON = (path) => request(path)
export const postJSON = (path, body) =>
  request(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) })

export const REPO = 'https://github.com/swenske/Janus'
export const DOCS = `${REPO}/blob/main/docs`

export function bytes(n) {
  if (!n) return ''
  const units = ['B', 'KiB', 'MiB', 'GiB']
  let i = 0
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024
    i++
  }
  return `${n.toFixed(i ? 1 : 0)} ${units[i]}`
}
