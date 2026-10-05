// Every Mermaid diagram carries an accessible title and description
// (accTitle/accDescr lines, which GitHub's renderer reads too): screen
// readers and search engines get no text out of the drawing otherwise.
import { visit } from 'unist-util-visit'
import { report } from './problems.mjs'

export default function remarkDiagrams() {
  return (tree, file) => {
    visit(tree, 'code', (node) => {
      if (node.lang !== 'mermaid') return
      for (const key of ['accTitle', 'accDescr']) {
        if (!new RegExp(`^\\s*${key}\\s*[:{]`, 'm').test(node.value)) report(file, node, `a Mermaid diagram without ${key}`)
      }
    })
  }
}
