// The docs' Mermaid diagrams (pre.mermaid, from remark-diagrams.mjs),
// drawn in the browser - Mermaid's renderer loaded only once a diagram
// comes near the screen: 250 KB a reader who never scrolls that far
// doesn't download, and nothing heavy before the page shows. Drawn again
// in the other theme when the reader switches.
const diagrams = [...document.querySelectorAll('pre.mermaid')]

if (diagrams.length) {
  let mermaid
  let queue = Promise.resolve()
  const theme = () => (document.documentElement.dataset.theme === 'dark' ? 'dark' : 'default')

  // One at a time: Mermaid draws in a shared scratch element.
  const draw = (pre) =>
    (queue = queue.then(async () => {
      mermaid ??= (await import('mermaid')).default
      mermaid.initialize({ startOnLoad: false, theme: theme() })
      pre.dataset.source ??= pre.textContent
      try {
        const { svg } = await mermaid.render(`diagram-${diagrams.indexOf(pre)}`, pre.dataset.source)
        pre.innerHTML = svg
      } catch (err) {
        pre.textContent = `Syntax error in this diagram: ${err.message}`
      }
      pre.dataset.drawn = theme()
    }))

  const near = new IntersectionObserver(
    (entries) => {
      for (const e of entries) {
        if (!e.isIntersecting) continue
        near.unobserve(e.target)
        draw(e.target)
      }
    },
    { rootMargin: '50% 0px' },
  )
  for (const pre of diagrams) near.observe(pre)

  new MutationObserver(() => {
    for (const pre of diagrams) if (pre.dataset.drawn && pre.dataset.drawn !== theme()) draw(pre)
  }).observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] })
}
