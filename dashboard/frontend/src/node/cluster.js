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
