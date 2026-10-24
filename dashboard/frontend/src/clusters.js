import { hunks, lineDiff } from './shared/diff.js'

// What the Clusters page (Clusters.jsx) works out by itself - tested in
// clusters.test.js. The clusters' API: docs/guide/clusters.md.

// The domains a cluster may manage on its members, in an apply's order,
// and the file of a version that configures each.
export const DOMAINS = [
  { id: 'haproxy', label: 'HAProxy configuration', file: 'haproxy.cfg' },
  { id: 'files', label: 'HAProxy files', file: null },
  { id: 'firewall', label: 'Firewall', file: 'firewall.nft' },
  { id: 'vrrp', label: 'VRRP', file: 'keepalived.conf' },
  { id: 'bgp', label: 'BGP', file: 'bird.conf' },
]

const LABEL = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/
const CLUSTER = /^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$/
const VAR = /^[A-Za-z_][A-Za-z0-9_]{0,63}$/

// memberDefaults are a node's member name and address as far as they can
// be guessed: its name as a label, and the address the Controller reaches
// it at - unless that's a loopback, which the other members can't use.
export function memberDefaults(node) {
  let name = String(node?.name || '')
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 63)
  if (!LABEL.test(name)) name = ''
  let address = ''
  const m = /^\[?([^\]]+?)\]?:\d+$/.exec(node?.address || '')
  if (m && !/^(127\.|0\.0\.0\.0$|::1?$|localhost$)/.test(m[1])) address = m[1]
  return { name, address }
}

// clusterNameError is why name can't be a cluster's, '' when it can.
export function clusterNameError(name) {
  if (!name) return 'a name'
  return CLUSTER.test(name) ? '' : 'lowercase letters, digits and dashes, 32 at most'
}

// memberNameError is why name can't be a member's, '' when it can.
export function memberNameError(name) {
  if (!name) return 'a name'
  return LABEL.test(name) ? '' : 'lowercase letters, digits and dashes'
}

// health is a cluster's badge.
export function health(c) {
  if (c.busy || c.phase) return { tone: 'info', label: c.phase || 'working', busy: true }
  if (c.error) return { tone: 'danger', label: 'failed' }
  if (c.problems?.length) return { tone: 'warn', label: `${c.problems.length} to look at` }
  return { tone: 'ok', label: 'in sync' }
}

// domainsOf are the domains a version's files configure.
export function domainsOf(paths) {
  const found = new Set()
  for (const p of paths || []) {
    if (p.startsWith('files/')) found.add('files')
    for (const d of DOMAINS) if (d.file === p) found.add(d.id)
  }
  return DOMAINS.map((d) => d.id).filter((d) => found.has(d))
}

// versionChanges is what to's files change from from's, by path:
// [{path, kind: added | removed | changed, diff}] - each diff hunks of
// lines (DiffView).
export function versionChanges(from, to) {
  const out = []
  const paths = [...new Set([...Object.keys(from || {}), ...Object.keys(to || {})])].sort()
  for (const path of paths) {
    const a = from?.[path]
    const b = to?.[path]
    if (a === b) continue
    const kind = a === undefined ? 'added' : b === undefined ? 'removed' : 'changed'
    out.push({ path, kind, diff: hunks(lineDiff(a ?? '', b ?? '')) })
  }
  return out
}

// varsText is a member's variables as "key=value" lines; parseVars reads
// them back, and throws on what isn't one.
export function varsText(vars) {
  return Object.keys(vars || {})
    .sort()
    .map((k) => `${k}=${vars[k]}`)
    .join('\n')
}

export function parseVars(text) {
  const out = {}
  for (const raw of String(text).split('\n')) {
    const line = raw.trim()
    if (!line || line.startsWith('#')) continue
    const i = line.indexOf('=')
    if (i <= 0) throw new Error(`"${line}": one key=value a line`)
    const key = line.slice(0, i).trim()
    if (!VAR.test(key)) throw new Error(`"${key}": letters, digits and _, starting with a letter`)
    out[key] = line.slice(i + 1).trim()
  }
  return out
}

// pathError is why path can't be a version's file, '' when it can.
export function pathError(path) {
  if (DOMAINS.some((d) => d.file === path)) return ''
  if (/^files\/[A-Za-z0-9_][A-Za-z0-9._-]{0,99}(\/[A-Za-z0-9_][A-Za-z0-9._-]{0,99})?$/.test(path)) return ''
  return 'haproxy.cfg, keepalived.conf, bird.conf, firewall.nft, or files/NAME'
}

// when is a time as the reader reads it, "-" for none.
export function when(t) {
  return t && !String(t).startsWith('0001') ? new Date(t).toLocaleString() : '-'
}
