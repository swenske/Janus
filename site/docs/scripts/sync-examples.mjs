// make docs-examples: copies each example back into the code blocks of
// the docs titled with its path (```lang title="examples/...") - the
// check in remark-examples.mjs fails the build until they match.
// Run from site/docs.
import { existsSync, readFileSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { pages, repoRoot } from '../src/lib/structure.mjs'

const block = /^(```+)([^\n]*title="(examples\/[^"]+)"[^\n]*)\n([\s\S]*?)^\1[ \t]*$/gm
for (const page of pages) {
  const file = path.join(repoRoot, page.file)
  const text = readFileSync(file, 'utf8')
  const next = text.replace(block, (all, fence, info, example, body) => {
    const src = path.join(repoRoot, example)
    if (!existsSync(src)) return all
    const content = readFileSync(src, 'utf8').replace(/\n$/, '')
    return `${fence}${info}\n${content}\n${fence}`
  })
  if (next !== text) {
    writeFileSync(file, next)
    console.log(`${page.file}: examples copied in`)
  }
}
