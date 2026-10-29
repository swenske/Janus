// The Consul page's model (Consul.jsx): the settings as the page edits
// them, what it sends, and how a node's state reads - kept apart so
// npm test holds them (consul.test.js).

// trustMode is how the settings trust the server's certificate.
export function trustMode(tls) {
  if (tls?.ca_pem) return 'ca'
  if (tls?.fingerprint) return 'fingerprint'
  return 'system'
}

// draftOf is the saved settings (GET /api/consul's) as the page edits
// them: the token never comes back - an empty one keeps the saved one.
export function draftOf(s) {
  return {
    enabled: !!s?.enabled,
    address: s?.address || '',
    trust: trustMode(s?.tls),
    ca_pem: s?.tls?.ca_pem || '',
    fingerprint: s?.tls?.fingerprint || '',
    server_name: s?.tls?.server_name || '',
    token: '',
    rpc_ca_pem: s?.rpc_ca_pem || '',
    selector: s?.selector || '',
    services: {
      janus: !!s?.services?.janus,
      exporters: !!s?.services?.exporters,
      frontends: !!s?.services?.frontends,
      urls: !!s?.services?.urls,
    },
    node_meta_from_labels: !!s?.node_meta_from_labels,
  }
}

// settingsOf is a draft as PUT /api/consul and POST /api/consul/test
// take it.
export function settingsOf(d) {
  const tls = { server_name: d.server_name.trim() || undefined }
  if (d.trust === 'ca') tls.ca_pem = d.ca_pem.trim() ? `${d.ca_pem.trim()}\n` : ''
  if (d.trust === 'fingerprint') tls.fingerprint = d.fingerprint.trim()
  return {
    enabled: d.enabled,
    address: d.address.trim().replace(/\/+$/, ''),
    tls,
    token: d.token.trim() || undefined,
    rpc_ca_pem: d.rpc_ca_pem.trim() ? `${d.rpc_ca_pem.trim()}\n` : '',
    selector: d.selector
      .split(',')
      .map((p) => p.trim())
      .filter(Boolean)
      .join(','),
    services: { ...d.services },
    node_meta_from_labels: d.node_meta_from_labels,
  }
}

// dirty: the draft says something else than the saved settings.
export function dirty(draft, saved) {
  return JSON.stringify(settingsOf(draft)) !== JSON.stringify(settingsOf(draftOf(saved)))
}

// A node's state against the Controller's Consul, as a badge's tone.
const STATE_TONES = {
  applied: 'ok',
  removed: 'warn',
  unsupported: 'warn',
  'another controller': 'warn',
  error: 'danger',
}

export function stateTone(state) {
  return STATE_TONES[state] || ''
}

// Its agent in the LAN pool.
const MEMBER_TONES = { alive: 'ok', leaving: 'warn', left: '', failed: 'danger' }

export function memberTone(member) {
  return MEMBER_TONES[member] || ''
}

// counts sums the nodes up: how many have the layer, how many fail.
export function counts(nodes) {
  const out = { applied: 0, error: 0, alive: 0, failed: 0, urls: 0 }
  for (const n of nodes || []) {
    if (n.state === 'applied') out.applied++
    if (n.state === 'error') out.error++
    if (n.member === 'alive') out.alive++
    if (n.member === 'failed') out.failed++
    out.urls += n.urls || 0
  }
  return out
}

// never: the zero time Go sends for "not yet".
export function never(t) {
  return !t || t.startsWith('0001')
}
