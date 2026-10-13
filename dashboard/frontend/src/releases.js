// compareRelease places the release a page is about to install against
// the version a node runs - as the Controller's Go does
// (updaterapi.Newer): 1 newer, 0 the same, -1 older; null when either
// can't be compared (not a release, a build that isn't after one).
const RELEASE = /^v(\d{4})\.(\d{2})\.(\d{2})(?:-(\d+))?$/
const DESCRIBE = /^v(\d{4})\.(\d{2})\.(\d{2})(?:-(\d+))?(?:-\d+-g[0-9a-f]+)?(?:-dirty)?$/

function parts(re, v) {
  const m = re.exec(v || '')
  return m ? m.slice(1, 5).map((x) => Number(x || 0)) : null
}

export function compareRelease(release, running) {
  const r = parts(RELEASE, release)
  const c = parts(DESCRIBE, running)
  if (!r || !c) return null
  for (let i = 0; i < 4; i++) if (r[i] !== c[i]) return r[i] > c[i] ? 1 : -1
  return 0
}
