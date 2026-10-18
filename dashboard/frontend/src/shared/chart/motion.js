import { useEffect, useState } from 'react'

const REDUCE = '(prefers-reduced-motion: reduce)'

// useReducedMotion follows the system's "reduce motion" setting.
export function useReducedMotion() {
  const [reduce, setReduce] = useState(() => typeof window !== 'undefined' && !!window.matchMedia?.(REDUCE).matches)
  useEffect(() => {
    const mq = window.matchMedia?.(REDUCE)
    if (!mq) return undefined
    const on = () => setReduce(mq.matches)
    mq.addEventListener('change', on)
    return () => mq.removeEventListener('change', on)
  }, [])
  return reduce
}

// useInView says whether an element is on screen (or about to be): a
// chart scrolled away doesn't animate.
export function useInView(ref) {
  const [inView, setInView] = useState(true)
  useEffect(() => {
    const el = ref.current
    if (!el || typeof IntersectionObserver === 'undefined') return undefined
    const io = new IntersectionObserver(([e]) => setInView(e.isIntersecting), { rootMargin: '100px' })
    io.observe(el)
    return () => io.disconnect()
  }, [ref])
  return inView
}
