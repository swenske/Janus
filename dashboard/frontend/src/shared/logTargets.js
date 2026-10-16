// The log targets' model (internal/logship's Target, as the API
// carries it), shared by the Controller's page and the node's.

// The sources a target can take: a node's, and the Controller's own.
export const NODE_SOURCES = [
  { id: 'janusd', label: "janusd's log" },
  { id: 'haproxy', label: "HAProxy's messages" },
  { id: 'access', label: "HAProxy's access logs" },
  { id: 'extensions', label: 'Extensions' },
  { id: 'kernel', label: 'Kernel' },
  { id: 'events', label: 'Event log' },
  { id: 'audit', label: 'Audit' },
]
export const NODE_DEFAULT_SOURCES = ['janusd', 'haproxy', 'extensions', 'kernel', 'events', 'audit']
export const CONTROLLER_SOURCES = [
  { id: 'controller', label: "The Controller's log" },
  { id: 'audit', label: 'Its audit' },
]

// A new target: syslog over TLS, the default sources.
export function newTarget(name, sources) {
  return { name, sources: [...sources], syslog: { address: '', transport: 'tls', format: 'rfc5424' }, tls: {} }
}

// trustMode is how a target trusts its server: system, ca or
// fingerprint.
export function trustMode(tls) {
  if (tls?.ca_pem) return 'ca'
  if (tls?.fingerprint) return 'fingerprint'
  return tls?.mode || 'system'
}

// cleanTarget is a target as the API takes it: the fields of its kind
// only, the trust mode's only, no empty secret (a secret left empty
// keeps the saved one).
export function cleanTarget(t) {
  const out = { name: t.name.trim(), sources: t.sources, disabled: !!t.disabled }
  if (t.elasticsearch) {
    const es = { ...t.elasticsearch }
    for (const k of ['api_key', 'password', 'username', 'namespace']) if (!es[k]) delete es[k]
    out.elasticsearch = es
  } else {
    out.syslog = { ...t.syslog }
    for (const k of ['framing', 'facility', 'format']) if (!out.syslog[k]) delete out.syslog[k]
  }
  const tls = { ...(t.tls || {}) }
  const mode = trustMode(tls)
  delete tls.mode
  if (mode !== 'ca') delete tls.ca_pem
  if (mode !== 'fingerprint') delete tls.fingerprint
  for (const k of Object.keys(tls)) if (!tls[k]) delete tls[k]
  const usesTLS = t.elasticsearch ? (t.elasticsearch.url || '').startsWith('https:') : out.syslog.transport === 'tls'
  if (usesTLS && Object.keys(tls).length) out.tls = tls
  return out
}

// withSources fills in the sources a target takes when it names none.
export function withSources(targets, defaults) {
  return (targets || []).map((t) => ({ ...t, sources: t.sources?.length ? t.sources : [...defaults], tls: t.tls || {} }))
}

// stable is v as JSON with its keys sorted: two targets compared field by
// field, whatever order their fields were set in.
function stable(v) {
  if (Array.isArray(v)) return `[${v.map(stable).join(',')}]`
  if (v && typeof v === 'object') {
    return `{${Object.keys(v)
      .sort()
      .map((k) => `${JSON.stringify(k)}:${stable(v[k])}`)
      .join(',')}}`
  }
  return JSON.stringify(v)
}

// sameTarget says whether two targets send the same - a draft against
// what's saved (secrets aren't read back: one typed in is a change).
export function sameTarget(a, b) {
  return stable(cleanTarget(a)) === stable(cleanTarget(b))
}

// targetWhere is where a target sends, in a few words.
export function targetWhere(t) {
  if (t.elasticsearch) return t.elasticsearch.url || '-'
  const s = t.syslog || {}
  return `${s.transport || 'udp'}://${s.address || '-'}`
}
