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
