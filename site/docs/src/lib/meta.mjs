// A page's title and description, read from its Markdown the way a
// reader sees it on GitHub: the title is the H1, the description the
// first paragraph after it (links and code marks removed, cut at about
// 155 characters - what search engines show).
import { readFileSync } from 'node:fs'
import path from 'node:path'
import { repoRoot } from './structure.mjs'

const plain = (s) =>
  s
    .replace(/!?\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/[`*_]/g, '')
    .replace(/\s+/g, ' ')
    .trim()

export function pageMeta(file) {
  const text = readFileSync(path.join(repoRoot, file), 'utf8')
  let title = ''
  let fence = null
  const para = []
  for (const line of text.split('\n')) {
    const f = /^\s*(```+|~~~+)/.exec(line)
    if (f) {
      if (!fence) fence = f[1]
      else if (line.trim().startsWith(fence)) fence = null
      if (title && para.length) break
      continue
    }
    if (fence) continue
    if (!title) {
      const h1 = /^# (.+)$/.exec(line)
      if (h1) title = plain(h1[1])
      continue
    }
    if (!line.trim()) {
      if (para.length) break
      continue
    }
    // Only prose makes a description: not a heading, list, table, quote,
    // image or HTML.
    if (/^(#|\||<|>|\s*[-*+] |\s*\d+\. |!\[)/.test(line)) {
      if (para.length) break
      continue
    }
    para.push(line.trim())
  }
  let description = plain(para.join(' '))
  if (description.length > 158) description = description.slice(0, 155).replace(/\s+\S*$/, '') + '…'
  return { title, description }
}
