// The Controller's screenshots (docs/assets/screenshots, made by make
// docs-screenshots) come in pairs: <id>-light.webp, which a page shows -
// on GitHub too - and <id>-dark.webp, which the site adds next to it.
// Each is hidden in the other theme (Starlight's light:sl-hidden and
// dark:sl-hidden).
import fs from 'node:fs'
import path from 'node:path'
import { visit } from 'unist-util-visit'
import { report } from './problems.mjs'

const hidden = (node, theme) => {
  node.data = { ...node.data, hProperties: { ...node.data?.hProperties, className: [`${theme}:sl-hidden`] } }
  return node
}

export default function remarkScreenshots() {
  return (tree, file) => {
    visit(tree, 'image', (node, index, parent) => {
      const m = /^(.*assets\/screenshots\/[^/]+)-light\.webp$/.exec(node.url)
      if (!m || !parent) return undefined
      const dark = `${m[1]}-dark.webp`
      if (!fs.existsSync(path.resolve(path.dirname(file.history[0]), dark))) {
        report(file, node, `${dark} doesn't exist: make docs-screenshots`)
        return undefined
      }
      hidden(node, 'dark')
      parent.children.splice(index + 1, 0, hidden({ type: 'image', url: dark, alt: node.alt, title: node.title }, 'light'))
      return index + 2
    })
  }
}
