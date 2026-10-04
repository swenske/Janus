// Whether the user touched the page lately: a request the page makes by
// itself (a periodic refresh) says so with X-Janus-Background unless they
// did in the last minute, and the Controller doesn't count it as use - an
// idle page lets its session end.

const ACTIVE_FOR = 60 * 1000
let lastActivity = Date.now()
for (const ev of ['pointerdown', 'pointermove', 'keydown', 'wheel', 'touchstart']) {
  window.addEventListener(ev, () => (lastActivity = Date.now()), { passive: true, capture: true })
}

// backgroundHeaders are the headers of a request the page makes by itself.
export function backgroundHeaders() {
  return Date.now() - lastActivity > ACTIVE_FOR ? { 'X-Janus-Background': '1' } : {}
}

// SIGNED_OUT is the event a request that found the session ended sends.
export const SIGNED_OUT = 'janus-signed-out'
