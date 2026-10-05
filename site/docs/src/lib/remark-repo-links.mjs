// Relative links are written for GitHub (`../docs/vrrp.md#applying`,
// `../versions.mk`) and rewritten here: to another page of the site, its
// route; to any other file or folder of the repository, GitHub at the
// build's ref (the release tag, or main). A relative link to nothing is
// a problem (problems.mjs).
//
// Two GitHub-isms read better on the site: a link whose text is the file
// it points at ([hypervisors.md](hypervisors.md)) shows the page's name
// instead, and a page's path in code (`docs/vrrp.md`) becomes a link to
// it, code kept.
import { existsSync, statSync } from 'node:fs'
import path from 'node:path'
import { SKIP, visit } from 'unist-util-visit'
import { pageMeta } from './meta.mjs'
import { report } from './problems.mjs'
import { pageOfFile, repoRoot } from './structure.mjs'

// A page's name in running text: its sidebar label, but for a section's
// index ("Overview") its title.
const nameOf = (page) => (page.label && page.label !== 'Overview' ? page.label : (page.title ?? pageMeta(page.file).title))

export default function remarkRepoLinks({ base, ref, repo }) {
  return (tree, file) => {
    const src = file.history[0]
    if (!src || !src.startsWith(repoRoot + path.sep)) return
    const dir = path.posix.dirname(path.relative(repoRoot, src).split(path.sep).join('/'))
    visit(tree, ['link', 'definition'], (node) => {
      const url = node.url
      if (!url || /^[a-z][a-z0-9+.-]*:/i.test(url) || url.startsWith('#') || url.startsWith('/')) return
      const at = url.indexOf('#')
      const target = at < 0 ? url : url.slice(0, at)
      const hash = at < 0 ? '' : url.slice(at)
      const rel = path.posix.normalize(path.posix.join(dir, decodeURI(target)))
      const page = pageOfFile(rel)
      if (page) {
        node.url = `${base}/${page.id}/${hash}`
        const only = node.children?.length === 1 ? node.children[0] : null
        if (only?.type === 'text' && [target, rel, path.posix.basename(rel)].includes(only.value)) only.value = nameOf(page)
        return
      }
      const abs = path.join(repoRoot, rel)
      if (rel.startsWith('../') || !existsSync(abs)) {
        report(file, node, `a link to ${url}: there's no ${rel} in the repository`)
        return
      }
      const kind = statSync(abs).isDirectory() ? 'tree' : 'blob'
      node.url = `${repo}/${kind}/${ref}/${rel.replace(/\/$/, '')}${hash}`
    })
    // A page's path in code, outside a link or a heading (whose anchor
    // would change): a link to that page.
    visit(tree, 'inlineCode', (node, index, parent) => {
      if (!parent || parent.type === 'link' || parent.type === 'heading') return
      const page = pageOfFile(node.value)
      if (!page) return
      parent.children[index] = { type: 'link', url: `${base}/${page.id}/`, children: [node] }
      return [SKIP, index + 1]
    })
  }
}
