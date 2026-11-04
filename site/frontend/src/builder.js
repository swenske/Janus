// What the builder decides without React: which platform a choice may
// keep, and the GitHub issue forms its "Need tester" and "Need something
// else…" links open (pure, node --test).
import { REPO } from './api.js'

// The issue form asking for testers of each image target variants.mk may
// suspend (.github/ISSUE_TEMPLATE - hack/docscheck checks each file
// exists, with its labels). A form sets its own labels: a labels=
// parameter needs the right to label, and GitHub answers 404 without it.
export const TESTER_FORMS = {
  vmware: 'need-tester-vmware.yml',
  baremetal: 'need-tester-baremetal.yml',
  'raspberry-pi': 'need-tester-raspberry-pi.yml',
}

// The form asking for an extension the catalog doesn't have.
export const EXTENSION_FORM = 'extension-request.yml'

const issueForm = (form) => `${REPO}/issues/new?template=${encodeURIComponent(form)}`

// testerIssueURL opens target's "Need tester" form - the form chooser
// for a target without one.
export function testerIssueURL(target) {
  const form = TESTER_FORMS[target]
  return form ? issueForm(form) : `${REPO}/issues/new/choose`
}

export const extensionRequestURL = () => issueForm(EXTENSION_FORM)

// selectablePlatform is the platform ID a choice may keep: a platform the
// site lists and doesn't suspend - a shared link or a bookmark naming a
// suspended (or unknown) one selects nothing.
export function selectablePlatform(platforms, id) {
  const p = (platforms || []).find((x) => x.id === id)
  return p && !p.suspended ? p.id : ''
}

// The slots of a disk installed before 128 MiB ones, which keeps them:
// such a node takes an image that doesn't fit them by being replaced.
export const OLD_SLOT_BYTES = 64 * 1024 * 1024

// imageWeight is about how much of a slot a schematic's root filesystem
// takes, from the release catalog's weights (hack/extpack weigh): the
// base, the HAProxy branch - the default when none is chosen - and the
// extensions. null for a release whose catalog doesn't say.
export function imageWeight(catalog, extensions, haproxy) {
  if (!catalog?.base_bytes || !catalog?.slot_bytes) return null
  const branch = (catalog.haproxy || []).find((v) => (haproxy ? v.name === haproxy : v.default))
  let total = catalog.base_bytes + (branch?.bytes || 0)
  for (const name of extensions) total += (catalog.extensions || []).find((e) => e.name === name)?.bytes || 0
  return { bytes: total, slot: catalog.slot_bytes, fits: total <= catalog.slot_bytes, fitsOld: total <= OLD_SLOT_BYTES }
}
