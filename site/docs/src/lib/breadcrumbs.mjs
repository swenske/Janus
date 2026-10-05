// A page's breadcrumbs - the docs' home, its audience (the sidebar
// topic), then the sidebar groups that hold it - from the route data
// Starlight and starlight-sidebar-topics give each page. The page itself
// isn't one: its title follows.
export function breadcrumbs(locals, base) {
  const topic = locals.starlightSidebarTopics?.topics.find((t) => t.isCurrent)
  if (!topic) return []
  const crumbs = [
    { label: 'Docs', href: `${base}/` },
    { label: topic.label, href: topic.link },
  ]
  const path = []
  const find = (entries) => {
    for (const e of entries) {
      if (e.type === 'link' && e.isCurrent) return true
      if (e.type === 'group') {
        path.push(e.label)
        if (find(e.entries)) return true
        path.pop()
      }
    }
    return false
  }
  if (!find(locals.starlightRoute.sidebar)) return []
  // The topic's index page sits outside any group: the topic is enough.
  if (locals.starlightRoute.entry.id === topic.link.replace(`${base}/`, '').replace(/\/$/, '')) return crumbs.slice(0, 1)
  return [...crumbs, ...path.map((label) => ({ label }))]
}
