import { Badge } from '../../shared/ui.jsx'
import { SecurityBadge } from '../../SecurityBadge.jsx'
import { SupportBadge } from '../../SupportBadge.jsx'

// UpdateBadge is the update state of a node, from /api/update-check -
// on the overview's Release card and the Update view, in its own file
// so the overview doesn't carry the Update view with it.
export function UpdateBadge({ uc }) {
  if (!uc) return null
  if (uc.state === 'building') return <Badge tone="info">Building</Badge>
  if (uc.state === 'failed') return <Badge tone="danger">Build failed</Badge>
  if (uc.state !== 'ready') return <Badge tone="warn">Unavailable</Badge>
  if (uc.security_update) {
    // Beside the version the node runs, when no update for its image has
    // the fixes: where they are, not "available".
    return (
      <SecurityBadge severity={uc.security_update}>
        {uc.update_available ? 'Security update available' : `Fixed in ${uc.security_release}`}
      </SecurityBadge>
    )
  }
  if (uc.support?.retired && !uc.update_available) return <SupportBadge support={uc.support} />
  return uc.update_available ? <Badge tone="accent">Update available</Badge> : <Badge tone="ok">Up to date</Badge>
}
