import { Check, Monitor, Moon, Palette, Sun } from 'lucide-react'
import { useEffect, useId, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { PALETTES, cyclePalette, setPalette, setPreference, setShortcut, shortcutOn, useAppearance } from './appearance.js'

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

// The shortcut doesn't type into a field.
const typing = (el) => el instanceof Element && !!el.closest('input, textarea, select, [contenteditable]:not([contenteditable="false"])')

// ThemePicker: the page's appearance - the mode (the system's, light or
// dark) and the palette, remembered per browser (appearance.js), applied
// at once to every open page of this Controller. T anywhere takes the
// next palette, shift+T the previous - unless turned off here.
export default function ThemePicker() {
  const { preference, palette, mode } = useAppearance()
  const [open, setOpen] = useState(false)
  const [shortcut, setShortcutState] = useState(shortcutOn)
  const wrap = useRef(null)
  const trigger = useRef(null)
  const name = useId()
  // The palette T just took, said a moment at the window's foot - each
  // press replaces it (a key: the fading starts again).
  const [said, setSaid] = useState(null)

  useEffect(() => {
    if (!shortcut) return undefined
    const onKey = (e) => {
      if ((e.key !== 't' && e.key !== 'T') || e.ctrlKey || e.metaKey || e.altKey || e.repeat || e.isComposing || typing(e.target)) return
      e.preventDefault()
      const p = cyclePalette(e.shiftKey ? -1 : 1)
      setSaid((s) => ({ name: p.name, key: (s?.key || 0) + 1 }))
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [shortcut])

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
      {said &&
        createPortal(
          <div key={said.key} className="tp-said" role="status" onAnimationEnd={() => setSaid(null)}>
            <span className="tp-swatch-dot" />
            {said.name}
            <span className="muted"> · {mode}</span>
          </div>,
          document.body,
        )}
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
          <label className="check small tp-shortcut">
            <input
              type="checkbox"
              checked={shortcut}
              onChange={(e) => {
                setShortcut(e.target.checked)
                setShortcutState(e.target.checked)
              }}
            />
            <span>
              <kbd>T</kbd> the next palette, <kbd>Shift</kbd>+<kbd>T</kbd> the previous - anywhere but in a field
            </span>
          </label>
          <div className="muted small">Saved in this browser.</div>
        </div>
      )}
    </div>
  )
}
