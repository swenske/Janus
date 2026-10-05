// The site's structure (structure.yaml): which file of the repository is
// which page, and the sidebar built from it. Checked when loaded - a
// listed file that doesn't exist, two pages on one route, or (in a strict
// build, DOCS_STRICT=1) a docs/ page listed nowhere fail the build.
import { existsSync, readdirSync, readFileSync } from 'node:fs'
import path from 'node:path'
import { parse } from 'yaml'

// Paths from where Astro runs - site/docs - not from this module, which
// pages get bundled elsewhere (dist/.prerender/chunks/).
export const repoRoot = path.resolve(process.cwd(), '../..')
const strict = process.env.DOCS_STRICT === '1'

const raw = parse(readFileSync(path.join(repoRoot, 'site/docs/structure.yaml'), 'utf8'))
const problems = []

// slugOf is a page's slug in its section (see structure.yaml).
function slugOf(file, section) {
  const own = `docs/${section}/`
  if (file.startsWith(own)) {
    const rel = file.slice(own.length).replace(/\.md$/, '')
    return rel === 'README' ? '' : rel.replace(/\/README$/, '')
  }
  if (path.posix.basename(file) === 'README.md') return path.posix.basename(path.posix.dirname(file))
  return path.posix.basename(file, '.md')
}

const pageKeys = new Set(['file', 'slug', 'title', 'label', 'description', 'badge'])
export const topics = raw.topics
export const pages = []
for (const topic of topics) {
  for (const section of topic.sections) {
    const add = (entry, group) => {
      const page = typeof entry === 'string' ? { file: entry } : { ...entry }
      for (const key of Object.keys(page)) {
        if (!pageKeys.has(key)) problems.push(`${page.file}: unknown key "${key}" - an unquoted value with a comma?`)
      }
      const slug = (page.slug ?? slugOf(page.file, section.path)).toLowerCase()
      page.id = slug ? `${section.path}/${slug}` : section.path
      Object.assign(page, { topic: topic.id, section: section.path, group })
      pages.push(page)
      return page
    }
    section.indexPage = add({ file: section.index, label: section.indexLabel ?? 'Overview' }, null)
    for (const group of section.groups ?? []) group.pageIds = group.pages.map((p) => add(p, group.label).id)
  }
}

const byFile = new Map()
const byId = new Map()
for (const page of pages) {
  if (!existsSync(path.join(repoRoot, page.file))) problems.push(`${page.file}: listed, but there's no such file`)
  if (byFile.has(page.file)) problems.push(`${page.file}: listed twice`)
  if (byId.has(page.id)) problems.push(`${page.file} and ${byId.get(page.id).file}: both on /${page.id}/`)
  byFile.set(page.file, page)
  byId.set(page.id, page)
}

export const excluded = new Set(raw.exclude ?? [])
for (const file of excluded) {
  if (byFile.has(file)) problems.push(`${file}: both a page and excluded`)
  if (strict && !existsSync(path.join(repoRoot, file))) problems.push(`${file}: excluded, but there's no such file`)
}

// Every Markdown file under docs/ is a page or excluded. Only a strict
// build (the Docker one, from a snapshot of the tracked files) fails on
// it: a working tree may hold untracked notes.
function markdownFiles(dir) {
  const out = []
  for (const e of readdirSync(path.join(repoRoot, dir), { withFileTypes: true })) {
    const rel = `${dir}/${e.name}`
    if (e.isDirectory()) out.push(...markdownFiles(rel))
    else if (e.name.endsWith('.md')) out.push(rel)
  }
  return out
}
for (const file of markdownFiles('docs')) {
  if (byFile.has(file) || excluded.has(file) || file.startsWith('docs/assets/')) continue
  const msg = `${file}: not in site/docs/structure.yaml - list it as a page, or under exclude`
  if (strict) problems.push(msg)
  else console.warn(`[janus-docs] ${msg}`)
}

export const popular = (raw.popular ?? []).map((file) => {
  if (!byFile.has(file)) problems.push(`${file}: in popular, but not a page`)
  return byFile.get(file)
})

if (problems.length) throw new Error(`site/docs/structure.yaml:\n  ${problems.join('\n  ')}`)

export const pageOfFile = (file) => byFile.get(file)
export const pageOfId = (id) => byId.get(id)

// The sidebar of each topic, for starlight-sidebar-topics: a topic with
// one section lists its groups; with several, one group per section.
export function sidebarTopics() {
  const sectionItems = (section) => [
    section.indexPage.id,
    ...(section.groups ?? []).map((g) => ({ label: g.label, items: g.pageIds })),
  ]
  return topics.map((topic) => ({
    id: topic.id,
    label: topic.label,
    icon: topic.icon,
    // The plugin adds the site's base itself.
    link: `/${topic.sections[0].path}/`,
    items:
      topic.sections.length === 1
        ? sectionItems(topic.sections[0])
        : topic.sections.map((s) => ({ label: s.label, items: sectionItems(s) })),
  }))
}
