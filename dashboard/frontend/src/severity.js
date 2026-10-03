// A node's or the Controller's security update, from the API's
// security_update: the worst vulnerability the releases after its version
// fix - "critical", "high", "medium", "low", or "unknown" (unrated) - read
// from their security.json (docs/upstreams.md).

const RANK = ['unknown', 'low', 'medium', 'high', 'critical']

export function securityTone(severity) {
  return severity === 'critical' || severity === 'high' ? 'danger' : 'warn'
}

// worstSeverity is the most severe of a list ('' when it's empty).
export function worstSeverity(list) {
  return list.filter(Boolean).reduce((w, s) => (RANK.indexOf(s) > RANK.indexOf(w) ? s : w), '')
}

export function securityText(severity) {
  return `Newer releases fix vulnerabilities this version has - the worst ${severity === 'unknown' ? 'unrated' : `rated ${severity}`}.`
}
