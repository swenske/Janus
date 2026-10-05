// docs/README.md - the docs' index for reading them on GitHub, which
// shows it under the folder's files - generated from structure.yaml:
//   node scripts/github-index.mjs --write   (make docs-index)
//   node scripts/github-index.mjs --check   (the Docker build: fails when
//                                            docs/README.md is stale)
// Run from site/docs.
import { readFileSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { pageMeta } from '../src/lib/meta.mjs'
import { repoRoot, topics } from '../src/lib/structure.mjs'

const link = (page) => {
  const name = page.label && page.label !== 'Overview' ? page.label : (page.title ?? pageMeta(page.file).title)
  return `[${name}](${path.posix.relative('docs', page.file)})`
}

const out = [
  '<!-- Generated from site/docs/structure.yaml: make docs-index -->',
  '',
  '# Janus documentation',
  '',
  'These pages are also a website - searchable, in light and dark, the',
  "newest release's and main's: [janus.sw-servers.net/docs](https://janus.sw-servers.net/docs/).",
  'This index is for reading them here, on GitHub.',
]
for (const topic of topics) {
  out.push('', `## ${topic.label}`, '', `${topic.description}`)
  for (const section of topic.sections) {
    const level = topic.sections.length > 1 ? '###' : ''
    if (level) out.push('', `${level} ${section.label}`)
    out.push('', `- ${link({ ...section.indexPage, label: section.indexPage.title ?? pageMeta(section.indexPage.file).title })}`)
    for (const group of section.groups ?? []) {
      out.push('', `${level ? '####' : '###'} ${group.label}`, '')
      for (const page of group.pages) {
        const p = typeof page === 'string' ? { file: page } : page
        out.push(`- ${link(p)}`)
      }
    }
  }
}
const text = out.join('\n') + '\n'
const file = path.join(repoRoot, 'docs/README.md')

if (process.argv.includes('--write')) {
  writeFileSync(file, text)
} else {
  let have = ''
  try {
    have = readFileSync(file, 'utf8')
  } catch {}
  if (have !== text) {
    console.error('docs/README.md is not what site/docs/structure.yaml says: make docs-index')
    process.exit(1)
  }
  console.log('docs/README.md: up to date')
}
