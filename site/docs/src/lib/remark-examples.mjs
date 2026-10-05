// The examples a page shows are files of examples/, which tests run or
// validate: a code block titled with one's path
//   ```hcl title="examples/terraform/libvirt/main.tf"
// must be that file, byte for byte (make docs-examples copies them in
// again), and every example must be shown somewhere.
import { existsSync, readdirSync, readFileSync } from 'node:fs'
import path from 'node:path'
import { visit } from 'unist-util-visit'
import { atBuildEnd, problem, report } from './problems.mjs'
import { repoRoot } from './structure.mjs'

const shown = new Set()
const titled = /title="(examples\/[^"]+)"/

function examples(dir = 'examples') {
  if (!existsSync(path.join(repoRoot, dir))) return []
  return readdirSync(path.join(repoRoot, dir), { withFileTypes: true }).flatMap((e) =>
    e.isDirectory() ? examples(`${dir}/${e.name}`) : e.name === 'README.md' ? [] : [`${dir}/${e.name}`],
  )
}

atBuildEnd(() => {
  for (const f of examples()) if (!shown.has(f)) problem(`${f}: an example no page shows`)
})

export default function remarkExamples() {
  return (tree, file) => {
    visit(tree, 'code', (node) => {
      const m = titled.exec(node.meta ?? '')
      if (!m) return
      shown.add(m[1])
      const src = path.join(repoRoot, m[1])
      if (!existsSync(src)) return report(file, node, `${m[1]}: no such example`)
      if (node.value !== readFileSync(src, 'utf8').replace(/\n$/, '')) {
        report(file, node, `the block titled ${m[1]} isn't that file any more: make docs-examples`)
      }
    })
  }
}
