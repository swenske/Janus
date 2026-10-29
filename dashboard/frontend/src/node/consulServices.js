// The agent's services on a node's Consul page (views/Consul.jsx): what
// kind each is, what its meta says that matters, its checks' output, and
// what to say when there's nothing to show - npm test holds them
// (consulServices.test.js).

// KINDS are the groups the page offers, in its order.
export const KINDS = [
  ['frontend', 'Frontends'],
  ['url', 'URLs'],
  ['exporter', 'Exporters'],
  ['other', 'Other'],
]

// kindOf is a service's group: haproxy-url's instances, the exporters
// (tag prometheus: janus-exporter, node-exporter, haproxy-exporter), the
// frontends (haproxy-<name>, tag haproxy), anything else - janus, the
// operator's own.
export function kindOf(s) {
  const tags = s.tags || []
  if (s.name === 'haproxy-url') return 'url'
  if (tags.includes('prometheus')) return 'exporter'
  if (tags.includes('haproxy') && (s.name || '').startsWith('haproxy-')) return 'frontend'
  return 'other'
}

// counts is how many services each kind has.
export function counts(services) {
  const out = { all: 0, frontend: 0, url: 0, exporter: 0, other: 0 }
  for (const s of services || []) {
    out.all++
    out[kindOf(s)]++
  }
  return out
}

// matches: s is of kind - all: any.
export function matches(s, kind) {
  return kind === 'all' || kindOf(s) === kind
}

// keyMeta is the meta worth a column: a URL's url, an exporter's
// scheme://…path, a frontend's name.
export function keyMeta(s) {
  const m = s.meta || {}
  if (m.url) return m.url
  if (m.metrics_path) return `${m.scheme || 'http'} ${m.metrics_path}`
  return m.frontend || ''
}

// address is host:port, an IPv6 host in brackets; the agent's own
// address when the service names none.
export function address(s) {
  const host = s.address || ''
  if (!s.port) return host || '-'
  return `${host.includes(':') ? `[${host}]` : host}:${s.port}`
}

// maintenanceReason is why a service is in maintenance: its maintenance
// check's output.
export function maintenanceReason(s) {
  const c = (s.checks || []).find((c) => c.id === `_service_maintenance:${s.id}`)
  return c?.output || ''
}

// checksText is the checks' outputs, a line each - a tooltip.
export function checksText(s) {
  return (s.checks || [])
    .map((c) => `${c.name || c.id}: ${c.status}${c.output ? ` - ${c.output.trim()}` : ''}`)
    .join('\n')
}

// agentHint is what to say instead of the services, or '' when they
// can be shown: the agent stopped, or refusing anonymous reads.
export function agentHint(st) {
  if (!st || st.state === 'not_enabled') return ''
  if (st.service_state && st.service_state !== 'running') {
    return `The agent isn't running (${st.service_state}): its services can't be read.`
  }
  if (st.error && /anonymous|ACL/i.test(st.error) && !st.managed) {
    return 'The agent refuses anonymous reads (ACLs): its services show once the Controller manages this agent (its Consul tab), or once your configuration gives it a default token that may read them.'
  }
  return ''
}
