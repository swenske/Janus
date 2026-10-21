// The Controller's screenshots (docs/assets/screenshots, made by make
// docs-screenshots) come in pairs: <id>-light.webp, which a page shows -
// on GitHub too - and <id>-dark.webp, which the site adds next to it.
// Each is hidden in the other theme (Starlight's light:sl-hidden and
// dark:sl-hidden). Each is taken at a device pixel ratio of 2 and has
// a 1x variant next to it, <id>-<theme>-1x.webp, a third of the bytes:
// the page gets all four, and the site's CSS shows the one for the
// theme and the screen's density - a hidden image is lazy and never
// laid out, so it isn't fetched. (A srcset can't do it: Astro rewrites
// an image node's src to its processed file, not a srcset's paths.)
import fs from 'node:fs'
import path from 'node:path'
import { visit } from 'unist-util-visit'
import { report } from './problems.mjs'

// hidden marks an image for one theme and one screen density: Starlight
// hides it in the other theme (light:sl-hidden, dark:sl-hidden), the
// site's CSS on the other density (shot-1x, shot-2x). Hidden images are
// lazy and never laid out, so a browser doesn't fetch them.
const hidden = (node, theme, density) => {
  node.data = { ...node.data, hProperties: { ...node.data?.hProperties, className: [`${theme}:sl-hidden`, `shot-${density}`] } }
  return node
}

export default function remarkScreenshots() {
  return (tree, file) => {
    visit(tree, 'image', (node, index, parent) => {
      const m = /^(.*assets\/screenshots\/[^/]+)-light\.webp$/.exec(node.url)
      if (!m || !parent) return undefined
      const dark = `${m[1]}-dark.webp`
      const light1x = `${m[1]}-light-1x.webp`
      const dark1x = `${m[1]}-dark-1x.webp`
      const dir = path.dirname(file.history[0])
      for (const f of [dark, light1x, dark1x]) {
        if (!fs.existsSync(path.resolve(dir, f))) {
          report(file, node, `${f} doesn't exist: make docs-screenshots`)
          return undefined
        }
      }
      const image = (url, theme, density) => hidden({ type: 'image', url, alt: node.alt, title: node.title }, theme, density)
      hidden(node, 'dark', '2x')
      parent.children.splice(index + 1, 0, image(light1x, 'dark', '1x'), image(dark, 'light', '2x'), image(dark1x, 'light', '1x'))
      return index + 4
    })
  }
}
