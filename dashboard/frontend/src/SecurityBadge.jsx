import { ShieldAlert } from 'lucide-react'
import { Badge } from './shared/ui.jsx'
import { securityText, securityTone } from './severity.js'

// SecurityBadge says a security update is available (severity: the API's
// security_update); nothing without one.
export function SecurityBadge({ severity, children }) {
  if (!severity) return null
  return (
    <span title={securityText(severity)} className="security-badge">
      <Badge tone={securityTone(severity)}>
        <ShieldAlert size={11} /> {children || 'security update'}
      </Badge>
    </span>
  )
}
