// Drops a page's H1: Starlight shows the title (the same H1, see
// meta.mjs) itself.
export default function remarkStripTitle() {
  return (tree) => {
    const i = tree.children.findIndex((n) => n.type === 'heading' && n.depth === 1)
    if (i >= 0) tree.children.splice(i, 1)
  }
}
