// variantNote says what an image component is (VersionResponse's
// haproxy and kernel): its branch or version, and whether the image's
// schematic pins it or follows the release's default.
export function variantNote(c, what) {
  if (!c) return ''
  const detail = what === 'version' ? c.version : c.variant
  if (!detail) return ''
  const how = c.pinned ? 'pinned' : c.release_default ? 'the default' : ''
  return `${what} ${detail}${how ? `, ${how}` : ''}`
}
