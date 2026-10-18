// One animation loop for every chart on the page: each subscriber is
// called at most fps times a second with the frame's time and the time
// since its own last call. The loop stops when nothing listens or the tab
// is hidden (the browser stops frames anyway), and picks up again.
const subscribers = new Set()
let frame = 0

function loop(now) {
  frame = 0
  for (const s of subscribers) {
    if (s.last == null) s.last = now
    if (now - s.last >= s.every - 1) {
      s.fn(now, now - s.last)
      s.last = now
    }
  }
  start()
}

function start() {
  if (!frame && subscribers.size && !document.hidden) frame = requestAnimationFrame(loop)
}

if (typeof document !== 'undefined') {
  document.addEventListener('visibilitychange', () => {
    for (const s of subscribers) s.last = null
    start()
  })
}

export function subscribe(fn, fps = 30) {
  const s = { fn, every: 1000 / fps, last: null }
  subscribers.add(s)
  start()
  return () => {
    subscribers.delete(s)
    if (!subscribers.size && frame) {
      cancelAnimationFrame(frame)
      frame = 0
    }
  }
}
