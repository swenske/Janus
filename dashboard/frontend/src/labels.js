// A node's labels (the Controller's labels.go): its own (`labels`,
// edited on its card), and those it inherits from the hypervisor its
// machine runs on (`inherited_labels`, `inherited_from`), changed on that
// hypervisor. Grants, scoped tokens and log forwarding profiles pick a
// node by both, its own winning on a key.

// The keys suggested for a hypervisor's labels - free-form otherwise.
export const PLACE_KEYS = ['dc', 'room', 'rack']
export const PLACE_EXAMPLE = 'dc=lgs, room=a, rack=12'

// effectiveLabels are what picks the node: the inherited labels, its
// own on top.
export function effectiveLabels(node) {
  return { ...(node?.inherited_labels || {}), ...(node?.labels || {}) }
}

// inheritedRows are the node's inherited labels in key order, each
// saying whether the node's own label overrides it.
export function inheritedRows(node) {
  const own = node?.labels || {}
  const inherited = node?.inherited_labels || {}
  return Object.keys(inherited)
    .sort()
    .map((key) => ({ key, value: inherited[key], overridden: Object.hasOwn(own, key) && own[key] !== inherited[key] }))
}
