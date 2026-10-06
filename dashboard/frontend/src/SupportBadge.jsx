import { CalendarClock, CalendarX } from 'lucide-react'
import { Badge } from './shared/ui.jsx'

// SupportBadge says the HAProxy branch a node's schematic pins is about to
// leave the releases' offer (its upstream support ends within six months)
// or left it (no newer update will come); nothing otherwise (support: the
// API's support, nodeproxy.Support).
export function SupportBadge({ support }) {
  if (!support || (!support.retired && !support.soon)) return null
  if (support.retired)
    return (
      <span title={`HAProxy ${support.variant} is no longer offered: ${support.last_release} is the last release with it - no newer update will come. Move to another branch: Update › Change the image.`}>
        <Badge tone="danger">
          <CalendarX size={11} /> HAProxy {support.variant} retired
        </Badge>
      </span>
    )
  return (
    <span title={`HAProxy ${support.variant}'s upstream support ends on ${support.eol}: plan a move to a newer branch (Update › Change the image).`}>
      <Badge tone="warn">
        <CalendarClock size={11} /> HAProxy {support.variant} until {support.eol}
      </Badge>
    </span>
  )
}
