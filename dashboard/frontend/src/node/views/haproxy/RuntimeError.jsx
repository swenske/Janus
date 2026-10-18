import { ErrorBox } from '../../../shared/ui.jsx'

// RuntimeError is a runtime diagnostic's error - a node whose Janus is
// older than the call (501) gets a plain explanation, not the gRPC text.
export function RuntimeError({ error, what }) {
  if (error?.status === 501) {
    return <div className="notice">This node's Janus is older than {what}: update it (System › Update) to see them here.</div>
  }
  return <ErrorBox error={error} />
}
