// The letsencrypt extension's configuration as the pages show it
// (ACMEConfig.jsx): pure.

// canon puts a configuration in the node's own form - its fields' order,
// empty ones left out - so a change shows as just that.
const ORDER = [
  'account',
  'certificates',
  'dns_providers',
  'directory',
  'directory_ca',
  'email',
  'accept_terms',
  'eab_key_id',
  'eab_hmac_key',
  'name',
  'domains',
  'type',
  'key_type',
  'challenge',
  'dns_provider',
  'profile',
  'settings',
  'resolvers',
  'propagation_wait_seconds',
  'path',
  'mode',
  'proxy_url',
  'proxy_username',
  'proxy_password',
  'proxy_ca',
  'account_thumbprint',
]
const rank = (k) => (ORDER.includes(k) ? ORDER.indexOf(k) : ORDER.length)
function canon(v) {
  if (Array.isArray(v)) return v.map(canon)
  if (v && typeof v === 'object') {
    const out = {}
    const keys = Object.keys(v).sort((a, b) => rank(a) - rank(b) || a.localeCompare(b))
    for (const k of keys) {
      const c = canon(v[k])
      const empty =
        c === '' ||
        c === false ||
        c === 0 ||
        c == null ||
        (Array.isArray(c) && !c.length) ||
        (typeof c === 'object' && !Array.isArray(c) && !Object.keys(c).length)
      // A secret setting sent empty is kept: it still counts.
      if (!empty || k === k.toUpperCase()) out[k] = c
    }
    return out
  }
  return v
}
export const pretty = (cfg) => JSON.stringify(canon(cfg ?? {}), null, 2)
export const lines = (text) =>
  text
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean)

// NODE_WORDS and CLUSTER_WORDS: whose way to the CA a PathCard shows.
export const NODE_WORDS = { title: "How this node reaches Let's Encrypt", certs: 'this node’s certificates' }
export const CLUSTER_WORDS = { title: "How the members reach Let's Encrypt", certs: 'the cluster’s certificates, on every member' }
