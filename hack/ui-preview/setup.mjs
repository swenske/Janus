// hack/ui-preview's Controller, set up through its API: its admin, the
// three nodes added and trusting its fleet, edge-1 and edge-2 running
// node.cfg, edge-3 many.cfg (many backends), each sending its logs - edge-1 to rsyslog through its second interface and
// to Elasticsearch, edge-2 to rsyslog over UDP, edge-3 to Elasticsearch
// and to a server that doesn't answer, with one target disabled; a
// second account, lea's (an operator), whose page of edge-1 load.mjs
// opens now and then. Prints an API token for janusctl (the TUI, the
// janusctl client hack/ui-preview.sh runs). Run by hack/ui-preview.sh in
// the browser image (Node), on the preview's network.
import fs from 'node:fs'

const { CONTROLLER, PASSWORD, VISITOR_PASSWORD, ES_URL, ES_KEY, ES_FP } = process.env
const NODES = Object.fromEntries(process.env.NODES.split(',').map((kv) => kv.split('=')))
const NET = process.env.NET // 10.0.30
const LOGNET = process.env.LOGNET // 10.0.31
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const jar = new Map()
async function call(method, path, body) {
  for (let i = 0; ; i++) {
    let resp
    try {
      resp = await fetch(CONTROLLER + path, {
        method,
        headers: { 'Content-Type': 'application/json', Cookie: [...jar].map(([k, v]) => `${k}=${v}`).join('; ') },
        body: body === undefined ? undefined : JSON.stringify(body),
      })
    } catch (err) {
      if (i > 60) throw err
      await sleep(1000)
      continue
    }
    for (const c of resp.headers.getSetCookie()) {
      const [kv] = c.split(';')
      const at = kv.indexOf('=')
      jar.set(kv.slice(0, at), kv.slice(at + 1))
    }
    const text = await resp.text()
    if (!resp.ok) throw new Error(`${method} ${path}: ${resp.status} ${text}`)
    const json = resp.headers.get('content-type')?.includes('json')
    return text && json ? JSON.parse(text) : text
  }
}
async function until(what, fn, seconds = 120) {
  for (let i = 0; i < seconds; i++) {
    if (await fn()) return
    await sleep(1000)
  }
  throw new Error(`waited ${seconds} s for ${what}`)
}

await call('POST', '/api/auth/setup', { password: PASSWORD, mfa_required: 'nobody' })
const ids = {}
for (const [name, ip] of Object.entries(NODES)) {
  const admin = fs.readFileSync(`/creds/${name}.admin`, 'utf8')
  const node = await call('POST', '/api/nodes', {
    name,
    address: `${ip}:9505`,
    ca_cert_pem: fs.readFileSync(`/creds/${name}.ca`, 'utf8'),
    bootstrap_cert_pem: admin.match(/-----BEGIN CERTIFICATE-----[\s\S]+?-----END CERTIFICATE-----\n/)[0],
    bootstrap_key_pem: admin.match(/-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]+?-----END [A-Z ]*PRIVATE KEY-----\n/)[0],
  })
  ids[name] = node.id
}
const fleet = await call('POST', '/api/fleet/setup')
const kit = await call('GET', '/api/fleet/recovery-kit')
await call('POST', '/api/fleet/confirm', { kit, passphrase: fleet.passphrase })
await until('the nodes to trust the fleet', async () => {
  const f = await call('GET', '/api/fleet')
  return Object.values(ids).every((id) => f.nodes?.[id]?.state === 'trusted')
})

const cfgs = { 'edge-3': fs.readFileSync('/preview/many.cfg', 'utf8') }
const cfg = fs.readFileSync('/preview/node.cfg', 'utf8')
const es = (name, sources) => ({ name, sources, elasticsearch: { url: ES_URL, api_key: ES_KEY, gzip: true }, tls: { fingerprint: ES_FP, server_name: 'localhost' } })
const logs = {
  'edge-1': [
    { name: 'rsyslog', sources: ['janusd', 'haproxy', 'access', 'events'], syslog: { address: `${LOGNET}.20:601`, transport: 'tcp' } },
    es('elastic', ['access', 'events', 'audit']),
  ],
  'edge-2': [{ name: 'rsyslog', sources: ['janusd', 'haproxy', 'access', 'kernel', 'events'], syslog: { address: `${NET}.20:514`, transport: 'udp' } }],
  'edge-3': [
    es('elastic', ['janusd', 'access', 'events']),
    { name: 'siem', syslog: { address: `${NET}.99:601`, transport: 'tcp' } },
    { name: 'archive', disabled: true, syslog: { address: `${NET}.20:514`, transport: 'udp' } },
  ],
}
const tlsPEM = Buffer.from(fs.readFileSync('/creds/tls.pem')).toString('base64')
for (const [name, id] of Object.entries(ids)) {
  // node.cfg's HTTPS certificate first: the configuration reads it.
  if (!cfgs[name]) await call('POST', `/nodes/${id}/api/haproxy/files`, { name: 'preview.pem', content_base64: tlsPEM })
  // Its last stage: one HAProxy refuses isn't accepted, the answer's 200.
  const applied = await call('POST', `/nodes/${id}/api/haproxy/config`, { config: cfgs[name] || cfg })
  if (!applied.accepted) throw new Error(`${name}: the configuration wasn't applied - ${applied.stage}: ${applied.message}`)
  await call('POST', `/nodes/${id}/api/log-forwarding`, { targets: logs[name] })
}
// many.cfg's down on purpose: only those at the application servers'.
await until('the application servers to be up', async () => {
  for (const id of Object.values(ids)) {
    const b = await call('GET', `/nodes/${id}/api/haproxy/backends`)
    const servers = (b.backends || []).flatMap((x) => x.servers || []).filter((s) => /^10\.0\.30\.1[12]:/.test(s.address))
    if (!servers.length || servers.some((s) => s.state.toLowerCase() !== 'up')) return false
  }
  return true
})
const token = await call('POST', '/api/tokens', { name: 'ui-preview-janusctl', expires_in_days: 7, role: 'admin' })

// lea, signed in once to choose her own password (asked at the first
// sign-in).
await call('POST', '/api/users', { name: 'lea', role: 'operator', password: 'lea-first-password-1' })
jar.clear()
await call('POST', '/api/auth/login', { name: 'lea', password: 'lea-first-password-1' })
await call('POST', '/api/auth/password', { current_password: 'lea-first-password-1', new_password: VISITOR_PASSWORD })
console.log(JSON.stringify({ token: token.token, nodes: ids }))
