// A wide table scrolls sideways (Starlight's styles): it takes keyboard
// focus, so it can be scrolled without a mouse (axe:
// scrollable-region-focusable).
import { visit } from 'unist-util-visit'

export default function rehypeTables() {
  return (tree) => {
    visit(tree, 'element', (node) => {
      if (node.tagName === 'table') node.properties.tabIndex = 0
    })
  }
}
