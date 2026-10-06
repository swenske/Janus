import { Monitor, Moon, Sun } from 'lucide-react'
import { useEffect, useState } from 'react'

const KEY = 'janus-theme'

function readStored() {
  try {
    return localStorage.getItem(KEY) || 'system'
  } catch {
    return 'system'
  }
}

function apply(theme) {
  const root = document.documentElement
  if (theme === 'system') root.removeAttribute('data-theme')
  else root.setAttribute('data-theme', theme)
}

// Apply the stored choice before React renders, so the page never
// flashes the wrong theme.
apply(readStored())

const ORDER = ['system', 'light', 'dark']
const ICONS = { system: Monitor, light: Sun, dark: Moon }

// ThemeToggle cycles system -> light -> dark, remembered per browser.
export function ThemeToggle() {
  const [theme, setTheme] = useState(readStored)
  useEffect(() => {
    apply(theme)
    try {
      localStorage.setItem(KEY, theme)
    } catch {
      // private window: the choice just isn't remembered
    }
  }, [theme])
  const Icon = ICONS[theme]
  const next = ORDER[(ORDER.indexOf(theme) + 1) % ORDER.length]
  return (
    <button className="ghost icon" title={`Theme: ${theme} (click for ${next})`} aria-label={`Theme: ${theme}`} onClick={() => setTheme(next)}>
      <Icon size={17} />
    </button>
  )
}

// Logo is the symbol, decorative: it's always beside the name.
export function Logo({ size = 28 }) {
  return <img src="/favicon.svg" width={size} height={size} alt="" style={{ display: 'block' }} />
}
