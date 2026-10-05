// What this site's own Markdown plugins find wrong in a page - a relative
// link to nothing, a diagram without its accessible title. Astro only
// logs a page that fails to render, and publishes it empty: the plugins
// report here instead (a warning in `astro dev`), and the build fails at
// its end with the whole list.
const found = []
const atEnd = []

// problem notes a problem found outside a page.
export function problem(message) {
  found.push(message)
}

// atBuildEnd runs fn once every page is built, before the list is read.
export function atBuildEnd(fn) {
  atEnd.push(fn)
}

export function report(file, node, message) {
  const where = node?.position ? `:${node.position.start.line}` : ''
  const src = (file.history[0] ?? '?').replace(/^.*?\/(docs|dashboard|image|local-dev)\//, '$1/')
  found.push(`${src}${where}: ${message}`)
  file.message(message, node)
}

export default function janusProblems() {
  return {
    name: 'janus-docs-problems',
    hooks: {
      'astro:build:done': () => {
        for (const fn of atEnd) fn()
        if (found.length) throw new Error(`the docs have ${found.length} problem(s):\n  ${found.join('\n  ')}`)
      },
    },
  }
}
