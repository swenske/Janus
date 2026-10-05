import { createContext, useContext } from 'react'

// Who the page is signed in as, and what that account's role lets it do -
// the Controller checks it on every request anyway: this only hides what
// would be refused.

const RANK = { reader: 1, operator: 2, admin: 3 }

export const ROLES = [
  { id: 'reader', label: 'Reader', about: 'sees everything, changes nothing' },
  { id: 'operator', label: 'Operator', about: 'runs nodes - HAProxy, services, reboots - and powers machines' },
  { id: 'admin', label: 'Admin', about: 'everything: accounts, the fleet, hypervisors, machines, approvals, updates' },
]

// DOMAINS a grant or a token may be narrowed to (internal/rbac, and the
// Controller's machines).
export const DOMAINS = [
  { id: 'haproxy', label: 'HAProxy', about: "configuration, files, maps, certificates, Let's Encrypt, reloads" },
  { id: 'services', label: 'Services', about: 'services, reboots, their logs' },
  { id: 'network', label: 'Network', about: 'network, firewall, VRRP, BGP, Consul' },
  { id: 'system', label: 'System', about: 'updates, access, files, capture, exporters, reset' },
  { id: 'machines', label: 'Machines', about: "a machine's power and console" },
]

export const MeContext = createContext({ name: '', role: 'reader', grants: [], needs: [] })

export function useMe() {
  return useContext(MeContext)
}

export function atLeast(role, need) {
  return (RANK[role] || 0) >= RANK[need]
}

// useCan answers whether the account may do what needs a role over
// everything.
export function useCan() {
  const me = useMe()
  return (need) => atLeast(me.role, need)
}

// labelString is labels as "key=value, ..." in key order.
export function labelString(l) {
  return Object.keys(l || {})
    .sort()
    .map((k) => `${k}=${l[k]}`)
    .join(', ')
}

// parseLabels reads "key=value, ..." - throws on a part without "=".
export function parseLabels(s) {
  const out = {}
  for (const part of s.split(',')) {
    const p = part.trim()
    if (!p) continue
    const i = p.indexOf('=')
    if (i < 1) throw new Error(`"${p}": labels are key=value, comma-separated`)
    out[p.slice(0, i).trim()] = p.slice(i + 1).trim()
  }
  return out
}

export function matches(selector, labels) {
  return Object.entries(selector || {}).every(([k, v]) => (labels || {})[k] === v)
}

// nodeCan answers whether the account may do what needs role on a node
// with labels, in domain (none: whatever the domain) - its role over
// everything, or a grant whose selector the node matches (the
// Controller's access.go; it decides anyway).
export function nodeCan(me, labels, need, domain) {
  if (atLeast(me.role, need)) return true
  return (me.grants || []).some((g) => matches(g.selector, labels) && atLeast(g.role, need) && (!domain || !g.domains?.length || g.domains.includes(domain)))
}
