// A node's cluster membership as its page shows it (/api/cluster,
// ClusterGet) - pure, tested by cluster.test.js; the context is in
// clusterContext.js, the components in components/ClusterLock.jsx.

// The writes each domain a cluster manages covers - internal/api
// cluster_lock.go's lockDomains: on a member, the node refuses them unless
// the cluster makes them, so the page doesn't offer them.
export const LOCKED_WRITES = {
  'HAProxyService/ApplyConfig': 'haproxy',
  'HAProxyService/FilePut': 'files',
  'HAProxyService/FileDelete': 'files',
  'HAProxyService/CertificateUpload': 'files',
  'HAProxyService/CertificateDelete': 'files',
  'NetworkService/VRRPApplyConfig': 'vrrp',
  'NetworkService/BGPApplyConfig': 'bgp',
  'NetworkService/FirewallApplyRuleset': 'firewall',
}

// What each domain is, in the node's own words (cluster_lock.go's
// domainWords).
export const DOMAIN_WORDS = {
  haproxy: "HAProxy's configuration",
  files: "HAProxy's files and certificates",
  vrrp: "VRRP's configuration",
  bgp: "BGP's configuration",
  firewall: "the firewall's ruleset",
}

// lockedDomain is the domain of cluster that method writes and the
// cluster manages here, else "".
export function lockedDomain(cluster, method) {
  const domain = LOCKED_WRITES[method]
  return domain && cluster?.member && cluster.managed?.includes(domain) ? domain : ''
}

// clusterLink is the cluster's page on this Controller: a cluster made
// with janusctl (advisory locks) isn't the Controller's.
export function clusterLink(cluster) {
  return cluster?.member && !cluster.advisory && cluster.id ? `/#/clusters/${encodeURIComponent(cluster.id)}` : ''
}

// vrrpRole sums up the node's VRRP instances: MASTER when it holds any
// virtual IP, else the instances' common state, else "".
export function vrrpRole(status) {
  const states = (status?.instances || []).map((i) => (i.state || '').toUpperCase()).filter(Boolean)
  if (!states.length) return ''
  if (states.includes('MASTER')) return 'MASTER'
  if (states.includes('FAULT')) return 'FAULT'
  return states.every((s) => s === states[0]) ? states[0] : states.join('/')
}

// bgpState sums up what the node itself knows of its BGP announcements
// (/api/network/bgp, BGPStatus): "announcing" when an established session
// exports a route, "withdrawn" when the sessions are up but export
// nothing - its haproxy_* routes held down (HAProxy not answering, the
// member drained) -, "down" when no session is established; "" when
// BIRD doesn't run a BGP session here. Whether the router uses the
// routes is the router's to say (routerRoles). partial: a session isn't
// established (one family's, say).
export function bgpState(status) {
  if (!status || status.state !== 'running') return { state: '', sessions: [] }
  const sessions = (status.protocols || [])
    .filter((p) => (p.protocol || '').toUpperCase() === 'BGP')
    .map((p) => ({
      name: p.name,
      established: (p.bgp_state || '').toLowerCase() === 'established',
      exported: (p.channels || []).reduce((n, c) => n + (c.exported || 0), 0),
    }))
  if (!sessions.length) return { state: '', sessions }
  const up = sessions.filter((s) => s.established)
  const partial = up.length > 0 && up.length < sessions.length
  if (!up.length) return { state: 'down', sessions, partial: false }
  const held = (status.protocols || []).some((p) => p.held_down)
  if (up.every((s) => s.exported === 0)) return { state: 'withdrawn', sessions, partial, held }
  return { state: 'announcing', sessions, partial, held }
}

// routerRoles is this member's routes on the router in front, as the
// cluster's read-only RouterOS hook reads them (/api/cluster/router): one
// role per service prefix - "multipath" (active, one of several equal
// routes), "master" (the only active one), "backup" (present, not
// active), "absent" (no route through it). label sums them up: one role,
// or each prefix's when they differ.
export function routerRoles(view) {
  const roles = view?.available ? view.roles || [] : []
  const distinct = [...new Set(roles.map((r) => r.role))]
  return { roles, label: distinct.length === 1 ? distinct[0] : roles.map((r) => `${r.role} (${r.prefix})`).join(', ') }
}

