// The version menu (SiteTitle.astro) is a <details>: it also closes on a
// click elsewhere and on Escape.
const menus = () => document.querySelectorAll<HTMLDetailsElement>('details.janus-version[open]')
document.addEventListener('click', (e) => {
  for (const d of menus()) if (!d.contains(e.target as Node)) d.open = false
})
document.addEventListener('keydown', (e) => {
  if (e.key !== 'Escape') return
  for (const d of menus()) {
    d.open = false
    d.querySelector('summary')?.focus()
  }
})
