// Mermaid diagrams: every one carries an accessible title and
// description (accTitle/accDescr lines, which GitHub's renderer reads
// too) - screen readers and search engines get no text out of the
// drawing otherwise - and becomes a <pre class="mermaid"> holding its
// source, which diagrams.mjs draws in the browser.
import { visit } from 'unist-util-visit'
import { report } from './problems.mjs'

const escape = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')

export default function remarkDiagrams() {
  return (tree, file) => {
    visit(tree, 'code', (node, index, parent) => {
      if (node.lang !== 'mermaid') return
      for (const key of ['accTitle', 'accDescr']) {
        if (!new RegExp(`^\\s*${key}\\s*[:{]`, 'm').test(node.value)) report(file, node, `a Mermaid diagram without ${key}`)
      }
      if (parent && typeof index === 'number') parent.children[index] = { type: 'html', value: `<pre class="mermaid">${escape(node.value)}</pre>` }
    })
  }
}
