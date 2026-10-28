// Extension requirements: an extension that only works with another
// (prometheus-bird-exporter needs bird) says so in the image factory's
// catalog (`requires`, internal/schematic's CatalogEntry). Every page
// that chooses extensions - the site's builder, the Controller's "create
// a node" and "create a cluster" forms, a node's "Change the image" -
// ticks the requirement with the extension, unticks the extension with
// its requirement and says why, and shows the image factory's own words
// for a list that still misses one. A schematic is still the list as
// chosen: nothing is added behind the user's back.

const entry = (catalog, name, former) => (catalog || []).find((e) => e.name === name || (former && (e.replaces || []).includes(name)))

// The extension a requirement names: by its name, else the one that had
// it before.
const required = (catalog, req) => entry(catalog, req, false) || entry(catalog, req, true)

// requirementError is the image factory's message for the first
// extension of chosen (in name order) missing one it requires - the
// words internal/schematic's RequirementError gives - or ''. release
// names the catalog's release ("v2026.10.29").
export function requirementError(chosen, catalog, release) {
  const names = [...new Set(chosen)].sort()
  for (const name of names) {
    const e = entry(catalog, name, false)
    for (const req of e?.requires || []) {
      const want = required(catalog, req)
      if (!want) return `extension "${name}" requires the "${req}" extension, which ${release ? `Janus ${release}` : 'this build (variants.mk)'} doesn't offer`
      if (!names.some((x) => x === want.name || (want.replaces || []).includes(x))) return `extension "${name}" requires the "${want.name}" extension: choose it too`
    }
  }
  return ''
}

// requirementsOf lists the extensions name requires (by the names the
// catalog offers them under), and theirs.
export function requirementsOf(name, catalog) {
  const out = []
  const walk = (n) => {
    for (const req of entry(catalog, n, false)?.requires || []) {
      const want = required(catalog, req)?.name
      if (want && !out.includes(want) && want !== name) {
        out.push(want)
        walk(want)
      }
    }
  }
  walk(name)
  return out
}

// dependentsOf lists the extensions requiring name, and those requiring
// them.
export function dependentsOf(name, catalog) {
  const out = []
  const walk = (n) => {
    for (const e of catalog || []) {
      if (out.includes(e.name) || e.name === name) continue
      if ((e.requires || []).some((req) => required(catalog, req)?.name === n)) {
        out.push(e.name)
        walk(e.name)
      }
    }
  }
  walk(name)
  return out
}

// withExtension is chosen with name ticked: its requirements come with
// it. added lists those ticked for it.
export function withExtension(chosen, name, catalog) {
  const added = requirementsOf(name, catalog).filter((r) => !chosen.includes(r))
  return { extensions: [...chosen.filter((x) => x !== name), name, ...added], added }
}

// withoutExtension is chosen with name unticked: what requires it goes
// too. dropped lists those unticked with it.
export function withoutExtension(chosen, name, catalog) {
  const dropped = dependentsOf(name, catalog).filter((d) => chosen.includes(d))
  return { extensions: chosen.filter((x) => x !== name && !dropped.includes(x)), dropped }
}

// toggleExtension ticks or unticks name in chosen, and says what came or
// went with it ('' when nothing did).
export function toggleExtension(chosen, name, on, catalog) {
  if (on) {
    const r = withExtension(chosen, name, catalog)
    return { extensions: r.extensions, note: r.added.length ? `${r.added.join(', ')} ticked too: ${name} requires ${plural(r.added)}.` : '' }
  }
  const r = withoutExtension(chosen, name, catalog)
  return { extensions: r.extensions, note: r.dropped.length ? `${r.dropped.join(', ')} unticked too: ${r.dropped.length > 1 ? 'they require' : 'it requires'} ${name}.` : '' }
}

const plural = (names) => (names.length > 1 ? 'them' : 'it')
