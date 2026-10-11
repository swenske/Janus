import { FlaskConical } from 'lucide-react'
import { Badge } from './shared/ui.jsx'

// PrereleaseBadge marks a node or Controller running a pre-release: a
// version published to be tested before it becomes a release - never an
// update to badge, but worth knowing about what runs it.
export function PrereleaseBadge() {
  return (
    <span title="A pre-release: published to be tested before it becomes a release, and not supported. The next release is offered as an update.">
      <Badge tone="warn">
        <FlaskConical size={11} /> pre-release
      </Badge>
    </span>
  )
}
