import { Check, Monitor, Moon, Palette, Sun } from 'lucide-react'
import { useEffect, useId, useRef, useState } from 'react'
import { PALETTES, setPalette, setPreference, useAppearance } from './appearance.js'

const PREFERENCE_OPTIONS = [
  { id: 'system', label: 'System', icon: Monitor },
  { id: 'light', label: 'Light', icon: Sun },
  { id: 'dark', label: 'Dark', icon: Moon },
]

// A palette's tile: the same little page in each of its modes, drawn in
// its own colours (palettes.css applies to any element carrying both
// attributes).
function Swatch({ palette, mode }) {
  return (
    <span className="tp-swatch" data-palette={palette} data-theme={mode}>
      <span className="tp-card">
        <span className="tp-rule" />
        <span className="tp-line" />
        <span className="tp-dots">
          <i className="c1" />
          <i className="c2" />
          <i className="c3" />
        </span>
      </span>
      <span className="tp-pill" />
    </span>
  )
}

// ThemePicker: the page's appearance - the mode (the system's, light or
// dark) and the palette, remembered per browser (appearance.js), applied
// at once to every open page of this Controller.
export default function ThemePicker() {
  const { preference, palette, mode } = useAppearance()
  const [open, setOpen] = useState(false)
  const wrap = useRef(null)
  const trigger = useRef(null)
  const name = useId()

  useEffect(() => {
    if (!open) return undefined
    const onDown = (e) => {
      if (!wrap.current?.contains(e.target)) setOpen(false)
    }
    const onKey = (e) => {
      if (e.key !== 'Escape') return
      setOpen(false)
      trigger.current?.focus()
    }
    document.addEventListener('pointerdown', onDown)
    document.addEventListener('keydown', onKey)
    // The checked mode takes the focus: arrows move within each group.
    wrap.current?.querySelector('input:checked')?.focus()
    return () => {
      document.removeEventListener('pointerdown', onDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  const current = PALETTES.find((p) => p.id === palette)?.name
  return (
    <div className="theme-picker" ref={wrap}>
      <button
        ref={trigger}
        className="ghost icon"
        title={`Appearance: ${current}, ${preference === 'system' ? `system (${mode})` : mode}`}
        aria-label="Appearance"
        aria-haspopup="dialog"
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
      >
        <Palette size={17} />
      </button>
      {open && (
        <div className="tp-panel card" role="dialog" aria-label="Appearance">
          <fieldset className="plain">
            <legend className="tp-legend">Mode</legend>
            <div className="tp-modes">
              {PREFERENCE_OPTIONS.map(({ id, label, icon: Icon }) => (
                <label key={id} className={`tp-mode${preference === id ? ' checked' : ''}`}>
                  <input type="radio" name={`${name}-mode`} value={id} checked={preference === id} onChange={() => setPreference(id)} />
                  <Icon size={15} aria-hidden="true" />
                  {label}
                </label>
              ))}
            </div>
          </fieldset>
          <fieldset className="plain">
            <legend className="tp-legend">Palette</legend>
            <div className="tp-palettes">
              {PALETTES.map((p) => (
                <label key={p.id} className={`tp-palette${palette === p.id ? ' checked' : ''}`}>
                  <input type="radio" name={`${name}-palette`} value={p.id} checked={palette === p.id} onChange={() => setPalette(p.id)} />
                  <span className="tp-swatches">
                    <Swatch palette={p.id} mode="light" />
                    <Swatch palette={p.id} mode="dark" />
                  </span>
                  <span className="tp-name">
                    {p.name}
                    {palette === p.id && <Check size={14} aria-hidden="true" />}
                  </span>
                </label>
              ))}
            </div>
          </fieldset>
          <div className="muted small">Saved in this browser.</div>
        </div>
      )}
    </div>
  )
}
