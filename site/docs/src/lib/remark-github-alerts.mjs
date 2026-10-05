// GitHub's alerts (a quote opening with [!NOTE], [!TIP], [!IMPORTANT],
// [!WARNING] or [!CAUTION]) become Starlight's asides: a page reads the
// same on GitHub and on the site.
const variants = {
  NOTE: ['note', 'Note'],
  TIP: ['tip', 'Tip'],
  IMPORTANT: ['note', 'Important'],
  WARNING: ['caution', 'Warning'],
  CAUTION: ['danger', 'Caution'],
}

export default function remarkGitHubAlerts() {
  const walk = (node) => {
    if (!node.children) return
    node.children = node.children.map((child) => {
      walk(child)
      if (child.type !== 'blockquote') return child
      const first = child.children[0]
      const text = first?.type === 'paragraph' && first.children[0]?.type === 'text' ? first.children[0] : null
      const m = text && /^\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\][ \t]*\n?/.exec(text.value)
      if (!m) return child
      const [name, label] = variants[m[1]]
      text.value = text.value.slice(m[0].length)
      if (!text.value) first.children.shift()
      const body = first.children.length ? child.children : child.children.slice(1)
      return {
        type: 'containerDirective',
        name,
        attributes: {},
        children: [{ type: 'paragraph', data: { directiveLabel: true }, children: [{ type: 'text', value: label }] }, ...body],
      }
    })
  }
  return (tree) => walk(tree)
}
