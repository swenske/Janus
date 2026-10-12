// The splash: Janus's logo while a page first loads - its orange bar
// filling up with the steps done, its two faces lighting in turn (Janus
// looking each way), the step under it. It shows for a second at least,
// never longer than 8 s (a step that hangs leaves the page to say why),
// fades, and leaves the DOM: it never stands in front of a click.
// Its times are CSS animations (theme.css), not timers: they run on the
// browser's own time, whatever a test does with the page's clock.
// Rules in docs/controller-ui.md.
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'

const SplashContext = createContext({ step: () => {}, finish: () => {}, reset: () => {}, generation: 0 })

// SplashProvider puts the splash over its children until every step of
// `steps` (keys, in order) is done, or finish() is called.
export function SplashProvider({ steps: order, children }) {
  const [steps, setSteps] = useState({}) // key -> { label, done }
  const [finished, setFinished] = useState(false)
  const [shownLongEnough, setShownLongEnough] = useState(false)
  const [capped, setCapped] = useState(false)
  const [gone, setGone] = useState(false)
  const [generation, setGeneration] = useState(0)
  const goneRef = useRef(false)
  useEffect(() => {
    goneRef.current = gone
  }, [gone])

  const step = useCallback((key, label, done) => {
    if (goneRef.current) return
    setSteps((s) => (s[key]?.label === label && s[key]?.done === done ? s : { ...s, [key]: { label, done } }))
  }, [])
  const finish = useCallback(() => setFinished(true), [])
  // reset shows the splash again (a sign-in, then the page's first load):
  // every step says again where it is.
  const reset = useCallback(() => {
    goneRef.current = false
    setSteps({})
    setFinished(false)
    setShownLongEnough(false)
    setCapped(false)
    setGone(false)
    setGeneration((g) => g + 1)
  }, [])

  const done = order.filter((k) => steps[k]?.done).length
  const ready = finished || done === order.length
  const leaving = (ready && shownLongEnough) || capped
  const onAnimationEnd = (e) => {
    if (e.animationName === 'splash-min') setShownLongEnough(true)
    if (e.animationName === 'splash-cap') setCapped(true)
    if (e.animationName === 'splash-out') setGone(true)
  }

  const value = useMemo(() => ({ step, finish, reset, generation }), [step, finish, reset, generation])
  const current = order.map((k) => steps[k]).find((s) => s && !s.done)
  return (
    <SplashContext.Provider value={value}>
      {children}
      {!gone && (
        <Splash
          key={generation}
          progress={ready ? 1 : done / order.length}
          label={ready ? '' : current?.label || 'Loading…'}
          leaving={leaving}
          onAnimationEnd={onAnimationEnd}
        />
      )}
    </SplashContext.Provider>
  )
}

// useSplashStep says where one of the splash's steps is: its label, and
// whether it's done.
export function useSplashStep(key, label, done) {
  const { step, generation } = useContext(SplashContext)
  useEffect(() => step(key, label, !!done), [step, key, label, done, generation])
}

export function useSplash() {
  return useContext(SplashContext)
}

function Splash({ progress, label, leaving, onAnimationEnd }) {
  const pct = Math.round(progress * 100)
  return (
    <div className={`splash${leaving ? ' leaving' : ''}`} role="status" aria-live="polite" onAnimationEnd={onAnimationEnd}>
      {/* The splash's times: a second at least, 8 s at most. */}
      <span className="splash-clock" aria-hidden="true" />
      {/* brand/favicon/favicon.svg, its bar a gauge */}
      <svg className="splash-logo" viewBox="8 8 84 84" width="112" height="112" aria-hidden="true">
        <defs>
          <clipPath id="splash-bar">
            <rect x="42" y="12" width="16" height="76" rx="3" />
          </clipPath>
        </defs>
        <rect className="splash-bar" x="42" y="12" width="16" height="76" rx="3" />
        <rect className="splash-fill" x="42" y="12" width="16" height="76" clipPath="url(#splash-bar)" style={{ transform: `scaleY(${progress})` }} />
        <path className="splash-face" d="M12 50L37 30V70Z" />
        <path className="splash-face right" d="M88 50L63 30V70Z" />
      </svg>
      <div className="splash-title">JANUS</div>
      <div className="splash-progress" role="progressbar" aria-label="Loading" aria-valuemin={0} aria-valuemax={100} aria-valuenow={pct}>
        <span style={{ width: `${pct}%` }} />
      </div>
      <div className="splash-step muted small">{label}</div>
    </div>
  )
}
