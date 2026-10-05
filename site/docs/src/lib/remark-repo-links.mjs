// Relative links are written for GitHub (`../docs/vrrp.md#applying`,
// `../versions.mk`) and rewritten here: to another page of the site, its
// route; to any other file or folder of the repository, GitHub at the
// build's ref (the release tag, or main). A relative link to nothing is
// a problem (problems.mjs).
import { existsSync, statSync } from 'node:fs'
import path from 'node:path'
import { visit } from 'unist-util-visit'
import { report } from './problems.mjs'
import { pageOfFile, repoRoot } from './structure.mjs'

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
  }
}
