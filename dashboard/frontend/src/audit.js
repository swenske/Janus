// The Audit page's filters and names (Audit.jsx), kept apart to be
// tested (audit.test.js). The filters live in the page's hash -
// #/audit?user=rita&result=refused - so a filtered view can be shared;
// GET /api/audit takes the same names (dashboard/backend/audit_subjects.go).

export const FILTERS = ['user', 'via', 'subject', 'result', 'kind', 'method', 'from', 'to', 'client', 'q']

// readFilters is the filters a route (the hash, without #) carries.
export function readFilters(route) {
  const i = route.indexOf('?')
  const q = new URLSearchParams(i >= 0 ? route.slice(i + 1) : '')
  const f = {}
  for (const k of FILTERS) {
    const v = q.get(k)
    if (v) f[k] = v
  }
  return f
}

// filtersRoute is the route that carries f.
export function filtersRoute(f) {
  const q = filtersQuery(f)
  return q ? `/audit?${q}` : '/audit'
}

// filtersQuery is f as a query string, in FILTERS' order, empty ones left
// out.
export function filtersQuery(f) {
  const q = new URLSearchParams()
  for (const k of FILTERS) if (f[k]) q.set(k, f[k])
  return q.toString()
}

// The kinds of action (audit.Categories).
export const KINDS = [
  ['signin', 'Sign-ins'],
  ['nodes', 'Nodes'],
  ['clusters', 'Clusters'],
  ['fleet', 'Fleet and PKI'],
  ['accounts', 'Accounts and tokens'],
  ['machines', 'Machines and hypervisors'],
  ['settings', 'Settings'],
]

export const RESULTS = [
  ['ok', 'Succeeded'],
  ['refused', 'Refused (4xx)'],
  ['failed', 'Failed (5xx)'],
]

// What an entry may be about (audit.Subject's kinds).
export const SUBJECT_KINDS = {
  node: 'Node',
  pending: 'Pending node',
  machine: 'Machine',
  cluster: 'Cluster',
  hypervisor: 'Hypervisor',
  account: 'Account',
  token: 'API token',
  'enrollment-token': 'Enrollment token',
  'sso-provider': 'Sign-on provider',
}

// subjectLabel names a subject: its name, else the start of its ID.
export function subjectLabel(s) {
  if (s.name) return s.name
  return s.id.length > 12 ? `${s.id.slice(0, 8)}…` : s.id
}

// subjectHref is where a subject that still exists is shown - null for
// one gone or without a page of its own.
export function subjectHref(s) {
  if (s.gone) return null
  switch (s.kind) {
    case 'node':
      return `/nodes/${encodeURIComponent(s.id)}/`
    case 'cluster':
      return `#/clusters/${encodeURIComponent(s.id)}`
    case 'hypervisor':
      return '#/hypervisors'
    case 'account':
      return '#/accounts'
    case 'token':
      return '#/tokens'
    default:
      return null
  }
}

// subjectGroups are the facets' subjects for a picker, by kind in
// SUBJECT_KINDS' order - current, the one a shared link names, kept even
// when the audit no longer has it.
export function subjectGroups(subjects, current) {
  const all = [...(subjects || [])]
  if (current && !all.some((s) => `${s.kind}:${s.id}` === current)) {
    const [kind, ...id] = current.split(':')
    all.push({ kind, id: id.join(':') })
  }
  const groups = []
  for (const kind of Object.keys(SUBJECT_KINDS)) {
    const items = all.filter((s) => s.kind === kind)
    if (items.length) groups.push({ kind, label: SUBJECT_KINDS[kind], items })
  }
  const other = all.filter((s) => !SUBJECT_KINDS[s.kind])
  if (other.length) groups.push({ kind: 'other', label: 'Other', items: other })
  return groups
}

// viaLabel says how a request came: "token 7f3a" -> "API token 7f3a".
export function viaLabel(via) {
  if (via === 'session') return 'Session'
  if (via === 'none') return 'No credential'
  if (via === 'token') return 'Any API token'
  if (via.startsWith('token ')) return `API token ${via.slice(6)}`
  return via
}

// The time filters are RFC 3339 in the hash (shared links mean the same
// moment anywhere), local in the inputs (datetime-local).
export function toLocalInput(iso) {
  if (!iso) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  const p = (n) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`
}

export function fromLocalInput(v) {
  if (!v) return ''
  const d = new Date(v)
  return Number.isNaN(d.getTime()) ? '' : d.toISOString().replace(/\.000Z$/, 'Z')
}
