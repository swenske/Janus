// Audits the built site (node scripts/check-dist.mjs dist) - what a
// reader would hit, not what the sources promise:
// - every link inside the site leads to a page that exists, and its
//   #fragment to an element of that page;
// - no relative or .md link survived the rewriting (remark-repo-links);
// - each page has one H1, a title, a description, content, and is in
//   its sidebar; the latest channel's pages a canonical link;
// - the sitemap only lists pages that exist, and the search index isn't
//   empty.
// Exits 1 with the list of problems.
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs'
import path from 'node:path'
import { fromHtml } from 'hast-util-from-html'
import { visit } from 'unist-util-visit'

const dist = path.resolve(process.argv[2] ?? 'dist')
const channel = process.env.DOCS_CHANNEL === 'next' ? 'next' : 'latest'
const base = channel === 'next' ? '/docs/next' : '/docs'
// Links out of the docs, to the rest of janus.sw-servers.net.
const siteRoutes = new Set(['/', '/builder/'])
const problems = []

function htmlFiles(dir) {
  const out = []
  for (const e of readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, e.name)
    if (e.isDirectory()) out.push(...htmlFiles(p))
    else if (e.name.endsWith('.html')) out.push(p)
  }
  return out
}

const text = (node) => (node.type === 'text' ? node.value : (node.children ?? []).map(text).join(''))
const hasClass = (node, c) => [].concat(node.properties?.className ?? []).includes(c)

const pages = new Map()
for (const file of htmlFiles(dist)) {
  const rel = path.relative(dist, file).split(path.sep).join('/')
  const tree = fromHtml(readFileSync(file, 'utf8'))
  const page = { rel, ids: new Set(['_top']), links: [], h1: 0, title: '', description: '', canonical: false, content: null, current: false }
  visit(tree, 'element', (node) => {
    const p = node.properties ?? {}
    if (p.id) page.ids.add(String(p.id))
    if (node.tagName === 'h1') page.h1++
    if (node.tagName === 'title') page.title = text(node).trim()
    if (node.tagName === 'meta' && p.name === 'description') page.description = String(p.content ?? '').trim()
    if (node.tagName === 'link' && [].concat(p.rel ?? []).includes('canonical')) page.canonical = true
    if (node.tagName === 'a' && typeof p.href === 'string') {
      page.links.push(p.href)
      if (p.ariaCurrent === 'page') page.current = true
    }
    if (node.tagName === 'div' && hasClass(node, 'sl-markdown-content')) page.content = text(node).trim()
  })
  pages.set(rel, page)
}

// The page a site path names, if it was built.
function target(pathname) {
  if (!pathname.startsWith(`${base}/`)) return null
  let rel = decodeURIComponent(pathname.slice(base.length + 1))
  if (rel === '' || rel.endsWith('/')) rel += 'index.html'
  return pages.has(rel) ? rel : null
}

for (const page of pages.values()) {
  const where = page.rel
  const special = where === '404.html'
  if (page.h1 !== 1) problems.push(`${where}: ${page.h1} <h1> elements`)
  if (!page.title) problems.push(`${where}: no <title>`)
  if (!page.description) problems.push(`${where}: no meta description`)
  if (channel === 'latest' && !special && !page.canonical) problems.push(`${where}: no canonical link`)
  if (channel === 'next' && page.canonical) problems.push(`${where}: a canonical link on the next channel`)
  if (where !== 'index.html' && !special) {
    if (!page.content) problems.push(`${where}: empty - did the page fail to render? (look for [ERROR] in the build's output)`)
    if (!page.current) problems.push(`${where}: not in its sidebar`)
  }
  for (const href of page.links) {
    if (/^(https?:|mailto:)/.test(href)) continue
    const at = href.indexOf('#')
    const pathname = at < 0 ? href : href.slice(0, at)
    const frag = at < 0 ? null : decodeURIComponent(href.slice(at + 1))
    if (pathname === '') {
      if (frag && !page.ids.has(frag)) problems.push(`${where}: a link to #${frag}, not on the page`)
      continue
    }
    if (!pathname.startsWith('/')) {
      problems.push(`${where}: a relative link (${href}) - the site's links are absolute`)
      continue
    }
    if (/\.md$/i.test(pathname)) {
      problems.push(`${where}: a link to ${href} - a Markdown file, not a page`)
      continue
    }
    if (!pathname.startsWith(`${base}/`)) {
      if (!siteRoutes.has(pathname)) problems.push(`${where}: a link to ${href}, outside the docs and the site`)
      continue
    }
    const rel = target(pathname)
    if (!rel) {
      const file = path.join(dist, decodeURIComponent(pathname.slice(base.length + 1)))
      if (!(existsSync(file) && statSync(file).isFile())) problems.push(`${where}: a link to ${href}, no such page`)
      continue
    }
    if (frag && !pages.get(rel).ids.has(frag)) problems.push(`${where}: a link to ${href}, no #${frag} on that page`)
  }
}

if (channel === 'latest') {
  const index = path.join(dist, 'sitemap-index.xml')
  if (!existsSync(index)) problems.push('no sitemap-index.xml')
  else {
    for (const [, loc] of readFileSync(index, 'utf8').matchAll(/<loc>([^<]+)<\/loc>/g)) {
      const sitemap = path.join(dist, new URL(loc).pathname.slice(base.length + 1))
      if (!existsSync(sitemap)) {
        problems.push(`sitemap-index.xml lists ${loc}, not built`)
        continue
      }
      for (const [, url] of readFileSync(sitemap, 'utf8').matchAll(/<loc>([^<]+)<\/loc>/g)) {
        if (!target(new URL(url).pathname)) problems.push(`${path.basename(sitemap)} lists ${url}, no such page`)
      }
    }
  }
}

const entry = path.join(dist, 'pagefind', 'pagefind-entry.json')
const indexed = existsSync(entry) ? Object.values(JSON.parse(readFileSync(entry, 'utf8')).languages ?? {}).reduce((n, l) => n + (l.page_count ?? 0), 0) : 0
if (indexed < pages.size - 1) problems.push(`the search index has ${indexed} pages, the site ${pages.size - 1} (and a 404)`)

if (problems.length) {
  console.error(`check-dist: ${problems.length} problem(s) in ${dist}:\n  ${problems.join('\n  ')}`)
  process.exit(1)
}
console.log(`check-dist: ${pages.size} pages, every link and fragment resolves (${channel})`)
