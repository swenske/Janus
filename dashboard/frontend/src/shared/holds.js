import { useEffect } from 'react'

// Actions under way, for the Read-only switch (shared/readOnly.jsx): an
// open confirmation, a request running, a trial waiting for its Confirm,
// an upgrade's stream - a page in edit mode never goes back to read-only
// in the middle of one; the delay counts from its end.

let holds = 0
let releasedAt = 0

// hold marks an action under way; call what it returns once it ends.
export function hold() {
  holds++
  let done = false
  return () => {
    if (done) return
    done = true
    holds--
    releasedAt = Date.now()
  }
}

// useHold holds while active is true.
export function useHold(active) {
  useEffect(() => (active ? hold() : undefined), [active])
}

// held reports whether an action is under way.
export function held() {
  return holds > 0
}

// lastReleaseAt is when the last action ended (0: none yet).
export function lastReleaseAt() {
  return releasedAt
}
